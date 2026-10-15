//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
)

// TestConsoleMemorystoreFlush (#1302): the Memorystore page lists the one
// instance as the admin API describes it, offers Flush, and Flush empties
// the server a Redis client reaches. The restart probe key
// TestMemorystoreAcrossRestart reads is put back afterwards.
func TestConsoleMemorystoreFlush(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	project := strings.TrimSpace(os.Getenv(EnvMemorystoreProject))
	if project == "" {
		t.Skipf("%s is not set", EnvMemorystoreProject)
	}
	rc := redisClient(t, h)
	ctx := h.Context()

	code, body := consoleDo(t, addr, http.MethodGet, "/api/resources/memorystore?project="+url.QueryEscape(project), "")
	if code != http.StatusOK {
		t.Fatalf("console list = %d: %s", code, body)
	}
	var l struct {
		Items []struct {
			Name    string
			Status  string
			Fields  map[string]string
			Actions []struct{ ID string }
		}
	}
	if err := json.Unmarshal([]byte(body), &l); err != nil {
		t.Fatal(err)
	}
	if len(l.Items) != 1 || l.Items[0].Name != "memorystore" || l.Items[0].Status != "READY" ||
		l.Items[0].Fields["Endpoint"] != h.Endpoint(EnvMemorystore) || l.Items[0].Fields["Version"] != "REDIS_7_2" {
		t.Fatalf("console list = %s", body)
	}
	if len(l.Items[0].Actions) != 1 || l.Items[0].Actions[0].ID != "flush" {
		t.Fatalf("instance actions = %+v, want flush", l.Items[0].Actions)
	}

	probe, probeErr := rc.Get(ctx, restartProbeKey).Result()
	t.Cleanup(func() {
		if probeErr == nil {
			_ = rc.Set(ctx, restartProbeKey, probe, 0).Err()
		}
	})
	key := "compat:" + h.Project() + ":flush"
	if err := rc.Set(ctx, key, "v", 0).Err(); err != nil {
		t.Fatal(err)
	}
	code, body = consoleDo(t, addr, http.MethodPost, "/api/actions/memorystore?project="+url.QueryEscape(project),
		fmt.Sprintf(`{"Name":%q,"Action":"flush"}`, "memorystore"))
	if code != http.StatusOK {
		t.Fatalf("flush = %d: %s", code, body)
	}
	if _, err := rc.Get(ctx, key).Result(); err != redis.Nil {
		t.Errorf("GET after Flush = %v, want no key", err)
	}
	if n, err := rc.DBSize(ctx).Result(); err != nil || n != 0 {
		t.Errorf("DBSIZE after Flush = %d, %v; want 0", n, err)
	}

	// Another project has no instance, so nothing to flush.
	code, _ = consoleDo(t, addr, http.MethodPost, "/api/actions/memorystore?project="+h.Project(),
		`{"Name":"memorystore","Action":"flush"}`)
	if code == http.StatusOK {
		t.Errorf("flush in a project with no instance succeeded")
	}
}
