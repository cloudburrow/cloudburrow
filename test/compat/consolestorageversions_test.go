//go:build compat

package compat

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
)

// TestConsoleStorageObjectVersionsHoldsAndRetention (#853), in the storage
// shard: on a bucket the official client made with versioning and object
// retention enabled, each console surface is driven through the console's
// own routes as its toggle, forms and buttons submit, and read back through
// the official client. Show versions lists both generations of an object,
// with the generations the client reports, live and noncurrent; a
// generation's page says which it is. Restore as live version makes the
// noncurrent bytes live as a new generation, keeping the replaced one;
// Delete version removes that generation alone. Edit holds and custom time
// sets a temporary hold, which then refuses the client's delete, and a
// custom time, whose removal the API refuses. Edit object retention sets an
// Unlocked configuration, whose shortening is refused without
// overrideUnlockedRetention and made with it. Run lifecycle now answers with
// what POST /_cloudburrow/lifecycle did, and the object a Delete rule
// matches is no longer live.
func TestConsoleStorageObjectVersionsHoldsAndRetention(t *testing.T) {
	h := New(t)
	addr := consoleAddr(t, h)
	sc := storageClient(t, h)
	ctx := h.Context()
	project := h.Project()
	bh := sc.Bucket(project + "-versions")
	if err := bh.SetObjectRetention(true).Create(ctx, project, &storage.BucketAttrs{VersioningEnabled: true}); err != nil {
		t.Fatalf("create a bucket with object retention: %v", err)
	}
	b := bh.BucketName()
	t.Cleanup(func() {
		// Holds and retention first, or nothing can be deleted.
		it := bh.Objects(h.Context(), &storage.Query{Versions: true})
		for {
			a, err := it.Next()
			if err != nil {
				break
			}
			_, _ = bh.Object(a.Name).Generation(a.Generation).OverrideUnlockedRetention(true).Update(h.Context(),
				storage.ObjectAttrsToUpdate{TemporaryHold: false, EventBasedHold: false, Retention: &storage.ObjectRetention{}})
		}
		emptyAndDelete(h.Context(), bh)
	})

	put := func(name, data string) *storage.ObjectAttrs {
		t.Helper()
		w := bh.Object(name).NewWriter(ctx)
		w.ContentType = "text/plain"
		if _, err := io.WriteString(w, data); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		return w.Attrs()
	}
	read := func(name string) string {
		t.Helper()
		r, err := bh.Object(name).NewReader(ctx)
		if err != nil {
			t.Fatalf("NewReader %s: %v", name, err)
		}
		defer r.Close()
		data, _ := io.ReadAll(r)
		return string(data)
	}
	type field struct{ Name, Type, Default string }
	type action struct {
		ID, Label, Confirm string
		Destructive        bool
		Fields             []field
	}
	type row struct {
		Name    string
		Fields  map[string]string
		Opens   []string
		Target  []string
		Actions []action
	}
	detail := func(path ...string) (actions []action, props map[string]string) {
		t.Helper()
		v := url.Values{"project": {project}, "name": path}
		code, body := consoleDo(t, addr, http.MethodGet, "/api/detail/storage?"+v.Encode(), "")
		var d struct {
			Unavailable string
			Actions     []action
			Sections    []struct {
				Groups []struct {
					Heading    string
					Properties []struct{ Label, Value string }
				}
			}
		}
		if err := json.Unmarshal([]byte(body), &d); code != http.StatusOK || err != nil || d.Unavailable != "" {
			t.Fatalf("console detail %v = %d (%v): %s", path, code, err, body)
		}
		props = map[string]string{}
		for _, s := range d.Sections {
			for _, g := range s.Groups {
				for _, p := range g.Properties {
					props[g.Heading+"/"+p.Label] = p.Value
				}
			}
		}
		return d.Actions, props
	}
	form := func(actions []action, id string) map[string]string {
		t.Helper()
		for _, a := range actions {
			if a.ID == id {
				out := map[string]string{}
				for _, f := range a.Fields {
					out[f.Name] = f.Default
				}
				return out
			}
		}
		t.Fatalf("the console does not offer %s: %+v", id, actions)
		return nil
	}
	act := func(path []string, id string, values map[string]string) (int, string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"Path": path, "Action": id, "Values": values})
		return consoleDo(t, addr, http.MethodPost, "/api/actions/storage?project="+project, string(body))
	}
	mustAct := func(path []string, id string, values map[string]string) string {
		t.Helper()
		code, body := act(path, id, values)
		if code != http.StatusOK {
			t.Fatalf("console %s on %v = %d: %s", id, path, code, body)
		}
		return body
	}
	refused := func(what string, code int, body, want string) {
		t.Helper()
		if code != http.StatusBadRequest || !strings.Contains(body, want) {
			t.Errorf("%s = %d %s; want the API's refusal naming %q", what, code, body, want)
		}
	}
	gen := func(n int64) string { return strconv.FormatInt(n, 10) }

	// Show versions.
	first := put("docs/a.txt", "one")
	second := put("docs/a.txt", "two")
	code, body := consoleDo(t, addr, http.MethodGet,
		"/api/objects/storage/versions?"+url.Values{"project": {project}, "name": {b, "docs"}}.Encode(), "")
	var listing struct{ Items []row }
	if err := json.Unmarshal([]byte(body), &listing); code != http.StatusOK || err != nil {
		t.Fatalf("console versions = %d (%v): %s", code, err, body)
	}
	if len(listing.Items) != 2 {
		t.Fatalf("Show versions lists %+v, want the two generations of a.txt", listing.Items)
	}
	old, live := listing.Items[0], listing.Items[1]
	if old.Fields["Generation"] != gen(first.Generation) || !strings.HasPrefix(old.Fields["State"], "Noncurrent") ||
		live.Fields["Generation"] != gen(second.Generation) || live.Fields["State"] != "Live" {
		t.Errorf("Show versions reads %+v and %+v; the client wrote generations %d then %d", old.Fields, live.Fields, first.Generation, second.Generation)
	}
	oldPath := []string{"_details", b, gen(first.Generation), "docs/a.txt"}
	if strings.Join(old.Opens, "|") != strings.Join(oldPath, "|") || strings.Join(old.Target, "|") != strings.Join(oldPath, "|") {
		t.Errorf("the noncurrent row opens %v and acts on %v", old.Opens, old.Target)
	}
	actions, props := detail(oldPath...)
	if props["Version/Generation"] != gen(first.Generation) || !strings.HasPrefix(props["Version/State"], "Noncurrent since ") {
		t.Errorf("the generation's page reads %v", props)
	}
	form(actions, "restoreversion")

	// Restore as live version, then Delete version.
	mustAct(oldPath, "restoreversion", nil)
	if got := read("docs/a.txt"); got != "one" {
		t.Errorf("after Restore as live version the client reads %q", got)
	}
	restored, err := bh.Object("docs/a.txt").Attrs(ctx)
	if err != nil || restored.Generation == first.Generation || restored.Generation == second.Generation {
		t.Errorf("the restore is not a new generation: %+v (%v)", restored, err)
	}
	if _, err := bh.Object("docs/a.txt").Generation(second.Generation).Attrs(ctx); err != nil {
		t.Errorf("the replaced generation is not kept: %v", err)
	}
	mustAct(oldPath, "deleteversion", nil)
	if _, err := bh.Object("docs/a.txt").Generation(first.Generation).Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Errorf("after Delete version the generation reads %v", err)
	}
	if _, err := bh.Object("docs/a.txt").Generation(second.Generation).Attrs(ctx); err != nil {
		t.Errorf("Delete version removed another generation: %v", err)
	}

	// Edit holds and custom time.
	objPage := []string{"_details", b, "docs/a.txt"}
	actions, _ = detail(objPage...)
	values := form(actions, "editholds")
	custom := "2026-01-02T15:04:05Z"
	values["temporaryHold"], values["customTime"] = "true", custom
	mustAct(objPage, "editholds", values)
	a, err := bh.Object("docs/a.txt").Attrs(ctx)
	if err != nil || !a.TemporaryHold || a.CustomTime.UTC().Format(time.RFC3339) != custom {
		t.Fatalf("after Edit holds the client reads hold %v, custom time %v (%v)", a.TemporaryHold, a.CustomTime, err)
	}
	if err := bh.Object("docs/a.txt").Generation(a.Generation).Delete(ctx); err == nil {
		t.Error("the client deleted a held generation")
	}
	actions, _ = detail(objPage...)
	values = form(actions, "editholds")
	if values["temporaryHold"] != "true" || values["customTime"] != custom {
		t.Errorf("Edit holds is prefilled with %v", values)
	}
	values["temporaryHold"], values["customTime"] = "false", ""
	code, body = act(objPage, "editholds", values)
	refused("removing the custom time", code, body, "cannot be removed")
	values["customTime"] = custom
	mustAct(objPage, "editholds", values)
	if a, _ := bh.Object("docs/a.txt").Attrs(ctx); a.TemporaryHold {
		t.Error("the hold was not released")
	}

	// Edit object retention.
	actions, _ = detail(objPage...)
	values = form(actions, "editretention")
	until := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	values["mode"], values["retainUntil"] = "Unlocked", until.Format(time.RFC3339)
	mustAct(objPage, "editretention", values)
	a, _ = bh.Object("docs/a.txt").Attrs(ctx)
	if a.Retention == nil || a.Retention.Mode != "Unlocked" || !a.Retention.RetainUntil.Equal(until) {
		t.Errorf("after Edit object retention the client reads %+v", a.Retention)
	}
	values["retainUntil"] = until.Add(-time.Hour).Format(time.RFC3339)
	code, body = act(objPage, "editretention", values)
	refused("shortening without the override", code, body, "overrideUnlockedRetention")
	values["override"] = "true"
	mustAct(objPage, "editretention", values)
	a, _ = bh.Object("docs/a.txt").Attrs(ctx)
	if a.Retention == nil || !a.Retention.RetainUntil.Equal(until.Add(-time.Hour)) {
		t.Errorf("after shortening with the override the client reads %+v", a.Retention)
	}
	values["mode"] = "None"
	mustAct(objPage, "editretention", values)
	if a, _ = bh.Object("docs/a.txt").Attrs(ctx); a.Retention != nil {
		t.Errorf("after removing it the client reads %+v", a.Retention)
	}

	// Run lifecycle now.
	if _, err := bh.Update(ctx, storage.BucketAttrsToUpdate{Lifecycle: &storage.Lifecycle{Rules: []storage.LifecycleRule{{
		Action:    storage.LifecycleAction{Type: storage.DeleteAction},
		Condition: storage.LifecycleCondition{AgeInDays: 0, MatchesPrefix: []string{"tmp/"}},
	}}}}); err != nil {
		t.Fatalf("set a lifecycle rule: %v", err)
	}
	put("tmp/x.txt", "x")
	actions, _ = detail(b)
	body = mustAct([]string{b}, "runlifecycle", form(actions, "runlifecycle"))
	var res struct {
		Result struct {
			Items []struct {
				Name   string
				Fields map[string]string
			}
		}
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatalf("decode the run: %v: %s", err, body)
	}
	deleted := -1
	for _, r := range res.Result.Items {
		if r.Name == "Versions deleted" {
			deleted, _ = strconv.Atoi(r.Fields["Count"])
		}
	}
	if deleted < 1 {
		t.Errorf("Run lifecycle now answered %s, want at least one version deleted", body)
	}
	if _, err := bh.Object("tmp/x.txt").Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Errorf("after the run the rule's object is still live (%v)", err)
	}
	if got := read("docs/a.txt"); got != "one" {
		t.Errorf("the run changed an object no rule matched: %q", got)
	}
}
