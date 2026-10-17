//go:build compat

package compat

import (
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// guardCaseVar names, in a child run of this test binary, the endpoint the
// child asks the harness for.
const guardCaseVar = "CB_HARNESS_GUARD_ENDPOINT"

// TestHarnessGuards (#1576): the harness's own guards, each in a child run
// of this test binary, since a guard ends its test with t.Fatalf. Cloud
// credentials in the environment (GOOGLE_APPLICATION_CREDENTIALS,
// GOOGLE_CLOUD_PROJECT, GCLOUD_PROJECT) fail New; a Google endpoint and a
// non-loopback one fail Endpoint before any dial; a loopback address, by IP
// or as localhost, is served. It needs no instance, so it runs in every
// shard.
func TestHarnessGuards(t *testing.T) {
	if addr, ok := os.LookupEnv(guardCaseVar); ok {
		h := New(t)
		os.Setenv(EnvStorage, addr) //nolint:errcheck // the child's own environment
		h.Endpoint(EnvStorage)
		t.Log("GUARD-PASSED")
		return
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	var base []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "CLOUDBURROW_TEST_") && !strings.HasPrefix(kv, "GOOGLE_") && !strings.HasPrefix(kv, "GCLOUD_PROJECT=") {
			base = append(base, kv)
		}
	}
	cases := []struct {
		name, endpoint string
		env            []string
		want           string // "" for a pass
	}{
		{"application credentials", "127.0.0.1:" + port, []string{"GOOGLE_APPLICATION_CREDENTIALS=/tmp/key.json"}, "GOOGLE_APPLICATION_CREDENTIALS is set"},
		{"cloud project", "127.0.0.1:" + port, []string{"GOOGLE_CLOUD_PROJECT=real"}, "GOOGLE_CLOUD_PROJECT is set"},
		{"gcloud project", "127.0.0.1:" + port, []string{"GCLOUD_PROJECT=real"}, "GCLOUD_PROJECT is set"},
		{"googleapis endpoint", "https://storage.googleapis.com:443", nil, "is a Google endpoint"},
		{"google endpoint", "www.google.com:443", nil, "is a Google endpoint"},
		{"private address", "10.0.0.1:9000", nil, "is not loopback"},
		{"hostname", "example.com:80", nil, "is not loopback"},
		{"unspecified address", "0.0.0.0:" + port, nil, "is not loopback"},
		{"loopback", "http://127.0.0.1:" + port, nil, ""},
		{"localhost", "localhost:" + port, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestHarnessGuards$", "-test.v", "-test.count=1")
			cmd.Env = append(append(append([]string{}, base...), c.env...), guardCaseVar+"="+c.endpoint)
			out, err := cmd.CombinedOutput()
			if c.want == "" {
				if err != nil || !strings.Contains(string(out), "GUARD-PASSED") {
					t.Errorf("endpoint %s was refused: %v\n%s", c.endpoint, err, out)
				}
				return
			}
			if err == nil || strings.Contains(string(out), "GUARD-PASSED") || !strings.Contains(string(out), c.want) {
				t.Errorf("endpoint %s with %v: want a failure saying %q, got %v\n%s", c.endpoint, c.env, c.want, err, out)
			}
		})
	}
}
