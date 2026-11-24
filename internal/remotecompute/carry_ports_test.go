package remotecompute

import (
	"reflect"
	"testing"
)

// A binding Docker Desktop lists twice is published once: a Linux dockerd
// refuses to bind the same address twice, and a kind node carried from a
// Mac would not start on the VM.
func TestPublishSpecsOncePerBinding(t *testing.T) {
	got := publishSpecs(map[string][]portBinding{
		"6443/tcp":  {{HostIP: "127.0.0.1", HostPort: "52220"}, {HostIP: "127.0.0.1", HostPort: "52220"}},
		"31080/tcp": {{HostIP: "127.0.0.1", HostPort: "23180"}, {HostIP: "127.0.0.1", HostPort: "23180"}, {HostIP: "::1", HostPort: "23180"}},
		"80/tcp":    {{}},
	})
	want := []string{"127.0.0.1:23180:31080/tcp", "[::1]:23180:31080/tcp", "127.0.0.1:52220:6443/tcp", "80/tcp"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("publishSpecs = %q, want %q", got, want)
	}
}
