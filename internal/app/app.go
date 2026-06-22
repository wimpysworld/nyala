// Package app holds the Nyala command-line surface and its dispatch. It owns one
// CLI struct, parsed by kong in cmd/nyala, and routes a parsed run to the viewer.
// The dispatch is thin: with no image it prints the welcome line, otherwise it
// hands the positional PNG to internal/viewer. It holds no rendering logic of its
// own.
package app

import (
	"fmt"
	"os"

	"github.com/alecthomas/kong"

	"github.com/wimpysworld/nyala/internal/viewer"
)

// Message returns the Nyala welcome line shown when the binary runs with no
// viewer target.
func Message() string {
	return "Welcome to Nyala."
}

// CLI is the parsed Nyala command line. cmd/nyala binds it to kong; the fields
// carry the colour mode and the optional viewer target. With no positional image
// the run prints the welcome line; otherwise the run is the viewer over the PNG.
type CLI struct {
	Version kong.VersionFlag `help:"Show version."`

	Color string `help:"Viewer colour mode: auto, truecolor, 256, or none." default:"auto" enum:"auto,truecolor,256,none"`

	Image string `arg:"" optional:"" help:"PNG to view."`
}

// Run dispatches a parsed CLI on the given tty. With no positional image it
// prints the welcome line and returns nil; otherwise it runs the viewer over the
// image, which detects terminal capabilities and resolves the colour mode.
func Run(cli CLI, tty *os.File) error {
	if cli.Image == "" {
		fmt.Println(Message())
		return nil
	}
	return viewer.Run(cli.Image, tty, cli.Color)
}
