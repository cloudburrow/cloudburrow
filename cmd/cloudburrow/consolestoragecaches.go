package main

// Anywhere Caches, bucket relocation and operation cancel in the console
// (#1444), through Google's generated JSON API client, as gcloud storage
// buckets anywhere-caches and relocate call them. A bucket's page has an
// Anywhere caches tab (zone, state, TTL, admission policy, pending update),
// which says that no cache exists locally, and the actions Create Anywhere
// Cache, Change Anywhere Cache (pause, resume, disable, update), Relocate
// bucket, Finalize relocation and Cancel operation; the operations they
// start show on the Operations tab.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	storagev1 "google.golang.org/api/storage/v1"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const noLocalCache = "CloudBurrow keeps no cache tier: a cache here is a record of its settings and state, and every read is served from the bucket."

// cachesSection is a bucket's Anywhere caches tab.
func (p storageProvider) cachesSection(ctx context.Context, bucket string) console.Section {
	sec := console.Section{ID: "anywherecaches", Label: "Anywhere caches", Listing: console.Listing{
		Columns: []string{"State", "TTL", "Admission policy", "Ingest on write", "Pending update", "Created"}, NameColumn: "Zone",
		Noun: "caches", Items: []console.Resource{}, Note: noLocalCache,
	}}
	s, err := jsonAPI(ctx, p.endpoint)
	if err != nil {
		sec.Listing.Unavailable = err.Error()
		return sec
	}
	err = s.AnywhereCaches.List(bucket).Context(ctx).Pages(ctx, func(page *storagev1.AnywhereCaches) error {
		for _, c := range page.Items {
			sec.Listing.Items = append(sec.Listing.Items, console.Resource{Name: c.AnywhereCacheId, Fields: map[string]string{
				"State": c.State, "TTL": c.Ttl, "Admission policy": c.AdmissionPolicy,
				"Ingest on write": fmt.Sprint(c.IngestOnWrite), "Pending update": fmt.Sprint(c.PendingUpdate),
				"Created": orDash(shortTime(c.CreateTime)),
			}})
		}
		return nil
	})
	if err != nil {
		sec.Listing.Unavailable = "cannot list the Anywhere Caches: " + err.Error()
	}
	sec.Listing.Total = len(sec.Listing.Items)
	return sec
}

// cacheActions are a bucket page's Anywhere Cache, relocation and cancel
// actions.
func cacheActions() []console.Action {
	return []console.Action{
		{ID: "createanywherecache", Label: "Create Anywhere Cache", Fields: []console.Field{
			{Name: "zone", Label: "Zone", Type: "text", Required: true, Pattern: `[a-z]+-[a-z]+[0-9]+-[a-z]`,
				Help: "A zone within the bucket's location, such as us-central1-a; one cache per zone. " + noLocalCache},
			{Name: "ttl", Label: "TTL", Type: "text", Default: "86400s", Help: "Whole seconds, 3600s to 604800s (1h to 7d)."},
			{Name: "admissionPolicy", Label: "Admission policy", Type: "select", Default: "admit-on-first-miss",
				Options: []string{"admit-on-first-miss", "admit-on-second-miss"}},
			{Name: "ingestOnWrite", Label: "Ingest on write", Type: "checkbox", Default: "false"},
		}},
		{ID: "changeanywherecache", Label: "Change Anywhere Cache", Fields: []console.Field{
			{Name: "zone", Label: "Cache (zone)", Type: "text", Required: true},
			{Name: "change", Label: "Change", Type: "select", Default: "pause", Options: []string{"pause", "resume", "disable", "update"},
				Help: "Pause a running cache; resume a paused or disabled one; disable one (deleted after an hour unless resumed); or update its TTL and admission policy."},
			{Name: "ttl", Label: "TTL (update)", Type: "text", Help: "Empty keeps it."},
			{Name: "admissionPolicy", Label: "Admission policy (update)", Type: "select", Options: []string{"", "admit-on-first-miss", "admit-on-second-miss"}},
		}},
		{ID: "relocatebucket", Label: "Relocate bucket", Fields: []console.Field{
			{Name: "location", Label: "Destination location", Type: "text", Required: true, Help: "A region (US-EAST1), a dual-region (NAM4) or a multi-region (US, EU, ASIA)."},
			{Name: "placement", Label: "Placement", Type: "text", Help: "For a configurable dual-region: two regions of the multi-region, comma-separated."},
			{Name: "dryRun", Label: "Dry run", Type: "checkbox", Default: "false", Help: "Validate only; the bucket is not moved."},
		}},
		{ID: "finalizerelocation", Label: "Finalize relocation", Fields: []console.Field{
			{Name: "operation", Label: "Operation ID", Type: "text", Required: true, Help: "A relocation whose finalization state is READY (its row on the Operations tab)."},
			{Name: "ttl", Label: "Write downtime TTL", Type: "text", Default: "43200s"},
		}},
		{ID: "canceloperation", Label: "Cancel operation", Fields: []console.Field{
			{Name: "operation", Label: "Operation ID", Type: "text", Required: true,
				Help: "A running Anywhere Cache create or relocation; an update cannot be cancelled."},
		}},
	}
}

// actOnCaches performs the actions of cacheActions on the bucket path names.
func (p storageProvider) actOnCaches(ctx context.Context, path []string, action string, values map[string]string) (bool, *console.Listing, error) {
	switch action {
	case "createanywherecache", "changeanywherecache", "relocatebucket", "finalizerelocation", "canceloperation":
	default:
		return false, nil, nil
	}
	if len(path) != 1 || strings.HasPrefix(path[0], "_") {
		return true, nil, errors.New("this is done on a bucket's page")
	}
	b := path[0]
	s, err := jsonAPI(ctx, p.endpoint)
	if err != nil {
		return true, nil, err
	}
	v := func(k string) string { return strings.TrimSpace(values[k]) }
	switch action {
	case "createanywherecache":
		op, err := s.AnywhereCaches.Insert(b, &storagev1.AnywhereCache{Zone: v("zone"), Ttl: v("ttl"),
			AdmissionPolicy: v("admissionPolicy"), IngestOnWrite: values["ingestOnWrite"] == "true"}).Context(ctx).Do()
		if err != nil {
			return true, nil, err
		}
		return true, operationResult(op), nil
	case "changeanywherecache":
		var err error
		switch v("change") {
		case "pause":
			_, err = s.AnywhereCaches.Pause(b, v("zone")).Context(ctx).Do()
		case "resume":
			_, err = s.AnywhereCaches.Resume(b, v("zone")).Context(ctx).Do()
		case "disable":
			_, err = s.AnywhereCaches.Disable(b, v("zone")).Context(ctx).Do()
		case "update":
			var op *storagev1.GoogleLongrunningOperation
			op, err = s.AnywhereCaches.Update(b, v("zone"), &storagev1.AnywhereCache{Ttl: v("ttl"), AdmissionPolicy: v("admissionPolicy")}).Context(ctx).Do()
			if err == nil {
				return true, operationResult(op), nil
			}
		default:
			err = fmt.Errorf("change %q is not pause, resume, disable or update", v("change"))
		}
		return true, nil, err
	case "relocatebucket":
		req := &storagev1.RelocateBucketRequest{DestinationLocation: v("location"), ValidateOnly: values["dryRun"] == "true"}
		if pl := v("placement"); pl != "" {
			req.DestinationCustomPlacementConfig = &storagev1.RelocateBucketRequestDestinationCustomPlacementConfig{}
			for _, r := range strings.Split(pl, ",") {
				req.DestinationCustomPlacementConfig.DataLocations = append(req.DestinationCustomPlacementConfig.DataLocations, strings.TrimSpace(r))
			}
		}
		op, err := s.Buckets.Relocate(b, req).Context(ctx).Do()
		if err != nil {
			return true, nil, err
		}
		return true, operationResult(op), nil
	case "finalizerelocation":
		return true, nil, s.Operations.AdvanceRelocateBucket(b, v("operation"), &storagev1.AdvanceRelocateBucketOperationRequest{Ttl: v("ttl")}).Context(ctx).Do()
	}
	return true, nil, s.Operations.Cancel(b, v("operation")).Context(ctx).Do()
}
