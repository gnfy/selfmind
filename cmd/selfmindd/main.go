package main

import (
	"context"
	"fmt"
	"os"

	gatewayrt "selfmind/internal/runtime/gateway"
)

func main() {
	if err := gatewayrt.Run(context.Background(), gatewayrt.Options{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
