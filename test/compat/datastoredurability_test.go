//go:build compat

package compat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/datastore"
)

const envDatastoreExpect = "CLOUDBURROW_TEST_DATASTORE_EXPECT"

type durable struct{ Note string }

// datastoreProbeProject is fixed, not per test: the restart probe reads what
// an earlier process wrote.
const datastoreProbeProject = "cb-datastore-probe"

func datastoreClient(t *testing.T, h *Harness, project string) *datastore.Client {
	t.Helper()
	t.Setenv("DATASTORE_EMULATOR_HOST", h.Endpoint(EnvDatastore))
	c, err := datastore.NewClient(h.Context(), project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// getWithin reads a key, retrying while the backend comes back: a
// port-forward accepts connections before the new pod serves.
func getWithin(t *testing.T, h *Harness, c *datastore.Client, key *datastore.Key, within time.Duration) (durable, error) {
	var got durable
	var err error
	for deadline := time.Now().Add(within); ; time.Sleep(2 * time.Second) {
		err = c.Get(h.Context(), key, &got)
		if err == nil || err == datastore.ErrNoSuchEntity || time.Now().After(deadline) {
			return got, err
		}
	}
}

// TestDatastoreSurvivesAPodRestart (#307): in persistent mode the emulator's
// on-disk store is on a volume, so an entity written before its pod is
// deleted is read back from the new pod. Measured, not assumed.
func TestDatastoreSurvivesAPodRestart(t *testing.T) {
	h := New(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	kubeconfig := filepath.Join(instanceDirFrom(t, strings.Fields(os.Getenv(EnvCLIArgs))), "kubeconfig")
	c := datastoreClient(t, h, h.Project())
	key := datastore.NameKey("Durable", "pod-restart", nil)
	if _, err := c.Put(h.Context(), key, &durable{Note: "written before the pod was deleted"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	restartWorkload(t, kubeconfig, "datastore")
	waitForEndpoint(t, h.Endpoint(EnvDatastore), 2*time.Minute)

	got, err := getWithin(t, h, c, key, 2*time.Minute)
	if err != nil || got.Note != "written before the pod was deleted" {
		t.Fatalf("after a pod restart in persistent mode: %+v, %v; want the entity written before it", got, err)
	}

	// Left for TestDatastoreAcrossRestart, which CI runs after stop/up.
	pc := datastoreClient(t, h, datastoreProbeProject)
	if _, err := pc.Put(h.Context(), datastore.NameKey("Durable", "probe", nil), &durable{Note: "written-before-stop"}); err != nil {
		t.Fatal(err)
	}
}

// TestDatastoreAcrossRestart: after stop and up in persistent mode the probe
// entity is there; in ephemeral mode it is gone.
func TestDatastoreAcrossRestart(t *testing.T) {
	expect := os.Getenv(envDatastoreExpect)
	if expect == "" {
		t.Skipf("%s is not set: this runs only after CI restarts the instance", envDatastoreExpect)
	}
	h := New(t)
	c := datastoreClient(t, h, datastoreProbeProject)
	got, err := getWithin(t, h, c, datastore.NameKey("Durable", "probe", nil), 2*time.Minute)
	switch expect {
	case "present":
		if err != nil || got.Note != "written-before-stop" {
			t.Fatalf("persistent mode after stop/up: %+v, %v; want the entity written before stop", got, err)
		}
	case "absent":
		if err != datastore.ErrNoSuchEntity {
			t.Fatalf("ephemeral mode after stop/up: %+v, %v; want no entity", got, err)
		}
	default:
		t.Fatalf("%s must be present or absent, not %q", envDatastoreExpect, expect)
	}
}
