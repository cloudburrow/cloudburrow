//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	su "google.golang.org/api/serviceusage/v1"
)

// apiSwitch disables and enables one API in one project with the official
// Service Usage client, enabling it again when the test ends.
type apiSwitch struct {
	t       *testing.T
	h       *Harness
	c       *su.Service
	project string
	api     string
}

func newAPISwitch(t *testing.T, h *Harness, project, api string) *apiSwitch {
	t.Helper()
	s := &apiSwitch{t: t, h: h, c: serviceUsageClient(t, h), project: project, api: api}
	t.Cleanup(func() {
		_, _ = s.c.Services.Enable("projects/"+project+"/services/"+api, &su.EnableServiceRequest{}).Context(context.Background()).Do()
	})
	return s
}

// number is the project's number, as Service Usage names the project.
func (s *apiSwitch) number() string {
	s.t.Helper()
	got, err := s.c.Services.Get("projects/" + s.project + "/services/" + s.api).Context(s.h.Context()).Do()
	if err != nil {
		s.t.Fatalf("services.get %s: %v", s.api, err)
	}
	n, _, _ := strings.Cut(strings.TrimPrefix(got.Name, "projects/"), "/")
	return n
}

func (s *apiSwitch) disable() {
	s.t.Helper()
	if _, err := s.c.Services.Disable("projects/"+s.project+"/services/"+s.api, &su.DisableServiceRequest{}).Context(s.h.Context()).Do(); err != nil {
		s.t.Fatalf("services.disable %s: %v", s.api, err)
	}
}

func (s *apiSwitch) enable() {
	s.t.Helper()
	if _, err := s.c.Services.Enable("projects/"+s.project+"/services/"+s.api, &su.EnableServiceRequest{}).Context(s.h.Context()).Do(); err != nil {
		s.t.Fatalf("services.enable %s: %v", s.api, err)
	}
}

// serviceDisabledError is nil when err is Google's SERVICE_DISABLED for
// api in project: PERMISSION_DENIED (403), the message, and an ErrorInfo
// with Google's metadata.
func serviceDisabledError(err error, api, title, n string) error {
	if err == nil {
		return fmt.Errorf("the call succeeded")
	}
	// As the clients document it: the APIError in the chain (the REST
	// clients wrap one, parsed from Google's body, in their
	// googleapi.Error), else one parsed from the error.
	var ae *apierror.APIError
	ok := errors.As(err, &ae)
	if !ok || ae.Reason() == "" {
		ae, ok = apierror.FromError(err)
	}
	if !ok {
		return fmt.Errorf("not a Google API error: %v", err)
	}
	if ae.Reason() != "SERVICE_DISABLED" || ae.Domain() != "googleapis.com" {
		return fmt.Errorf("reason %q domain %q: %v", ae.Reason(), ae.Domain(), err)
	}
	if code := ae.HTTPCode(); code != -1 && code != http.StatusForbidden {
		return fmt.Errorf("HTTP %d, want 403: %v", code, err)
	}
	if st := ae.GRPCStatus(); st != nil && st.Code().String() != "PermissionDenied" {
		return fmt.Errorf("gRPC %v, want PermissionDenied", st.Code())
	}
	md := ae.Metadata()
	url := "https://console.developers.google.com/apis/api/" + api + "/overview?project=" + n
	if md["service"] != api || md["consumer"] != "projects/"+n || md["activationUrl"] != url || md["containerInfo"] != n {
		return fmt.Errorf("metadata %v (project number %s)", md, n)
	}
	want := title + " has not been used in project " + n + " before or it is disabled. Enable it by visiting " + url + " then retry."
	if !strings.Contains(err.Error(), want) {
		return fmt.Errorf("message lacks %q: %v", want, err)
	}
	return nil
}

// eventually retries f until it returns nil, for the servers in the
// cluster, which see a change within apienable.CacheFor.
func untilPropagated(t *testing.T, what string, f func() error) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		err := f()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %v", what, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// covers: serviceusage.services.disable, serviceusage.services.enable
//
// TestServiceDisabledStorage (#1914), with cloud.google.com/go/storage:
// the Cloud Storage API disabled in a bucket's project fails a read of
// the bucket with Google's SERVICE_DISABLED (403, ErrorInfo, message),
// and once it is enabled again the read succeeds.
func TestServiceDisabledStorage(t *testing.T) {
	h := New(t)
	c := storageClient(t, h)
	sw := newAPISwitch(t, h, h.Project(), "storage.googleapis.com")
	name := fmt.Sprintf("sd-%d", time.Now().UnixNano())
	b := c.Bucket(name)
	if err := b.Create(h.Context(), h.Project(), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Delete(context.Background()) })
	sw.disable()
	untilPropagated(t, "bucket.Attrs with storage disabled", func() error {
		_, err := b.Attrs(h.Context())
		return serviceDisabledError(err, "storage.googleapis.com", "Cloud Storage API", sw.number())
	})
	sw.enable()
	untilPropagated(t, "bucket.Attrs with storage enabled again", func() error {
		_, err := b.Attrs(h.Context())
		return err
	})
}

// covers: serviceusage.services.disable, serviceusage.services.enable
//
// TestServiceDisabledPubSub (#1914), with cloud.google.com/go/pubsub/v2
// over gRPC: GetTopic in a project whose Pub/Sub API is disabled fails
// PERMISSION_DENIED with SERVICE_DISABLED; enabled again, it succeeds.
func TestServiceDisabledPubSub(t *testing.T) {
	h := New(t)
	c := pubsubClient(t, h)
	sw := newAPISwitch(t, h, h.Project(), "pubsub.googleapis.com")
	name := topic(t, h, c, fmt.Sprintf("sd-%d", time.Now().UnixNano()))
	sw.disable()
	untilPropagated(t, "GetTopic with pubsub disabled", func() error {
		_, err := c.TopicAdminClient.GetTopic(h.Context(), &pubsubpb.GetTopicRequest{Topic: name})
		return serviceDisabledError(err, "pubsub.googleapis.com", "Cloud Pub/Sub API", sw.number())
	})
	sw.enable()
	untilPropagated(t, "GetTopic with pubsub enabled again", func() error {
		_, err := c.TopicAdminClient.GetTopic(h.Context(), &pubsubpb.GetTopicRequest{Topic: name})
		return err
	})
}

// covers: serviceusage.services.disable, serviceusage.services.enable
//
// TestServiceDisabledSecretManager (#1914), with
// cloud.google.com/go/secretmanager over gRPC: served in process, so the
// refusal is immediate, and so is the success once it is enabled.
func TestServiceDisabledSecretManager(t *testing.T) {
	h := New(t)
	c := secretsClient(t, h)
	sw := newAPISwitch(t, h, h.Project(), "secretmanager.googleapis.com")
	list := func() error {
		it := c.ListSecrets(h.Context(), &secretmanagerpb.ListSecretsRequest{Parent: secretsParent(h)})
		_, err := it.Next()
		if err == iterator.Done {
			return nil
		}
		return err
	}
	if err := list(); err != nil {
		t.Fatal(err)
	}
	sw.disable()
	if err := serviceDisabledError(list(), "secretmanager.googleapis.com", "Secret Manager API", sw.number()); err != nil {
		t.Fatalf("ListSecrets with secretmanager disabled: %v", err)
	}
	sw.enable()
	if err := list(); err != nil {
		t.Fatalf("ListSecrets enabled again: %v", err)
	}
}

// covers: serviceusage.services.disable, serviceusage.services.enable
//
// TestServiceDisabledBigQuery (#1914), with cloud.google.com/go/bigquery
// over REST. The emulator serves one project, which other tests share, so
// the call names a quota project of the test's own (x-goog-user-project,
// option.WithQuotaProject), whose BigQuery API is the one disabled:
// Google checks the API is enabled in the consumer project the call is
// billed to.
func TestServiceDisabledBigQuery(t *testing.T) {
	h := New(t)
	_, project := bigqueryClient(t, h)
	c, err := bigquery.NewClient(h.Context(), project, option.WithEndpoint(h.Endpoint(EnvBigQuery)),
		option.WithoutAuthentication(), option.WithQuotaProject(h.Project()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	sw := newAPISwitch(t, h, h.Project(), "bigquery.googleapis.com")
	list := func() error {
		_, err := c.Datasets(h.Context()).Next()
		if err == iterator.Done {
			return nil
		}
		return err
	}
	if err := list(); err != nil {
		t.Fatal(err)
	}
	sw.disable()
	untilPropagated(t, "datasets.list with bigquery disabled", func() error {
		return serviceDisabledError(list(), "bigquery.googleapis.com", "BigQuery API", sw.number())
	})
	sw.enable()
	untilPropagated(t, "datasets.list with bigquery enabled again", list)
}

// covers: serviceusage.services.disable, serviceusage.services.enable
//
// TestServiceDisabledFirestore (#1914), with cloud.google.com/go/firestore
// over gRPC: a document read in a project whose Firestore API is disabled
// fails with SERVICE_DISABLED; enabled again, it succeeds.
func TestServiceDisabledFirestore(t *testing.T) {
	h := New(t)
	c := firestoreClient(t, h)
	sw := newAPISwitch(t, h, h.Project(), "firestore.googleapis.com")
	doc := c.Collection("sd").Doc("d")
	if _, err := doc.Set(h.Context(), map[string]any{"a": 1}); err != nil {
		t.Fatal(err)
	}
	sw.disable()
	untilPropagated(t, "Get with firestore disabled", func() error {
		_, err := doc.Get(h.Context())
		return serviceDisabledError(err, "firestore.googleapis.com", "Cloud Firestore API", sw.number())
	})
	sw.enable()
	untilPropagated(t, "Get with firestore enabled again", func() error {
		_, err := doc.Get(h.Context())
		return err
	})
}

// covers: serviceusage.services.disable, serviceusage.services.enable
//
// TestGcloudServicesDisableEnforced (#1914): `gcloud services disable
// pubsub.googleapis.com` makes `gcloud pubsub topics list` in the project
// fail with Google's message, and `gcloud services enable` makes it work
// again.
func TestGcloudServicesDisableEnforced(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvPubSub)
	h.Endpoint(EnvServiceUsage)
	g := newGcloudSession(t, h)
	project := h.Project()
	sw := newAPISwitch(t, h, project, "pubsub.googleapis.com")
	g.must("services", "disable", "pubsub.googleapis.com", "--project", project, "--force")
	want := "Cloud Pub/Sub API has not been used in project " + sw.number() + " before or it is disabled"
	untilPropagated(t, "gcloud pubsub topics list with pubsub disabled", func() error {
		out, err := g.run(nil, "pubsub", "topics", "list", "--project", project)
		if err == nil || !strings.Contains(out, want) {
			return fmt.Errorf("err %v, output:\n%s", err, out)
		}
		return nil
	})
	g.must("services", "enable", "pubsub.googleapis.com", "--project", project)
	untilPropagated(t, "gcloud pubsub topics list with pubsub enabled again", func() error {
		out, err := g.run(nil, "pubsub", "topics", "list", "--project", project)
		if err != nil {
			return fmt.Errorf("%v:\n%s", err, out)
		}
		return nil
	})
}
