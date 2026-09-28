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

	"github.com/cloudburrow/cloudburrow/internal/bigqueryimage"
	"github.com/cloudburrow/cloudburrow/internal/bigqueryimage/bigqueryimagetest"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/doctor"
	"github.com/cloudburrow/cloudburrow/internal/prefetch"
	"github.com/cloudburrow/cloudburrow/internal/storageimage/storageimagetest"
)

// A CLI built without the BigQuery emulator (#1061) refuses `up` with
// BigQuery enabled before it creates anything, naming the fix, as it does
// without the storage server.
func TestUpRefusesACLIWithoutTheBigQueryEmulatorBeforeCreatingACluster(t *testing.T) {
	storageimagetest.Present(t)
	bigqueryimagetest.Missing(t)
	log := fakeTools(t)
	stateDir := t.TempDir()
	args := append([]string{"--state-dir", stateDir, "--name", "nobq", "--services", "bigquery"}, osAssignedPortsBut("")...)
	var stderr bytes.Buffer
	err := runUp(context.Background(), args, io.Discard, &stderr)
	if !errors.Is(err, bigqueryimage.ErrNotEmbedded) {
		t.Fatalf("runUp() = %v, want ErrNotEmbedded", err)
	}
	for _, want := range []string{"make build", "make bigquery-binaries", "--services without bigquery", "Nothing was created"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q: %v", want, err)
		}
	}
	if !strings.Contains(stderr.String(), "FAIL    embedded bigquery") {
		t.Errorf("stderr has no doctor row:\n%s", stderr.String())
	}
	if got := calls(t, log); strings.Contains(got, "kind create") {
		t.Errorf("before refusing, up ran:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "nobq")); err == nil {
		t.Error("the refusal created the instance directory")
	}
}

// Without BigQuery the same CLI is not refused; with the emulator embedded,
// BigQuery is not either, and prefetch plans the image this CLI builds
// instead of pulling one.
func TestBigQueryPreflightAndOfflinePlan(t *testing.T) {
	load := func(services string) config.Config {
		t.Helper()
		cfg, err := config.Load(config.Options{Args: []string{"--state-dir", t.TempDir(), "--services", services},
			Output: io.Discard, Getenv: func(string) string { return "" }})
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	t.Run("missing, without BigQuery", func(t *testing.T) {
		bigqueryimagetest.Missing(t)
		cfg := load("tasks")
		if err := preflightBigQuery(cfg, "arm64", io.Discard); err != nil {
			t.Errorf("preflight = %v", err)
		}
		if r := bigQueryEmbedResult(cfg, "arm64"); r.Level != doctor.LevelOK || !strings.Contains(r.Detail, "not needed") {
			t.Errorf("doctor row = %+v", r)
		}
		if plan := offlinePlan(context.Background(), cfg, prefetch.ExecRunner{}); plan.BigQuery {
			t.Errorf("the plan builds a BigQuery image without BigQuery: %+v", plan)
		}
	})
	t.Run("present, with BigQuery", func(t *testing.T) {
		bigqueryimagetest.Present(t)
		cfg := load("bigquery")
		if err := preflightBigQuery(cfg, "arm64", io.Discard); err != nil {
			t.Errorf("preflight = %v", err)
		}
		plan := offlinePlan(context.Background(), cfg, prefetch.ExecRunner{})
		want, err := bigqueryimage.TagFor(plan.Arch)
		if err != nil {
			t.Fatal(err)
		}
		if !plan.BigQuery || plan.BigQueryImage != want {
			t.Errorf("plan BigQuery %v %q, want %q", plan.BigQuery, plan.BigQueryImage, want)
		}
		for ref := range plan.Images {
			if strings.Contains(ref, "bigquery") {
				t.Errorf("the plan pulls %s", ref)
			}
		}
	})
}
