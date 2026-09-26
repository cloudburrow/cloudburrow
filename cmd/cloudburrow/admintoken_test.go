package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
)

// The admin token (#553) is minted with enough entropy, written beside the
// runtime file readable by the owner only, never into the runtime file, and
// removed with it when the instance stops.
func TestAdminTokenLivesBesideTheRuntimeFileAndLeavesWithIt(t *testing.T) {
	tok, err := newAdminToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 64 {
		t.Errorf("token is %d hex characters, want 64 (32 bytes)", len(tok))
	}
	if again, _ := newAdminToken(); again == tok {
		t.Error("two minted tokens are equal")
	}

	cfg := config.Default()
	cfg.Name = "tok"
	cfg.StateDir = t.TempDir()
	control := lifecycle.NewControlServer(0, nil)
	if err := control.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Stop(context.Background()) })
	r := &runtimeFile{cfg: cfg, control: control, token: tok}
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(adminTokenPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != tok {
		t.Error("the token file does not hold the minted token")
	}
	if fi, _ := os.Stat(adminTokenPath(cfg)); fi.Mode().Perm() != 0o600 {
		t.Errorf("the token file is %v, want 0600", fi.Mode().Perm())
	}
	if raw, _ := os.ReadFile(runtimePath(cfg)); strings.Contains(string(raw), tok) {
		t.Error("the token is in the runtime file, which a diagnose bundle includes")
	}
	if got, err := readAdminToken(cfg); err != nil || got != tok {
		t.Errorf("readAdminToken = %q, %v", got, err)
	}
	if err := r.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(adminTokenPath(cfg)); !os.IsNotExist(err) {
		t.Errorf("the token file survived stop: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.InstanceDir(), "up.json")); !os.IsNotExist(err) {
		t.Errorf("the runtime file survived stop: %v", err)
	}
}

// mountAdmin mounts the routes behind the token (#553): on the control
// server, /admin refuses a request without it and serves one with it, while
// health stays open.
func TestMountAdminRequiresTheToken(t *testing.T) {
	control := lifecycle.NewControlServer(0, nil)
	cfg := config.Default()
	mountAdmin(control, admin.NewRecorder(10, nil), cfg, adminDeps{}, "t0k")
	if err := control.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Stop(context.Background()) })
	get := func(path, auth string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, "http://"+control.Addr()+path, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get("/admin/events", ""); code != http.StatusUnauthorized {
		t.Errorf("GET /admin/events without the token = %d, want 401", code)
	}
	if code := get("/admin/events", "Bearer wrong"); code != http.StatusUnauthorized {
		t.Errorf("GET /admin/events with a wrong token = %d, want 401", code)
	}
	if code := get("/admin/events", "Bearer t0k"); code != http.StatusOK {
		t.Errorf("GET /admin/events with the token = %d, want 200", code)
	}
	if code := get("/healthz", ""); code == http.StatusUnauthorized {
		t.Error("GET /healthz asks for the token")
	}
}
