package serve

import (
	"context"
	"strconv"

	"github.com/matthewmueller/joy/api/serve"
	"github.com/pkg/errors"
	kingpin "gopkg.in/alecthomas/kingpin.v2"
)

// New build command
func New(ctx context.Context, root *kingpin.Application) {
	cmd := root.Command("serve", "start a development server with livereload")
	packages := cmd.Arg("packages", "packages to bundle").Required().Strings()
	port := cmd.Flag("port", "port to serve from").Short('p').Default("8080").String()
	// dev := cmd.Flag("dev", "generate a development build").Short('d').Bool()
	joyPath := cmd.Flag("joy", "Joy state path").Hidden().String()

	cmd.Action(func(_ *kingpin.ParseContext) (err error) {
		port, e := strconv.Atoi(*port)
		if e != nil {
			return errors.Wrap(e, "invalid port")
		}

		// serve the files
		if err := serve.Serve(&serve.Config{
			Context:     ctx,
			Packages:    *packages,
			Port:        port,
			Development: true,
			JoyPath:     *joyPath,
		}); err != nil {
			return errors.Wrapf(err, "error serving")
		}

		return nil
	})
}
