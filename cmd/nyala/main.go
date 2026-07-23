package main

import (
	"fmt"
	"os"

	"github.com/alecthomas/kong"

	"github.com/wimpysworld/nyala/internal/app"
)

// version is overridden at release time via -ldflags -X main.version; it stays
// "dev" for local builds and feeds kong's {version} template variable and the
// viewer's About line.
var version = "dev"

func main() {
	var cli app.CLI
	kong.Parse(
		&cli,
		kong.Name("nyala"),
		kong.Description("A browser for the terminal."),
		kong.UsageOnError(),
		kong.Vars{"version": version},
	)

	if err := app.Run(cli, version, os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, "nyala:", err)
		os.Exit(1)
	}
}
