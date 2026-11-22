package monitoring

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

func fakeInventory(rs ...InventoryResource) ResourceInventory {
	return func(context.Context, string) ([]InventoryResource, error) { return rs, nil }
}

func vm(name, zone string, labels map[string]string, tags ...string) InventoryResource {
	return InventoryResource{Resource: &monitoredres.MonitoredResource{Type: "gce_instance",
		Labels: map[string]string{"project_id": "proj-a", "instance_id": name + "-id", "zone": zone}},
		Metadata: ResourceMetadata{Name: name, Region: RegionOf(zone), UserLabels: labels, NetworkTags: tags,
			SystemLabels: map[string]string{"machine_type": "e2-small", "spot_instance": "false"}}}
}

func memberNames(t *testing.T, s *Server, group, filter string) string {
	t.Helper()
	resp, err := s.ListGroupMembers(context.Background(), &monitoringpb.ListGroupMembersRequest{Name: group, Filter: filter})
	if err != nil {
		t.Fatalf("ListGroupMembers %s: %v", filter, err)
	}
	var out []string
	for _, m := range resp.GetMembers() {
		out = append(out, m.GetType()+":"+m.GetLabels()["instance_id"]+m.GetLabels()["bucket_name"])
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// Groups' Name, Tag and Region criteria (#2146): the products' resources
// are members with their metadata, matched by resource.metadata.* and
// metadata.user_labels / system_labels.
func TestGroupsMatchResourceMetadata(t *testing.T) {
	s, _ := newTestServer(t, "")
	ctx := context.Background()
	s.AddResourceInventory(fakeInventory(
		vm("web-1", "us-central1-a", map[string]string{"env": "prod"}, "http-server"),
		vm("web-2", "europe-west2-b", map[string]string{"env": "dev"}),
		vm("db-1", "us-central1-f", nil, "db")))
	s.AddResourceInventory(fakeInventory(InventoryResource{
		Resource: &monitoredres.MonitoredResource{Type: "gcs_bucket", Labels: map[string]string{"project_id": "proj-a", "bucket_name": "logs", "location": "us"}},
		Metadata: ResourceMetadata{Name: "logs", Region: "us", UserLabels: map[string]string{"env": "prod"}}}))
	mk := func(filter string) string {
		t.Helper()
		g, err := s.CreateGroup(ctx, &monitoringpb.CreateGroupRequest{Name: "projects/proj-a", Group: &monitoringpb.Group{DisplayName: filter, Filter: filter}})
		if err != nil {
			t.Fatalf("CreateGroup %s: %v", filter, err)
		}
		return g.GetName()
	}
	for filter, want := range map[string]string{
		`resource.metadata.name = has_substring("web")`:                                   "gce_instance:web-1-id,gce_instance:web-2-id",
		`resource.metadata.name != has_substring("web")`:                                  "gce_instance:db-1-id,gcs_bucket:logs",
		`resource.metadata.region = "us-central1"`:                                        "gce_instance:db-1-id,gce_instance:web-1-id",
		`resource.metadata.tag."env" = has_substring("prod")`:                             "gce_instance:web-1-id,gcs_bucket:logs",
		`resource.metadata.tag."http-server" = has_substring("")`:                         "gce_instance:web-1-id",
		`metadata.user_labels."env" = "dev"`:                                              "gce_instance:web-2-id",
		`resource.type = "gce_instance" AND metadata.system_labels.spot_instance = false`: "gce_instance:db-1-id,gce_instance:web-1-id,gce_instance:web-2-id",
		`resource.metadata.region = "us-central1" OR resource.metadata.name = "logs"`:     "gce_instance:db-1-id,gce_instance:web-1-id,gcs_bucket:logs",
	} {
		if got := memberNames(t, s, mk(filter), ""); got != want {
			t.Errorf("%s: members %s, want %s", filter, got, want)
		}
	}
	// ListGroupMembers' own filter takes the metadata too.
	all := mk(`resource.type = "gce_instance"`)
	if got := memberNames(t, s, all, `resource.metadata.tag."db" = has_substring("")`); got != "gce_instance:db-1-id" {
		t.Errorf("members filtered by tag: %s", got)
	}
	for _, bad := range []string{`resource.metadata.zone = "x"`, `metadata.foo.bar = "x"`} {
		_, err := s.CreateGroup(ctx, &monitoringpb.CreateGroupRequest{Name: "projects/proj-a", Group: &monitoringpb.Group{DisplayName: "bad", Filter: bad}})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v, want INVALID_ARGUMENT", bad, err)
		}
	}
	// Time series filters still refuse metadata.
	if _, err := parseFilter(`metadata.user_labels.a = "b"`, filterSeries); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a series filter took metadata: %v", err)
	}
	if RegionOf("us-central1-a") != "us-central1" || RegionOf("EUROPE-WEST2") != "europe-west2" || RegionOf("us") != "us" || RegionOf("global") != "global" {
		t.Error("RegionOf")
	}
}

// Log check failures and disabled (#2146): REST-only fields through the
// overlay, kept in the state file, written to Cloud Logging on failure.
func TestUptimeLogCheckFailures(t *testing.T) {
	state := filepath.Join(t.TempDir(), "monitoring.json")
	s, _ := newTestServer(t, state)
	ctx := context.Background()
	var mu sync.Mutex
	var logged []UptimeLogEntry
	s.SetUptimeLogger(func(_ context.Context, project string, e UptimeLogEntry) error {
		mu.Lock()
		defer mu.Unlock()
		if project == "proj-a" {
			logged = append(logged, e)
		}
		return nil
	})
	h := NewRESTHandler(s)
	do := func(method, path, body string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, path, rec.Code, rec.Body)
		}
		var m map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return m
	}
	// Port 1 on localhost refuses: every run fails.
	c := do("POST", "/v3/projects/proj-a/uptimeCheckConfigs", `{"displayName":"down","logCheckFailures":true,
		"monitoredResource":{"type":"uptime_url","labels":{"host":"127.0.0.1","project_id":"proj-a"}},
		"tcpCheck":{"port":1},"period":"60s","timeout":"2s"}`)
	name, _ := c["name"].(string)
	if c["logCheckFailures"] != true || name == "" {
		t.Fatalf("created: %v", c)
	}
	if got := do("GET", "/v3/"+name, ""); got["logCheckFailures"] != true {
		t.Fatalf("got: %v", got)
	}
	s.RunUptimeChecks(ctx)
	mu.Lock()
	n := len(logged)
	var first UptimeLogEntry
	if n > 0 {
		first = logged[0]
	}
	mu.Unlock()
	if n != 1 || first.Resource.GetType() != "uptime_url" || first.Labels["check_id"] != name[strings.LastIndex(name, "/")+1:] ||
		!strings.Contains(first.Payload["@type"].(string), "UptimeCheckResult") || first.Payload["errorMessage"] == "" {
		t.Fatalf("logged %d: %+v", n, first)
	}
	// Restarted from the state file, the fields are kept.
	s2, err := NewServer(state)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s2.GetUptimeCheckConfig(ctx, &monitoringpb.GetUptimeCheckConfigRequest{Name: name})
	if err != nil || !LogCheckFailures(got) {
		t.Fatalf("after a restart: %v %v", got, err)
	}
	// An update mask clears it; disabled stops the runs.
	upd := do("PATCH", "/v3/"+name+"?updateMask=logCheckFailures,disabled", `{"logCheckFailures":false,"disabled":true}`)
	if upd["logCheckFailures"] != nil || upd["disabled"] != true {
		t.Fatalf("patched: %v", upd)
	}
	cfg, _ := s.GetUptimeCheckConfig(ctx, &monitoringpb.GetUptimeCheckConfigRequest{Name: name})
	if LogCheckFailures(cfg) || !UptimeDisabled(cfg) {
		t.Fatalf("after the patch: %v", cfg)
	}
	// Over gRPC too, by the field mask's proto name.
	if err := SetLogCheckFailures(cfg, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateUptimeCheckConfig(ctx, &monitoringpb.UpdateUptimeCheckConfigRequest{UptimeCheckConfig: cfg,
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"log_check_failures"}}}); err != nil {
		t.Fatal(err)
	}
	cfg, _ = s.GetUptimeCheckConfig(ctx, &monitoringpb.GetUptimeCheckConfigRequest{Name: name})
	if !LogCheckFailures(cfg) {
		t.Error("log_check_failures by gRPC mask")
	}
}
