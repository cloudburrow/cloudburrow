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

// Docker Desktop's own labels describe its VM's ports and are not carried;
// the instance's and kind's are.
func TestDockerDesktopLabelsStay(t *testing.T) {
	for k, want := range map[string]bool{
		"desktop.docker.io/ports/6443/tcp": true,
		"desktop.docker.io/binds/0/Source": true,
		"io.x-k8s.kind.cluster":            false,
		"io.cloudburrow.instance":          false,
	} {
		if got := dockerDesktopLabel(k); got != want {
			t.Errorf("dockerDesktopLabel(%q) = %v", k, got)
		}
	}
}
