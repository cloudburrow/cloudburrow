//go:build compat

package compat

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"google.golang.org/api/iterator"
)

// TestConsoleBigQueryCreateDatasetTagsAndUploadTable (#1861).
//
// Create dataset's Tags are Resource Manager tag bindings: a dataset
// created with a tag reads back through the official TagBindings client
// bound to the tag value, and a tag value that does not exist is refused
// before any dataset is made. A region from Google's list is the dataset's
// location. Create table from Upload loads the file into a new table, read
// back with the official BigQuery client, and refuses a table that exists
// already (WRITE_EMPTY) and an Upload sent without a file.
func TestConsoleBigQueryCreateDatasetTagsAndUploadTable(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	addr := consoleAddr(t, h)
	rmEndpoint := shardEndpoint(h, "CLOUDBURROW_TEST_BIGQUERY_RESOURCEMANAGER", EnvResourceManager)
	if addr == "" || rmEndpoint == "" {
		t.Skip("needs the console and Resource Manager")
	}
	ctx := h.Context()
	q := "?project=" + url.QueryEscape(project)
	stamp := strings.ReplaceAll(h.Project(), "-", "_")

	kc, err := resourcemanager.NewTagKeysClient(ctx, rmOptionsAt(rmEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kc.Close() })
	vc, err := resourcemanager.NewTagValuesClient(ctx, rmOptionsAt(rmEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vc.Close() })
	bc, err := resourcemanager.NewTagBindingsClient(ctx, rmOptionsAt(rmEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bc.Close() })
	short := fmt.Sprintf("studio%d", time.Now().UnixNano()%1e9)
	kop, err := kc.CreateTagKey(ctx, &rmpb.CreateTagKeyRequest{TagKey: &rmpb.TagKey{Parent: "projects/" + project, ShortName: short}})
	if err != nil {
		t.Fatalf("CreateTagKey: %v", err)
	}
	key, err := kop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateTagKey: %v", err)
	}
	vop, err := vc.CreateTagValue(ctx, &rmpb.CreateTagValueRequest{TagValue: &rmpb.TagValue{Parent: key.GetName(), ShortName: "prod"}})
	if err != nil {
		t.Fatalf("CreateTagValue: %v", err)
	}
	prod, err := vop.Wait(ctx)
	if err != nil {
		t.Fatalf("CreateTagValue: %v", err)
	}
	id := "studio_tags_" + stamp
	target := "//bigquery.googleapis.com/projects/" + project + "/datasets/" + id
	t.Cleanup(func() {
		it := bc.ListTagBindings(ctx, &rmpb.ListTagBindingsRequest{Parent: target})
		for {
			b, err := it.Next()
			if err != nil {
				break
			}
			if op, err := bc.DeleteTagBinding(ctx, &rmpb.DeleteTagBindingRequest{Name: b.GetName()}); err == nil {
				_ = op.Wait(ctx)
			}
		}
		_ = c.Dataset(id).DeleteWithContents(ctx)
		if op, err := vc.DeleteTagValue(ctx, &rmpb.DeleteTagValueRequest{Name: prod.GetName()}); err == nil {
			_, _ = op.Wait(ctx)
		}
		if op, err := kc.DeleteTagKey(ctx, &rmpb.DeleteTagKeyRequest{Name: key.GetName()}); err == nil {
			_, _ = op.Wait(ctx)
		}
	})

	// The form offers Tags once a tag value exists.
	code, out := consoleDo(t, addr, http.MethodGet, "/api/services", "")
	if code != http.StatusOK || !strings.Contains(out, `"name":"tags"`) || !strings.Contains(out, key.GetNamespacedName()) {
		t.Errorf("Create dataset offers no Tags naming %s: %d", key.GetNamespacedName(), code)
	}

	tags, _ := json.Marshal(map[string]string{key.GetNamespacedName(): "nosuch"})
	body, _ := json.Marshal(map[string]string{"datasetId": id, "locationType": "Region", "region": "europe-west2", "tags": string(tags)})
	code, out = consoleDo(t, addr, http.MethodPost, "/api/resources/bigquery"+q, string(body))
	if code != http.StatusBadRequest || !strings.Contains(consoleError(t, out), "no tag value "+key.GetNamespacedName()+"/nosuch") {
		t.Errorf("an unknown tag value = %d %s, want a refusal naming it", code, out)
	}
	if _, err := c.Dataset(id).Metadata(ctx); err == nil {
		t.Fatal("a dataset was created with a refused tag")
	}

	tags, _ = json.Marshal(map[string]string{key.GetNamespacedName(): "prod"})
	body, _ = json.Marshal(map[string]string{"datasetId": id, "locationType": "Region", "region": "europe-west2", "tags": string(tags)})
	if code, out := consoleDo(t, addr, http.MethodPost, "/api/resources/bigquery"+q, string(body)); code != http.StatusOK {
		t.Fatalf("Create dataset with a tag = %d: %s", code, out)
	}
	md, err := c.Dataset(id).Metadata(ctx)
	if err != nil || md.Location != "europe-west2" {
		t.Fatalf("the dataset reads back as %+v, %v; want europe-west2", md, err)
	}
	var bound []string
	it := bc.ListTagBindings(ctx, &rmpb.ListTagBindingsRequest{Parent: target})
	for {
		b, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("ListTagBindings: %v", err)
		}
		bound = append(bound, b.GetTagValue())
	}
	if len(bound) != 1 || bound[0] != prod.GetName() {
		t.Errorf("the dataset's tag bindings are %v, want %s", bound, prod.GetName())
	}

	// Create table from Upload.
	csv := []byte("name,n\nann,1\nbob,2\n")
	values := map[string]string{"source": "Upload", "format": "CSV", "autodetect": "true", "tableId": "uploaded",
		"tableType": "TABLE", "skipLeadingRows": "1"}
	code, out = consoleUploadAction(t, addr, project, []string{id}, "createtable", values, "people.csv", csv)
	if code != http.StatusOK {
		t.Fatalf("Create table from Upload = %d: %s", code, out)
	}
	rit := c.Dataset(id).Table("uploaded").Read(ctx)
	var rows []string
	for {
		var r []bigquery.Value
		err := rit.Next(&r)
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("read the uploaded table: %v", err)
		}
		rows = append(rows, fmt.Sprint(r))
	}
	if len(rows) != 2 {
		t.Errorf("the uploaded table holds %v, want 2 rows", rows)
	}
	code, out = consoleUploadAction(t, addr, project, []string{id}, "createtable", values, "people.csv", csv)
	if code != http.StatusBadRequest {
		t.Errorf("Create table from Upload of an existing table = %d %s, want a refusal", code, out)
	}
	code, out = consoleAct(t, addr, "bigquery", project, []string{id}, "createtable",
		map[string]string{"source": "Upload", "tableId": "nofile"})
	if code != http.StatusBadRequest || !strings.Contains(consoleError(t, out), "needs a file") {
		t.Errorf("Upload without a file = %d %s", code, out)
	}
}
