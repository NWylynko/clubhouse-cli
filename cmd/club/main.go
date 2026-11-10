package main

import (
	"os"

	"github.com/nwylynko/clubhouse-cli/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
