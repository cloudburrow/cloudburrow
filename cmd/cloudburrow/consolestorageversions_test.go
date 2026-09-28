package main

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// versionedBucket makes a bucket with versioning and object retention
// enabled through the official client, as an application would.
func versionedBucket(t *testing.T, c *storage.Client, name string) {
	t.Helper()
	if err := c.Bucket(name).SetObjectRetention(true).Create(context.Background(), "p",
		&storage.BucketAttrs{VersioningEnabled: true}); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
}

func actionIDList(actions []console.Action) string {
	var ids []string
	for _, a := range actions {
		ids = append(ids, a.ID)
	}
	return strings.Join(ids, ",")
}

func pageProps(d console.Detail) map[string]string {
	out := map[string]string{}
	for _, s := range d.Sections {
		for _, g := range s.Groups {
			for _, pr := range g.Properties {
				out[g.Heading+"/"+pr.Label] = pr.Value
			}
		}
	}
	return out
}

// Show versions lists a folder's subfolders and every generation of each
// object in it, live and noncurrent; a generation opens its own page, whose
// trail leads to its object; a noncurrent one is restored as live as a new
// generation with its bytes, and a deleted one is gone (#853).
func TestStorageObjectVersionsListOpenRestoreDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, c := newObjectOpsProvider(t)
	versionedBucket(t, c, "ver")
	putObject(t, c, "ver", "dir/a.txt", "one", nil)
	first, _ := c.Bucket("ver").Object("dir/a.txt").Attrs(ctx)
	putObject(t, c, "ver", "dir/a.txt", "two", nil)
	second, _ := c.Bucket("ver").Object("dir/a.txt").Attrs(ctx)
	putObject(t, c, "ver", "dir/sub/x.txt", "x", nil)

	l, err := p.ObjectVersions(ctx, "p", []string{"ver", "dir"})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Items) != 3 || l.Items[0].Name != "sub/" || strings.Join(l.Items[0].Opens, "/") != "ver/dir/sub" {
		t.Fatalf("versions = %+v, want the folder sub/ then two generations of a.txt", l.Items)
	}
	old, cur := l.Items[1], l.Items[2]
	if old.Name != "a.txt#"+strconv.FormatInt(first.Generation, 10) || old.Fields["Generation"] != strconv.FormatInt(first.Generation, 10) ||
		!strings.HasPrefix(old.Fields["State"], "Noncurrent since ") {
		t.Errorf("the first generation's row = %+v", old)
	}
	if cur.Fields["Generation"] != strconv.FormatInt(second.Generation, 10) || cur.Fields["State"] != "Live" {
		t.Errorf("the live generation's row = %+v", cur)
	}
	oldPath := generationPath("ver", first.Generation, "dir/a.txt")
	if strings.Join(old.Opens, "|") != strings.Join(oldPath, "|") || strings.Join(old.Target, "|") != strings.Join(oldPath, "|") {
		t.Errorf("the noncurrent row opens %v and acts on %v, want %v", old.Opens, old.Target, oldPath)
	}
	if actionIDList(old.Actions) != "restoreversion,deleteversion" || actionIDList(cur.Actions) != "deleteversion" {
		t.Errorf("row actions: noncurrent %s, live %s", actionIDList(old.Actions), actionIDList(cur.Actions))
	}
	if a := actionByID(t, old.Actions, "deleteversion"); !a.Destructive || !strings.Contains(a.Confirm, strconv.FormatInt(first.Generation, 10)) {
		t.Errorf("Delete version is not a confirmed destructive action naming the generation: %+v", a)
	}

	// The generation's page.
	page, err := p.Detail(ctx, "p", oldPath)
	if err != nil || page.Unavailable != "" {
		t.Fatalf("generation page: %v %q", err, page.Unavailable)
	}
	props := pageProps(page)
	if props["Version/Generation"] != strconv.FormatInt(first.Generation, 10) || !strings.HasPrefix(props["Version/State"], "Noncurrent since ") ||
		!strings.Contains(props["Object/Size"], "3 bytes") {
		t.Errorf("the generation page reads %v", props)
	}
	var crumbs []string
	for _, cr := range page.Trail {
		crumbs = append(crumbs, cr.Label)
	}
	if want := "ver/dir/a.txt/Generation " + strconv.FormatInt(first.Generation, 10); strings.Join(crumbs, "/") != want ||
		strings.Join(page.Trail[2].Path, "|") != strings.Join(objectPath("ver", "dir/a.txt"), "|") {
		t.Errorf("trail = %+v, want %s with a.txt opening the object's page", page.Trail, want)
	}
	if got := actionIDList(p.DetailActions(ctx, "p", oldPath)); got != "restoreversion,deleteversion,editholds,editretention" {
		t.Errorf("the noncurrent page offers %s", got)
	}
	livePath := generationPath("ver", second.Generation, "dir/a.txt")
	if got := actionIDList(p.DetailActions(ctx, "p", livePath)); got != "deleteversion,editholds,editretention" {
		t.Errorf("the live generation's page offers %s", got)
	}
	if err := p.ActAt(ctx, "p", livePath, "restoreversion", nil); err == nil || !strings.Contains(err.Error(), "already the live version") {
		t.Errorf("restoring the live generation = %v", err)
	}

	// Restore as live: a new generation with the first's bytes.
	if err := p.ActAt(ctx, "p", oldPath, "restoreversion", nil); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := readObject(t, c, "ver", "dir/a.txt"); got != "one" {
		t.Errorf("after the restore the object reads %q", got)
	}
	restored, _ := c.Bucket("ver").Object("dir/a.txt").Attrs(ctx)
	if restored.Generation == first.Generation || restored.Generation == second.Generation {
		t.Errorf("the restore made no new generation: %d", restored.Generation)
	}
	if _, err := c.Bucket("ver").Object("dir/a.txt").Generation(second.Generation).Attrs(ctx); err != nil {
		t.Errorf("the version the restore replaced is not kept as noncurrent: %v", err)
	}

	// Delete version: that generation is gone, the rest stay.
	if err := p.ActAt(ctx, "p", oldPath, "deleteversion", nil); err != nil {
		t.Fatalf("delete version: %v", err)
	}
	if _, err := c.Bucket("ver").Object("dir/a.txt").Generation(first.Generation).Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Errorf("the deleted generation is still there (%v)", err)
	}
	if page, _ := p.Detail(ctx, "p", oldPath); !strings.Contains(page.Unavailable, "no generation") {
		t.Errorf("the deleted generation's page reads %q", page.Unavailable)
	}
	if got := p.DetailActions(ctx, "p", oldPath); len(got) != 0 {
		t.Errorf("a deleted generation offers %s", actionIDList(got))
	}
	if _, err := p.ObjectVersions(ctx, "p", []string{"_details"}); err == nil {
		t.Error("versions were listed for a page that is no bucket")
	}
}

// Edit holds and custom time, and Edit object retention on a bucket that
// has it, are objects.patch; the API's own refusals come back: a held
// object cannot be deleted, a custom time cannot be removed, and an
// Unlocked retention is shortened only with the override (#853).
func TestStorageObjectHoldsAndRetention(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, c := newObjectOpsProvider(t)
	versionedBucket(t, c, "ret")
	putObject(t, c, "ret", "a.txt", "alpha", nil)
	putObject(t, c, "ops", "plain.txt", "plain", nil)

	if got := actionIDList(p.DetailActions(ctx, "p", objectPath("ops", "plain.txt"))); strings.Contains(got, "editretention") {
		t.Errorf("a bucket without object retention offers %s", got)
	}
	path := objectPath("ret", "a.txt")
	holds := formValues(actionByID(t, p.DetailActions(ctx, "p", path), "editholds"))
	if holds["temporaryHold"] != "false" || holds["eventBasedHold"] != "false" || holds["customTime"] != "" {
		t.Errorf("Edit holds is prefilled with %v", holds)
	}
	custom := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	holds["temporaryHold"], holds["customTime"] = "true", custom.Format(time.RFC3339)
	if err := p.ActAt(ctx, "p", path, "editholds", holds); err != nil {
		t.Fatalf("edit holds: %v", err)
	}
	a, _ := c.Bucket("ret").Object("a.txt").Attrs(ctx)
	if !a.TemporaryHold || a.EventBasedHold || !a.CustomTime.Equal(custom) || a.Metageneration != 2 {
		t.Errorf("after Edit holds Attrs reads hold %v/%v custom %v metageneration %d", a.TemporaryHold, a.EventBasedHold, a.CustomTime, a.Metageneration)
	}
	if page, _ := p.Detail(ctx, "p", path); pageProps(page)["Protection/Temporary hold"] != "On" ||
		pageProps(page)["Object/Custom time"] != "2026-01-02T15:04:05Z" {
		t.Errorf("the object page reads %v", pageProps(page))
	}
	if err := c.Bucket("ret").Object("a.txt").Delete(ctx); err == nil {
		t.Error("a held object was deleted")
	}
	holds = formValues(actionByID(t, p.DetailActions(ctx, "p", path), "editholds"))
	if holds["temporaryHold"] != "true" || holds["customTime"] != "2026-01-02T15:04:05Z" {
		t.Errorf("Edit holds is prefilled with %v after the edit", holds)
	}
	holds["temporaryHold"], holds["customTime"] = "false", ""
	if err := p.ActAt(ctx, "p", path, "editholds", holds); err == nil || !strings.Contains(err.Error(), "cannot be removed") {
		t.Errorf("removing the custom time = %v, want the API's refusal", err)
	}
	holds["customTime"] = custom.Format(time.RFC3339)
	if err := p.ActAt(ctx, "p", path, "editholds", holds); err != nil {
		t.Fatalf("release the hold: %v", err)
	}

	// Object retention.
	retention := actionByID(t, p.DetailActions(ctx, "p", path), "editretention")
	if retention.Confirm == "" {
		t.Error("Edit object retention does not confirm")
	}
	values := formValues(retention)
	if values["mode"] != "None" || values["override"] != "false" {
		t.Errorf("Edit object retention is prefilled with %v", values)
	}
	far := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	values["mode"], values["retainUntil"] = "Unlocked", far.Format(time.RFC3339)
	if err := p.ActAt(ctx, "p", path, "editretention", values); err != nil {
		t.Fatalf("set retention: %v", err)
	}
	a, _ = c.Bucket("ret").Object("a.txt").Attrs(ctx)
	if a.Retention == nil || a.Retention.Mode != "Unlocked" || !a.Retention.RetainUntil.Equal(far) {
		t.Errorf("after Edit object retention Attrs reads %+v", a.Retention)
	}
	values["retainUntil"] = far.Add(-time.Hour).Format(time.RFC3339)
	if err := p.ActAt(ctx, "p", path, "editretention", values); err == nil || !strings.Contains(err.Error(), "overrideUnlockedRetention") {
		t.Errorf("shortening without the override = %v, want the API's refusal", err)
	}
	values["override"] = "true"
	if err := p.ActAt(ctx, "p", path, "editretention", values); err != nil {
		t.Fatalf("shorten with the override: %v", err)
	}
	a, _ = c.Bucket("ret").Object("a.txt").Attrs(ctx)
	if a.Retention == nil || !a.Retention.RetainUntil.Equal(far.Add(-time.Hour)) {
		t.Errorf("after shortening Attrs reads %+v", a.Retention)
	}
	values["mode"] = "None"
	if err := p.ActAt(ctx, "p", path, "editretention", values); err != nil {
		t.Fatalf("remove with the override: %v", err)
	}
	if a, _ = c.Bucket("ret").Object("a.txt").Attrs(ctx); a.Retention != nil {
		t.Errorf("after removal Attrs reads %+v", a.Retention)
	}
}

// Run lifecycle now is on every bucket's page, labelled a CloudBurrow
// extension, and answers with what POST /_cloudburrow/lifecycle did (#853).
func TestStorageRunLifecycleNow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, c := newObjectOpsProvider(t)
	if _, err := c.Bucket("ops").Update(ctx, storage.BucketAttrsToUpdate{Lifecycle: &storage.Lifecycle{Rules: []storage.LifecycleRule{{
		Action: storage.LifecycleAction{Type: storage.DeleteAction}, Condition: storage.LifecycleCondition{AgeInDays: 0, MatchesPrefix: []string{"tmp/"}},
	}}}}); err != nil {
		t.Fatal(err)
	}
	putObject(t, c, "ops", "tmp/a.txt", "a", nil)
	putObject(t, c, "ops", "keep.txt", "k", nil)

	d, err := p.Detail(ctx, "p", []string{"ops"})
	if err != nil {
		t.Fatal(err)
	}
	run := actionByID(t, d.Actions, "runlifecycle")
	if !strings.Contains(run.Label, "CloudBurrow extension") || !strings.Contains(run.Fields[0].Help, "/_cloudburrow/lifecycle") {
		t.Errorf("Run lifecycle now is not labelled a CloudBurrow extension: %+v", run)
	}
	actionByID(t, p.DetailActions(ctx, "p", []string{"ops"}), "runlifecycle")
	if got := actionIDList(p.DetailActions(ctx, "p", []string{"ops", "tmp"})); strings.Contains(got, "runlifecycle") {
		t.Errorf("a folder page offers %s", got)
	}

	res, err := p.ActAtResult(ctx, "p", []string{"ops"}, "runlifecycle", formValues(run))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range res.Items {
		got[r.Name] = r.Fields["Count"]
	}
	if got["Versions deleted"] != "1" || got["Storage class changed"] != "0" || got["Kept by a hold or retention"] != "0" {
		t.Errorf("the run answered %v", got)
	}
	if _, err := c.Bucket("ops").Object("tmp/a.txt").Attrs(ctx); !errors.Is(err, storage.ErrObjectNotExist) {
		t.Errorf("the rule's object is still there (%v)", err)
	}
	if readObject(t, c, "ops", "keep.txt") != "k" {
		t.Error("an object no rule matched was changed")
	}
	if _, err := p.ActAtResult(ctx, "p", []string{"ops", "tmp"}, "runlifecycle", nil); err == nil {
		t.Error("the lifecycle ran from a folder page")
	}
}
