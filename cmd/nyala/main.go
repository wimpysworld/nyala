package main

import (
	"fmt"

	"github.com/alecthomas/kong"

	"github.com/wimpysworld/nyala/internal/app"
)

var version = "dev"

var cli struct {
	Version kong.VersionFlag `help:"Show version."`
}

func main() {
	kong.Parse(&cli,
		kong.Name("nyala"),
		kong.Description("A browser for the terminal."),
		kong.UsageOnError(),
		kong.Vars{"version": version},
	)

	fmt.Println(app.Message())
}
