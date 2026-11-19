//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/api/googleapi"
	loggingjson "google.golang.org/api/logging/v2"
)

// TestLoggingListSinksInScope (#1577): ListSinks' `filter`, which the
// REST reference documents and loggingpb lacks, through Google's generated
// Logging v2 JSON client: in_scope("ANCESTOR") lists a project's
// ancestors' intercepting sinks, in_scope("ALL") its own sinks too, an empty filter
// and in_scope("DEFAULT") its own only
// (https://docs.cloud.google.com/logging/docs/reference/v2/rest/v2/projects.sinks/list).
// The sinks are made with the official ConfigClient over gRPC.
// covers: google.logging.v2.ConfigServiceV2/CreateSink, google.logging.v2.ConfigServiceV2/ListSinks
func TestLoggingListSinksInScope(t *testing.T) {
	h := New(t)
	opts := loggingClientOptions(h)
	ctx := h.Context()
	folder, project := loggingParentsFixture(t, h, "lis-")
	cc, err := vlogging.NewConfigClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	js := loggingJSONService(t, h)
	suffix := fmt.Sprint(time.Now().UnixNano())
	dest := "logging.googleapis.com/projects/" + h.Project()
	mk := func(parent string, sk *loggingpb.LogSink) string {
		t.Helper()
		if _, err := cc.CreateSink(ctx, &loggingpb.CreateSinkRequest{Parent: parent, Sink: sk}); err != nil {
			t.Fatalf("CreateSink in %s: %v", parent, err)
		}
		t.Cleanup(func() {
			_ = cc.DeleteSink(context.Background(), &loggingpb.DeleteSinkRequest{SinkName: parent + "/sinks/" + sk.GetName()})
		})
		return sk.GetName()
	}
	own := mk("projects/"+project, &loggingpb.LogSink{Name: "own-" + suffix, Destination: dest})
	grab := mk(folder, withIntercept(&loggingpb.LogSink{Name: "grab-" + suffix, Destination: dest, IncludeChildren: true}))
	plain := mk(folder, &loggingpb.LogSink{Name: "plain-" + suffix, Destination: dest, IncludeChildren: true})

	list := func(filter string) map[string]bool {
		t.Helper()
		out := map[string]bool{}
		call := js.Projects.Sinks.List("projects/" + project).PageSize(1)
		if filter != "" {
			call = call.Filter(filter)
		}
		if err := call.Pages(ctx, func(r *loggingjson.ListSinksResponse) error {
			for _, s := range r.Sinks {
				out[s.Name] = s.InterceptChildren
			}
			return nil
		}); err != nil {
			t.Fatalf("ListSinks filter %q: %v", filter, err)
		}
		return out
	}
	for _, tc := range []struct {
		filter      string
		want, never []string
	}{
		{"", []string{own}, []string{grab, plain}},
		{`in_scope("DEFAULT")`, []string{own}, []string{grab, plain}},
		{`in_scope("ANCESTOR")`, []string{grab}, []string{own, plain}},
		{`in_scope("ALL")`, []string{own, grab}, []string{plain}},
	} {
		got := list(tc.filter)
		for _, n := range tc.want {
			ic, ok := got[n]
			if !ok {
				t.Errorf("filter %q listed %v, missing %s", tc.filter, listedSinkNames(got), n)
			}
			if ic != (n == grab) {
				t.Errorf("filter %q: %s interceptChildren = %v", tc.filter, n, ic)
			}
		}
		for _, n := range tc.never {
			if _, ok := got[n]; ok {
				t.Errorf("filter %q listed %s", tc.filter, n)
			}
		}
	}
	var gerr *googleapi.Error
	if _, err := js.Projects.Sinks.List("projects/" + project).Filter(`name="x"`).Context(ctx).Do(); !errors.As(err, &gerr) || gerr.Code != http.StatusBadRequest {
		t.Errorf("an unsupported filter = %v, want 400", err)
	}
}

func listedSinkNames(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
