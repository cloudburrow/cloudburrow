package serviceusage

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"regexp"
	"testing"

	"google.golang.org/api/option"
	sub "google.golang.org/api/serviceusage/v1beta1"

	"github.com/cloudburrow/cloudburrow/internal/resource"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// generateServiceIdentity through the official v1beta1 client: the
// Certificate Authority Service agent of the project, the same at every
// call; a service with no primary agent answers an empty identity; a served
// service not looked up is 501; an unserved service is refused as enable
// refuses it.
func TestGenerateServiceIdentityThroughTheOfficialClient(t *testing.T) {
	srv := NewServer("", []API{{Name: "privateca.googleapis.com"}, {Name: "storage.googleapis.com"}, {Name: "example.googleapis.com"}},
		[]Unserved{{Name: "spanner.googleapis.com", Reason: "start the instance with --services spanner"}}, store.NewMemory(), anyProject{})
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	c, err := sub.NewService(context.Background(), option.WithEndpoint(hs.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	gen := func(svc string) (map[string]any, error) {
		op, err := c.Services.GenerateServiceIdentity("projects/su-id/services/" + svc).Do()
		if err != nil {
			return nil, err
		}
		if !op.Done || op.Name != "operations/finished.DONE_OPERATION" {
			t.Errorf("%s: operation %s done=%v", svc, op.Name, op.Done)
		}
		var resp map[string]any
		return resp, json.Unmarshal(op.Response, &resp)
	}
	want := fmt.Sprintf("service-%d@gcp-sa-privateca.iam.gserviceaccount.com", resource.ProjectNumber("su-id"))
	a, err := gen("privateca.googleapis.com")
	if err != nil || a["email"] != want || len(a["uniqueId"].(string)) != 21 {
		t.Fatalf("privateca identity = %v, %v; want %s with a 21-digit uniqueId", a, err, want)
	}
	if b, _ := gen("privateca.googleapis.com"); b["uniqueId"] != a["uniqueId"] {
		t.Errorf("uniqueId changed between calls: %v, %v", a["uniqueId"], b["uniqueId"])
	}
	if s, err := gen("storage.googleapis.com"); err != nil || len(s) != 1 {
		t.Errorf("storage identity: %v, %v; want an empty ServiceIdentity, it has no primary agent", s, err)
	}
	if _, err := gen("example.googleapis.com"); code(err) != 501 {
		t.Errorf("a served API not looked up: %v; want 501, its agent not guessed", err)
	}
	if _, err := gen("spanner.googleapis.com"); code(err) != 400 {
		t.Errorf("unserved service: %v; want 400 FAILED_PRECONDITION", err)
	}
	if _, err := c.Services.List("projects/su-id").Do(); code(err) != 501 {
		t.Errorf("another v1beta1 method: %v; want 501", err)
	}
}

// Each served service's answer, as Google's service agent list gives its
// primary service agent (https://cloud.google.com/iam/docs/service-agents):
// the agent's email and a 21-digit uniqueId, or, for a service with no
// primary agent, a done operation with an empty ServiceIdentity, as gcloud
// documents.
func TestGenerateServiceIdentityEveryEntry(t *testing.T) {
	want := map[string]string{
		"privateca.googleapis.com":              "gcp-sa-privateca.iam.gserviceaccount.com",
		"container.googleapis.com":              "container-engine-robot.iam.gserviceaccount.com",
		"monitoring.googleapis.com":             "gcp-sa-monitoring-notification.iam.gserviceaccount.com",
		"compute.googleapis.com":                "compute-system.iam.gserviceaccount.com",
		"cloudkms.googleapis.com":               "gcp-sa-ekms.iam.gserviceaccount.com",
		"pubsub.googleapis.com":                 "gcp-sa-pubsub.iam.gserviceaccount.com",
		"secretmanager.googleapis.com":          "gcp-sa-secretmanager.iam.gserviceaccount.com",
		"sqladmin.googleapis.com":               "gcp-sa-cloud-sql.iam.gserviceaccount.com",
		"cloudtasks.googleapis.com":             "gcp-sa-cloudtasks.iam.gserviceaccount.com",
		"cloudscheduler.googleapis.com":         "gcp-sa-cloudscheduler.iam.gserviceaccount.com",
		"bigquerydatatransfer.googleapis.com":   "gcp-sa-bigquerydatatransfer.iam.gserviceaccount.com",
		"run.googleapis.com":                    "serverless-robot-prod.iam.gserviceaccount.com",
		"firestore.googleapis.com":              "gcp-sa-firestore.iam.gserviceaccount.com",
		"firebaserules.googleapis.com":          "firebase-rules.iam.gserviceaccount.com",
		"bigtableadmin.googleapis.com":          "gcp-sa-bigtable.iam.gserviceaccount.com",
		"spanner.googleapis.com":                "gcp-sa-spanner.iam.gserviceaccount.com",
		"redis.googleapis.com":                  "cloud-redis.iam.gserviceaccount.com",
		"backupdr.googleapis.com":               "gcp-sa-backupdr.iam.gserviceaccount.com",
		"apikeys.googleapis.com":                "gcp-sa-apikeys.iam.gserviceaccount.com",
		"containeranalysis.googleapis.com":      "container-analysis.iam.gserviceaccount.com",
		"containerscanning.googleapis.com":      "gcp-sa-containerscanning.iam.gserviceaccount.com",
		"observability.googleapis.com":          "gcp-sa-observability.iam.gserviceaccount.com",
		"logging.googleapis.com":                "",
		"storageinsights.googleapis.com":        "gcp-sa-storageinsights.iam.gserviceaccount.com",
		"storagetransfer.googleapis.com":        "",
		"storagebatchoperations.googleapis.com": "",
		"storage.googleapis.com":                "",
		"bigquery.googleapis.com":               "",
		"bigquerystorage.googleapis.com":        "",
		"datastore.googleapis.com":              "",
		"bigtable.googleapis.com":               "",
		"cloudresourcemanager.googleapis.com":   "",
		"serviceusage.googleapis.com":           "",
		"cloudquotas.googleapis.com":            "",
		"siteverification.googleapis.com":       "",
		"analyticshub.googleapis.com":           "gcp-sa-analyticshub.iam.gserviceaccount.com",
		"workflows.googleapis.com":              "gcp-sa-workflows.iam.gserviceaccount.com",
		"parametermanager.googleapis.com":       "gcp-sa-pm.iam.gserviceaccount.com",
		"vpcaccess.googleapis.com":              "gcp-sa-vpcaccess.iam.gserviceaccount.com",
		"workflowexecutions.googleapis.com":     "",
		"kmsinventory.googleapis.com":           "",
	}
	if len(want) != len(serviceAgents)+len(noPrimaryAgent) {
		t.Fatalf("want has %d services; the tables have %d", len(want), len(serviceAgents)+len(noPrimaryAgent))
	}
	var apis []API
	for name := range want {
		apis = append(apis, API{Name: name})
	}
	hs := httptest.NewServer(NewServer("", apis, nil, store.NewMemory(), anyProject{}))
	t.Cleanup(hs.Close)
	c, err := sub.NewService(context.Background(), option.WithEndpoint(hs.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	n := resource.ProjectNumber("su-every")
	if n < 100000000000 || n > 999999999999 {
		t.Errorf("project number %d: want 12 digits", n)
	}
	digits := regexp.MustCompile(`^1[0-9]{20}$`)
	for svc, domain := range want {
		op, err := c.Services.GenerateServiceIdentity("projects/su-every/services/" + svc).Do()
		if err != nil {
			t.Errorf("%s: %v", svc, err)
			continue
		}
		var resp map[string]any
		if err := json.Unmarshal(op.Response, &resp); err != nil {
			t.Fatal(err)
		}
		if !op.Done || resp["@type"] != typePrefixV1beta1+"ServiceIdentity" {
			t.Errorf("%s: operation %+v, response %v", svc, op, resp)
		}
		if domain == "" {
			if len(resp) != 1 {
				t.Errorf("%s: %v; want an empty ServiceIdentity, it has no primary service agent", svc, resp)
			}
			if ServiceAgent("su-every", svc) != "" {
				t.Errorf("%s: ServiceAgent names an agent", svc)
			}
			continue
		}
		email := fmt.Sprintf("service-%d@%s", n, domain)
		if resp["email"] != email || !digits.MatchString(fmt.Sprint(resp["uniqueId"])) {
			t.Errorf("%s: %v; want %s with a 21-digit uniqueId", svc, resp, email)
		}
		if ServiceAgent("su-every", svc) != email {
			t.Errorf("%s: ServiceAgent = %s; want %s", svc, ServiceAgent("su-every", svc), email)
		}
	}
	// Distinct agents have distinct uniqueIds.
	seen := map[string]string{}
	for svc := range serviceAgents {
		id := uniqueID(ServiceAgent("su-every", svc))
		if other, dup := seen[id]; dup {
			t.Errorf("%s and %s share uniqueId %s", svc, other, id)
		}
		seen[id] = svc
	}
}
