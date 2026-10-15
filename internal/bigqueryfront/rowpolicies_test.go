package bigqueryfront

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/metadata"
)

// The caller is the account CloudBurrow's credentials present (#1218).
func TestCallerIdentityIsTheCredentialsAccount(t *testing.T) {
	if got, want := callerIdentity("p1"), metadata.ServiceAccountEmail("p1"); got != want {
		t.Fatalf("callerIdentity = %q, want %q", got, want)
	}
	for g, want := range map[string]bool{
		"serviceAccount:" + metadata.ServiceAccountEmail("p1"): true,
		"allUsers":                                    true,
		"allAuthenticatedUsers":                       true,
		"user:someone@example.com":                    false,
		"group:eng@example.com":                       false,
		"domain:example.com":                          false,
		"serviceAccount:x@p1.iam.gserviceaccount.com": false,
	} {
		if got := grants(g, "p1"); got != want {
			t.Errorf("grants(%q) = %v, want %v", g, got, want)
		}
	}
}

func TestParseRowPolicyDDL(t *testing.T) {
	sql := "CREATE OR REPLACE ROW ACCESS POLICY eu ON ds.t GRANT TO ('user:a@b.c', \"allUsers\") FILTER USING (region = 'eu' AND (n > 1))"
	toks, _ := lex(sql)
	d, is, msg := parseRowPolicyDDL(toks, sql)
	if !is || msg != "" || !d.replace || d.id != "eu" || strings.Join(d.tablePath, ".") != "ds.t" ||
		strings.Join(d.grantees, ",") != "user:a@b.c,allUsers" || d.filter != "region = 'eu' AND (n > 1)" {
		t.Fatalf("got %+v %v %q", d, is, msg)
	}
	for sql, want := range map[string]rowPolicyDDL{
		"DROP ROW ACCESS POLICY IF EXISTS eu ON ds.t": {drop: true, ifExists: true, id: "eu"},
		"DROP ALL ROW ACCESS POLICIES ON ds.t":        {drop: true, all: true},
	} {
		toks, _ := lex(sql)
		d, is, msg := parseRowPolicyDDL(toks, sql)
		if !is || msg != "" || d.drop != want.drop || d.all != want.all || d.ifExists != want.ifExists || d.id != want.id {
			t.Errorf("%s: got %+v %v %q", sql, d, is, msg)
		}
	}
	for _, sql := range []string{"CREATE ROW ACCESS POLICY eu ON ds.t GRANT TO ('x')", "DROP ROW ACCESS POLICY eu"} {
		toks, _ := lex(sql)
		if _, is, msg := parseRowPolicyDDL(toks, sql); !is || msg == "" {
			t.Errorf("%s: want a syntax error, got %v %q", sql, is, msg)
		}
	}
	toks, _ = lex("CREATE TABLE ds.t (a INT64)")
	if _, is, _ := parseRowPolicyDDL(toks, ""); is {
		t.Error("CREATE TABLE read as a row access policy statement")
	}
}

// policyTables answers tables.get of the tables named, with creationTime 1,
// and a view v reading ds.t.
func policyTables(names ...string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, n := range names {
			if strings.HasSuffix(r.URL.Path, "/datasets/ds/tables/"+n) {
				writeJSON(w, http.StatusOK, map[string]any{"type": "TABLE", "creationTime": "1"})
				return
			}
		}
		if strings.HasSuffix(r.URL.Path, "/datasets/ds/tables/v") {
			writeJSON(w, http.StatusOK, map[string]any{"type": "VIEW", "creationTime": "1", "view": map[string]any{"query": "SELECT * FROM `p.ds.t`"}})
			return
		}
		writeError(w, http.StatusNotFound, "notFound", "no")
	})
}

func TestFilterQuery(t *testing.T) {
	p := &rowPolicies{}
	f := front{next: policyTables("t", "u"), base: "/bigquery/v2/projects/p", policies: p}
	r := httptest.NewRequest("GET", "/", nil)
	mine := rowPolicy{Filter: "region = 'eu'", Grantees: []string{"serviceAccount:" + callerIdentity("p")}}
	mine.Ref.ProjectID, mine.Ref.DatasetID, mine.Ref.TableID, mine.Ref.PolicyID = "p", "ds", "t", "eu"
	theirs := rowPolicy{Filter: "region = 'us'", Grantees: []string{"user:x@example.com"}}
	theirs.Ref = mine.Ref
	theirs.Ref.PolicyID = "us"
	for _, pol := range []rowPolicy{mine, theirs} {
		if _, err := p.put(pol, "1", false, false); err != nil {
			t.Fatal(err)
		}
	}
	for sql, want := range map[string]string{
		"SELECT * FROM ds.t": "SELECT * FROM (SELECT * FROM ds.t WHERE (region = 'eu')) AS `t`",
		"SELECT x.a FROM `p.ds.t` AS x JOIN ds.u ON x.a = u.a": "SELECT x.a FROM (SELECT * FROM `p.ds.t` WHERE (region = 'eu')) AS x JOIN ds.u ON x.a = u.a",
		"SELECT * FROM ds.u":              "SELECT * FROM ds.u",
		"INSERT INTO ds.t (a) VALUES (1)": "INSERT INTO ds.t (a) VALUES (1)",
	} {
		got, _, why := f.filterQuery(r, sql)
		if why != "" || got != want {
			t.Errorf("%s:\n got %q (%s)\nwant %q", sql, got, why, want)
		}
	}
	for _, sql := range []string{
		"DELETE FROM ds.t WHERE TRUE",
		"UPDATE ds.t SET a = 1 WHERE TRUE",
		"SELECT * FROM ds.v",
		"SELECT * FROM `ds.t*`",
		"SELECT * FROM ds.t FOR SYSTEM_TIME AS OF CURRENT_TIMESTAMP()",
		"SELECT * FROM ds.fn(1)",
	} {
		if _, _, why := f.filterQuery(r, sql); why == "" {
			t.Errorf("%s: not refused", sql)
		}
	}
	// No policy for the caller: no rows.
	if err := p.remove(policyKey("p", "ds", "t"), []string{"eu"}, false); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := f.filterQuery(r, "SELECT * FROM ds.t"); !strings.Contains(got, "WHERE FALSE") {
		t.Errorf("no grant: %q", got)
	}
	// The table made again: its policies are gone.
	f.next = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"type": "TABLE", "creationTime": "2"})
	})
	if got, changed, _ := f.filterQuery(r, "SELECT * FROM ds.t"); changed || got != "SELECT * FROM ds.t" {
		t.Errorf("stale policies applied: %q", got)
	}
}
