package main

// Rapid Caches in the console (#1505), through Google's generated JSON API
// client's RapidCaches service. A bucket's page has a Rapid caches tab (zone,
// type, state, TTL, admission policy, ingest on write, pending update), which
// says that no cache exists locally, and Create Rapid Cache; each cache's row
// offers Edit Rapid Cache, prefilled from rapidCaches.get, and Disable Rapid
// Cache while it runs. Create, edit and disable are long-running operations,
// answered with the operation, which then shows on the Operations tab.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	storagev1 "google.golang.org/api/storage/v1"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// rapidCachePage is the first segment of a Rapid Cache's action path,
// [rapidCachePage, bucket, zone]; a bucket's name cannot begin with an
// underscore, and the zone is last, so Disable asks for it back.
const rapidCachePage = "_rapidcache"

func rapidCachePath(bucket, id string) []string { return []string{rapidCachePage, bucket, id} }

var rapidAdmissionPolicies = []string{"admit-on-first-miss", "admit-on-second-miss", "no_read_admission"}

// rapidSection is a bucket's Rapid caches tab, from rapidCaches.list.
func (p storageProvider) rapidSection(ctx context.Context, bucket string) console.Section {
	sec := console.Section{ID: "rapidcaches", Label: "Rapid caches", Listing: console.Listing{
		Columns: []string{"Type", "State", "TTL", "Admission policy", "Ingest on write", "Pending update", "Created"}, NameColumn: "Zone",
		Noun: "caches", Items: []console.Resource{}, Note: noLocalCache,
	}}
	s, err := jsonAPI(ctx, p.endpoint)
	if err != nil {
		sec.Listing.Unavailable = err.Error()
		return sec
	}
	err = s.RapidCaches.List(bucket).Context(ctx).Pages(ctx, func(page *storagev1.RapidCaches) error {
		for _, c := range page.Items {
			row := console.Resource{Name: c.RapidCacheId, Fields: map[string]string{
				"Type": c.CacheType, "State": c.State, "TTL": c.Ttl, "Admission policy": c.AdmissionPolicy,
				"Ingest on write": fmt.Sprint(c.IngestOnWrite), "Pending update": fmt.Sprint(c.PendingUpdate),
				"Created": orDash(shortTime(c.CreateTime)),
			}, Target: rapidCachePath(bucket, c.RapidCacheId), Actions: rapidRowActions(c)}
			sec.Listing.Items = append(sec.Listing.Items, row)
		}
		return nil
	})
	if err != nil {
		sec.Listing.Unavailable = "cannot list the Rapid Caches: " + err.Error()
	}
	sec.Listing.Total = len(sec.Listing.Items)
	return sec
}

// createRapidAction is Create Rapid Cache, on a bucket's page.
func createRapidAction() console.Action {
	return console.Action{ID: "createrapidcache", Label: "Create Rapid Cache", Fields: []console.Field{
		{Name: "zone", Label: "Zone", Type: "text", Required: true, Pattern: `[a-z]+-[a-z]+[0-9]+-[a-z]`,
			Help: "A zone within the bucket's location, such as us-central1-a; it is the cache's ID, one cache per zone. " + noLocalCache},
		{Name: "cacheType", Label: "Cache type", Type: "select", Default: "rapid-cache", Options: []string{"rapid-cache", "rapid-cache-ultra"}},
		{Name: "ttl", Label: "TTL", Type: "text", Default: "86400s", Help: "Whole seconds, 3600s to 604800s (1h to 7d)."},
		{Name: "admissionPolicy", Label: "Admission policy", Type: "select", Default: "admit-on-first-miss", Options: rapidAdmissionPolicies},
		{Name: "ingestOnWrite", Label: "Ingest on write", Type: "checkbox", Default: "false"},
	}}
}

// rapidRowActions are a cache's row actions: Edit Rapid Cache, prefilled
// with its settings, and Disable Rapid Cache, while it runs.
func rapidRowActions(c *storagev1.RapidCache) []console.Action {
	if c.State != "running" {
		return nil
	}
	return []console.Action{
		{ID: "editrapidcache", Label: "Edit Rapid Cache", Fields: []console.Field{
			{Name: "ttl", Label: "TTL", Type: "text", Default: c.Ttl, Help: "Whole seconds, 3600s to 604800s (1h to 7d)."},
			{Name: "admissionPolicy", Label: "Admission policy", Type: "select", Default: c.AdmissionPolicy, Options: rapidAdmissionPolicies},
			{Name: "ingestOnWrite", Label: "Ingest on write", Type: "checkbox", Default: fmt.Sprint(c.IngestOnWrite)},
		}},
		{ID: "disablerapidcache", Label: "Disable Rapid Cache", Destructive: true,
			Confirm: fmt.Sprintf("The Rapid Cache in %s is disabled, and deleted an hour later; a create in its zone then makes a new one. "+
				"Managed folders that name it keep their policy.", c.RapidCacheId)},
	}
}

// rapidActions offers a cache's row actions at its path, read with
// rapidCaches.get.
func (p storageProvider) rapidActions(ctx context.Context, path []string) []console.Action {
	if len(path) != 3 || path[0] != rapidCachePage {
		return nil
	}
	s, err := jsonAPI(ctx, p.endpoint)
	if err != nil {
		return nil
	}
	c, err := s.RapidCaches.Get(path[1], path[2]).Context(ctx).Do()
	if err != nil {
		return nil
	}
	return rapidRowActions(c)
}

// actOnRapid performs Create, Edit and Disable Rapid Cache.
func (p storageProvider) actOnRapid(ctx context.Context, path []string, action string, values map[string]string) (bool, *console.Listing, error) {
	switch action {
	case "createrapidcache", "editrapidcache", "disablerapidcache":
	default:
		return false, nil, nil
	}
	s, err := jsonAPI(ctx, p.endpoint)
	if err != nil {
		return true, nil, err
	}
	v := func(k string) string { return strings.TrimSpace(values[k]) }
	var op *storagev1.GoogleLongrunningOperation
	switch action {
	case "createrapidcache":
		if len(path) != 1 || strings.HasPrefix(path[0], "_") {
			return true, nil, errors.New("a Rapid Cache is created on its bucket's page")
		}
		op, err = s.RapidCaches.Insert(path[0], &storagev1.RapidCache{Zone: v("zone"), CacheType: v("cacheType"), Ttl: v("ttl"),
			AdmissionPolicy: v("admissionPolicy"), IngestOnWrite: values["ingestOnWrite"] == "true"}).Context(ctx).Do()
	default:
		if len(path) != 3 || path[0] != rapidCachePage {
			return true, nil, errors.New("a Rapid Cache is addressed " + rapidCachePage + "/bucket/zone")
		}
		if action == "disablerapidcache" {
			op, err = s.RapidCaches.Disable(path[1], path[2]).Context(ctx).Do()
			break
		}
		// rapidCaches.update sets what it is sent; ingestOnWrite false is
		// sent too, so clearing the box turns it off.
		c := &storagev1.RapidCache{Ttl: v("ttl"), AdmissionPolicy: v("admissionPolicy"),
			IngestOnWrite: values["ingestOnWrite"] == "true", ForceSendFields: []string{"IngestOnWrite"}}
		op, err = s.RapidCaches.Update(path[1], path[2], c).Context(ctx).Do()
	}
	if err != nil {
		return true, nil, err
	}
	return true, operationResult(op), nil
}
