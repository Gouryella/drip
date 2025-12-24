package main

import (
	"fmt"
	"os"

	"drip/internal/client/cli"
)

var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildTime = "unknown"
)

func main() {
	cli.SetVersion(Version, GitCommit, BuildTime)

	if err := cli.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
