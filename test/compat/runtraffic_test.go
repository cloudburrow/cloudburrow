//go:build compat

package compat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.cloud.run.v2.Services/UpdateService, google.cloud.run.v2.Services/GetService
//
// TestRunTrafficSplitAndTags (#1224): through the official client, a service
// with two revisions is split 50/50 between the latest and the first, which
// is pinned by name, with a tag on each; traffic reads back as set and
// traffic_statuses as Knative resolved it, with a URL per tag; requests to
// the service's URL reach both revisions; each tag's URL answers with its
// own revision; and a split that does not sum to 100, or that names a
// revision the service does not have, is INVALID_ARGUMENT.
func TestRunTrafficSplitAndTags(t *testing.T) {
	h := New(t)
	c := runClient(t, h)
	ctx := runWait(t)
	if os.Getenv(envKubeconfig) == "" {
		if cli := os.Getenv(EnvCLI); cli != "" {
			t.Setenv(envKubeconfig, filepath.Join(instanceDirFrom(t, cliArgs()), "kubeconfig"))
		}
	}
	id := "compat-traffic"
	name := runParent(h) + "/services/" + id
	service := func(image, target string) *runpb.Service {
		return &runpb.Service{Name: name, Template: &runpb.RevisionTemplate{Containers: []*runpb.Container{{
			Image: image, Env: []*runpb.EnvVar{{Name: "TARGET", Values: &runpb.EnvVar_Value{Value: target}}}}}}}
	}
	op, err := c.CreateService(ctx, &runpb.CreateServiceRequest{Parent: runParent(h), ServiceId: id,
		Service: service("ghcr.io/knative/helloworld-go:latest", "first")})
	if err != nil {
		t.Fatalf("CreateService: %v", err)
	}
	t.Cleanup(func() { _, _ = c.DeleteService(h.Context(), &runpb.DeleteServiceRequest{Name: name}) })
	first, err := waitLRO(ctx, op)
	if err != nil {
		t.Fatalf("waiting for the service: %v", err)
	}
	firstRev := first.GetLatestReadyRevision()
	if tr := first.GetTraffic(); len(tr) != 1 || tr[0].GetType() != runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST || tr[0].GetPercent() != 100 {
		t.Errorf("a new service's traffic = %v, want 100%% to LATEST", tr)
	}

	uop, err := c.UpdateService(ctx, &runpb.UpdateServiceRequest{Service: service(helloDigest, "second")})
	if err != nil {
		t.Fatalf("UpdateService: %v", err)
	}
	second, err := waitLRO(ctx, uop)
	if err != nil {
		t.Fatalf("waiting for the update: %v", err)
	}
	secondRev := second.GetLatestReadyRevision()
	if firstRev == "" || secondRev == "" || firstRev == secondRev {
		t.Fatalf("revisions %q and %q; want two distinct ready revisions", firstRev, secondRev)
	}

	latest := runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_LATEST
	revision := runpb.TrafficTargetAllocationType_TRAFFIC_TARGET_ALLOCATION_TYPE_REVISION
	for _, bad := range [][]*runpb.TrafficTarget{
		{{Type: latest, Percent: 50}},
		{{Type: latest, Percent: 50}, {Type: revision, Revision: id + "-99999", Percent: 50}},
	} {
		svc := service(helloDigest, "second")
		svc.Traffic = bad
		if _, err := c.UpdateService(ctx, &runpb.UpdateServiceRequest{Service: svc}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("UpdateService with traffic %v = %v, want InvalidArgument", bad, err)
		}
	}

	split := service(helloDigest, "second")
	split.Traffic = []*runpb.TrafficTarget{
		{Type: latest, Percent: 50, Tag: "next"},
		{Type: revision, Revision: firstRev, Percent: 50, Tag: "old"},
	}
	sop, err := c.UpdateService(ctx, &runpb.UpdateServiceRequest{Service: split})
	if err != nil {
		t.Fatalf("UpdateService with a split: %v", err)
	}
	if _, err := waitLRO(ctx, sop); err != nil {
		t.Fatalf("waiting for the split: %v", err)
	}
	// traffic_statuses is Knative's status, which settles a moment after
	// the route is ready.
	var got *runpb.Service
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(time.Second) {
		got, err = c.GetService(ctx, &runpb.GetServiceRequest{Name: name})
		if err != nil {
			t.Fatalf("GetService: %v", err)
		}
		if len(got.GetTrafficStatuses()) == 2 || time.Now().After(deadline) {
			break
		}
	}
	tr := got.GetTraffic()
	if len(tr) != 2 || tr[0].GetType() != latest || tr[0].GetPercent() != 50 || tr[0].GetTag() != "next" ||
		tr[1].GetType() != revision || tr[1].GetRevision() != firstRev || tr[1].GetPercent() != 50 || tr[1].GetTag() != "old" {
		t.Errorf("traffic = %v, want the split as set", tr)
	}
	tagURI := map[string]string{}
	for _, st := range got.GetTrafficStatuses() {
		tagURI[st.GetTag()] = st.GetUri()
		want := firstRev
		if st.GetTag() == "next" {
			want = secondRev
		}
		if st.GetRevision() != want || st.GetPercent() != 50 || st.GetUri() == "" {
			t.Errorf("traffic status %v; want revision %s at 50%% with a tag URL", st, want)
		}
	}
	if len(got.GetTrafficStatuses()) != 2 {
		t.Fatalf("traffic_statuses = %v, want two", got.GetTrafficStatuses())
	}

	base := ingress(t)
	seen := map[string]int{}
	host := hostOf(t, got.GetUri())
	for i := 0; i < 40; i++ {
		body, code := secretEnvGet(t, base, host)
		if code != 200 {
			t.Fatalf("GET %s = %d %q", got.GetUri(), code, body)
		}
		switch {
		case strings.Contains(body, "Hello first"):
			seen["first"]++
		case strings.Contains(body, "Hello second"):
			seen["second"]++
		}
	}
	if seen["first"] == 0 || seen["second"] == 0 {
		t.Errorf("40 requests to the split service reached %v; want both revisions", seen)
	}
	t.Logf("50/50 split over 40 requests: %v", seen)

	for tag, want := range map[string]string{"next": "Hello second", "old": "Hello first"} {
		uri := tagURI[tag]
		if uri == "" {
			t.Errorf("tag %s has no URL", tag)
			continue
		}
		for i := 0; i < 5; i++ {
			if body, code := secretEnvGet(t, base, hostOf(t, uri)); code != 200 || !strings.Contains(body, want) {
				t.Errorf("GET %s (tag %s) = %d %q; want %q", uri, tag, code, body, want)
				break
			}
		}
	}
}
