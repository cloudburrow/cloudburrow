//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/datastore"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Fault rules inside the cluster (#2113): Pub/Sub, Bigtable, Spanner,
// Firestore, Datastore and Cloud Storage are served in the cluster, behind
// a front in each pod, and the CLI holds the rules; the fronts now ask the
// host for them (internal/faultgate), so a rule for each is accepted and
// fails the official client's next matching call with its code, once for
// a count-1 rule, and /admin/events records the fault. Each rule names a
// method and the test's project. PERMISSION_DENIED is retried by none of
// these clients, so the call surfaces it.

// clusterFault adds rule, calls call until it fails with want (the front
// keeps the rules for up to a second), checks the next call is not
// faulted, and that /admin/events has the fault for service.
func clusterFault(t *testing.T, h *Harness, service, rule string, want codes.Code, call func() error) {
	t.Helper()
	control := h.Endpoint(EnvControl)
	t.Cleanup(func() { adminDo(t, http.MethodDelete, "http://"+control+"/admin/faults", "", nil) })
	code, body := adminDo(t, http.MethodPost, "http://"+control+"/admin/faults", "application/json", strings.NewReader(rule))
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("POST /admin/faults %s = %d %s; want the rule accepted", rule, code, body)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		err := call()
		if status.Code(err) == want {
			if !strings.Contains(err.Error(), "injected fault") {
				t.Errorf("the faulted call's error %v does not name the injected fault", err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("15 s after the rule, the call = %v; want %v", err, want)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := call(); err != nil {
		t.Errorf("the call after the count-1 rule's fault = %v; want the ordinary answer", err)
	}
	code, body = adminDo(t, http.MethodGet, "http://"+control+"/admin/events?service="+service, "", nil)
	if code != http.StatusOK || !strings.Contains(body, `"fault"`) {
		t.Errorf("/admin/events?service=%s = %d %s; want the fault", service, code, body)
	}
}

func TestFaultsInClusterPubSub(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	name := topic(t, h, c, fmt.Sprintf("faulted-%d", time.Now().UnixNano()))
	clusterFault(t, h, "pubsub", `{"service":"pubsub","method":"GetTopic","project":"`+h.Project()+`","code":"PERMISSION_DENIED","count":1}`,
		codes.PermissionDenied, func() error {
			_, err := c.TopicAdminClient.GetTopic(h.Context(), &pubsubpb.GetTopicRequest{Topic: name})
			return err
		})
}

func TestFaultsInClusterBigtable(t *testing.T) {
	h := New(t)
	table := fmt.Sprintf("faulted-%d", time.Now().UnixNano()%1e8)
	_, c := bigtableTable(t, h, table)
	clusterFault(t, h, "bigtable", `{"service":"bigtable","method":"ReadRows","project":"`+h.Project()+`","code":"PERMISSION_DENIED","count":1}`,
		codes.PermissionDenied, func() error {
			_, err := c.Open(table).ReadRow(h.Context(), "nope")
			return err
		})
}

func TestFaultsInClusterSpanner(t *testing.T) {
	h := New(t)
	ia, err := instance.NewInstanceAdminClient(h.Context(), spannerOpts(h.Endpoint(EnvSpanner))...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ia.Close() })
	clusterFault(t, h, "spanner", `{"service":"spanner","method":"GetInstanceConfig","project":"`+h.Project()+`","code":"PERMISSION_DENIED","count":1}`,
		codes.PermissionDenied, func() error {
			_, err := ia.GetInstanceConfig(h.Context(), &instancepb.GetInstanceConfigRequest{Name: "projects/" + h.Project() + "/instanceConfigs/emulator-config"})
			return err
		})
}

func TestFaultsInClusterFirestore(t *testing.T) {
	h := New(t)
	c := firestoreClient(t, h)
	doc := c.Collection("faulted").Doc(fmt.Sprintf("d%d", time.Now().UnixNano()))
	if _, err := doc.Set(h.Context(), map[string]any{"n": 1}); err != nil {
		t.Fatal(err)
	}
	clusterFault(t, h, "firestore", `{"service":"firestore","method":"GetDocument","project":"`+h.Project()+`","code":"PERMISSION_DENIED","count":1}`,
		codes.PermissionDenied, func() error {
			_, err := c.Collection("faulted").Doc(doc.ID).Get(h.Context())
			return err
		})
}

func TestFaultsInClusterDatastore(t *testing.T) {
	h := New(t)
	c := datastoreClient(t, h, h.Project())
	key := datastore.NameKey("Faulted", fmt.Sprintf("k%d", time.Now().UnixNano()), nil)
	if _, err := c.Put(h.Context(), key, &struct{ N int }{1}); err != nil {
		t.Fatal(err)
	}
	clusterFault(t, h, "datastore", `{"service":"datastore","method":"Lookup","project":"`+h.Project()+`","code":"PERMISSION_DENIED","count":1}`,
		codes.PermissionDenied, func() error {
			var v struct{ N int }
			return c.Get(h.Context(), key, &v)
		})
}

// TestFaultsInClusterStorage: the storage server in the cluster answers a
// faulted call with storage's own JSON error body and the rule's status.
func TestFaultsInClusterStorage(t *testing.T) {
	h := New(t)
	control := h.Endpoint(EnvControl)
	h.Endpoint(EnvStorage)
	t.Cleanup(func() { adminDo(t, http.MethodDelete, "http://"+control+"/admin/faults", "", nil) })
	code, body := adminDo(t, http.MethodPost, "http://"+control+"/admin/faults", "application/json",
		strings.NewReader(`{"service":"storage","method":"storage.buckets.get","httpStatus":503,"count":1}`))
	if code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("a storage rule = %d %s; want it accepted", code, body)
	}
	path := "/storage/v1/b/" + h.Project() + "-faulted"
	deadline := time.Now().Add(15 * time.Second)
	for {
		code, body := rawStorage(t, h, http.MethodGet, path, "")
		if code == http.StatusServiceUnavailable {
			if !strings.Contains(body, `"error"`) {
				t.Errorf("the faulted call's body %s is not storage's JSON error", body)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("15 s after the rule, GET bucket = %d %s; want 503", code, body)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if code, body := rawStorage(t, h, http.MethodGet, path, ""); code != http.StatusNotFound {
		t.Errorf("the next call = %d %s; want the ordinary 404", code, body)
	}
}
