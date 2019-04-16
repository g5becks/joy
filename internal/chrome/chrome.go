package chrome

import (
	"context"
	"io"
	"io/ioutil"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/apex/log"
	"github.com/cenkalti/backoff"
	"github.com/mafredri/cdp"
	"github.com/mafredri/cdp/devtool"
	"github.com/mafredri/cdp/protocol/runtime"
	"github.com/mafredri/cdp/rpcc"
	"github.com/pkg/errors"

	"golang.org/x/sync/errgroup"
)

// Chrome struct
type Chrome struct {
	settings *Settings
	addr     url.URL
	cmd      *exec.Cmd
	ctx      context.Context
	eg       *errgroup.Group
}

// Settings struct
type Settings struct {
	ExecutablePath string
	Flags          []string
	Stdout         io.Writer
	Stderr         io.Writer
	Addr           string
}

var defaultFlags = []string{
	"--disable-background-networking",
	"--disable-background-timer-throttling",
	"--disable-client-side-phishing-detection",
	"--disable-default-apps",
	"--disable-extensions",
	"--disable-hang-monitor",
	"--disable-popup-blocking",
	"--disable-prompt-on-repost",
	"--disable-sync",
	"--disable-translate",
	"--metrics-recording-only",
	"--no-first-run",
	"--safebrowsing-disable-auto-update",

	"--enable-automation",
	"--password-store=basic",
	"--use-mock-keychain",

	"--headless",
	"--disable-gpu",
	"--hide-scrollbars",
	"--mute-audio",
}

var errStopped = errors.New("stopped")

// Start chrome, downloading if necessary
func Start(ctx context.Context, chromePath string) (*Chrome, error) {
exists:
	fullpath, err := Exists(chromePath)
	if err != nil {
		return nil, errors.Wrapf(err, "couldn't find chrome")
	} else if fullpath == "" {
		log.Infof("downloading headless chrome (this only needs to be done once)")
		if err := Download(chromePath); err != nil {
			return nil, errors.Wrapf(err, "error downloading chrome")
		}
		goto exists
	}

	return New(ctx, &Settings{
		ExecutablePath: fullpath,
		Stderr:         ioutil.Discard,
		Stdout:         ioutil.Discard,
	})
}

// New chrome
func New(parent context.Context, settings *Settings) (*Chrome, error) {
	eg, ctx := errgroup.WithContext(parent)

	if settings.Stdout == nil {
		settings.Stdout = os.Stdout
	}

	addr, err := resolveAddr(settings.Addr)
	if err != nil {
		return nil, err
	}

	// default flags
	flags := append(
		settings.Flags,
		defaultFlags...,
	)

	// user data dir
	tmp := os.TempDir()
	flags = append(flags, "--user-data-dir="+path.Join(tmp))

	// remote debugging port
	flags = append(flags, "--remote-debugging-port="+addr.Port())

	// create the command
	cmd := exec.CommandContext(ctx, settings.ExecutablePath, flags...)

	// copy from stdout
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	} else if settings.Stdout == nil {
		// TODO: write stdout to log.Debug
		settings.Stdout = ioutil.Discard
	}
	go io.Copy(settings.Stdout, stdout)

	// copy from stderr
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	} else if settings.Stderr == nil {
		// TODO: write stderr to log.Debug
		settings.Stderr = ioutil.Discard
	}
	go io.Copy(settings.Stderr, stderr)

	// run the command
	if e := cmd.Start(); e != nil {
		return nil, e
	}

	// wait until the command exits
	eg.Go(cmd.Wait)

	c := &Chrome{
		settings: settings,
		addr:     addr,
		cmd:      cmd,
		eg:       eg,
		ctx:      ctx,
	}

	// handle stopping
	// when ctx is cancelled
	eg.Go(c.doClose)

	return c, nil
}

// Target struct
type Target struct {
	devtools *devtool.DevTools
	target   *devtool.Target
	eg       *errgroup.Group
	ctx      context.Context
	chrome   *Chrome
	conn     *rpcc.Conn
}

// Target creates a new target
func (c *Chrome) Target() (*Target, error) {
	eg, ctx := errgroup.WithContext(c.ctx)

	// connect to the headless chrome
	devt, tar, conn, err := dial(ctx, c.addr.String())
	if err != nil {
		return nil, err
	}

	t := &Target{
		chrome:   c,
		devtools: devt,
		target:   tar,
		ctx:      ctx,
		eg:       eg,
		conn:     conn,
	}

	eg.Go(t.doClose)

	return t, nil
}

func dial(parent context.Context, addr string) (*devtool.DevTools, *devtool.Target, *rpcc.Conn, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()

	b := backoff.WithContext(backoff.NewExponentialBackOff(), ctx)
	devt := devtool.New(addr)

	for {
		tar, conn, err := doDial(ctx, devt)
		if err == nil {
			return devt, tar, conn, nil
		}

		sleep := b.NextBackOff()
		if sleep == backoff.Stop {
			return nil, nil, nil, errors.New("target dial timed out")
		}

		time.Sleep(sleep)
	}
}

func doDial(ctx context.Context, devt *devtool.DevTools) (*devtool.Target, *rpcc.Conn, error) {
	dt, err := devt.Create(ctx)
	if err != nil {
		return nil, nil, errors.Wrap(err, "devtool create error")
	}

	// Initiate a new RPC connection to the Chrome Debugging Protocol target.
	// TODO: check 1MB buffer size. I'm not sure chrome can fully handle this with the headers and whatnot.
	conn, err := rpcc.DialContext(ctx, dt.WebSocketDebuggerURL, rpcc.WithWriteBufferSize(1000000))
	if err != nil {
		return nil, nil, errors.Wrap(err, "rpcc error")
	}

	return dt, conn, nil
}

// Run the script
func (t *Target) Run(source string) (result string, err error) {
	c := cdp.NewClient(t.conn)
	ctx := t.ctx

	if e := c.Runtime.Enable(ctx); e != nil {
		return "", e
	}

	console, err := c.Runtime.ConsoleAPICalled(ctx)
	if err != nil {
		return "", err
	}
	defer console.Close()

	var lines []string
	// done := make(chan bool)
	go func() {
		for {
			msg, e := console.Recv()
			if e != nil {
				return
			}
			var line []string
			for _, arg := range msg.Args {
				line = append(line, strings.TrimSpace(formatValue(arg)))
			}

			// TODO: finish support for fmt.Printf("hi %s, mars")
			// if len(line) > 1 {
			// 	var args []interface{}
			// 	for i := 1; i < len(line); i++ {
			// 		args = append(args, line[i])
			// 	}
			// 	lines = append(lines, fmt.Sprintf(line[0], args...))
			// 	continue
			// }

			lines = append(lines, strings.Join(line, " "))
		}
	}()

	// TODO: HACK but probably will work (until I compile with regenerator)
	awaitPromise := false
	if strings.Contains(source, "async function main") {
		awaitPromise = true
	}

	// evaluate
	generatePreview := true
	res, err := c.Runtime.Evaluate(ctx, &runtime.EvaluateArgs{
		Expression:      source,
		AwaitPromise:    &awaitPromise,
		GeneratePreview: &generatePreview,
	})

	if err != nil {
		return "", err
	}
	if res.ExceptionDetails != nil {
		return "", res.ExceptionDetails
	}

	// give it a bit of time for the console event to come back
	// TODO: better way to do this?
	time.Sleep(100 * time.Millisecond)

	return strings.Join(lines, "\n"), nil
}

func (t *Target) doClose() error {
	<-t.ctx.Done()

	// close the connection
	// TODO: multierror
	t.conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return t.devtools.Close(ctx, t.target)
}

// Close the target
func (t *Target) Close() (err error) {
	t.eg.Go(func() error { return errStopped })
	if e := t.eg.Wait(); e != errStopped {
		return e
	}
	return nil
}

// Close with an error
func (c *Chrome) Close() error {
	c.eg.Go(func() error { return errStopped })
	if e := c.eg.Wait(); e != errStopped {
		return e
	}
	return nil
}

// if our context is cancelled,
// try stopping the process
func (c *Chrome) doClose() error {
	<-c.ctx.Done()

	if c.cmd.Process == nil {
		return nil
	}

	// sigint
	if e := c.cmd.Process.Signal(syscall.SIGINT); e != nil {
		return e
	}

	// wait until the command exits
	done := make(chan error, 1)
	go func() {
		done <- c.cmd.Wait()
	}()

	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		if c.cmd.Process == nil {
			return nil
		}
		// kill after the context elapsed
		c.cmd.Process.Signal(syscall.SIGKILL)
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			return errors.New("couldn't kill process after 10s")
		}
	}
}

// resolves an chrome devtools address
func resolveAddr(address string) (u url.URL, err error) {
	var port int

	hostname := "127.0.0.1"
	defaultURL := url.URL{
		Scheme: "http",
		Host:   "127.0.0.1:9222",
	}

	if address == "" {
		port, err = findPort()
		if err != nil {
			return u, errors.Wrap(err, "unable to find an available port")
		}
		defaultURL.Host = hostname + ":" + strconv.Itoa(port)
		return defaultURL, nil
	}

	addr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		return u, errors.Wrap(err, "did not understand the address")
	}

	if addr.IP != nil {
		hostname = addr.IP.String()
	}

	if addr.Port == 0 {
		port, err = findPort()
		if err != nil {
			return u, errors.Wrap(err, "unable to find an available port")
		}
	} else {
		port = addr.Port
	}

	defaultURL.Host = hostname + ":" + strconv.Itoa(port)
	return defaultURL, nil
}

// get an available port
func findPort() (int, error) {
	addr, err := net.ResolveTCPAddr("tcp", "localhost:0")
	if err != nil {
		return 0, err
	}

	l, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return 0, err
	}
	defer l.Close()

	return l.Addr().(*net.TCPAddr).Port, nil
}
