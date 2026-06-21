package main

import (
	"fmt"
	"os"

	"github.com/alecthomas/kong"

	"github.com/wimpysworld/nyala/internal/app"
)

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

	if err := app.Run(cli, "testdata", os.Stdin); err != nil {
		fmt.Fprintln(os.Stderr, "nyala:", err)
		os.Exit(1)
	}
}
