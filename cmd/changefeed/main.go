// Command changefeed records every production change on one timeline.
package main

import (
	"os"
	_ "time/tzdata" // freeze windows use IANA time zones; the image may lack /usr/share/zoneinfo

	"github.com/yousafkhamza/changefeed/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
