package main

import (
	"fmt"
	"os"
	"rcm/internal/cli"
)

func main() {
	if err := cli.Run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "rcm:", err)
		os.Exit(1)
	}
}
