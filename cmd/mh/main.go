// Command mh is the docker-style command-line client for the microhosted
// daemon. See internal/cli and docs/cli.md.
package main

import (
	"os"

	"microhosted/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
