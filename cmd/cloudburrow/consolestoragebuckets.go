package main

// Cloud Storage bucket settings in the console (#789): Edit bucket, Lock
// retention policy, the soft-deleted objects of a bucket with Restore, the
// Deleted buckets page with Restore, and managed folders in the browser.
//
// Every one goes to the storage server's own API, the calls an application
// makes. Edit bucket is buckets.patch on the JSON API, against the
// metageneration just read, with the fields the form holds and nothing else:
// the lifecycle rules and CORS configurations are the JSON the API takes, so
// a rule it does not accept is refused with its own message rather than
// reshaped by the console first. Lock retention policy is the official
// client's LockRetentionPolicy, with the metageneration the API requires;
// an object is restored through ObjectHandle.Restore; a bucket is listed and
// restored through Google's generated JSON API client, since the official
// client has no call for either.
//
// Managed folders are listed and not made: managedFolders.insert and delete
// answer 501 on this instance, so no managed folder can exist here and the
// console offers no control that would make or remove one.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	urlpkg "net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	storagev1 "google.golang.org/api/storage/v1"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// softObjectPage is the first segment of a soft-deleted object's action
// path, [softObjectPage, bucket, generation, name]. Like objectPage it can
// never be a bucket, whose name cannot begin with an underscore. The name is
// last so the confirmation and the notification name the object.
const softObjectPage = "_softdeleted"

func softObjectPath(bucket string, generation int64, name string) []string {
	return []string{softObjectPage, bucket, strconv.FormatInt(generation, 10), name}
}

// softDeletedLimit is how many soft-deleted objects a bucket's page lists.
// Past it the section says so rather than stopping silently.
const softDeletedLimit = 1000

// bucketStorageClasses are the default storage classes buckets.patch
// accepts on this instance, legacy ones included, so a bucket made with one
// can be edited without the form refusing its own value.
const bucketStorageClassPattern = `^(STANDARD|NEARLINE|COLDLINE|ARCHIVE|MULTI_REGIONAL|REGIONAL|DURABLE_REDUCED_AVAILABILITY)$`

// bucketMeta is a bucket as buckets.get returns it: what the configuration
// tab shows and the edit form is prefilled from. The lifecycle rules and CORS
// configurations are kept as the API's JSON.
type bucketMeta struct {
	Name           string            `json:"name"`
	Location       string            `json:"location"`
	LocationType   string            `json:"locationType"`
	StorageClass   string            `json:"storageClass"`
	TimeCreated    string            `json:"timeCreated"`
	Updated        string            `json:"updated"`
	Metageneration string            `json:"metageneration"`
	Labels         map[string]string `json:"labels"`
	Versioning     struct {
		Enabled bool `json:"enabled"`
	} `json:"versioning"`
	DefaultEventBasedHold bool `json:"defaultEventBasedHold"`
	IAMConfiguration      struct {
		UniformBucketLevelAccess struct {
			Enabled bool `json:"enabled"`
		} `json:"uniformBucketLevelAccess"`
	} `json:"iamConfiguration"`
	RetentionPolicy *struct {
		RetentionPeriod string `json:"retentionPeriod"`
		EffectiveTime   string `json:"effectiveTime"`
		IsLocked        bool   `json:"isLocked"`
	} `json:"retentionPolicy"`
	SoftDeletePolicy *struct {
		RetentionDurationSeconds string `json:"retentionDurationSeconds"`
		EffectiveTime            string `json:"effectiveTime"`
	} `json:"softDeletePolicy"`
	Lifecycle struct {
		Rule []json.RawMessage `json:"rule"`
	} `json:"lifecycle"`
	CORS []json.RawMessage `json:"cors"`
}

func (b bucketMeta) retentionSeconds() string {
	if b.RetentionPolicy == nil {
		return ""
	}
	return b.RetentionPolicy.RetentionPeriod
}

func (b bucketMeta) retentionLocked() bool {
	return b.RetentionPolicy != nil && b.RetentionPolicy.IsLocked
}

func (b bucketMeta) softDeleteSeconds() string {
	if b.SoftDeletePolicy == nil || b.SoftDeletePolicy.RetentionDurationSeconds == "" {
		return "0"
	}
	return b.SoftDeletePolicy.RetentionDurationSeconds
}

// bucketURL is a bucket's JSON API address.
func (p storageProvider) bucketURL(bucket string) string {
	return fmt.Sprintf("http://%s/storage/v1/b/%s", p.endpoint, urlpkg.PathEscape(bucket))
}

func (p storageProvider) readBucket(ctx context.Context, bucket string) (bucketMeta, error) {
	var b bucketMeta
	err := getJSON(ctx, p.bucketURL(bucket), &b)
	return b, err
}

// jsonAPI is Google's generated JSON API client against the storage
// server, for the calls the official client does not make: listing and
// restoring soft-deleted buckets, and listing managed folders.
func jsonAPI(ctx context.Context, endpoint string) (*storagev1.Service, error) {
	return storagev1.NewService(ctx, option.WithEndpoint("http://"+endpoint+"/storage/v1/"),
		option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
}

// indentJSON is a JSON array for a textarea: one element per line block,
// empty for none.
func indentJSON(items []json.RawMessage) string {
	if len(items) == 0 {
		return ""
	}
	out, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return ""
	}
	return string(out)
}

// bucketEditForm is Edit bucket, prefilled from the bucket.
//
// Only what buckets.patch keeps on this instance, and nothing it refuses:
// the location is shown and cannot change; ACLs, IP filtering and object
// retention's enablement are refused by the API or set only at creation, so
// they are not on the form.
func bucketEditForm(b bucketMeta) *console.EditForm {
	const class, protection, rules = "Default storage class", "Protection", "Lifecycle and CORS"
	retentionHelp := "Optional. Seconds an object must be kept before it can be deleted or replaced, 1 to 3155760000; empty removes the policy."
	if b.retentionLocked() {
		retentionHelp = "Locked: seconds an object must be kept. It can be increased, and never reduced or removed."
	}
	return &console.EditForm{
		Label: "Edit bucket",
		Fields: []console.Field{
			{Name: "location", Label: "Location", Type: "text", Immutable: true, Default: b.Location,
				Help: "A bucket's location is fixed when it is created."},
			{Name: "labels", Label: "Labels", Type: "map", Default: console.FormatMap(b.Labels),
				Help: "One key=value per line. A label removed here is removed from the bucket."},
			{Name: "storageClass", Label: "Default storage class", Type: "text", Required: true,
				Section: class, Default: b.StorageClass, Pattern: bucketStorageClassPattern,
				Help: "STANDARD, NEARLINE, COLDLINE or ARCHIVE, in capitals: the class a new object gets when it names none. Existing objects keep theirs."},
			{Name: "versioning", Label: "Object versioning", Type: "checkbox", Section: protection,
				Default: strconv.FormatBool(b.Versioning.Enabled),
				Help:    "On, an overwritten or deleted object is kept as a noncurrent version. Off, versions already kept stay."},
			{Name: "softDeleteSeconds", Label: "Soft delete retention (seconds)", Type: "text", Required: true,
				Section: protection, Default: b.softDeleteSeconds(), Pattern: `^[0-9]+$`,
				Help: "How long a deleted object can be restored: 0 turns soft delete off, otherwise 604800 (7 days) to 7776000 (90 days). A change applies to what is deleted after it."},
			{Name: "retentionPeriod", Label: "Retention period (seconds)", Type: "text",
				Section: protection, Default: b.retentionSeconds(), Pattern: `^[0-9]*$`, Help: retentionHelp},
			{Name: "defaultEventBasedHold", Label: "Default event-based hold", Type: "checkbox", Section: protection,
				Default: strconv.FormatBool(b.DefaultEventBasedHold),
				Help:    "On, every new object is held until the hold is released, and cannot be deleted or replaced while it is."},
			{Name: "lifecycle", Label: "Lifecycle rules", Type: "textarea", Section: rules,
				Default: indentJSON(b.Lifecycle.Rule),
				Help: `Optional. The lifecycle "rule" array as the JSON API takes it, such as ` +
					`[{"action":{"type":"Delete"},"condition":{"age":30}}]; empty removes every rule.`},
			{Name: "cors", Label: "CORS configurations", Type: "textarea", Section: rules,
				Default: indentJSON(b.CORS),
				Help: `Optional. The "cors" array as the JSON API takes it, such as ` +
					`[{"origin":["http://localhost:8080"],"method":["GET"],"maxAgeSeconds":3600}]; empty removes it.`},
		},
		Note: "Saved through buckets.patch against the metageneration just read, so a change made " +
			"meanwhile is refused rather than overwritten. The location cannot be changed, and a " +
			"retention policy is locked with Lock retention policy, on this page.",
	}
}

// jsonArrayField reads a textarea holding a JSON array: nil for empty, which
// the patch sends as null to remove the setting.
func jsonArrayField(values map[string]string, key, label string) ([]any, error) {
	raw := strings.TrimSpace(values[key])
	if raw == "" {
		return nil, nil
	}
	var out []any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("%s: not a JSON array: %w", label, err)
	}
	return out, nil
}

// Edit implements console.Editor: buckets.patch with the form's fields.
func (p storageProvider) Edit(ctx context.Context, _ string, path []string, values map[string]string) error {
	if len(path) != 1 || strings.HasPrefix(path[0], "_") {
		return errors.New("only a bucket's settings are edited here: an object's are changed with Edit metadata")
	}
	bucket := path[0]
	cur, err := p.readBucket(ctx, bucket)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", bucket, err)
	}

	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return fmt.Errorf("labels: %w", err)
	}
	patchLabels := map[string]any{}
	for k := range cur.Labels {
		patchLabels[k] = nil // removed unless the form keeps it
	}
	for k, v := range labels {
		if strings.TrimSpace(k) == "" {
			return errors.New("labels: a key cannot be empty")
		}
		patchLabels[k] = v
	}
	lifecycle, err := jsonArrayField(values, "lifecycle", "lifecycle rules")
	if err != nil {
		return err
	}
	cors, err := jsonArrayField(values, "cors", "CORS configurations")
	if err != nil {
		return err
	}

	// The API's own types for each: numbers as strings, as it sends them,
	// and a removal as null. The server validates every one.
	body := map[string]any{
		"storageClass":          strings.TrimSpace(values["storageClass"]),
		"versioning":            map[string]any{"enabled": values["versioning"] == "true"},
		"defaultEventBasedHold": values["defaultEventBasedHold"] == "true",
		"softDeletePolicy": map[string]any{
			"retentionDurationSeconds": strings.TrimSpace(values["softDeleteSeconds"])},
		"retentionPolicy": nil,
		"lifecycle":       nil,
		"cors":            nil,
	}
	if len(patchLabels) > 0 {
		body["labels"] = patchLabels
	}
	if v := strings.TrimSpace(values["retentionPeriod"]); v != "" {
		body["retentionPolicy"] = map[string]any{"retentionPeriod": v}
	}
	if lifecycle != nil {
		body["lifecycle"] = map[string]any{"rule": lifecycle}
	}
	if cors != nil {
		body["cors"] = cors
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch,
		p.bucketURL(bucket)+"?ifMetagenerationMatch="+urlpkg.QueryEscape(cur.Metageneration), bytes.NewReader(raw))
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

// lockRetentionAction is Lock retention policy, offered only on a bucket
// whose policy exists and is not locked: locking one that is not there is
// refused by the API, and a locked one is locked for good.
func lockRetentionAction(b bucketMeta) []console.Action {
	if b.RetentionPolicy == nil || b.RetentionPolicy.IsLocked || b.RetentionPolicy.RetentionPeriod == "" {
		return nil
	}
	period := b.RetentionPolicy.RetentionPeriod
	return []console.Action{{
		ID: "lockretention", Label: "Lock retention policy", Destructive: true,
		Confirm: fmt.Sprintf("Locking is permanent. The retention policy of %s seconds can then never be "+
			"removed or reduced, only increased; no object in %s can be deleted or replaced until it is "+
			"%s seconds old; and the bucket cannot be deleted while it holds one.", period, b.Name, period),
	}}
}

// restoreAction restores a soft-deleted bucket or object.
func restoreAction() console.Action {
	return console.Action{ID: "restore", Label: "Restore"}
}

// lockRetention is buckets.lockRetentionPolicy through the official client,
// with the metageneration just read, as the API requires.
func lockRetention(ctx context.Context, c *storage.Client, bucket string) error {
	bh := c.Bucket(bucket)
	attrs, err := bh.Attrs(ctx)
	if err != nil {
		return err
	}
	return bh.If(storage.BucketConditions{MetagenerationMatch: attrs.MetaGeneration}).LockRetentionPolicy(ctx)
}

// softObjectTarget reads a soft-deleted object's action path.
func softObjectTarget(path []string) (bucket string, generation int64, name string, err error) {
	if len(path) != 4 || path[0] != softObjectPage {
		return "", 0, "", errors.New("a soft-deleted object is addressed " + softObjectPage + "/bucket/generation/name")
	}
	generation, err = strconv.ParseInt(path[2], 10, 64)
	if err != nil {
		return "", 0, "", fmt.Errorf("generation %q is not a number", path[2])
	}
	return path[1], generation, path[3], nil
}

// softDeletedActions offers Restore on a soft-deleted object that is still
// there to restore.
func (p storageProvider) softDeletedActions(ctx context.Context, path []string) []console.Action {
	bucket, gen, name, err := softObjectTarget(path)
	if err != nil {
		return nil
	}
	c, err := p.storageClient(ctx)
	if err != nil {
		return nil
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Bucket(bucket).Object(name).Generation(gen).SoftDeleted().Attrs(ctx); err != nil {
		return nil
	}
	return []console.Action{restoreAction()}
}

// restoreObject is objects.restore through the official client.
func restoreObject(ctx context.Context, c *storage.Client, path []string) error {
	bucket, gen, name, err := softObjectTarget(path)
	if err != nil {
		return err
	}
	_, err = c.Bucket(bucket).Object(name).Generation(gen).Restore(ctx, &storage.RestoreOptions{})
	return err
}

// deletedObjects is a bucket's soft-deleted objects, each with Restore.
func (p storageProvider) deletedObjects(ctx context.Context, b bucketMeta) console.Section {
	sec := console.Section{ID: "deleted", Label: "Deleted objects", Listing: console.Listing{
		Columns: []string{"Generation", "Size", "Deleted", "Hard delete"}, NameColumn: "Name",
		Noun: "deleted objects", Items: []console.Resource{},
	}}
	if b.softDeleteSeconds() == "0" {
		sec.Listing.Note = "Soft delete is off for this bucket: an object deleted now is gone at once. " +
			"Objects deleted while it was on are listed until their hard delete time."
	}
	c, err := p.storageClient(ctx)
	if err != nil {
		sec.Listing.Unavailable = err.Error()
		return sec
	}
	defer func() { _ = c.Close() }()
	it := c.Bucket(b.Name).Objects(ctx, &storage.Query{SoftDeleted: true})
	for {
		o, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			sec.Listing.Unavailable = "cannot list the soft-deleted objects: " + err.Error()
			return sec
		}
		if len(sec.Listing.Items) == softDeletedLimit {
			sec.Listing.Note = strings.TrimSpace(sec.Listing.Note + fmt.Sprintf(
				" The first %d soft-deleted objects are listed; list the rest with objects.list softDeleted=true.", softDeletedLimit))
			break
		}
		sec.Listing.Items = append(sec.Listing.Items, console.Resource{
			Name: o.Name,
			Fields: map[string]string{
				"Generation":  strconv.FormatInt(o.Generation, 10),
				"Size":        formatBytes(o.Size),
				"Deleted":     stampOrDash(o.SoftDeleteTime),
				"Hard delete": stampOrDash(o.HardDeleteTime),
			},
			Target:  softObjectPath(b.Name, o.Generation, o.Name),
			Actions: []console.Action{restoreAction()},
		})
	}
	sec.Listing.Total = len(sec.Listing.Items)
	return sec
}

func stampOrDash(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.UTC().Format(time.RFC3339)
}

// managedFolders are the managed folders directly under a prefix, by their
// name there ("reports" for "logs/reports/" under "logs/"), from
// managedFolders.list.
func (p storageProvider) managedFolders(ctx context.Context, bucket, prefix string) (map[string]bool, error) {
	s, err := jsonAPI(ctx, p.endpoint)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	err = s.ManagedFolders.List(bucket).Prefix(prefix).Context(ctx).Pages(ctx, func(page *storagev1.ManagedFolders) error {
		for _, f := range page.Items {
			rest := strings.TrimSuffix(strings.TrimPrefix(f.Name, prefix), "/")
			if rest != "" && !strings.Contains(rest, "/") {
				out[rest] = true
			}
		}
		return nil
	})
	return out, err
}

// storageDeletedProvider is Cloud Storage's Deleted buckets page (#789): the
// project's soft-deleted buckets, each with its generation and hard delete
// time, and Restore.
type storageDeletedProvider struct{ endpoint string }

func (storageDeletedProvider) ID() string    { return "storage-deleted" }
func (storageDeletedProvider) Title() string { return "Deleted buckets" }

func (p storageDeletedProvider) List(ctx context.Context, project string) (console.Listing, error) {
	out := console.Listing{
		Columns:    []string{"Generation", "Deleted", "Hard delete"},
		NameColumn: "Bucket",
		Noun:       "deleted buckets",
		Items:      []console.Resource{},
	}
	if project == "" {
		out.Prompt = "Cloud Storage lists deleted buckets per project. Choose one in the toolbar."
		return out, nil
	}
	s, err := jsonAPI(ctx, p.endpoint)
	if err != nil {
		return console.Listing{}, err
	}
	err = s.Buckets.List(project).SoftDeleted(true).Context(ctx).Pages(ctx, func(page *storagev1.Buckets) error {
		for _, b := range page.Items {
			out.Items = append(out.Items, console.Resource{
				Name: b.Name,
				Fields: map[string]string{
					"Generation":  strconv.FormatInt(b.Generation, 10),
					"Deleted":     shortTime(b.SoftDeleteTime),
					"Hard delete": shortTime(b.HardDeleteTime),
				},
				Target:  []string{strconv.FormatInt(b.Generation, 10), b.Name},
				Actions: []console.Action{restoreAction()},
			})
		}
		return nil
	})
	if err != nil {
		return console.Listing{}, fmt.Errorf("list deleted buckets: %w", err)
	}
	sort.SliceStable(out.Items, func(i, j int) bool { return out.Items[i].Name < out.Items[j].Name })
	out.Total = len(out.Items)
	return out, nil
}

// deletedBucketTarget reads a deleted bucket's action path, [generation,
// name]: the name last, so the notification names the bucket.
func deletedBucketTarget(path []string) (name string, generation int64, err error) {
	if len(path) != 2 {
		return "", 0, errors.New("a deleted bucket is addressed generation/name")
	}
	generation, err = strconv.ParseInt(path[0], 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("generation %q is not a number", path[0])
	}
	return path[1], generation, nil
}

// DetailActions implements console.PathActor: Restore, on a bucket still
// soft-deleted at that generation.
func (p storageDeletedProvider) DetailActions(ctx context.Context, _ string, path []string) []console.Action {
	name, gen, err := deletedBucketTarget(path)
	if err != nil {
		return nil
	}
	s, err := jsonAPI(ctx, p.endpoint)
	if err != nil {
		return nil
	}
	if _, err := s.Buckets.Get(name).SoftDeleted(true).Generation(gen).Context(ctx).Do(); err != nil {
		return nil
	}
	return []console.Action{restoreAction()}
}

// ActAt implements console.PathActor: buckets.restore.
func (p storageDeletedProvider) ActAt(ctx context.Context, _ string, path []string, action string, _ map[string]string) error {
	if action != "restore" {
		return fmt.Errorf("%s is not an action on a deleted bucket", action)
	}
	name, gen, err := deletedBucketTarget(path)
	if err != nil {
		return err
	}
	s, err := jsonAPI(ctx, p.endpoint)
	if err != nil {
		return err
	}
	_, err = s.Buckets.Restore(name, gen).Context(ctx).Do()
	return err
}

var (
	_ console.Editor    = storageProvider{}
	_ console.PathActor = storageDeletedProvider{}
)
