package main

import (
	"context"
	"fmt"
	"os"

	"selfmind/internal/cliapp"
	"selfmind/internal/crashreport"
)

func main() {
	os.Exit(run())
}

func run() (exitCode int) {
	defer func() {
		if recovered := recover(); recovered != nil {
			path, err := crashreport.Write(recovered)
			if err != nil {
				fmt.Fprintf(os.Stderr, "SelfMind crashed: %v\n", recovered)
			} else {
				fmt.Fprintf(os.Stderr, "SelfMind crashed. A local report was written to %s\n", path)
			}
			exitCode = 2
		}
	}()
	return cliapp.Run(context.Background(), os.Args, os.Stdin, os.Stdout, os.Stderr)
}
