package main

import (
	"os"

	"github.com/chuck-chuck-chuck-net/slaptain/operator/cmd/slctl/cmd"
)

func main() {
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}
