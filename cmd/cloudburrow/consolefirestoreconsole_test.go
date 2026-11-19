package main

import (
	"context"
	"strings"
	"testing"
	"time"

	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
)

// TestConsoleFirestoreCreateDatabasePage (#1845): Google's Create a
// Firestore database values make the CreateDatabase request; the choices
// the local Firestore cannot serve are refused naming why.
func TestConsoleFirestoreCreateDatabasePage(t *testing.T) {
	req, err := createDatabaseRequest("p", map[string]string{"database": "orders", "edition": "ENTERPRISE", "mode": "native",
		"rules": "restrictive", "locationType": "region", "region": "europe-west3", "multiRegion": "nam5",
		"encryption": "google", "pitr": "true", "deleteProtection": "true"})
	if err != nil {
		t.Fatal(err)
	}
	db := req.GetDatabase()
	if req.GetDatabaseId() != "orders" || db.GetLocationId() != "europe-west3" || db.GetDatabaseEdition() != adminpb.Database_ENTERPRISE ||
		db.GetType() != adminpb.Database_FIRESTORE_NATIVE ||
		db.GetPointInTimeRecoveryEnablement() != adminpb.Database_POINT_IN_TIME_RECOVERY_ENABLED ||
		db.GetDeleteProtectionState() != adminpb.Database_DELETE_PROTECTION_ENABLED {
		t.Errorf("the request = %v", req)
	}
	req, err = createDatabaseRequest("p", map[string]string{"database": "eu", "locationType": "multi-region", "multiRegion": "eur3"})
	if err != nil || req.GetDatabase().GetLocationId() != "eur3" {
		t.Errorf("a multi-region: %v, %v", req, err)
	}
	for _, bad := range []map[string]string{
		{"database": "x123", "mode": "datastore"},
		{"database": "x123", "rules": "open"},
		{"database": "x123", "encryption": "kms"},
		{"database": "x123", "locationType": "region", "region": "mars-north1"},
		{"database": "x123", "locationType": "multi-region", "multiRegion": "us"},
	} {
		if _, err := createDatabaseRequest("p", bad); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
	_, fields := firestoreDatabasesProvider{}.CreateForm()
	var names []string
	for _, f := range fields {
		names = append(names, f.Name)
	}
	if got := strings.Join(names, ","); got != "database,edition,mode,rules,locationType,multiRegion,region,encryption,pitr,deleteProtection" {
		t.Errorf("the fields = %s", got)
	}
}

// TestConsoleFirestoreTTLPage (#1845): Create Policy makes a TTL policy
// with its expiration offset, the Time-to-live tab lists it with its
// status, and Delete removes the policy alone.
func TestConsoleFirestoreTTLPage(t *testing.T) {
	p := firestoreAdminFront(t)
	ctx := context.Background()
	at := []string{"(default)"}
	if err := p.ActAt(ctx, "p", at, "createttl", map[string]string{"collectionGroup": "sessions", "field": "expireAt",
		"offset": "3", "offsetUnit": "hours"}); err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "p", at, "createttl", map[string]string{"collectionGroup": "sessions", "field": "expireAt"}); err == nil {
		t.Error("a second policy on the same field was accepted")
	}
	d, err := p.Detail(ctx, "p", at)
	if err != nil {
		t.Fatal(err)
	}
	ttl := fsSection(d, "ttl")
	if ttl.Label != "Time-to-live" || len(ttl.Actions) != 1 || ttl.Actions[0].Label != "Create Policy" || len(ttl.Listing.Items) != 1 {
		t.Fatalf("the Time-to-live tab = %+v", ttl)
	}
	row := ttl.Listing.Items[0]
	if row.Fields["Collection group"] != "sessions" || row.Fields["Timestamp field"] != "expireAt" ||
		row.Fields["Expiration offset"] != "3 hours" || row.Fields["Status"] != "Active" {
		t.Errorf("the policy row = %v", row.Fields)
	}
	if acts := p.DetailActions(ctx, "p", row.ActsOn); len(acts) != 1 || acts[0].ID != "deletettl" {
		t.Errorf("the row's actions = %+v", acts)
	}
	if err := p.ActAt(ctx, "p", row.ActsOn, "deletettl", nil); err != nil {
		t.Fatal(err)
	}
	d, _ = p.Detail(ctx, "p", at)
	if n := len(fsSection(d, "ttl").Listing.Items); n != 0 {
		t.Errorf("%d policies after Delete", n)
	}
	var ids []string
	for _, s := range d.Sections {
		ids = append(ids, s.ID)
	}
	if got := strings.Join(ids, ","); got != "query,indexes,fields,importexport,schedules,ttl" {
		t.Errorf("the tabs = %s", got)
	}
}

// TestConsoleFirestoreQueryBuilderValues (#1845): each WHERE value is of
// its chosen type, and a list for the operators that take one.
func TestConsoleFirestoreQueryBuilderValues(t *testing.T) {
	if v, err := queryValue("number", "42", "=="); err != nil || v != int64(42) {
		t.Errorf("number: %v, %v", v, err)
	}
	if v, err := queryValue("boolean", "true", "=="); err != nil || v != true {
		t.Errorf("boolean: %v, %v", v, err)
	}
	if v, err := queryValue("timestamp", "2026-01-02T15:04:05Z", "=="); err != nil || !v.(time.Time).Equal(time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)) {
		t.Errorf("timestamp: %v, %v", v, err)
	}
	if v, err := queryValue("string", "a, b", "in"); err != nil || len(v.([]any)) != 2 {
		t.Errorf("in: %v, %v", v, err)
	}
	if _, err := queryValue("number", "abc", "=="); err == nil {
		t.Error("abc was a number")
	}
	if label, fields := (firestoreDatabasesProvider{}).QueryForm(nil); label != "" || fields != nil {
		t.Error("the Query builder was offered off a database's page")
	}
}
