// Command amgo is a standalone CLI for native coding-agent session messaging.
package main

import (
	"os"

	"amgo/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
