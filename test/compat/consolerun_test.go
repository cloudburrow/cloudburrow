//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	run "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// consoleRunEditForm reads a Cloud Run service's page from the console and
// returns its edit form's label, and the values the form would submit — every
// field's prefilled default except the immutable ones, which the browser does
// not send — together with the name field's help.
func consoleRunEditForm(t *testing.T, addr, project, id string) (label string, values map[string]string, nameHelp string) {
	t.Helper()
	code, body := consoleDo(t, addr, http.MethodGet, "/api/detail/run?project="+project+"&name="+id, "")
	if code != http.StatusOK {
		t.Fatalf("console detail = %d: %s", code, body)
	}
	var detail struct {
		Unavailable string
		Edit        *struct {
			Label  string
			Fields []struct {
				Name, Default, Help string
				Immutable           bool
			}
		}
	}
	if err := json.Unmarshal([]byte(body), &detail); err != nil {
		t.Fatalf("decode detail: %v: %s", err, body)
	}
	if detail.Edit == nil {
		t.Fatalf("the service page offers no edit (unavailable: %q): %s", detail.Unavailable, body)
	}
	values = map[string]string{}
	for _, f := range detail.Edit.Fields {
		if f.Immutable {
			if f.Name == "name" {
				nameHelp = f.Help
				if f.Default != id {
					t.Errorf("the service name field shows %q, want %q", f.Default, id)
				}
			}
			continue
		}
		values[f.Name] = f.Default
	}
	return detail.Edit.Label, values, nameHelp
}

func consoleRunEdit(t *testing.T, addr, project, id string, values map[string]string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"Path": []string{id}, "Values": values})
	return consoleDo(t, addr, http.MethodPatch, "/api/resources/run?project="+project, string(body))
}

// covers: google.cloud.run.v2.Services/UpdateService, google.cloud.run.v2.Revisions/GetRevision
//
// TestConsoleRunEditAndDeployNewRevision (#595): the console's "Edit and
// deploy new revision" changes one environment variable of a service the
// official client deployed, and the official client then reads a new
// revision carrying it. A submitted change to the service name is refused
// with the name field's own help text, and a rollout of an image that does
// not exist is recorded in the operations ledger with the adapter's message,
// leaving the working revision serving. The first revision, now serving
// nothing, is deleted from its row's Delete revision, which the serving one
// does not offer (#785).
func TestConsoleRunEditAndDeployNewRevision(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	c := runClient(t, h)
	project := h.Project()
	id := "compat-console-edit"
	name := runParent(h) + "/services/" + id

	op, err := c.CreateService(h.Context(), &runpb.CreateServiceRequest{
		Parent: runParent(h), ServiceId: id,
		Service: &runpb.Service{Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{
			Image: "ghcr.io/knative/helloworld-go:latest",
			Env:   []*runpb.EnvVar{{Name: "TARGET", Values: &runpb.EnvVar_Value{Value: "first"}}},
		}}}},
	})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, _ = c.DeleteService(dctx, &runpb.DeleteServiceRequest{Name: name})
	})
	// The first revision is the slow part: on a loaded runner it took longer
	// than the harness's one-minute context (#809's run shard, 2026-09-28).
	wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer wcancel()
	first, err := op.Wait(wctx)
	if err != nil {
		t.Fatalf("waiting for the service: %v", err)
	}

	label, values, nameHelp := consoleRunEditForm(t, addr, project, id)
	if label != "Edit and deploy new revision" {
		t.Errorf("edit label = %q", label)
	}
	if values["image"] != "ghcr.io/knative/helloworld-go:latest" {
		t.Errorf("image prefilled as %q", values["image"])
	}
	var env map[string]string
	if err := json.Unmarshal([]byte(values["env"]), &env); err != nil || env["TARGET"] != "first" {
		t.Fatalf("env prefilled as %q (%v), want TARGET=first", values["env"], err)
	}

	// A rename is refused, with the sentence the form already showed.
	renamed := map[string]string{"name": id + "-renamed"}
	for k, v := range values {
		renamed[k] = v
	}
	code, body := consoleRunEdit(t, addr, project, id, renamed)
	if code != http.StatusBadRequest {
		t.Fatalf("rename = %d, want 400: %s", code, body)
	}
	var refusal struct{ Error string }
	_ = json.Unmarshal([]byte(body), &refusal)
	if nameHelp == "" || refusal.Error != nameHelp {
		t.Errorf("rename refused with %q, want the name field's own help %q", refusal.Error, nameHelp)
	}

	// Change one variable and deploy.
	env["TARGET"] = "second"
	encoded, _ := json.Marshal(env)
	values["env"] = string(encoded)
	code, body = consoleRunEdit(t, addr, project, id, values)
	switch {
	case code == http.StatusOK:
	case code == http.StatusBadRequest && strings.Contains(body, "may still become ready") && strings.Contains(body, `"operation"`):
		// The console stops waiting after its own minute and says the
		// rollout carries on. In a loaded merge-queue build it took longer
		// than that (#765), so the revision history is read, as the message
		// says, until a new revision is serving.
		t.Logf("the console stopped waiting: %s", body)
	default:
		t.Fatalf("console edit = %d: %s", code, body)
	}

	var got *runpb.Service
	for deadline := time.Now().Add(5 * time.Minute); ; {
		got, err = c.GetService(h.Context(), &runpb.GetServiceRequest{Name: name})
		if err != nil {
			t.Fatalf("GetService: %v", err)
		}
		if r := got.GetLatestReadyRevision(); (r != "" && r != first.GetLatestReadyRevision()) || time.Now().After(deadline) {
			break
		}
		time.Sleep(3 * time.Second)
	}
	serving := got.GetLatestReadyRevision()
	if serving == "" || serving == first.GetLatestReadyRevision() {
		t.Fatalf("serving revision after the edit = %q, before = %q: no new revision is serving",
			serving, first.GetLatestReadyRevision())
	}
	rc, err := run.NewRevisionsClient(h.Context(), runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	rev, err := rc.GetRevision(h.Context(), &runpb.GetRevisionRequest{Name: name + "/revisions/" + serving})
	if err != nil {
		t.Fatalf("GetRevision(%s): %v", serving, err)
	}
	if rev.GetGeneration() != 2 {
		t.Errorf("new revision generation = %d, want 2", rev.GetGeneration())
	}
	target := ""
	for _, c := range rev.GetContainers() {
		for _, e := range c.GetEnv() {
			if e.GetName() == "TARGET" {
				target = e.GetValue()
			}
		}
	}
	if target != "second" {
		t.Errorf("the new revision's TARGET = %q, want second: %v", target, rev.GetContainers())
	}

	// An image that does not exist: the rollout fails, the adapter's reason
	// is in the ledger, and the working revision keeps serving.
	values["image"] = "ghcr.io/knative/helloworld-go:cloudburrow-no-such-tag"
	code, body = consoleRunEdit(t, addr, project, id, values)
	if code != http.StatusBadRequest {
		t.Fatalf("bad-image edit = %d, want 400: %s", code, body)
	}
	var failed struct{ Error, Operation string }
	_ = json.Unmarshal([]byte(body), &failed)

	code, body = consoleDo(t, addr, http.MethodGet, "/api/operations?project="+project, "")
	if code != http.StatusOK {
		t.Fatalf("operations = %d: %s", code, body)
	}
	var ledger struct {
		Operations []struct{ ID, Kind, Resource, State, Error string }
	}
	if err := json.Unmarshal([]byte(body), &ledger); err != nil {
		t.Fatalf("decode operations: %v", err)
	}
	var entry *struct{ ID, Kind, Resource, State, Error string }
	for i := range ledger.Operations {
		if ledger.Operations[i].ID == failed.Operation {
			entry = &ledger.Operations[i]
		}
	}
	if entry == nil {
		t.Fatalf("the failed edit %q is not in the ledger: %s", failed.Operation, body)
	}
	if entry.Kind != "update" || entry.State != "FAILED" {
		t.Errorf("ledger entry = %+v, want a FAILED update", *entry)
	}
	// The adapter's own words for a revision that failed, and its code.
	if !strings.Contains(entry.Error, "FailedPrecondition") || !strings.Contains(entry.Error, "revision failed") {
		t.Errorf("ledger error = %q, want the adapter's FailedPrecondition \"revision failed\" message", entry.Error)
	}
	if strings.Contains(entry.Error, "rpc error") {
		t.Errorf("the gRPC envelope reached the ledger: %q", entry.Error)
	}
	t.Logf("bad-image rollout recorded as: %s", entry.Error)

	after, err := c.GetService(h.Context(), &runpb.GetServiceRequest{Name: name})
	if err != nil {
		t.Fatalf("GetService: %v", err)
	}
	if after.GetLatestReadyRevision() != serving {
		t.Errorf("serving revision after a failed rollout = %q, want %q still serving",
			after.GetLatestReadyRevision(), serving)
	}

	// Delete revision (#785): the first revision serves nothing now, so its
	// row and its page offer the delete; the serving one offers none. The
	// console's delete is NOT_FOUND through the official RevisionsClient.
	old := first.GetLatestReadyRevision()
	svcPage := consoleDetailOf(t, addr, "run", project, id)
	rowOffers := map[string]bool{}
	for _, sec := range svcPage.Sections {
		if sec.ID != "revisions" {
			continue
		}
		for _, row := range sec.Listing.Items {
			for _, a := range row.Actions {
				if a.ID == "delete-revision" {
					rowOffers[row.Name] = true
				}
			}
		}
	}
	if !rowOffers[old] || rowOffers[serving] {
		t.Errorf("Delete revision is offered on the rows %v; want %s and not the serving %s", rowOffers, old, serving)
	}
	if page := consoleDetailOf(t, addr, "run", project, id, serving); page.offers("delete-revision") {
		t.Errorf("the serving revision %s offers Delete revision", serving)
	}
	if page := consoleDetailOf(t, addr, "run", project, id, old); !page.offers("delete-revision") {
		t.Errorf("the superseded revision %s does not offer Delete revision: %v", old, page.Actions)
	}
	consoleActAt(t, addr, "run", project, "delete-revision", id, old)
	if _, err := rc.GetRevision(h.Context(), &runpb.GetRevisionRequest{Name: name + "/revisions/" + old}); status.Code(err) != codes.NotFound {
		t.Errorf("GetRevision(%s) after the console's delete = %v, want NotFound", old, err)
	}
	if _, err := rc.GetRevision(h.Context(), &runpb.GetRevisionRequest{Name: name + "/revisions/" + serving}); err != nil {
		t.Errorf("the serving revision is gone after deleting another: %v", err)
	}
}
