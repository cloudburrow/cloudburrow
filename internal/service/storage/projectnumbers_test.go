package storage

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// TestProjectNumbersPushed (#1544): a number the host sends for a project
// is the one its buckets and service agent report; a project not sent keeps
// the derived number; the set survives being read from the store again.
func TestProjectNumbersPushed(t *testing.T) {
	meta := NewMemMetaStore()
	s, err := NewServer(Options{Hosts: []string{"storage.localhost"}, Meta: meta})
	if err != nil {
		t.Fatal(err)
	}
	h := httptest.NewServer(s)
	t.Cleanup(h.Close)
	resource.SetProjectNumberResolver(s.ProjectNumberOf)
	t.Cleanup(func() { resource.SetProjectNumberResolver(nil) })

	const fresh = int64(123456789012)
	if code, body := raw(t, "POST", h.URL+ProjectNumbersPath, `{"recreated-proj":`+strconv.FormatInt(fresh, 10)+`}`); code != 204 {
		t.Fatalf("push: %d %s", code, body)
	}
	raw(t, "POST", h.URL+"/storage/v1/b?project=recreated-proj", `{"name":"numbered"}`)
	raw(t, "POST", h.URL+"/storage/v1/b?project=plain-proj", `{"name":"derived"}`)
	for bucket, want := range map[string]int64{"numbered": fresh, "derived": resource.DerivedProjectNumber("plain-proj")} {
		_, body := raw(t, "GET", h.URL+"/storage/v1/b/"+bucket, "")
		var b struct {
			ProjectNumber string `json:"projectNumber"`
		}
		_ = json.Unmarshal([]byte(body), &b)
		if b.ProjectNumber != strconv.FormatInt(want, 10) {
			t.Fatalf("%s projectNumber = %q, want %d (%s)", bucket, b.ProjectNumber, want, body)
		}
	}
	_, body := raw(t, "GET", h.URL+"/storage/v1/projects/recreated-proj/serviceAccount", "")
	if want := "service-" + strconv.FormatInt(fresh, 10) + "@"; !strings.Contains(body, want) {
		t.Fatalf("service agent %s, want %s", body, want)
	}
	// Read again from the store, by a server started on it, as after a
	// restart.
	again, err := NewServer(Options{Hosts: []string{"storage.localhost"}, Meta: meta})
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := again.ProjectNumberOf("recreated-proj"); !ok || n != fresh {
		t.Fatalf("after reload %d %v", n, ok)
	}
}
