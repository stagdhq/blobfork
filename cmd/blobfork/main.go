// Command blobfork forks a blob container: a proxy in front of Azurite that
// gives the app a container holding every file of the real one, its own
// writes on top, and the real container left as it is.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/stagdhq/blobfork/internal/cli"
)

// Set at build time: -ldflags "-X main.version=v0.1.0".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := cli.New(version).Run(ctx, os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "blobfork:", err)
		os.Exit(1)
	}
}
