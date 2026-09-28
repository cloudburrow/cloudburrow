package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/doctor"
	"github.com/cloudburrow/cloudburrow/internal/storageimage"
)

// storageEmbedResult is doctor's row for the builtin Cloud Storage server
// this CLI embeds (#686): which Linux builds it has, and whether one is for
// nodeArch. A plain `go build` or `go install` embeds none since #585.
func storageEmbedResult(cfg config.Config, nodeArch string) doctor.Result {
	missing := map[string]string{}
	for arch, err := range storageimage.Check() {
		var ne *storageimage.NotEmbeddedError
		switch {
		case err == nil:
			missing[arch] = ""
		case errors.As(err, &ne):
			missing[arch] = ne.Reason
		default:
			missing[arch] = err.Error()
		}
	}
	// BigQuery's validating front runs from the same image (#902).
	return doctor.EmbeddedStorage(needsStorageImage(cfg), nodeArch, missing)
}

// needsStorageImage reports whether `up` builds the cloudburrow-storage
// image: for Cloud Storage's server, and for BigQuery's validating front,
// which runs in the emulator's pod (#902).
func needsStorageImage(cfg config.Config) bool {
	return serviceEnabled(cfg, config.ServiceStorage) || serviceEnabled(cfg, config.ServiceBigQuery)
}

// preflightStorage refuses `up` with Cloud Storage or BigQuery enabled from a CLI that
// has no storage server for the node's architecture, before anything is
// created. Found by the storage-image component instead, it failed after
// kind had spent minutes creating a cluster it then left behind.
func preflightStorage(cfg config.Config, nodeArch string, stderr io.Writer) error {
	res := storageEmbedResult(cfg, nodeArch)
	if res.Level != doctor.LevelFail {
		return nil
	}
	fmt.Fprintf(stderr, "cloudburrow up: this CLI cannot start Cloud Storage or BigQuery; nothing was created\n\n")
	doctor.Report{Results: []doctor.Result{res}}.Write(stderr)
	_, err := storageimage.Binary(nodeArch)
	if err == nil {
		err = storageimage.ErrNotEmbedded
	}
	return fmt.Errorf("Cloud Storage or BigQuery is enabled, but %w; or pass --services without storage and bigquery. Nothing was created", err)
}
