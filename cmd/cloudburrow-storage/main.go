// Command cloudburrow-storage is the builtin Cloud Storage server alone, the
// binary the in-cluster storage Deployment runs (#514). It is cross-built for
// Linux and embedded in the cloudburrow CLI, which builds the image from it at
// `up`, so no image is published or downloaded. It takes the flags of
// `cloudburrow storage-server`.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/cloudburrow/cloudburrow/internal/storageserver"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := storageserver.Run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if !errors.Is(err, storageserver.ErrUsage) {
			fmt.Fprintln(os.Stderr, "cloudburrow-storage:", err)
		}
		os.Exit(2)
	}
}
