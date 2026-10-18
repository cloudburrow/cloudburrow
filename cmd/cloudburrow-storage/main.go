// Command cloudburrow-storage is the builtin Cloud Storage server alone, the
// binary the in-cluster storage Deployment runs (#514). It is cross-built for
// Linux and embedded in the cloudburrow CLI, which builds the image from it at
// `up`, so no image is published or downloaded. It takes the flags of
// `cloudburrow storage-server`.
//
// `cloudburrow-storage bigquery-front` is instead the validating front the
// BigQuery pod runs beside the emulator (internal/bigqueryfront, #902), and
// `cloudburrow-storage pubsub-front` the front the Pub/Sub pod runs beside
// Google's emulator (internal/pubsubfront, #873), and `cloudburrow-storage
// bigtable-front` the front the Bigtable pod runs beside Google's emulator
// (internal/bigtablefront, #1206), and `cloudburrow-storage call-front` the
// call front the Spanner, Firestore and Datastore pods run beside theirs
// (internal/callfront, #1288), all from the same image, so the cluster
// needs no second locally built image.
//
// `cloudburrow-storage supervise [flags] -- command...` runs a backend's
// process inside its container and restarts it at once when it ends or the
// front fails it (internal/supervisor, #1091), and `cloudburrow-storage
// install-self <path>` copies the binary to path, for the init container
// that puts the supervisor in the backend's container.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/cloudburrow/cloudburrow/internal/bigqueryfront"
	"github.com/cloudburrow/cloudburrow/internal/bigtablefront"
	"github.com/cloudburrow/cloudburrow/internal/callfront"
	"github.com/cloudburrow/cloudburrow/internal/debugpprof"
	"github.com/cloudburrow/cloudburrow/internal/pubsubfront"
	"github.com/cloudburrow/cloudburrow/internal/storageserver"
	"github.com/cloudburrow/cloudburrow/internal/supervisor"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Off unless CLOUDBURROW_DEBUG_PPROF names a loopback address (#1609).
	// install-self is an init container's one-shot copy: it never listens.
	// A debug aid that cannot listen (two containers of a pod given the
	// same port) is named, and the command goes on without it.
	if len(os.Args) < 2 || os.Args[1] != "install-self" {
		if _, err := debugpprof.Start(os.Getenv, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, "cloudburrow-storage: warning:", err)
		}
	}
	if len(os.Args) > 1 && os.Args[1] == "bigquery-front" {
		if err := bigqueryfront.Run(ctx, os.Args[2:], os.Stdout, os.Stderr); err != nil {
			if !errors.Is(err, bigqueryfront.ErrUsage) {
				fmt.Fprintln(os.Stderr, "cloudburrow-storage bigquery-front:", err)
			}
			os.Exit(2)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "supervise" {
		if err := supervisor.Run(ctx, os.Args[2:], os.Stdout, os.Stderr); err != nil {
			if !errors.Is(err, supervisor.ErrUsage) {
				fmt.Fprintln(os.Stderr, "cloudburrow-storage supervise:", err)
			}
			os.Exit(2)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "install-self" {
		if len(os.Args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: cloudburrow-storage install-self <path>")
			os.Exit(2)
		}
		if err := supervisor.InstallSelf(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, "cloudburrow-storage install-self:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "bigtable-front" {
		if err := bigtablefront.Run(ctx, os.Args[2:], os.Stdout, os.Stderr); err != nil {
			if !errors.Is(err, bigtablefront.ErrUsage) {
				fmt.Fprintln(os.Stderr, "cloudburrow-storage bigtable-front:", err)
			}
			os.Exit(2)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "call-front" {
		if err := callfront.Run(ctx, os.Args[2:], os.Stdout, os.Stderr); err != nil {
			if !errors.Is(err, callfront.ErrUsage) {
				fmt.Fprintln(os.Stderr, "cloudburrow-storage call-front:", err)
			}
			os.Exit(2)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "pubsub-front" {
		if err := pubsubfront.Run(ctx, os.Args[2:], os.Stdout, os.Stderr); err != nil {
			if !errors.Is(err, pubsubfront.ErrUsage) {
				fmt.Fprintln(os.Stderr, "cloudburrow-storage pubsub-front:", err)
			}
			os.Exit(2)
		}
		return
	}
	if err := storageserver.Run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if !errors.Is(err, storageserver.ErrUsage) {
			fmt.Fprintln(os.Stderr, "cloudburrow-storage:", err)
		}
		os.Exit(2)
	}
}
