package main

// Cloud Storage object versions, holds, object retention and Run lifecycle
// now in the console (#853).
//
//   - Show versions, on a bucket's or folder's object list, reads every
//     version with objects.list versions=true: each row is one generation,
//     live or noncurrent, and opens that generation's own page,
//     _details/<bucket>/<generation>/<object>. A noncurrent one can be
//     restored as live, which is objects.rewrite through the official
//     client's Copier with the generation as its source, against the live
//     generation just read; any one can be deleted, objects.delete with its
//     generation, confirmed by the object's name.
//   - Edit holds and custom time, on an object's page and a generation's, is
//     objects.patch of temporaryHold, eventBasedHold and customTime against
//     the metageneration just read.
//   - Edit object retention, on a bucket made with object retention enabled
//     only, is objects.patch of retention (mode and retainUntilTime), with
//     overrideUnlockedRetention when the form asks for it.
//   - Run lifecycle now, on a bucket's page, is CloudBurrow's own POST
//     /_cloudburrow/lifecycle, which applies every bucket's lifecycle rules
//     at once. Google has no such call; the action says so in its label.
//
// A refusal is the API's own message: a hold or retention that blocks a
// restore or a delete, a custom time moved earlier, a retention loosened
// without the override.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	urlpkg "net/url"
	"strconv"
	"strings"

	"cloud.google.com/go/storage"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// generationPath is one generation's page, [objectPage, bucket, generation,
// name]: four segments where an object's live page has three, and the name
// last, so a confirmation asks for the object's name back.
func generationPath(bucket string, generation int64, name string) []string {
	return []string{objectPage, bucket, strconv.FormatInt(generation, 10), name}
}

// objectTarget reads an object's page path, live (generation 0) or one
// generation's.
func objectTarget(path []string) (bucket, name string, generation int64, err error) {
	switch {
	case len(path) == 3 && path[0] == objectPage:
		return path[1], path[2], 0, nil
	case len(path) == 4 && path[0] == objectPage:
		generation, err = strconv.ParseInt(path[2], 10, 64)
		if err != nil || generation <= 0 {
			return "", "", 0, fmt.Errorf("generation %q is not a number", path[2])
		}
		return path[1], path[3], generation, nil
	}
	return "", "", 0, errors.New("an object's page is " + objectPage + "/bucket/object, or " +
		objectPage + "/bucket/generation/object for one generation")
}

// versionsLimit is how many rows Show versions lists. Past it the listing
// says so rather than stopping silently.
const versionsLimit = 1000

// versionJSON is one version as objects.list versions=true returns it.
type versionJSON struct {
	Name        string `json:"name"`
	Generation  string `json:"generation"`
	Size        string `json:"size"`
	ContentType string `json:"contentType"`
	Updated     string `json:"updated"`
	TimeDeleted string `json:"timeDeleted"`
}

// ObjectVersions implements console.ObjectVersioner: the folders under a
// prefix, then every version of each object in it, each name's oldest
// first, as the API lists them.
func (p storageProvider) ObjectVersions(ctx context.Context, _ string, prefixPath []string) (console.Listing, error) {
	if len(prefixPath) == 0 || prefixPath[0] == "" || strings.HasPrefix(prefixPath[0], "_") {
		return console.Listing{}, errors.New("versions are listed in a bucket or one of its folders")
	}
	bucket := prefixPath[0]
	prefix := ""
	if len(prefixPath) > 1 {
		prefix = strings.Join(prefixPath[1:], "/") + "/"
	}
	out := console.Listing{
		Columns:    []string{"Generation", "State", "Size", "Updated"},
		NameColumn: "Name",
		Noun:       "versions",
		Items:      []console.Resource{},
	}
	var folders []string
	token := ""
	for {
		q := urlpkg.Values{"versions": {"true"}, "delimiter": {"/"}, "prefix": {prefix}}
		if token != "" {
			q.Set("pageToken", token)
		}
		var page struct {
			Items         []versionJSON `json:"items"`
			Prefixes      []string      `json:"prefixes"`
			NextPageToken string        `json:"nextPageToken"`
		}
		if err := getJSON(ctx, p.bucketURL(bucket)+"/o?"+q.Encode(), &page); err != nil {
			return console.Listing{}, fmt.Errorf("cannot list the versions in %s: %w", bucket, err)
		}
		folders = append(folders, page.Prefixes...)
		for _, v := range page.Items {
			if len(out.Items) == versionsLimit {
				out.Note = fmt.Sprintf("The first %d versions are listed; list the rest with objects.list versions=true.", versionsLimit)
				break
			}
			if row, ok := versionRow(bucket, prefix, v); ok {
				out.Items = append(out.Items, row)
			}
		}
		if page.NextPageToken == "" || out.Note != "" {
			break
		}
		token = page.NextPageToken
	}
	var rows []console.Resource
	for _, pre := range folders {
		name := strings.TrimSuffix(strings.TrimPrefix(pre, prefix), "/")
		if name == "" {
			continue
		}
		rows = append(rows, console.Resource{
			Name:   name + "/",
			Opens:  append(append([]string{}, prefixPath...), name),
			Fields: map[string]string{"Generation": "—", "State": "Folder", "Size": "—", "Updated": "—"},
		})
	}
	out.Items = append(rows, out.Items...)
	out.Total = len(out.Items)
	return out, nil
}

// versionRow is one generation in Show versions, opening its own page.
func versionRow(bucket, prefix string, v versionJSON) (console.Resource, bool) {
	name := strings.TrimPrefix(v.Name, prefix)
	gen, err := strconv.ParseInt(v.Generation, 10, 64)
	if name == "" || err != nil {
		return console.Resource{}, false
	}
	size := "—"
	if n, err := strconv.ParseInt(v.Size, 10, 64); err == nil {
		size = formatBytes(n)
	}
	live := v.TimeDeleted == ""
	state := "Live"
	if !live {
		state = "Noncurrent since " + shortTime(v.TimeDeleted)
	}
	return console.Resource{
		// Named as gcloud storage names one generation, name#generation, so
		// every row, its menu and its info panel is told apart.
		Name: name + "#" + v.Generation,
		Fields: map[string]string{
			"Generation": v.Generation, "State": state, "Size": size, "Updated": shortTime(v.Updated),
		},
		Opens:   generationPath(bucket, gen, v.Name),
		Target:  generationPath(bucket, gen, v.Name),
		Actions: versionActions(gen, v.Name, live),
	}, true
}

// versionActions are what can be done to one generation from its row and
// its page: a noncurrent one restored as live, and any one deleted.
func versionActions(generation int64, name string, live bool) []console.Action {
	gen := strconv.FormatInt(generation, 10)
	var out []console.Action
	if !live {
		out = append(out, console.Action{ID: "restoreversion", Label: "Restore as live version"})
	}
	after := "The object keeps its other versions."
	if live {
		after = "It is the live version, so the object then has no live version; its noncurrent versions are kept."
	}
	return append(out, console.Action{
		ID: "deleteversion", Label: "Delete version", Destructive: true,
		Confirm: fmt.Sprintf("Generation %s of %s is deleted, not kept as a noncurrent version. %s "+
			"With soft delete on for the bucket it can be restored from Deleted objects until its hard delete time.",
			gen, name, after),
	})
}

// restoreVersion makes a noncurrent generation the live version again:
// objects.rewrite of it onto its own name through the official client's
// Copier, against the live generation just read, so a write made meanwhile
// is refused rather than replaced. On a versioned bucket the version it
// replaces is kept as noncurrent.
func restoreVersion(ctx context.Context, c *storage.Client, bucket, name string, generation int64) error {
	o := c.Bucket(bucket).Object(name)
	dst := o.If(storage.Conditions{DoesNotExist: true})
	live, err := o.Attrs(ctx)
	switch {
	case err == nil && live.Generation == generation:
		return fmt.Errorf("generation %d is already the live version of %s", generation, name)
	case err == nil:
		dst = o.If(storage.Conditions{GenerationMatch: live.Generation})
	case !errors.Is(err, storage.ErrObjectNotExist):
		return err
	}
	_, err = dst.CopierFrom(o.Generation(generation)).Run(ctx)
	return err
}

// protectionActions are Edit holds and custom time, on every object and
// generation, and Edit object retention, where the bucket was made with
// object retention enabled: the API refuses retention anywhere else.
func protectionActions(a *storage.ObjectAttrs, objectRetention bool) []console.Action {
	out := []console.Action{{
		ID: "editholds", Label: "Edit holds and custom time",
		Fields: []console.Field{
			{Name: "temporaryHold", Label: "Temporary hold", Type: "checkbox", Section: "Holds",
				Default: strconv.FormatBool(a.TemporaryHold),
				Help:    "On, the object cannot be deleted or replaced until the hold is released. It does not affect retention."},
			{Name: "eventBasedHold", Label: "Event-based hold", Type: "checkbox", Section: "Holds",
				Default: strconv.FormatBool(a.EventBasedHold),
				Help:    "On, the object cannot be deleted or replaced; releasing it starts the bucket's retention period over."},
			{Name: "customTime", Label: "Custom time", Type: "text", Section: "Custom time",
				Default: stamp(a.CustomTime),
				Help:    "Optional. An RFC 3339 time, such as 2026-01-02T15:04:05Z, that lifecycle conditions can refer to. Once set it can only be moved later and cannot be removed."},
		},
	}}
	if !objectRetention {
		return out
	}
	mode, until := "None", ""
	if r := a.Retention; r != nil {
		mode, until = r.Mode, stamp(r.RetainUntil)
	}
	return append(out, console.Action{
		ID: "editretention", Label: "Edit object retention",
		Confirm: "Object retention keeps " + a.Name + " from being deleted or replaced until its retain-until time. " +
			"A Locked configuration can never be removed or shortened, only extended.",
		Fields: []console.Field{
			{Name: "mode", Label: "Mode", Type: "select", Default: mode,
				Options: []string{"None", "Unlocked", "Locked"},
				Help:    "None removes the configuration. Unlocked can be changed with the override below; Locked only extended."},
			{Name: "retainUntil", Label: "Retain until", Type: "text", Default: until,
				Help: "An RFC 3339 time in the future, such as 2030-01-02T15:04:05Z."},
			{Name: "override", Label: "Override unlocked retention", Type: "checkbox", Default: "false",
				Help: "Sends overrideUnlockedRetention=true, which removing, shortening or locking an Unlocked configuration needs."},
		},
	})
}

// objectURL is an object's JSON API address.
func (p storageProvider) objectURL(bucket, name string) string {
	return p.bucketURL(bucket) + "/o/" + urlpkg.PathEscape(name)
}

// patchObject is objects.patch on the JSON API an SDK calls, with the query
// parameters given: the generation, the metageneration it must match and any
// override. A refusal is the API's own.
func (p storageProvider) patchObject(ctx context.Context, bucket, name string, q urlpkg.Values, body map[string]any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch,
		p.objectURL(bucket, name)+"?"+q.Encode(), bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return apiError(resp)
	}
	return nil
}

// objectHandle is the live object or one generation of it.
func objectHandle(c *storage.Client, bucket, name string, generation int64) *storage.ObjectHandle {
	o := c.Bucket(bucket).Object(name)
	if generation > 0 {
		o = o.Generation(generation)
	}
	return o
}

// patchQuery is the patch's generation and the metageneration just read.
func patchQuery(a *storage.ObjectAttrs, generation int64) urlpkg.Values {
	q := urlpkg.Values{"ifMetagenerationMatch": {strconv.FormatInt(a.Metageneration, 10)}}
	if generation > 0 {
		q.Set("generation", strconv.FormatInt(generation, 10))
	}
	return q
}

// editHolds is objects.patch of the two holds and the custom time. An
// emptied custom time is sent as null, which the API refuses once one is
// set, with its own message; one never set is left out.
func (p storageProvider) editHolds(ctx context.Context, c *storage.Client, bucket, name string, generation int64, values map[string]string) error {
	cur, err := objectHandle(c, bucket, name, generation).Attrs(ctx)
	if err != nil {
		return err
	}
	body := map[string]any{
		"temporaryHold":  values["temporaryHold"] == "true",
		"eventBasedHold": values["eventBasedHold"] == "true",
	}
	if ct := strings.TrimSpace(values["customTime"]); ct != "" {
		body["customTime"] = ct
	} else if !cur.CustomTime.IsZero() {
		body["customTime"] = nil
	}
	return p.patchObject(ctx, bucket, name, patchQuery(cur, generation), body)
}

// editRetention is objects.patch of the object's retention: None sends null,
// which removes it.
func (p storageProvider) editRetention(ctx context.Context, c *storage.Client, bucket, name string, generation int64, values map[string]string) error {
	cur, err := objectHandle(c, bucket, name, generation).Attrs(ctx)
	if err != nil {
		return err
	}
	var retention any
	switch mode := strings.TrimSpace(values["mode"]); mode {
	case "", "None":
	case "Unlocked", "Locked":
		until := strings.TrimSpace(values["retainUntil"])
		if until == "" {
			return errors.New("name the time to retain the object until")
		}
		retention = map[string]any{"mode": mode, "retainUntilTime": until}
	default:
		return fmt.Errorf("retention mode %q is not None, Unlocked or Locked", mode)
	}
	q := patchQuery(cur, generation)
	if values["override"] == "true" {
		q.Set("overrideUnlockedRetention", "true")
	}
	return p.patchObject(ctx, bucket, name, q, map[string]any{"retention": retention})
}

// objectPageActions are an object's page's and a generation's: the object
// actions on the live page, the version actions on a generation's, and on
// both the protection edits.
func (p storageProvider) objectPageActions(ctx context.Context, path []string) []console.Action {
	bucket, name, gen, err := objectTarget(path)
	if err != nil {
		return nil
	}
	c, err := p.storageClient(ctx)
	if err != nil {
		return nil
	}
	defer func() { _ = c.Close() }()
	attrs, err := objectHandle(c, bucket, name, gen).Attrs(ctx)
	if err != nil {
		// What cannot be read has nothing that can be done to it; its page
		// says why.
		return nil
	}
	b, err := p.readBucket(ctx, bucket)
	retention := err == nil && b.objectRetentionEnabled()
	if gen == 0 {
		return append(objectActions(bucket, attrsMeta(attrs)), protectionActions(attrs, retention)...)
	}
	return append(versionActions(gen, name, attrs.Deleted.IsZero()), protectionActions(attrs, retention)...)
}

// actOnObjectPage performs the actions of an object's page and a
// generation's that are not the object actions of #790; handled is false
// for those.
func (p storageProvider) actOnObjectPage(ctx context.Context, c *storage.Client, path []string, action string, values map[string]string) (bool, error) {
	switch action {
	case "restoreversion", "deleteversion", "editholds", "editretention":
	default:
		return false, nil
	}
	bucket, name, gen, err := objectTarget(path)
	if err != nil {
		return true, err
	}
	switch action {
	case "restoreversion", "deleteversion":
		if gen == 0 {
			return true, fmt.Errorf("%s acts on one generation, from its row in Show versions or its page", action)
		}
		if action == "restoreversion" {
			return true, restoreVersion(ctx, c, bucket, name, gen)
		}
		return true, c.Bucket(bucket).Object(name).Generation(gen).Delete(ctx)
	case "editholds":
		return true, p.editHolds(ctx, c, bucket, name, gen, values)
	default:
		return true, p.editRetention(ctx, c, bucket, name, gen, values)
	}
}

// runLifecycleAction is Run lifecycle now, on a bucket's page.
func runLifecycleAction() console.Action {
	return console.Action{
		ID: "runlifecycle", Label: "Run lifecycle now (CloudBurrow extension)",
		Fields: []console.Field{{
			Name: "scope", Label: "Applies to", Type: "text", Immutable: true,
			Default: "Every bucket on this instance",
			Help: "A CloudBurrow extension, not a Google Cloud call: POST /_cloudburrow/lifecycle applies every " +
				"bucket's lifecycle rules now, on the instance's clock, as the storage server otherwise does on its " +
				"schedule. A version a rule would delete is kept while a hold or retention protects it.",
		}},
	}
}

// lifecycleResult is what POST /_cloudburrow/lifecycle answers.
type lifecycleResult struct {
	Deleted      int `json:"deleted"`
	ClassChanged int `json:"storageClassChanged"`
	Protected    int `json:"protected"`
	Aborted      int `json:"multipartUploadsAborted"`
}

// runLifecycle is CloudBurrow's POST /_cloudburrow/lifecycle, answered with
// what it did.
func (p storageProvider) runLifecycle(ctx context.Context) (*console.Listing, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+p.endpoint+"/_cloudburrow/lifecycle", nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp)
	}
	var res lifecycleResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, fmt.Errorf("the lifecycle run answered: %w", err)
	}
	row := func(label string, n int) console.Resource {
		return console.Resource{Name: label, Fields: map[string]string{"Count": strconv.Itoa(n)}}
	}
	items := []console.Resource{
		row("Versions deleted", res.Deleted),
		row("Storage class changed", res.ClassChanged),
		row("Kept by a hold or retention", res.Protected),
		row("Multipart uploads aborted", res.Aborted),
	}
	return &console.Listing{Columns: []string{"Count"}, NameColumn: "Result", Noun: "results",
		Items: items, Total: len(items)}, nil
}

// ActAtResult implements console.ResultActor: Run lifecycle now answers with
// what it did; every other action with nothing.
func (p storageProvider) ActAtResult(ctx context.Context, project string, path []string, action string, values map[string]string) (*console.Listing, error) {
	if action == "runlifecycle" {
		if len(path) != 1 || strings.HasPrefix(path[0], "_") {
			return nil, errors.New("the lifecycle is run from a bucket's page")
		}
		return p.runLifecycle(ctx)
	}
	return nil, p.ActAt(ctx, project, path, action, values)
}

var (
	_ console.ObjectVersioner = storageProvider{}
	_ console.ResultActor     = storageProvider{}
)
