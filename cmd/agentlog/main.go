// Command agentlog reads the session history left behind by AI coding agents
// and makes it searchable, readable and portable.
package main

import (
	"os"

	"github.com/liyixuan201211/agentlog/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:]))
}
