//go:build compat

package compat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// consoleBigtableInstancePage is the part of a console Bigtable instance page
// these tests read.
type consoleBigtableInstancePage struct {
	Unavailable string
	Summary     []struct{ Label, Value string }
	Actions     []bigtableAction
	Sections    []struct {
		ID      string
		Listing struct {
			Items []struct {
				Name    string
				Fields  map[string]string
				ActsOn  []string
				Actions []bigtableAction
			}
		}
	}
}

func consoleBigtableInstance(t *testing.T, addr, project, id string) consoleBigtableInstancePage {
	t.Helper()
	q := url.Values{"project": {project}, "name": {id}}
	code, body := consoleDo(t, addr, http.MethodGet, "/api/detail/bigtable-instances?"+q.Encode(), "")
	if code != http.StatusOK {
		t.Fatalf("console detail %s = %d: %s", id, code, body)
	}
	var page consoleBigtableInstancePage
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("decode detail: %v: %s", err, body)
	}
	if page.Unavailable != "" {
		t.Fatalf("the instance page is unavailable: %s", page.Unavailable)
	}
	return page
}

// TestConsoleBigtableInstanceAdmin (#1358).
//
// The Bigtable instances screen against the instance admin API the Bigtable
// pod's front serves, each change read back with the official
// InstanceAdminClient. Create instance makes the instance, its labels and
// its first cluster as InstanceInfo and GetCluster read them, and the
// screen lists it. Edit instance, prefilled from it, changes its display
// name, type and labels. Add cluster adds an autoscaled cluster; Edit nodes
// on a cluster's row sets a fixed size, then autoscaling. Create app
// profile, Edit app profile and Delete app profile change what
// GetAppProfile reads. Delete cluster is refused, with the API's message,
// for a cluster an app profile routes to and for the last one, and removes
// any other. The Permissions tab's grant is what the client's instance IAM
// handle reads, a stale etag is refused with the client's own message, and
// Test permissions answers as the client does. Delete instance removes it.
// The changes go through the console, so only the client's reads are
// claimed.
//
// covers: google.bigtable.admin.v2.BigtableInstanceAdmin/GetInstance, google.bigtable.admin.v2.BigtableInstanceAdmin/GetCluster, google.bigtable.admin.v2.BigtableInstanceAdmin/ListClusters, google.bigtable.admin.v2.BigtableInstanceAdmin/GetAppProfile
func TestConsoleBigtableInstanceAdmin(t *testing.T) {
	h := New(t)
	iac := bigtableInstanceAdmin(t, h)
	addr := consoleAddr(t, h)
	ctx := h.Context()
	project := h.Project()
	id := fmt.Sprintf("cb-con-%d", time.Now().UnixNano()%1e9)
	c1, c2 := id+"-c1", id+"-c2"

	if code, out := consoleCreate(t, addr, "bigtable-instances", project, map[string]string{
		"instance": id, "displayName": "Console made", "type": "DEVELOPMENT", "labels": `{"team":"data"}`,
		"cluster": c1, "zone": "us-east1-c", "storage": "HDD", "nodes": "0"}); code != http.StatusOK && code != http.StatusCreated {
		t.Fatalf("Create instance = %d: %s", code, out)
	}
	t.Cleanup(func() { _ = iac.DeleteInstance(h.Context(), id) })
	info, err := iac.InstanceInfo(ctx, id)
	if err != nil || info.DisplayName != "Console made" || info.InstanceType != bigtable.DEVELOPMENT || info.Labels["team"] != "data" {
		t.Fatalf("after Create instance, InstanceInfo = %+v, %v", info, err)
	}
	if cl, err := iac.GetCluster(ctx, id, c1); err != nil || cl.Zone != "us-east1-c" || cl.StorageType != bigtable.HDD {
		t.Fatalf("after Create instance, GetCluster = %+v, %v", cl, err)
	}
	code, body := consoleDo(t, addr, http.MethodGet, "/api/resources/bigtable-instances?project="+project, "")
	if code != http.StatusOK || !strings.Contains(body, `"name":"`+id+`"`) {
		t.Errorf("the screen's listing = %d: %s", code, body)
	}

	// Edit instance, from the form the page draws.
	page := consoleBigtableInstance(t, addr, project, id)
	values, ok := actionDefaults(page.Actions, "editinstance")
	if !ok || values["displayName"] != "Console made" || values["type"] != "DEVELOPMENT" {
		t.Fatalf("Edit instance's form = %v (offered %v)", values, ok)
	}
	values["displayName"], values["type"], values["labels"] = "Console edited", "PRODUCTION", `{"team":"ml","env":"test"}`
	if code, out := consoleAct(t, addr, "bigtable-instances", project, []string{id}, "editinstance", values); code != http.StatusOK {
		t.Fatalf("Edit instance = %d: %s", code, out)
	}
	info, err = iac.InstanceInfo(ctx, id)
	if err != nil || info.DisplayName != "Console edited" || info.InstanceType != bigtable.PRODUCTION || info.Labels["env"] != "test" || info.Labels["team"] != "ml" {
		t.Errorf("after Edit instance, InstanceInfo = %+v, %v", info, err)
	}

	// A production cluster needs nodes: give c1 two, then add c2 autoscaled.
	if code, out := consoleAct(t, addr, "bigtable-instances", project, []string{id, "clusters", c1}, "editnodes",
		map[string]string{"nodes": "2", "autoscaling": "false"}); code != http.StatusOK {
		t.Fatalf("Edit nodes = %d: %s", code, out)
	}
	if cl, _ := iac.GetCluster(ctx, id, c1); cl == nil || cl.ServeNodes != 2 || cl.AutoscalingConfig != nil {
		t.Errorf("after Edit nodes, GetCluster = %+v", cl)
	}
	if code, out := consoleAct(t, addr, "bigtable-instances", project, []string{id}, "addcluster", map[string]string{
		"cluster": c2, "zone": "europe-west1-d", "storage": "SSD", "autoscaling": "true", "minNodes": "2", "maxNodes": "6", "cpuTarget": "55"}); code != http.StatusOK {
		t.Fatalf("Add cluster = %d: %s", code, out)
	}
	cl, err := iac.GetCluster(ctx, id, c2)
	if err != nil || cl.Zone != "europe-west1-d" || cl.AutoscalingConfig == nil || cl.AutoscalingConfig.MinNodes != 2 ||
		cl.AutoscalingConfig.MaxNodes != 6 || cl.AutoscalingConfig.CPUTargetPercent != 55 {
		t.Fatalf("after Add cluster, GetCluster = %+v, %v", cl, err)
	}
	if code, out := consoleAct(t, addr, "bigtable-instances", project, []string{id, "clusters", c1}, "editnodes",
		map[string]string{"autoscaling": "true", "minNodes": "1", "maxNodes": "4", "cpuTarget": "70"}); code != http.StatusOK {
		t.Fatalf("Edit nodes to autoscaling = %d: %s", code, out)
	}
	if cl, _ := iac.GetCluster(ctx, id, c1); cl == nil || cl.AutoscalingConfig == nil || cl.AutoscalingConfig.MaxNodes != 4 {
		t.Errorf("after Edit nodes to autoscaling, GetCluster = %+v", cl)
	}
	page = consoleBigtableInstance(t, addr, project, id)
	if len(page.Sections) < 2 || len(page.Sections[0].Listing.Items) != 2 {
		t.Fatalf("the Clusters tab = %+v", page.Sections)
	}

	// App profiles.
	if code, out := consoleAct(t, addr, "bigtable-instances", project, []string{id}, "createprofile", map[string]string{
		"profile": "reports", "description": "Reports", "routing": "SINGLE_CLUSTER", "routeTo": c2, "transactionalWrites": "true"}); code != http.StatusOK {
		t.Fatalf("Create app profile = %d: %s", code, out)
	}
	ap, err := iac.GetAppProfile(ctx, id, "reports")
	if err != nil || ap.GetDescription() != "Reports" || ap.GetSingleClusterRouting().GetClusterId() != c2 || !ap.GetSingleClusterRouting().GetAllowTransactionalWrites() {
		t.Fatalf("after Create app profile, GetAppProfile = %v, %v", ap, err)
	}
	if code, out := consoleAct(t, addr, "bigtable-instances", project, []string{id, "clusters", c2}, "deletecluster", nil); code == http.StatusOK ||
		!strings.Contains(consoleError(t, out), "routes to cluster") {
		t.Errorf("Delete cluster routed to by an app profile = %d: %s", code, out)
	}
	if code, out := consoleAct(t, addr, "bigtable-instances", project, []string{id, "appProfiles", "reports"}, "editprofile",
		map[string]string{"description": "Any cluster", "routing": "MULTI_CLUSTER"}); code != http.StatusOK {
		t.Fatalf("Edit app profile = %d: %s", code, out)
	}
	if ap, _ := iac.GetAppProfile(ctx, id, "reports"); ap.GetDescription() != "Any cluster" || ap.GetMultiClusterRoutingUseAny() == nil {
		t.Errorf("after Edit app profile, GetAppProfile = %v", ap)
	}
	if code, out := consoleAct(t, addr, "bigtable-instances", project, []string{id, "appProfiles", "reports"}, "deleteprofile", nil); code != http.StatusOK {
		t.Fatalf("Delete app profile = %d: %s", code, out)
	}
	if _, err := iac.GetAppProfile(ctx, id, "reports"); status.Code(err) != codes.NotFound {
		t.Errorf("after Delete app profile, GetAppProfile: %v, want NotFound", err)
	}
	if code, out := consoleAct(t, addr, "bigtable-instances", project, []string{id, "clusters", c2}, "deletecluster", nil); code != http.StatusOK {
		t.Fatalf("Delete cluster = %d: %s", code, out)
	}
	if cs, err := iac.Clusters(ctx, id); err != nil || len(cs) != 1 || cs[0].Name != c1 {
		t.Errorf("after Delete cluster, Clusters = %v, %v", cs, err)
	}
	if code, out := consoleAct(t, addr, "bigtable-instances", project, []string{id, "clusters", c1}, "deletecluster", nil); code == http.StatusOK ||
		!strings.Contains(consoleError(t, out), "last cluster") {
		t.Errorf("Delete of the last cluster = %d: %s", code, out)
	}

	// Permissions, through the client's instance IAM handle.
	consolePermissionsRoundTrip(t, h, addr, "bigtable-instances", []string{id}, handleOf(iac.InstanceIAM(id)),
		"roles/bigtable.user", "bigtable.tables.readRows")

	if code, out := consoleAct(t, addr, "bigtable-instances", project, []string{id}, "deleteinstance", nil); code != http.StatusOK {
		t.Fatalf("Delete instance = %d: %s", code, out)
	}
	if _, err := iac.InstanceInfo(ctx, id); status.Code(err) != codes.NotFound {
		t.Errorf("after Delete instance, InstanceInfo: %v, want NotFound", err)
	}
}
