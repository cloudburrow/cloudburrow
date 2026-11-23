//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	aiplatform "cloud.google.com/go/aiplatform/apiv1"
	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	"github.com/google/uuid"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// notebookClients are the official NotebookService client (cloud.google.com/go/aiplatform's
// is gRPC only) at the local Vertex AI endpoint the run shard's instance
// serves, with that endpoint's address for the JSON (REST) checks.
func notebookClients(t *testing.T, h *Harness) (string, map[string]*aiplatform.NotebookClient) {
	t.Helper()
	addr := runShardEndpoint(h, envRunLocalAI, EnvPrediction)
	g, err := aiplatform.NewNotebookClient(h.Context(), option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatalf("aiplatform.NewNotebookClient: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })
	return addr, map[string]*aiplatform.NotebookClient{"grpc": g}
}

// notebookJSON sends one JSON request to the endpoint's REST surface and
// returns the status and body.
func notebookJSON(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// covers: google.cloud.aiplatform.v1.NotebookService/CreateNotebookRuntimeTemplate, google.cloud.aiplatform.v1.NotebookService/GetNotebookRuntimeTemplate, google.cloud.aiplatform.v1.NotebookService/ListNotebookRuntimeTemplates, google.cloud.aiplatform.v1.NotebookService/UpdateNotebookRuntimeTemplate, google.cloud.aiplatform.v1.NotebookService/DeleteNotebookRuntimeTemplate
//
// TestColabEnterpriseRuntimeTemplates (#2105): through the official
// aiplatform NotebookClient (gRPC; the Go client has no REST transport) and
// the JSON surface, a runtime template is created
// with Google's defaults filled in (e2-standard-4, pd-standard 100 GB, idle
// shutdown 180 minutes, USER_DEFINED), read, listed with the documented
// filter and order_by, updated only on the documented mask paths, set as
// the default over REST (notebookRuntimeTemplates:setDefault), and
// deleted; out-of-range idle timeouts and disk sizes, a long display name
// and an undocumented filter field are INVALID_ARGUMENT.
func TestColabEnterpriseRuntimeTemplates(t *testing.T) {
	h := New(t)
	addr, clients := notebookClients(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	parent := fmt.Sprintf("projects/%s/locations/us-central1", h.Project())
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	for transport, c := range clients {
		t.Run(transport, func(t *testing.T) {
			for _, bad := range []*aiplatformpb.NotebookRuntimeTemplate{
				{DisplayName: strings.Repeat("n", 129)},
				{DisplayName: "x", IdleShutdownConfig: &aiplatformpb.NotebookIdleShutdownConfig{IdleTimeout: durationpb.New(time.Minute)}},
				{DisplayName: "x", DataPersistentDiskSpec: &aiplatformpb.PersistentDiskSpec{DiskType: "pd-standard", DiskSizeGb: 70000}},
			} {
				if _, err := c.CreateNotebookRuntimeTemplate(ctx, &aiplatformpb.CreateNotebookRuntimeTemplateRequest{Parent: parent, NotebookRuntimeTemplate: bad}); quotaCode(err) != codes.InvalidArgument {
					t.Errorf("create %v = %v, want INVALID_ARGUMENT", bad, err)
				}
			}
			id := "tpl-" + transport + "-" + suffix
			op, err := c.CreateNotebookRuntimeTemplate(ctx, &aiplatformpb.CreateNotebookRuntimeTemplateRequest{Parent: parent, NotebookRuntimeTemplateId: id,
				NotebookRuntimeTemplate: &aiplatformpb.NotebookRuntimeTemplate{DisplayName: "Template " + transport, Description: "d",
					Labels: map[string]string{"team": transport}}})
			if err != nil {
				t.Fatalf("CreateNotebookRuntimeTemplate: %v", err)
			}
			tpl, err := op.Wait(ctx)
			if err != nil {
				t.Fatalf("the create operation: %v", err)
			}
			t.Cleanup(func() {
				if op, err := c.DeleteNotebookRuntimeTemplate(context.Background(), &aiplatformpb.DeleteNotebookRuntimeTemplateRequest{Name: tpl.GetName()}); err == nil {
					_ = op.Wait(context.Background())
				}
			})
			if tpl.GetName() != parent+"/notebookRuntimeTemplates/"+id || tpl.GetMachineSpec().GetMachineType() != "e2-standard-4" ||
				tpl.GetDataPersistentDiskSpec().GetDiskType() != "pd-standard" || tpl.GetDataPersistentDiskSpec().GetDiskSizeGb() != 100 ||
				tpl.GetIdleShutdownConfig().GetIdleTimeout().AsDuration() != 180*time.Minute ||
				tpl.GetNotebookRuntimeType() != aiplatformpb.NotebookRuntimeType_USER_DEFINED || tpl.GetCreateTime() == nil || tpl.GetEtag() == "" {
				t.Errorf("created template = %v", tpl)
			}
			got, err := c.GetNotebookRuntimeTemplate(ctx, &aiplatformpb.GetNotebookRuntimeTemplateRequest{Name: tpl.GetName()})
			if err != nil || got.GetDisplayName() != "Template "+transport {
				t.Errorf("GetNotebookRuntimeTemplate = %v, %v", got, err)
			}
			it := c.ListNotebookRuntimeTemplates(ctx, &aiplatformpb.ListNotebookRuntimeTemplatesRequest{Parent: parent,
				Filter: fmt.Sprintf(`notebookRuntimeTemplate=%q AND labels.team=%s`, id, transport), OrderBy: "create_time desc"})
			n := 0
			for {
				x, err := it.Next()
				if err != nil {
					break
				}
				n++
				if x.GetName() != tpl.GetName() {
					t.Errorf("listed %s", x.GetName())
				}
			}
			if n != 1 {
				t.Errorf("the filtered list has %d templates, want 1", n)
			}
			if _, err := c.ListNotebookRuntimeTemplates(ctx, &aiplatformpb.ListNotebookRuntimeTemplatesRequest{Parent: parent, Filter: `description="d"`}).Next(); quotaCode(err) != codes.InvalidArgument {
				t.Errorf("an undocumented filter field = %v, want INVALID_ARGUMENT", err)
			}
			if _, err := c.UpdateNotebookRuntimeTemplate(ctx, &aiplatformpb.UpdateNotebookRuntimeTemplateRequest{
				NotebookRuntimeTemplate: &aiplatformpb.NotebookRuntimeTemplate{Name: tpl.GetName(), MachineSpec: &aiplatformpb.MachineSpec{MachineType: "n1-standard-8"}},
				UpdateMask:              &fieldmaskpb.FieldMask{Paths: []string{"machine_spec.machine_type"}}}); quotaCode(err) != codes.InvalidArgument {
				t.Errorf("updating machine_spec = %v, want INVALID_ARGUMENT", err)
			}
			up, err := c.UpdateNotebookRuntimeTemplate(ctx, &aiplatformpb.UpdateNotebookRuntimeTemplateRequest{
				NotebookRuntimeTemplate: &aiplatformpb.NotebookRuntimeTemplate{Name: tpl.GetName(), DisplayName: "Renamed " + transport,
					SoftwareConfig: &aiplatformpb.NotebookSoftwareConfig{Env: []*aiplatformpb.EnvVar{{Name: "STAGE", Value: "test"}}}},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name", "software_config.env"}}})
			if err != nil || up.GetDisplayName() != "Renamed "+transport || len(up.GetSoftwareConfig().GetEnv()) != 1 || up.GetLabels()["team"] != transport {
				t.Errorf("UpdateNotebookRuntimeTemplate = %v, %v", up, err)
			}
		})
	}
	// The JSON surface (google.api.http bindings): create, its
	// operation, get, list, patch, Set as Default (REST only, in the
	// discovery document) and delete.
	base := "http://" + addr + "/v1/" + parent + "/notebookRuntimeTemplates"
	restID := "tpl-json-" + suffix
	code, body := notebookJSON(t, http.MethodPost, base+"?notebookRuntimeTemplateId="+restID, `{"displayName":"JSON template","machineSpec":{"machineType":"n1-standard-4"}}`)
	if code != http.StatusOK || !strings.Contains(body, `"name"`) {
		t.Fatalf("REST create = %d %s", code, body)
	}
	var op struct {
		Name string `json:"name"`
		Done bool   `json:"done"`
	}
	if err := json.Unmarshal([]byte(body), &op); err != nil || !strings.Contains(op.Name, "/operations/") {
		t.Fatalf("REST create operation = %s", body)
	}
	if code, body := notebookJSON(t, http.MethodGet, "http://"+addr+"/v1/"+op.Name, ""); code != http.StatusOK || !strings.Contains(body, `"done": true`) && !strings.Contains(body, `"done":true`) {
		t.Errorf("REST operation = %d %s", code, body)
	}
	if code, body := notebookJSON(t, http.MethodGet, base+"/"+restID, ""); code != http.StatusOK || !strings.Contains(body, "n1-standard-4") || !strings.Contains(body, "pd-standard") {
		t.Errorf("REST get = %d %s", code, body)
	}
	if code, body := notebookJSON(t, http.MethodGet, base+"?filter="+url.QueryEscape(`notebookRuntimeTemplate="`+restID+`"`), ""); code != http.StatusOK || !strings.Contains(body, restID) {
		t.Errorf("REST list = %d %s", code, body)
	}
	if code, body := notebookJSON(t, http.MethodPatch, base+"/"+restID+"?updateMask=displayName", `{"displayName":"Patched"}`); code != http.StatusOK || !strings.Contains(body, "Patched") {
		t.Errorf("REST patch = %d %s", code, body)
	}
	if code, body := notebookJSON(t, http.MethodPost, base+":setDefault", `{"name":"`+parent+`/notebookRuntimeTemplates/`+restID+`"}`); code != http.StatusOK || !strings.Contains(body, `"notebookRuntimeTemplate"`) {
		t.Errorf("setDefault = %d %s", code, body)
	}
	if code, body := notebookJSON(t, http.MethodDelete, base+"/"+restID, ""); code != http.StatusOK {
		t.Errorf("REST delete = %d %s", code, body)
	}
	if code, _ := notebookJSON(t, http.MethodGet, base+"/"+restID, ""); code != http.StatusNotFound {
		t.Errorf("REST get after delete = %d, want 404", code)
	}
}

// covers: google.cloud.aiplatform.v1.NotebookService/AssignNotebookRuntime, google.cloud.aiplatform.v1.NotebookService/GetNotebookRuntime, google.cloud.aiplatform.v1.NotebookService/ListNotebookRuntimes, google.cloud.aiplatform.v1.NotebookService/StopNotebookRuntime, google.cloud.aiplatform.v1.NotebookService/StartNotebookRuntime, google.cloud.aiplatform.v1.NotebookService/UpgradeNotebookRuntime, google.cloud.aiplatform.v1.NotebookService/DeleteNotebookRuntime
//
// TestColabEnterpriseRuntimes (#2105): a runtime assigned from a template
// through the official NotebookClient is a notebook runtime of the
// instance's (a kernel container beside its BigQuery): the operation ends
// with it RUNNING and HEALTHY, with the template's settings and its
// inline post-startup script run; it lists by runtimeState and
// runtimeUser, stops (STOPPED), starts again, upgrades (nothing to do),
// and is deleted. The same ID assigned again is the existing assignment;
// a template with accelerators cannot start one (FAILED_PRECONDITION); a
// failing post-startup script fails the assignment, the runtime ERROR.
// The JSON surface reads the runtime the client made.
func TestColabEnterpriseRuntimes(t *testing.T) {
	h := New(t)
	_, clients := notebookClients(t, h)
	// The runtime queries the instance's BigQuery.
	runShardEndpoint(h, "CLOUDBURROW_TEST_RUN_BIGQUERY", EnvBigQuery)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	t.Cleanup(cancel)
	c := clients["grpc"]
	addr := runShardEndpoint(h, envRunLocalAI, EnvPrediction)
	parent := fmt.Sprintf("projects/%s/locations/us-central1", h.Project())
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	mkTemplate := func(id string, tpl *aiplatformpb.NotebookRuntimeTemplate) *aiplatformpb.NotebookRuntimeTemplate {
		t.Helper()
		op, err := c.CreateNotebookRuntimeTemplate(ctx, &aiplatformpb.CreateNotebookRuntimeTemplateRequest{Parent: parent, NotebookRuntimeTemplateId: id, NotebookRuntimeTemplate: tpl})
		if err != nil {
			t.Fatal(err)
		}
		out, err := op.Wait(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if op, err := c.DeleteNotebookRuntimeTemplate(context.Background(), &aiplatformpb.DeleteNotebookRuntimeTemplateRequest{Name: out.GetName()}); err == nil {
				_ = op.Wait(context.Background())
			}
		})
		return out
	}
	tpl := mkTemplate("rt-tpl-"+suffix, &aiplatformpb.NotebookRuntimeTemplate{DisplayName: "Runtime template",
		IdleShutdownConfig: &aiplatformpb.NotebookIdleShutdownConfig{IdleTimeout: durationpb.New(30 * time.Minute)},
		SoftwareConfig: &aiplatformpb.NotebookSoftwareConfig{Env: []*aiplatformpb.EnvVar{{Name: "STAGE", Value: "test"}},
			PostStartupScriptConfig: &aiplatformpb.PostStartupScriptConfig{PostStartupScript: `test "$STAGE" = test`}}})
	user := "dev@example.com"
	assign := func(tplName, id string) (*aiplatform.AssignNotebookRuntimeOperation, error) {
		return c.AssignNotebookRuntime(ctx, &aiplatformpb.AssignNotebookRuntimeRequest{Parent: parent, NotebookRuntimeTemplate: tplName, NotebookRuntimeId: id,
			NotebookRuntime: &aiplatformpb.NotebookRuntime{DisplayName: "Runtime " + id, RuntimeUser: user}})
	}
	id := "rt-" + suffix
	op, err := assign(tpl.GetName(), id)
	if err != nil {
		t.Fatalf("AssignNotebookRuntime: %v", err)
	}
	rt, err := op.Wait(ctx)
	if err != nil {
		t.Fatalf("the assign operation: %v", err)
	}
	t.Cleanup(func() {
		if op, err := c.DeleteNotebookRuntime(context.Background(), &aiplatformpb.DeleteNotebookRuntimeRequest{Name: rt.GetName()}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	if rt.GetRuntimeState() != aiplatformpb.NotebookRuntime_RUNNING || rt.GetHealthState() != aiplatformpb.NotebookRuntime_HEALTHY ||
		rt.GetNotebookRuntimeTemplateRef().GetNotebookRuntimeTemplate() != tpl.GetName() || rt.GetRuntimeUser() != user ||
		rt.GetIdleShutdownConfig().GetIdleTimeout().AsDuration() != 30*time.Minute || rt.GetExpirationTime() == nil {
		t.Errorf("assigned runtime = %v", rt)
	}
	again, err := assign(tpl.GetName(), id)
	if err != nil {
		t.Fatal(err)
	}
	if x, err := again.Wait(ctx); err != nil || x.GetName() != rt.GetName() {
		t.Errorf("assigning %s again = %v, %v; want the existing assignment", id, x, err)
	}
	if code, body := notebookJSON(t, http.MethodGet, "http://"+addr+"/v1/"+rt.GetName(), ""); code != http.StatusOK || !strings.Contains(body, "RUNNING") {
		t.Errorf("REST get runtime = %d %s", code, body)
	}
	it := c.ListNotebookRuntimes(ctx, &aiplatformpb.ListNotebookRuntimesRequest{Parent: parent, Filter: fmt.Sprintf(`runtimeState=RUNNING AND runtimeUser=%q AND notebookRuntime=%q`, user, id)})
	if x, err := it.Next(); err != nil || x.GetName() != rt.GetName() {
		t.Errorf("ListNotebookRuntimes = %v, %v", x, err)
	}
	stop, err := c.StopNotebookRuntime(ctx, &aiplatformpb.StopNotebookRuntimeRequest{Name: rt.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stop.Wait(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got, _ := c.GetNotebookRuntime(ctx, &aiplatformpb.GetNotebookRuntimeRequest{Name: rt.GetName()}); got.GetRuntimeState() != aiplatformpb.NotebookRuntime_STOPPED {
		t.Errorf("after stop = %v", got.GetRuntimeState())
	}
	start, err := c.StartNotebookRuntime(ctx, &aiplatformpb.StartNotebookRuntimeRequest{Name: rt.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := start.Wait(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if got, _ := c.GetNotebookRuntime(ctx, &aiplatformpb.GetNotebookRuntimeRequest{Name: rt.GetName()}); got.GetRuntimeState() != aiplatformpb.NotebookRuntime_RUNNING {
		t.Errorf("after start = %v", got.GetRuntimeState())
	}
	upg, err := c.UpgradeNotebookRuntime(ctx, &aiplatformpb.UpgradeNotebookRuntimeRequest{Name: rt.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upg.Wait(ctx); err != nil {
		t.Errorf("upgrade: %v", err)
	}

	gpu := mkTemplate("rt-gpu-"+suffix, &aiplatformpb.NotebookRuntimeTemplate{DisplayName: "GPU",
		MachineSpec: &aiplatformpb.MachineSpec{MachineType: "g2-standard-4", AcceleratorType: aiplatformpb.AcceleratorType_NVIDIA_L4, AcceleratorCount: 1}})
	if _, err := assign(gpu.GetName(), "rt-gpu-"+suffix); quotaCode(err) != codes.FailedPrecondition {
		t.Errorf("a runtime with accelerators = %v, want FAILED_PRECONDITION", err)
	}
	bad := mkTemplate("rt-bad-"+suffix, &aiplatformpb.NotebookRuntimeTemplate{DisplayName: "Bad script",
		SoftwareConfig: &aiplatformpb.NotebookSoftwareConfig{PostStartupScriptConfig: &aiplatformpb.PostStartupScriptConfig{PostStartupScript: "exit 7"}}})
	bop, err := assign(bad.GetName(), "rt-bad-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bop.Wait(ctx); err == nil {
		t.Error("a failing post-startup script: the assignment succeeded")
	}
	if got, err := c.GetNotebookRuntime(ctx, &aiplatformpb.GetNotebookRuntimeRequest{Name: parent + "/notebookRuntimes/rt-bad-" + suffix}); err != nil || got.GetRuntimeState() != aiplatformpb.NotebookRuntime_ERROR {
		t.Errorf("the failed runtime = %v, %v", got, err)
	}
	if op, err := c.DeleteNotebookRuntime(ctx, &aiplatformpb.DeleteNotebookRuntimeRequest{Name: parent + "/notebookRuntimes/rt-bad-" + suffix}); err == nil {
		_ = op.Wait(ctx)
	}

	del, err := c.DeleteNotebookRuntime(ctx, &aiplatformpb.DeleteNotebookRuntimeRequest{Name: rt.GetName()})
	if err != nil {
		t.Fatal(err)
	}
	if err := del.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetNotebookRuntime(ctx, &aiplatformpb.GetNotebookRuntimeRequest{Name: rt.GetName()}); quotaCode(err) != codes.NotFound {
		t.Errorf("a deleted runtime = %v, want NOT_FOUND", err)
	}
}
