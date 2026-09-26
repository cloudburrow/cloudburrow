package main

import (
	"context"
	"errors"
	"io"

	"github.com/cloudburrow/cloudburrow/internal/storageserver"
)

// runStorageServer is `cloudburrow storage-server`: the builtin Cloud Storage
// server on its own (#488), shared with the cloudburrow-storage binary the
// in-cluster Deployment runs (#514).
func runStorageServer(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	err := storageserver.Run(ctx, args, stdout, stderr)
	if errors.Is(err, storageserver.ErrUsage) {
		return errUsage
	}
	return err
}
