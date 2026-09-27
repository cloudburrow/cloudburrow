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
	return doctor.EmbeddedStorage(serviceEnabled(cfg, config.ServiceStorage), nodeArch, missing)
}

// preflightStorage refuses `up` with Cloud Storage enabled from a CLI that
// has no storage server for the node's architecture, before anything is
// created. Found by the storage-image component instead, it failed after
// kind had spent minutes creating a cluster it then left behind.
func preflightStorage(cfg config.Config, nodeArch string, stderr io.Writer) error {
	res := storageEmbedResult(cfg, nodeArch)
	if res.Level != doctor.LevelFail {
		return nil
	}
	fmt.Fprintf(stderr, "cloudburrow up: this CLI cannot start Cloud Storage; nothing was created\n\n")
	doctor.Report{Results: []doctor.Result{res}}.Write(stderr)
	_, err := storageimage.Binary(nodeArch)
	if err == nil {
		err = storageimage.ErrNotEmbedded
	}
	return fmt.Errorf("Cloud Storage is enabled, but %w; or pass --services without storage. Nothing was created", err)
}
