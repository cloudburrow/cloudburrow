package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func strp(s string) *string { return &s }

// fakeDBStudio is a backend whose server accepts one password.
func fakeDBStudio(t *testing.T, engine string, ran *[]string) dbStudioBackend {
	return dbStudioBackend{
		product: "Cloud SQL", savedQueries: filepath.Join(t.TempDir(), "saved.json"), iamPrincipal: "sa@p.iam.gserviceaccount.com",
		engine: func(context.Context, string, string) (string, error) { return engine, nil },
		options: func(context.Context, string, string) (map[string]any, error) {
			return map[string]any{"databases": []string{"shop"}}, nil
		},
		run: func(_ context.Context, l *dbStudioLogin, sql string) ([]dbStudioResult, error) {
			*ran = append(*ran, sql)
			l.role = l.user
			if l.iam != "" {
				l.role = "sa@p.iam"
			}
			if l.iam == "" && l.password != "right" {
				return nil, errors.New(`FATAL: password authentication failed for user "` + l.user + `"`)
			}
			switch {
			case strings.HasPrefix(sql, "SELECT n.nspname FROM"):
				return []dbStudioResult{
					{Columns: []string{"nspname"}, Rows: [][]*string{{strp("public")}}},
					{Columns: []string{"a", "b", "c"}, Rows: [][]*string{{strp("public"), strp("items"), strp("r")}, {strp("public"), strp("v1"), strp("v")},
						{strp("public"), strp("seq"), strp("S")}}},
					{Columns: []string{"a", "b", "c", "d"}, Rows: [][]*string{{strp("public"), strp("items"), strp("id"), strp("integer")}}},
					{Columns: []string{"a", "b", "c"}, Rows: [][]*string{{strp("public"), strp("items"), strp("items_pkey")}}},
					{Columns: []string{"a", "b", "c"}, Rows: [][]*string{{strp("public"), strp("f"), strp("FUNCTION")}, {strp("public"), strp("p"), strp("PROCEDURE")}}},
				}, nil
			case strings.Contains(sql, "INSERT"):
				return []dbStudioResult{{Message: "INSERT 0 1"}, {Columns: []string{"n"}, Rows: [][]*string{{strp("1")}, {nil}}}}, nil
			}
			return []dbStudioResult{{Message: "CREATE TABLE"}}, nil
		},
	}
}

func studioCall(t *testing.T, b dbStudioBackend, call string, body map[string]any) (map[string]any, error) {
	t.Helper()
	raw, _ := json.Marshal(body)
	out, err := b.call(context.Background(), "p", call, raw)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	j, _ := json.Marshal(out)
	_ = json.Unmarshal(j, &m)
	return m, nil
}

// TestDBStudioSignInAndRun (#2208): a wrong password is refused with the
// server's words; the right one answers a session the explorer and runs
// use; a script mixing statements shows only its query's rows; IAM signs
// in as the console's principal; signing out ends the session.
func TestDBStudioSignInAndRun(t *testing.T) {
	var ran []string
	b := fakeDBStudio(t, "POSTGRES", &ran)
	if _, err := studioCall(t, b, "signin", map[string]any{"instance": "i", "database": "shop", "user": "u", "password": "wrong"}); err == nil ||
		!strings.Contains(err.Error(), `password authentication failed for user "u"`) {
		t.Fatalf("a wrong password: %v", err)
	}
	if _, err := studioCall(t, b, "signin", map[string]any{"instance": "i", "user": "u", "password": "right"}); err == nil {
		t.Fatal("a sign-in without a database was accepted")
	}
	s, err := studioCall(t, b, "signin", map[string]any{"instance": "i", "database": "shop", "user": "u", "password": "right"})
	if err != nil || s["session"] == "" || s["user"] != "u" {
		t.Fatalf("signin = %v, %v", s, err)
	}
	sess := s["session"].(string)
	if _, err := studioCall(t, b, "run", map[string]any{"instance": "other", "session": sess, "sql": "SELECT 1"}); !errors.Is(err, errDBStudioSignedOut) {
		t.Errorf("a session used for another instance: %v", err)
	}
	s, _ = studioCall(t, b, "signin", map[string]any{"instance": "i", "database": "shop", "user": "u", "password": "right"})
	sess = s["session"].(string)
	ex, err := studioCall(t, b, "explorer", map[string]any{"instance": "i", "session": sess})
	if err != nil {
		t.Fatal(err)
	}
	j, _ := json.Marshal(ex)
	for _, want := range []string{`"name":"items"`, `"items_pkey"`, `"sequences":["seq"]`, `"functions":["f"]`, `"procedures":["p"]`, `"name":"v1"`} {
		if !strings.Contains(string(j), want) {
			t.Errorf("the explorer %s lacks %s", j, want)
		}
	}
	r, err := studioCall(t, b, "run", map[string]any{"instance": "i", "session": sess, "sql": "INSERT INTO t VALUES (1); SELECT n FROM t"})
	if err != nil {
		t.Fatal(err)
	}
	res := r["results"].([]any)
	if len(res) != 1 || len(res[0].(map[string]any)["rows"].([]any)) != 2 {
		t.Errorf("a mixed script's results %v", r)
	}
	r, _ = studioCall(t, b, "run", map[string]any{"instance": "i", "session": sess, "sql": "CREATE TABLE t (n int)"})
	if r["message"] != "Statement executed successfully" {
		t.Errorf("a DDL run %v", r)
	}
	if _, err := studioCall(t, b, "signout", map[string]any{"instance": "i", "session": sess}); err != nil {
		t.Fatal(err)
	}
	if _, err := studioCall(t, b, "run", map[string]any{"instance": "i", "session": sess, "sql": "SELECT 1"}); !errors.Is(err, errDBStudioSignedOut) {
		t.Errorf("a run after Sign out: %v", err)
	}
	s, err = studioCall(t, b, "signin", map[string]any{"instance": "i", "database": "shop", "iam": true})
	if err != nil || s["user"] != "sa@p.iam" {
		t.Errorf("an IAM sign-in = %v, %v", s, err)
	}
	o, err := studioCall(t, b, "options", map[string]any{"instance": "i"})
	if err != nil || o["iamPrincipal"] != "sa@p.iam.gserviceaccount.com" {
		t.Errorf("options = %v, %v", o, err)
	}
	// Saved queries.
	q, err := studioCall(t, b, "savequery", map[string]any{"instance": "i", "name": "q1", "sql": "SELECT 1"})
	if err != nil {
		t.Fatal(err)
	}
	l, _ := studioCall(t, b, "savedqueries", map[string]any{"instance": "i"})
	if qs := l["queries"].([]any); len(qs) != 1 || qs[0].(map[string]any)["name"] != "q1" {
		t.Errorf("saved queries %v", l)
	}
	if _, err := studioCall(t, b, "deletequery", map[string]any{"instance": "i", "id": q["id"]}); err != nil {
		t.Error(err)
	}
}

// TestDBStudioTruncatesAt10MB: Google's response limit.
func TestDBStudioTruncatesAt10MB(t *testing.T) {
	big := strings.Repeat("x", 1<<20)
	b := dbStudioBackend{run: func(context.Context, *dbStudioLogin, string) ([]dbStudioResult, error) {
		r := dbStudioResult{Columns: []string{"v"}}
		for range 12 {
			r.Rows = append(r.Rows, []*string{&big})
		}
		return []dbStudioResult{r}, nil
	}}
	out, err := b.runScript(context.Background(), &dbStudioLogin{}, "SELECT v")
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	res := m["results"].([]dbStudioResult)
	if len(res[0].Rows) != 10 || !res[0].Partial || !strings.Contains(m["message"].(string), "10 MB") {
		t.Errorf("12 MB of rows came back as %d rows, partial %v, %v", len(res[0].Rows), res[0].Partial, m["message"])
	}
}
