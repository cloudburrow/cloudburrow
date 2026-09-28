package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/doctor"
	"github.com/cloudburrow/cloudburrow/internal/storageimage"
	"github.com/cloudburrow/cloudburrow/internal/storageimage/storageimagetest"
)

// A CLI built by a plain `go build ./cmd/cloudburrow` embeds no storage
// server (#585). With the default services, which include Cloud Storage,
// `up` refuses before it creates anything (#686): no `kind create`, no
// instance directory, and only `docker version` asked of Docker, for the
// node's architecture. It once found this in the storage-image component,
// after kind had created the cluster.
func TestUpRefusesACLIWithoutTheStorageServerBeforeCreatingACluster(t *testing.T) {
	storageimagetest.Missing(t)
	log := fakeTools(t)
	stateDir := t.TempDir()
	args := append([]string{"--state-dir", stateDir, "--name", "nostorage"}, osAssignedPortsBut("")...)
	var stderr bytes.Buffer
	err := runUp(context.Background(), args, io.Discard, &stderr)
	if !errors.Is(err, storageimage.ErrNotEmbedded) {
		t.Fatalf("runUp() = %v, want ErrNotEmbedded", err)
	}
	for _, want := range []string{"make build", "make storage-binaries", "release", "--services without storage", "no linux/arm64 build", "Nothing was created"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q: %v", want, err)
		}
	}
	if !strings.Contains(stderr.String(), "FAIL    embedded storage") || !strings.Contains(stderr.String(), "nothing was created") {
		t.Errorf("stderr has no doctor row:\n%s", stderr.String())
	}
	got := strings.TrimSpace(calls(t, log))
	if got != "docker version --format {{.Server.Arch}}" || strings.Contains(got, "kind create") {
		t.Errorf("before refusing, up ran:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "nostorage")); err == nil {
		t.Error("the refusal created the instance directory")
	}
}

// Without Cloud Storage or BigQuery the same CLI is not refused, and doctor
// passes the row; BigQuery alone needs the image for its front (#902); with
// the server embedded, storage is not refused either.
func TestStoragePreflightPassesWithoutStorageOrWithTheServer(t *testing.T) {
	load := func(services ...string) config.Config {
		t.Helper()
		args := []string{"--state-dir", t.TempDir()}
		if len(services) > 0 {
			args = append(args, "--services", strings.Join(services, ","))
		}
		cfg, err := config.Load(config.Options{Args: args, Output: io.Discard, Getenv: func(string) string { return "" }})
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	t.Run("missing, without storage", func(t *testing.T) {
		storageimagetest.Missing(t)
		cfg := load("pubsub", "tasks")
		if err := preflightStorage(cfg, "arm64", io.Discard); err != nil {
			t.Errorf("preflight = %v", err)
		}
		if r := storageEmbedResult(cfg, "arm64"); r.Level != doctor.LevelOK || !strings.Contains(r.Detail, "not needed") {
			t.Errorf("doctor row = %+v", r)
		}
	})
	t.Run("missing, with BigQuery alone", func(t *testing.T) {
		storageimagetest.Missing(t)
		if err := preflightStorage(load("bigquery", "tasks"), "arm64", io.Discard); err == nil {
			t.Error("BigQuery, whose front runs from the storage image (#902), started without it")
		}
	})
	t.Run("missing, with storage", func(t *testing.T) {
		storageimagetest.Missing(t)
		r := storageEmbedResult(load(), "amd64")
		if r.Level != doctor.LevelFail || !(doctor.Report{Results: []doctor.Result{r}}).Blocking() ||
			!strings.Contains(r.Detail, "none embedded") || !strings.Contains(r.Remedy, "make build") {
			t.Errorf("doctor row = %+v", r)
		}
	})
	t.Run("placeholder", func(t *testing.T) {
		storageimagetest.Use(t, storageimagetest.FS(map[string][]byte{
			"cloudburrow-storage-linux-amd64": {},
			"cloudburrow-storage-linux-arm64": []byte("placeholder"),
		}))
		err := preflightStorage(load(), "arm64", io.Discard)
		if !errors.Is(err, storageimage.ErrNotEmbedded) || !strings.Contains(err.Error(), "not an executable") {
			t.Errorf("preflight = %v", err)
		}
	})
	t.Run("present", func(t *testing.T) {
		storageimagetest.Present(t)
		cfg := load()
		if err := preflightStorage(cfg, "arm64", io.Discard); err != nil {
			t.Errorf("preflight = %v", err)
		}
		r := storageEmbedResult(cfg, "amd64")
		if r.Level != doctor.LevelOK || r.Detail != "linux/amd64, linux/arm64 embedded, matching the node (linux/amd64)" {
			t.Errorf("doctor row = %+v", r)
		}
	})
}
