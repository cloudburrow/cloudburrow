//go:build compat

package compat

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
)

// restartEmulatorPath is the BigQuery front's path that restarts the
// emulator as its SQL engine's failure does
// (internal/bigqueryfront/engine.go, RestartEmulatorPath).
const restartEmulatorPath = "/cloudburrow/bigquery-restart-emulator"

// emulatorRestartsInARow is how many restarts TestBigQueryEmulatorRestartsPromptly
// forces: more than the seven after which Kubernetes' back-off kept the
// emulator down for minutes (#1091).
const emulatorRestartsInARow = 8

// emulatorBackWithin bounds each restart: the supervisor starts the
// emulator's process again within a second or two (internal/supervisor);
// Kubernetes' back-off reached five minutes.
const emulatorBackWithin = 20 * time.Second

// TestBigQueryEmulatorRestartsPromptly (#1091): the emulator restarted
// eight times in a row, as its SQL engine's failure restarts it (#989), is
// back within seconds each time, however many restarts came before; while
// it restarts the front answers 503 backendError with Retry-After, never
// 502 "connection refused"; and the official Go client, which retries a
// 503, lists datasets through a restart without an error. Each restart
// empties the emulator (it keeps its data in memory): a dataset made
// before it is gone after it.
//
// Measured first, on main: Kubernetes restarted the emulator's container,
// and after the seventh restart on one instance its back-off
// (CrashLoopBackOff, "back-off 5m0s") kept it down for minutes, every
// request answered 502 "dial tcp 127.0.0.1:9051: connect: connection
// refused".
//
// covers: bigquery.datasets.list, bigquery.datasets.insert, bigquery.datasets.get
func TestBigQueryEmulatorRestartsPromptly(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	endpoint := h.Endpoint(EnvBigQuery)
	list := endpoint + "/bigquery/v2/projects/" + project + "/datasets"
	restart := func(i int) {
		t.Helper()
		resp, err := http.Post(endpoint+restartEmulatorPath, "application/json", nil)
		if err != nil {
			t.Fatalf("restart %d: POST %s: %v", i, restartEmulatorPath, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("restart %d: POST %s: %d, want 202", i, restartEmulatorPath, resp.StatusCode)
		}
	}
	var took []string
	for i := 1; i <= emulatorRestartsInARow; i++ {
		ds := c.Dataset(fmt.Sprintf("%s_restart_%d", strings.ReplaceAll(h.Project(), "-", "_"), i))
		if err := ds.Create(ctx, nil); err != nil {
			t.Fatalf("restart %d: create dataset: %v", i, err)
		}
		restart(i)
		began := time.Now()
		unavailable := 0
		for {
			resp, err := http.Get(list)
			if err != nil {
				t.Fatalf("restart %d: datasets.list: %v", i, err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
			if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" ||
				!strings.Contains(string(body), `"reason":"backendError"`) {
				t.Fatalf("restart %d: datasets.list while restarting: %d %v %s, want 503 backendError with Retry-After",
					i, resp.StatusCode, resp.Header, body)
			}
			unavailable++
			if time.Since(began) > emulatorBackWithin {
				t.Fatalf("restart %d: the emulator is not back after %s", i, emulatorBackWithin)
			}
			time.Sleep(100 * time.Millisecond)
		}
		took = append(took, time.Since(began).Round(100*time.Millisecond).String())
		if unavailable == 0 {
			t.Errorf("restart %d: datasets.list was never 503: the emulator did not restart", i)
		}
		// The restart emptied the emulator.
		var e *googleapi.Error
		if _, err := ds.Metadata(ctx); !errors.As(err, &e) || e.Code != http.StatusNotFound {
			t.Errorf("restart %d: the dataset made before the restart: %v, want 404", i, err)
		}
	}
	t.Logf("%d restarts in a row, each back in: %s", emulatorRestartsInARow, strings.Join(took, ", "))

	// The official client, asked to list datasets right after a restart,
	// retries the front's 503s and succeeds.
	restart(emulatorRestartsInARow + 1)
	it := c.Datasets(ctx)
	if _, err := it.Next(); err != nil && !errors.Is(err, iterator.Done) {
		t.Errorf("Client.Datasets through a restart: %v", err)
	}
	ds := c.Dataset(strings.ReplaceAll(h.Project(), "-", "_") + "_after_restarts")
	if err := ds.Create(ctx, &bigquery.DatasetMetadata{Description: "after"}); err != nil {
		t.Fatalf("create a dataset after the restarts: %v", err)
	}
	t.Cleanup(func() { _ = ds.Delete(ctx) })
	if md, err := ds.Metadata(ctx); err != nil || md.Description != "after" {
		t.Errorf("the dataset made after the restarts: %+v %v", md, err)
	}
}
