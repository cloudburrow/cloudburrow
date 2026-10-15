//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
	// The instance `up` runs, beside any that CreateInstance made.
	i := -1
	for n, it := range l.Items {
		if it.Name == "memorystore" {
			i = n
		}
	}
	if i < 0 || l.Items[i].Status != "READY" ||
		l.Items[i].Fields["Endpoint"] != h.Endpoint(EnvMemorystore) || l.Items[i].Fields["Version"] != "REDIS_7_2" {
		t.Fatalf("console list = %s", body)
	}
	if len(l.Items[i].Actions) != 1 || l.Items[i].Actions[0].ID != "flush" {
		t.Fatalf("instance actions = %+v, want flush", l.Items[i].Actions)
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

// TestConsoleMemorystoreCreateEditDelete (#1302): Create instance on the
// Memorystore page makes an instance with AUTH through CreateInstance,
// which the official client reads back and go-redis reaches with the AUTH
// string its page shows; Edit resizes it and changes its labels through
// UpdateInstance; Delete removes it through DeleteInstance. The instance
// `up` runs has no Edit form, and cannot be deleted.
func TestConsoleMemorystoreCreateEditDelete(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c, project := memorystoreAdminClient(t, h)
	ctx := h.Context()
	id := fmt.Sprintf("console-%d", time.Now().UnixNano()%1e8)
	name := "projects/" + project + "/locations/us-central1/instances/" + id
	t.Cleanup(func() {
		if op, err := c.DeleteInstance(h.Context(), &redispb.DeleteInstanceRequest{Name: name}); err == nil {
			_ = op.Wait(h.Context())
		}
	})
	q := "?project=" + url.QueryEscape(project)
	body, _ := json.Marshal(map[string]string{"id": id, "region": "us-central1", "memorySizeGb": "1", "authEnabled": "true",
		"displayName": "console cache", "labels": `{"team":"a"}`, "redisConfigs": `{"maxmemory-policy":"allkeys-lru"}`})
	if code, out := consoleDo(t, addr, http.MethodPost, "/api/resources/memorystore"+q, string(body)); code != http.StatusOK {
		t.Fatalf("console create = %d: %s", code, out)
	}
	inst, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if err != nil {
		t.Fatalf("GetInstance after Create instance: %v", err)
	}
	if inst.GetState() != redispb.Instance_READY || !inst.GetAuthEnabled() || inst.GetDisplayName() != "console cache" ||
		inst.GetLabels()["team"] != "a" || inst.GetRedisConfigs()["maxmemory-policy"] != "allkeys-lru" {
		t.Errorf("created from the console: %v", inst)
	}

	var page struct {
		Sections []struct {
			Groups []struct {
				Properties []struct{ Label, Value string }
			}
		}
		Edit *struct{ Label string }
	}
	code, out := consoleDo(t, addr, http.MethodGet, "/api/detail/memorystore"+q+"&name="+id, "")
	if code != http.StatusOK {
		t.Fatalf("instance page = %d: %s", code, out)
	}
	if err := json.Unmarshal([]byte(out), &page); err != nil {
		t.Fatal(err)
	}
	props := map[string]string{}
	for _, s := range page.Sections {
		for _, g := range s.Groups {
			for _, p := range g.Properties {
				props[p.Label] = p.Value
			}
		}
	}
	auth, err := c.GetInstanceAuthString(ctx, &redispb.GetInstanceAuthStringRequest{Name: name})
	if err != nil || props["AUTH string"] != auth.GetAuthString() || page.Edit == nil {
		t.Fatalf("instance page = %s (AUTH string %v)", out, err)
	}
	if props["Endpoint"] != net.JoinHostPort(inst.GetHost(), strconv.Itoa(int(inst.GetPort()))) {
		t.Errorf("page endpoint %q, instance %s:%d", props["Endpoint"], inst.GetHost(), inst.GetPort())
	}
	rc := redis.NewClient(&redis.Options{Addr: props["Endpoint"], Password: props["AUTH string"]})
	defer rc.Close()
	if err := rc.Set(ctx, "console", "1", 0).Err(); err != nil {
		t.Errorf("SET with the page's endpoint and AUTH string: %v", err)
	}

	if code, out := consoleEdit(t, addr, "memorystore", project, []string{id}, map[string]string{
		"displayName": "resized", "memorySizeGb": "2", "labels": `{"team":"b"}`, "redisConfigs": `{"maxmemory-policy":"allkeys-lru"}`}); code != http.StatusOK {
		t.Fatalf("console edit = %d: %s", code, out)
	}
	if inst, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name}); err != nil || inst.GetMemorySizeGb() != 2 ||
		inst.GetLabels()["team"] != "b" || inst.GetDisplayName() != "resized" {
		t.Errorf("after Edit, GetInstance = %v, %v", inst, err)
	}
	if cfg, err := rc.ConfigGet(ctx, "maxmemory").Result(); err != nil || cfg["maxmemory"] != strconv.Itoa(2<<30) {
		t.Errorf("after Edit, maxmemory = %v, %v", cfg, err)
	}

	// The instance `up` runs: no Edit form, and Delete is refused.
	code, out = consoleDo(t, addr, http.MethodGet, "/api/detail/memorystore"+q+"&name=memorystore", "")
	if code != http.StatusOK || strings.Contains(out, `"edit"`) {
		t.Errorf("the page of the instance up runs = %d: %s", code, out)
	}
	if code, _ := consoleDo(t, addr, http.MethodDelete, "/api/resources/memorystore"+q+"&name=memorystore", ""); code == http.StatusOK {
		t.Error("Delete of the instance up runs succeeded")
	}

	if code, out := consoleDo(t, addr, http.MethodDelete, "/api/resources/memorystore"+q+"&name="+id, ""); code != http.StatusOK {
		t.Fatalf("console delete = %d: %s", code, out)
	}
	if _, err := c.GetInstance(ctx, &redispb.GetInstanceRequest{Name: name}); status.Code(err) != codes.NotFound {
		t.Errorf("GetInstance after Delete = %v, want NOT_FOUND", err)
	}
}
