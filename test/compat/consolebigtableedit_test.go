//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
)

// bigtablePage is the part of a console Bigtable page these tests read.
type bigtablePage struct {
	Unavailable string
	Actions     []bigtableAction
	Sections    []struct {
		ID      string
		Listing struct {
			Items []struct {
				Name    string
				ActsOn  []string
				Actions []bigtableAction
			}
		}
	}
}

type bigtableAction struct {
	ID          string
	Destructive bool
	Leaves      bool
	Fields      []struct{ Name, Default string }
}

func bigtableConsolePage(t *testing.T, addr, project string, path ...string) bigtablePage {
	t.Helper()
	q := url.Values{"project": {project}, "name": path}
	code, body := consoleDo(t, addr, http.MethodGet, "/api/detail/bigtable?"+q.Encode(), "")
	if code != http.StatusOK {
		t.Fatalf("console detail %v = %d: %s", path, code, body)
	}
	var page bigtablePage
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("decode detail: %v: %s", err, body)
	}
	return page
}

func bigtableConsoleAct(t *testing.T, addr, project string, path []string, action string, values map[string]string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"Path": path, "Action": action, "Values": values})
	return consoleDo(t, addr, http.MethodPost, "/api/actions/bigtable?project="+url.QueryEscape(project), string(body))
}

// confirmedDelete reports whether a delete is one the client confirms by the
// name typed back: destructive, with no inputs.
func confirmedDelete(actions []bigtableAction, id string) (bigtableAction, bool) {
	for _, a := range actions {
		if a.ID == id {
			return a, a.Destructive && len(a.Fields) == 0
		}
	}
	return bigtableAction{}, false
}

func bigtableFamilyPolicy(t *testing.T, admin *bigtable.AdminClient, table, family string) (string, bool) {
	t.Helper()
	info, err := admin.TableInfo(context.Background(), table)
	if err != nil {
		t.Fatalf("admin TableInfo: %v", err)
	}
	for _, f := range info.FamilyInfos {
		if f.Name == family {
			return f.FullGCPolicy.String(), true
		}
	}
	return "", false
}

// TestConsoleBigtableFamiliesAndRowWrites.
//
// A table's column families and cells changed from the console (#797), each
// read back through the official clients against the emulator. Add column
// family with a GC policy is returned by the admin client's TableInfo with
// that policy; Edit GC policy, from the family's row in the Schema section,
// changes it; Delete column family leaves TableInfo without it. Write cell,
// on the table's page and on a row's page, is read by the data client's
// ReadRow with its value and the timestamp given; Delete cells removes that
// column's cells and Delete row the row. A family that exists and a write to
// a family that does not are refused with the emulator's own messages, and
// each delete is offered as one the client confirms by the name typed back.
// The changes go through the console, so only the clients' reads are
// claimed.
//
// covers: google.bigtable.admin.v2.BigtableTableAdmin/GetTable, google.bigtable.v2.Bigtable/ReadRows
func TestConsoleBigtableFamiliesAndRowWrites(t *testing.T) {
	h := New(t)
	btAddr := h.Endpoint(EnvBigtable)
	addr := consoleAddr(t, h)
	t.Setenv("BIGTABLE_EMULATOR_HOST", btAddr)
	ctx := h.Context()
	project := h.Project()

	admin, err := bigtable.NewAdminClient(ctx, project, "cloudburrow")
	if err != nil {
		t.Fatalf("NewAdminClient: %v", err)
	}
	defer admin.Close()
	client, err := bigtable.NewClient(ctx, project, "cloudburrow")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer client.Close()

	table := fmt.Sprintf("console-edits-%d", time.Now().UnixNano())
	t.Cleanup(func() { _ = admin.DeleteTable(context.Background(), table) })
	if code, out := consoleDo(t, addr, http.MethodPost, "/api/resources/bigtable?project="+project,
		fmt.Sprintf(`{"table":%q,"families":"cf"}`, table)); code != http.StatusOK {
		t.Fatalf("console create = %d: %s", code, out)
	}

	// Add column family.
	code, out := bigtableConsoleAct(t, addr, project, []string{table}, "addfamily",
		map[string]string{"family": "stats", "maxVersions": "2", "maxAge": "7d"})
	if code != http.StatusOK {
		t.Fatalf("Add column family = %d: %s", code, out)
	}
	if policy, ok := bigtableFamilyPolicy(t, admin, table, "stats"); !ok {
		t.Fatal("TableInfo does not return the family the console added")
	} else if policy != "(versions() > 2 || age() > 7d)" {
		t.Errorf("the added family's GC policy = %s, want (versions() > 2 || age() > 7d)", policy)
	}
	code, out = bigtableConsoleAct(t, addr, project, []string{table}, "addfamily", map[string]string{"family": "stats"})
	if code == http.StatusOK || !strings.Contains(consoleError(t, out), "stats") {
		t.Errorf("adding an existing family = %d: %s; want the emulator's refusal", code, out)
	}

	// The family's row offers Edit GC policy and a confirmed delete, at its
	// own path.
	var target []string
	var familyActions []bigtableAction
	for _, s := range bigtableConsolePage(t, addr, project, table).Sections {
		for _, item := range s.Listing.Items {
			if s.ID == "families" && item.Name == "stats" {
				target, familyActions = item.ActsOn, item.Actions
			}
		}
	}
	if len(target) == 0 {
		t.Fatal("the Schema section offers no actions on the added family")
	}
	if _, ok := confirmedDelete(familyActions, "deletefamily"); !ok {
		t.Errorf("Delete column family is not a confirmed delete: %+v", familyActions)
	}

	// Edit GC policy.
	if code, out := bigtableConsoleAct(t, addr, project, target, "editgc",
		map[string]string{"maxVersions": "1", "maxAge": "1h", "both": "true"}); code != http.StatusOK {
		t.Fatalf("Edit GC policy = %d: %s", code, out)
	}
	if policy, _ := bigtableFamilyPolicy(t, admin, table, "stats"); policy != "(versions() > 1 && age() > 1h)" {
		t.Errorf("the edited GC policy = %s, want (versions() > 1 && age() > 1h)", policy)
	}

	// Write cell, from the table's page and from the row's.
	stamp := time.Now().UTC().Truncate(time.Millisecond).Add(-time.Minute)
	if code, out := bigtableConsoleAct(t, addr, project, []string{table}, "writecell", map[string]string{
		"row": "user#1", "family": "cf", "qualifier": "name", "value": "Ada",
		"timestamp": stamp.Format(time.RFC3339Nano)}); code != http.StatusOK {
		t.Fatalf("Write cell = %d: %s", code, out)
	}
	if code, out := bigtableConsoleAct(t, addr, project, []string{table, "user#1"}, "writecell",
		map[string]string{"family": "cf", "qualifier": "email", "value": "ada@example.com"}); code != http.StatusOK {
		t.Fatalf("Write cell on the row's page = %d: %s", code, out)
	}
	tbl := client.Open(table)
	row, err := tbl.ReadRow(ctx, "user#1")
	if err != nil {
		t.Fatalf("ReadRow: %v", err)
	}
	cells := map[string]bigtable.ReadItem{}
	for _, item := range row["cf"] {
		cells[item.Column] = item
	}
	if c := cells["cf:name"]; string(c.Value) != "Ada" || !c.Timestamp.Time().Equal(stamp) {
		t.Errorf("ReadRow cf:name = %q at %s, want Ada at %s", c.Value, c.Timestamp.Time(), stamp)
	}
	if c := cells["cf:email"]; string(c.Value) != "ada@example.com" {
		t.Errorf("ReadRow cf:email = %q, want ada@example.com", c.Value)
	}
	code, out = bigtableConsoleAct(t, addr, project, []string{table}, "writecell",
		map[string]string{"row": "user#1", "family": "nope", "value": "x"})
	if code == http.StatusOK || !strings.Contains(consoleError(t, out), "nope") {
		t.Errorf("a write to a family that does not exist = %d: %s; want the emulator's refusal", code, out)
	}

	// Delete cells in a column, from the cell's row on the row's page.
	rowPage := bigtableConsolePage(t, addr, project, table, "user#1")
	offered := false
	for _, s := range rowPage.Sections {
		for _, item := range s.Listing.Items {
			if item.Name == "cf:email" {
				_, offered = confirmedDelete(item.Actions, "deletecells")
			}
		}
	}
	if !offered {
		t.Error("the cf:email cell offers no confirmed Delete cells")
	}
	if code, out := bigtableConsoleAct(t, addr, project, []string{table, "user#1", "cf:email"}, "deletecells", nil); code != http.StatusOK {
		t.Fatalf("Delete cells = %d: %s", code, out)
	}
	row, err = tbl.ReadRow(ctx, "user#1")
	if err != nil {
		t.Fatalf("ReadRow after Delete cells: %v", err)
	}
	for _, item := range row["cf"] {
		if item.Column == "cf:email" {
			t.Errorf("ReadRow still returns %s after Delete cells", item.Column)
		}
	}
	if len(row["cf"]) != 1 {
		t.Errorf("Delete cells removed more than its column: %+v", row)
	}

	// Delete row, a confirmed delete that leaves the row's page.
	if a, ok := confirmedDelete(rowPage.Actions, "deleterow"); !ok || !a.Leaves {
		t.Errorf("Delete row is not a confirmed delete that leaves the page: %+v", rowPage.Actions)
	}
	if code, out := bigtableConsoleAct(t, addr, project, []string{table, "user#1"}, "deleterow", nil); code != http.StatusOK {
		t.Fatalf("Delete row = %d: %s", code, out)
	}
	if row, err := tbl.ReadRow(ctx, "user#1"); err != nil || len(row) != 0 {
		t.Errorf("ReadRow after Delete row = %v, %v; want no row", row, err)
	}

	// Delete column family.
	if code, out := bigtableConsoleAct(t, addr, project, target, "deletefamily", nil); code != http.StatusOK {
		t.Fatalf("Delete column family = %d: %s", code, out)
	}
	if _, ok := bigtableFamilyPolicy(t, admin, table, "stats"); ok {
		t.Error("TableInfo still returns the family the console deleted")
	}
}
