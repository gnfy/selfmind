package main

import (
	"context"
	"os"

	"selfmind/internal/cliapp"
)

func main() {
	os.Exit(cliapp.Run(context.Background(), os.Args, os.Stdin, os.Stdout, os.Stderr))
}
