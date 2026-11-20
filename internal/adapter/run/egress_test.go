package run

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	eventarcpb "cloud.google.com/go/eventarc/apiv1/eventarcpb"

	"github.com/cloudburrow/cloudburrow/internal/egress"
	"github.com/cloudburrow/cloudburrow/internal/egress/egresstest"
)

// egressFixture is a served API, a local URL that redirects to Google, and
// the process's map serving the API as name.
func egressFixture(t *testing.T, name string) (*egresstest.Recorder, string) {
	t.Helper()
	rec := &egresstest.Recorder{}
	api := httptest.NewServer(rec.Handler())
	t.Cleanup(api.Close)
	redirect := httptest.NewServer(http.RedirectHandler("https://storage.googleapis.com/b/x", http.StatusFound))
	t.Cleanup(redirect.Close)
	egress.SetLocal(func(a string) string {
		if a == name {
			return api.URL
		}
		return ""
	})
	t.Cleanup(func() { egress.SetLocal(nil) })
	return rec, redirect.URL
}

func wantLocal(t *testing.T, rec *egresstest.Recorder, path string) {
	t.Helper()
	if got := rec.Requests(); len(got) != 1 || !strings.HasPrefix(got[0], "POST "+path+" host=127.0.0.1:") {
		t.Errorf("local delivery: %v, want one POST %s", got, path)
	}
}

// TestEventarcHTTPDestinationsSendNothingToGoogle (#2015): a trigger's
// http_endpoint and a pipeline's HTTP destination on a served googleapis.com
// API reach this instance's endpoint of it; an unserved API, another Google
// host, or a local URL redirecting to Google is refused; the loopback-only
// sink beneath each delivery's own client counts no attempt off the machine.
func TestEventarcHTTPDestinationsSendNothingToGoogle(t *testing.T) {
	const path = "/v1/projects/p/locations/l/workflows/w/executions"
	t.Run("trigger", func(t *testing.T) {
		rec, redirect := egressFixture(t, "workflowexecutions")
		sink := egresstest.New(t)
		f := (&FunctionsServer{}).WithEvents(Events{})
		egress.SetNext(f.events.Client, sink.Transport())
		deliver := func(uri string) error {
			tr := &eventarcpb.Trigger{Name: "projects/p/locations/l/triggers/t", Destination: &eventarcpb.Destination{
				Descriptor_: &eventarcpb.Destination_HttpEndpoint{HttpEndpoint: &eventarcpb.HttpEndpoint{Uri: uri}}}}
			_, err := f.deliverEvent(context.Background(), tr, &CloudEvent{Headers: http.Header{}, Data: []byte("{}")})
			return err
		}
		if err := deliver("https://workflowexecutions.googleapis.com" + path); err != nil {
			t.Fatalf("served API: %v", err)
		}
		wantLocal(t, rec, path)
		for _, u := range []string{"https://translate.googleapis.com/v3/x", "https://x-uc.a.run.app/", redirect} {
			if err := deliver(u); err == nil || !strings.Contains(err.Error(), "refused") {
				t.Errorf("%s: %v, want a local refusal", u, err)
			}
		}
		sink.RequireNone(t)
	})
	t.Run("pipeline", func(t *testing.T) {
		rec, redirect := egressFixture(t, "workflowexecutions")
		sink := egresstest.New(t)
		prev := egress.SetNext(guardedDefault, sink.Transport())
		t.Cleanup(func() { egress.SetNext(guardedDefault, prev) })
		f := &FunctionsServer{}
		deliver := func(uri string) error {
			d := &eventarcpb.Pipeline_Destination{DestinationDescriptor: &eventarcpb.Pipeline_Destination_HttpEndpoint_{
				HttpEndpoint: &eventarcpb.Pipeline_Destination_HttpEndpoint{Uri: uri}}}
			return f.eaDeliverHTTP(context.Background(), "projects/p/locations/l/pipelines/x", d,
				map[string]any{"id": "1", "data": map[string]any{"a": 1}, "datacontenttype": "application/json"}, nil)
		}
		if err := deliver("https://workflowexecutions.googleapis.com" + path); err != nil {
			t.Fatalf("served API: %v", err)
		}
		wantLocal(t, rec, path)
		for _, u := range []string{"https://translate.googleapis.com/v3/x", "https://x-uc.a.run.app/", redirect} {
			if err := deliver(u); err == nil || !strings.Contains(err.Error(), "refused") {
				t.Errorf("%s: %v, want a local refusal", u, err)
			}
		}
		sink.RequireNone(t)
	})
}

// TestSourceManagerHooksSendNothingToGoogle (#2015): a hook on a Google host
// is not delivered, and one whose local target redirects to Google is
// refused by the guard on the hook client; no attempt leaves the machine.
func TestSourceManagerHooksSendNothingToGoogle(t *testing.T) {
	var rec egresstest.Recorder
	local := httptest.NewServer(rec.Handler())
	defer local.Close()
	_, redirect := egressFixture(t, "")
	sink := egresstest.New(t)
	prev := egress.SetNext(ssmHookClient, sink.Transport())
	t.Cleanup(func() { egress.SetNext(ssmHookClient, prev) })
	for _, u := range []string{"https://cloudbuild.googleapis.com/v1/x", "https://x.sourcemanager.dev/hook", redirect} {
		if d := deliver(context.Background(), ssmTarget{name: "h", url: u}, "PUSH", map[string]any{}); d.Err == "" {
			t.Errorf("%s was delivered: %+v", u, d)
		}
	}
	if d := deliver(context.Background(), ssmTarget{name: "h", url: local.URL + "/hook"}, "PUSH", map[string]any{}); d.Err != "" {
		t.Errorf("a local hook: %s", d.Err)
	}
	wantLocal(t, &rec, "/hook")
	sink.RequireNone(t)
}
