// lux is the Lux command-line client. See `lux help`.
package main

import (
	"os"

	"github.com/open-gitagent/lux/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
