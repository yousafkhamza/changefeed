// Command changefeed records every production change on one timeline.
package main

import (
	"os"

	"github.com/yousafkhamza/changefeed/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
