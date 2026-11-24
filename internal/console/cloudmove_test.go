package console

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
	"github.com/cloudburrow/cloudburrow/internal/byoc/byoctest"
	"github.com/cloudburrow/cloudburrow/internal/byoc/keychain"
)

type fakeMover struct {
	mu     sync.Mutex
	moves  map[string]MoveStatus
	refuse error
}

func (f *fakeMover) StartMove(instance, vm string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refuse != nil {
		return f.refuse
	}
	f.moves[instance] = MoveStatus{Instance: instance, To: vm, Started: time.Now(), Steps: []MoveStep{{Step: "queued", Message: "Starting"}}}
	return nil
}

func (f *fakeMover) MoveStatus(instance string) (MoveStatus, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.moves[instance]
	return st, ok
}

func TestComputeMoveStartsAndReports(t *testing.T) {
	aws := byoctest.New(byoc.AWS)
	m, err := byoc.NewManager(byoc.Options{Dir: t.TempDir(), Keychain: keychain.NewMemory(), Drivers: []byoc.Driver{aws},
		IPLookup: func(context.Context) (string, error) { return "203.0.113.1", nil }, OpenBrowser: func(string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := m.ConnectKey(context.Background(), byoc.AWS, byoc.AccessKey{AccessKeyID: "A", SecretAccessKey: "B"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create(context.Background(), byoc.CreateRequest{Instance: "dev-vm", Connection: conn.ID, Region: "us-east-1"}, true, nil); err != nil {
		t.Fatal(err)
	}
	s := New("", func(context.Context) Status { return Status{} })
	s.SetCloudCompute(m, "dev", func(_ context.Context, authz string) error {
		if authz != "Bearer "+computeTestToken {
			return &InstanceRefusal{Status: http.StatusUnauthorized, Message: "missing or wrong admin token"}
		}
		return nil
	})
	mover := &fakeMover{moves: map[string]MoveStatus{}}
	s.SetMover(mover)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	if code := computeCall(t, srv, "POST", "/api/compute/machines/dev-vm/move", `{"instance":"dev"}`, false, nil); code != http.StatusUnauthorized {
		t.Fatalf("a move without the token = %d", code)
	}
	if code := computeCall(t, srv, "POST", "/api/compute/machines/nope/move", `{}`, true, nil); code != http.StatusNotFound {
		t.Fatalf("a move to no VM = %d", code)
	}
	var st MoveStatus
	if code := computeCall(t, srv, "POST", "/api/compute/machines/dev-vm/move", `{}`, true, &st); code != http.StatusAccepted || st.Instance != "dev" || st.To != "dev-vm" {
		t.Fatalf("move = %d %+v", code, st)
	}
	if code := computeCall(t, srv, "GET", "/api/compute/moves/dev", "", true, &st); code != http.StatusOK || len(st.Steps) != 1 {
		t.Fatalf("status = %d %+v", code, st)
	}
	var none map[string]any
	if code := computeCall(t, srv, "GET", "/api/compute/moves/other", "", true, &none); code != http.StatusOK || none["none"] != true {
		t.Fatalf("status of no move = %d %v", code, none)
	}
	mover.refuse = errors.New("dev is being moved already")
	var out map[string]string
	if code := computeCall(t, srv, "POST", "/api/compute/machines/dev-vm/move", `{}`, true, &out); code != http.StatusConflict || out["error"] == "" {
		t.Fatalf("a refused move = %d %v", code, out)
	}
	var ov computeOverview
	if code := computeCall(t, srv, "GET", "/api/compute", "", true, &ov); code != http.StatusOK || ov.LocalCompute {
		t.Fatalf("overview = %d, local %v with no compute source", code, ov.LocalCompute)
	}
}
