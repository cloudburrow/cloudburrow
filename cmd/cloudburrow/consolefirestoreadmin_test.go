package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	admin "cloud.google.com/go/firestore/apiv1/admin"
	adminpb "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/callfront"
	"github.com/cloudburrow/cloudburrow/internal/calllog"
	"github.com/cloudburrow/cloudburrow/internal/firestoreadmin"
)

// TestConsoleFirestoreDatabasesIndexesAndFields (#1331): the Firestore
// databases screen lists (default) and a database Create database made, a
// database's page shows its settings, its composite indexes and its field
// overrides as the Admin API returns them, and Delete database removes it.
// The API is the call front's own (internal/firestoreadmin), in front of a
// stand-in for the emulator.
func TestConsoleFirestoreDatabasesIndexesAndFields(t *testing.T) {
	emu := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer emu.Close()
	up := strings.TrimPrefix(emu.URL, "http://")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := callfront.Server(callfront.HandlerWith(up, &calllog.Ring{}, t.Logf, firestoreadmin.New(up)), t.Logf)
	go func() { _ = srv.Serve(l) }()
	defer srv.Close()
	p := firestoreDatabasesProvider{endpoint: l.Addr().String()}
	ctx := context.Background()

	name, err := p.Create(ctx, "p", map[string]string{"database": "orders", "location": "eur3", "concurrency": "OPTIMISTIC"})
	if err != nil || name != "orders" {
		t.Fatalf("Create = %q, %v", name, err)
	}
	list, err := p.List(ctx, "p")
	if err != nil || list.Unavailable != "" || len(list.Items) != 2 {
		t.Fatalf("List = %+v, %v", list, err)
	}
	if got := list.Items[1]; got.Name != "orders" || got.Fields["Location"] != "eur3" || got.Fields["Concurrency"] != "Optimistic" || got.Fields["Type"] != "Native" {
		t.Errorf("the orders row = %+v", got)
	}

	c, err := admin.NewFirestoreAdminClient(ctx, localOpts(p.endpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	db := "projects/p/databases/orders"
	if _, err := c.CreateIndex(ctx, &adminpb.CreateIndexRequest{Parent: db + "/collectionGroups/lines", Index: &adminpb.Index{
		QueryScope: adminpb.Index_COLLECTION, Fields: []*adminpb.Index_IndexField{
			{FieldPath: "sku", ValueMode: &adminpb.Index_IndexField_Order_{Order: adminpb.Index_IndexField_ASCENDING}},
			{FieldPath: "qty", ValueMode: &adminpb.Index_IndexField_Order_{Order: adminpb.Index_IndexField_DESCENDING}},
		}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpdateField(ctx, &adminpb.UpdateFieldRequest{
		Field:      &adminpb.Field{Name: db + "/collectionGroups/sessions/fields/expireAt", TtlConfig: &adminpb.Field_TtlConfig{}},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"ttl_config"}}}); err != nil {
		t.Fatal(err)
	}

	d, err := p.Detail(ctx, "p", []string{"orders"})
	if err != nil || d.Unavailable != "" || len(d.Sections) != 2 {
		t.Fatalf("Detail = %+v, %v", d, err)
	}
	props := map[string]string{}
	for _, pr := range d.Summary {
		props[pr.Label] = pr.Value
	}
	if props["Location"] != "eur3" || props["Concurrency mode"] != "Optimistic" || props["Delete protection"] != "Disabled" {
		t.Errorf("the summary = %v", props)
	}
	idx := d.Sections[0].Listing
	if len(idx.Items) != 1 || idx.Items[0].Fields["Collection group"] != "lines" ||
		idx.Items[0].Fields["Fields"] != "sku Ascending, qty Descending, __name__ Descending" || idx.Items[0].Fields["State"] != "Ready" {
		t.Errorf("the Indexes tab = %+v", idx.Items)
	}
	fields := d.Sections[1].Listing
	if len(fields.Items) != 1 || fields.Items[0].Name != "expireAt" || fields.Items[0].Fields["TTL"] != "Active" ||
		fields.Items[0].Fields["Indexes"] != "Inherited" {
		t.Errorf("the Field overrides tab = %+v", fields.Items)
	}

	if err := p.Delete(ctx, "p", "orders"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if list, _ := p.List(ctx, "p"); len(list.Items) != 1 || list.Items[0].Name != "(default)" {
		t.Errorf("after Delete = %+v", list.Items)
	}
}
