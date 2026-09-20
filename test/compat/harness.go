//go:build compat

package compat

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// Environment variables the harness reads to find a running instance.
const (
	EnvStorage = "CLOUDBURROW_TEST_STORAGE"
	EnvPubSub  = "CLOUDBURROW_TEST_PUBSUB"
	EnvTasks   = "CLOUDBURROW_TEST_TASKS"
	EnvRun     = "CLOUDBURROW_TEST_RUN"
)

// Harness locates a running CloudBurrow instance and refuses to touch anything
// that is not local.
type Harness struct {
	t *testing.T
	// project is unique per test, so concurrent tests cannot collide and no
	// test depends on another's leftovers.
	project string
}

// New returns a harness for one test.
//
// It fails loudly rather than skipping when credentials are present: a
// compatibility run that silently talked to real GCP would be both expensive
// and wrong, and a skip would hide it.
func New(t *testing.T) *Harness {
	t.Helper()
	refuseCloudCredentials(t)
	return &Harness{
		t:       t,
		project: fmt.Sprintf("cb-test-%d", time.Now().UnixNano()%1e12),
	}
}

// Project returns this test's unique project ID.
func (h *Harness) Project() string { return h.project }

// refuseCloudCredentials fails when the environment would let a client reach
// real Google Cloud.
//
// The harness must never load application default credentials. If it did, a
// misconfigured endpoint would silently succeed against production.
func refuseCloudCredentials(t *testing.T) {
	t.Helper()
	for _, v := range []string{"GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_CLOUD_PROJECT", "GCLOUD_PROJECT"} {
		if os.Getenv(v) != "" {
			t.Fatalf("%s is set; the compatibility harness refuses to run with cloud credentials in the environment", v)
		}
	}
}

// Endpoint returns the local endpoint for a service, skipping the test when no
// instance is running.
//
// Skipping here is honest: the service may simply not be deployed. What is not
// acceptable is a blanket skip that lets an unimplemented operation appear to
// pass, which is why each service test asks for its own endpoint.
func (h *Harness) Endpoint(envVar string) string {
	h.t.Helper()
	addr := strings.TrimSpace(os.Getenv(envVar))
	if addr == "" {
		h.t.Skipf("%s is not set; start an instance and export its endpoints (see test/compat/README.md)", envVar)
	}
	h.requireLocal(addr)
	h.requireReachable(addr)
	return addr
}

// requireLocal refuses any endpoint that is not loopback.
//
// This is the guard that keeps a compatibility run from reaching a real Google
// endpoint because of a stray environment variable.
func (h *Harness) requireLocal(addr string) {
	h.t.Helper()
	host := stripScheme(addr)
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	if strings.HasSuffix(host, ".googleapis.com") || strings.HasSuffix(host, ".google.com") {
		h.t.Fatalf("endpoint %q is a Google endpoint; the harness only talks to local instances", addr)
	}
	if host == "localhost" {
		return
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		h.t.Fatalf("endpoint %q is not loopback; the harness refuses non-local endpoints", addr)
	}
}

// requireReachable fails fast with a useful message rather than letting an SDK
// call time out opaquely.
func (h *Harness) requireReachable(addr string) {
	h.t.Helper()
	hostPort := stripScheme(addr)
	conn, err := net.DialTimeout("tcp", hostPort, 3*time.Second)
	if err != nil {
		h.t.Fatalf("endpoint %s is not reachable: %v\nis `cloudburrow up` running?", hostPort, err)
	}
	conn.Close()
}

func stripScheme(s string) string {
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "https://")
	return strings.TrimSuffix(s, "/")
}

// Context returns a context with a bounded deadline, so a hanging call fails
// the test instead of stalling the suite.
func (h *Harness) Context() context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	h.t.Cleanup(cancel)
	return ctx
}
