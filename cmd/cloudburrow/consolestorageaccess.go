package main

// Cloud Storage console parity, round 2 (#1843), from Google's public
// documentation (using-uniform-bucket-level-access, using-object-versioning,
// using-autoclass, view-object-metadata):
//
//   - the Permissions tab's access control, with Switch to: Uniform or
//     Fine-grained, as buckets.patch of uniformBucketLevelAccess;
//   - Edit object versioning on the Protection tab, with Google's two
//     noncurrent-version limits, which are lifecycle rules;
//   - Autoclass on the Configuration tab, with Edit Autoclass. The storage
//     server stores autoclass and moves no object between classes
//     (docs/compatibility.md, "Bucket fields"), which the tab says.

import (
	"context"
	"errors"
	"fmt"
	urlpkg "net/url"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// permissionsPlaceholder is the Permissions tab's place among the bucket's
// tabs, with its access control and Switch to.
func permissionsPlaceholder(b bucketMeta) console.Section {
	sec := console.Section{ID: "permissions", Label: "Permissions", Kind: console.KindPermissions}
	if b.hierarchical() {
		sec.Note = "Access control: Uniform. A bucket with hierarchical namespace keeps uniform access for good."
		return sec
	}
	if b.IAMConfiguration.UniformBucketLevelAccess.Enabled {
		sec.Note = "Access control: Uniform. Access is granted by IAM alone."
	} else {
		sec.Note = "Access control: Fine-grained. Objects also carry ACLs, edited with an object's Edit access; " +
			"the bucket's ACLs are on its Access control tab. ACLs are stored and not enforced."
	}
	sec.Actions = []console.Action{switchAccessAction(b)}
	return sec
}

// switchAccessAction is the Permissions tab's Switch to.
func switchAccessAction(b bucketMeta) console.Action {
	other := accessUniform
	if b.IAMConfiguration.UniformBucketLevelAccess.Enabled {
		other = accessFineGrained
	}
	return console.Action{
		ID: "switchaccess", Label: "Switch to",
		Fields: []console.Field{
			{Name: "metageneration", Type: "hidden", Default: b.Metageneration},
			{Name: "uniformAccess", Label: "Access control", Type: "radio", Default: other,
				Options: []string{accessUniform, accessFineGrained},
				Help: "Uniform: IAM alone grants access. Fine-grained: objects also carry ACLs. " +
					"Google allows switching back from Uniform within 90 days; here it can always be switched back."},
		},
	}
}

// editVersioningAction is the Protection tab's Edit object versioning.
func editVersioningAction(b bucketMeta) console.Action {
	state := "Disabled"
	if b.Versioning.Enabled {
		state = "Enabled"
	}
	return console.Action{
		// Google's "Object versioning" dialog, ending in Confirm, with "Add
		// recommended lifecycle rules to manage version costs" (#1843,
		// using-object-versioning).
		ID: "editversioning", Label: "Edit object versioning", Title: "Object versioning", Submit: "Confirm",
		Fields: []console.Field{
			{Name: "metageneration", Type: "hidden", Default: b.Metageneration},
			{Name: "versioning", Label: "Object versioning", Type: "radio", Default: state,
				Options: []string{"Enabled", "Disabled"},
				Help:    "Enabled, an overwritten or deleted object is kept as a noncurrent version."},
			{Name: "recommended", Label: "Add recommended lifecycle rules to manage version costs", Type: "checkbox",
				Default: "false", ShowWhen: "versioning=Enabled",
				Help: "Adds lifecycle rules deleting noncurrent versions, by count and by age, as set below."},
			{Name: "maxVersions", Label: "Max. number of versions per object", Type: "text", Pattern: `^[0-9]*$`,
				ShowWhen: "versioning=Enabled&recommended",
				Help:     "Adds a lifecycle rule deleting noncurrent versions past this many newer ones."},
			{Name: "expireDays", Label: "Expire noncurrent versions after (days)", Type: "text", Pattern: `^[0-9]*$`,
				ShowWhen: "versioning=Enabled&recommended",
				Help:     "Adds a lifecycle rule deleting a version this many days after it becomes noncurrent."},
		},
	}
}

// editAutoclassAction is the Configuration tab's Edit Autoclass.
func editAutoclassAction(b bucketMeta) console.Action {
	state := "Disabled"
	if b.autoclassOn() {
		state = "Enabled"
	}
	return console.Action{
		ID: "editautoclass", Label: "Edit Autoclass",
		Fields: []console.Field{
			{Name: "metageneration", Type: "hidden", Default: b.Metageneration},
			{Name: "autoclass", Label: "Autoclass", Type: "radio", Default: state, Options: []string{"Enabled", "Disabled"},
				Help: "Stored as autoclass.enabled. This instance moves no object between classes by itself."},
		},
	}
}

func (b bucketMeta) autoclassOn() bool { return b.Autoclass != nil && b.Autoclass.Enabled }

// bucketTabActions are the actions of the tabs above, which the bucket's
// DetailActions offers too.
func bucketTabActions(b bucketMeta) []console.Action {
	var out []console.Action
	if !b.hierarchical() {
		out = append(out, switchAccessAction(b), editVersioningAction(b))
	}
	return append(out, editAutoclassAction(b))
}

// actOnBucketTabs performs them.
func (p storageProvider) actOnBucketTabs(ctx context.Context, path []string, action string, values map[string]string) (bool, error) {
	switch action {
	case "switchaccess", "editversioning", "editautoclass":
	default:
		return false, nil
	}
	if len(path) != 1 || strings.HasPrefix(path[0], "_") {
		return true, errors.New("this is changed on a bucket's page")
	}
	bucket, gen := path[0], values["metageneration"]
	switch action {
	case "switchaccess":
		v := values["uniformAccess"]
		if v != accessUniform && v != accessFineGrained {
			return true, fmt.Errorf("access control is %s or %s", accessUniform, accessFineGrained)
		}
		return true, p.patchBucket(ctx, bucket, gen, map[string]any{"iamConfiguration": map[string]any{
			"uniformBucketLevelAccess": map[string]any{"enabled": v == accessUniform}}})
	case "editautoclass":
		return true, p.patchBucket(ctx, bucket, gen, map[string]any{"autoclass": map[string]any{
			"enabled": values["autoclass"] == "Enabled"}})
	}
	// Edit object versioning: the setting, and each limit as a lifecycle
	// rule added to the bucket's, as Google's dialog does.
	b, err := p.readBucket(ctx, bucket)
	if err != nil {
		return true, err
	}
	on := values["versioning"] == "Enabled"
	body := map[string]any{"versioning": map[string]any{"enabled": on}}
	var added []any
	// The limits are the recommended rules' (#1843): read only while that
	// box is checked; a caller that sends no box at all keeps the limits
	// it sends.
	recommended := values["recommended"]
	if recommended == "true" && strings.TrimSpace(values["maxVersions"]) == "" && strings.TrimSpace(values["expireDays"]) == "" {
		return true, errors.New("the recommended lifecycle rules need a max. number of versions or days to expire noncurrent versions after")
	}
	for _, lim := range []struct{ key, cond string }{{"maxVersions", "numNewerVersions"}, {"expireDays", "daysSinceNoncurrentTime"}} {
		v := strings.TrimSpace(values[lim.key])
		if v == "" || recommended == "false" {
			continue
		}
		if !on {
			return true, errors.New("noncurrent version limits need object versioning enabled")
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return true, fmt.Errorf("%q is not a whole number of at least 1", v)
		}
		added = append(added, map[string]any{"action": map[string]any{"type": "Delete"},
			"condition": map[string]any{lim.cond: n, "isLive": false}})
	}
	if len(added) > 0 {
		rules := make([]any, 0, len(b.Lifecycle.Rule)+len(added))
		for _, r := range b.Lifecycle.Rule {
			rules = append(rules, r)
		}
		body["lifecycle"] = map[string]any{"rule": append(rules, added...)}
	}
	return true, p.patchBucket(ctx, bucket, gen, body)
}

// autoclassText is the Configuration tab's Autoclass, which says it is
// stored only.
func autoclassText(b bucketMeta) string {
	if b.autoclassOn() {
		return "Enabled (stored; this instance moves no object between classes)"
	}
	return "Disabled"
}

// objectAddresses are an object's Public URL, Authenticated URL and gsutil
// URI, each copied with Google's button label.
func (p storageProvider) objectAddresses(project, bucket, name string, gen int64) []console.Property {
	esc := urlpkg.PathEscape(name)
	public := fmt.Sprintf("http://%s/storage/v1/b/%s/o/%s?alt=media", p.endpoint, urlpkg.PathEscape(bucket), esc)
	q := urlpkg.Values{"project": {project}, "name": {bucket, name}}
	gs := "gs://" + bucket + "/" + name
	if gen > 0 {
		public += "&generation=" + strconv.FormatInt(gen, 10)
		gs += "#" + strconv.FormatInt(gen, 10)
	}
	return []console.Property{
		{Label: "Public URL", Value: public, Copy: "Copy URL"},
		{Label: "Authenticated URL", Value: "/api/objects/storage/download?" + q.Encode(), Copy: "Copy authenticated URL"},
		{Label: "gsutil URI", Value: gs, Copy: "Copy gsutil URI"},
	}
}

// objectPageTarget is Detail.Object for a live object's page, which offers
// Download and Delete; a generation's page has its own version actions.
func objectPageTarget(bucket, name string, gen int64) []string {
	if gen > 0 {
		return nil
	}
	return []string{bucket, name}
}

// The Buckets list's View options (#1843): live buckets, or soft-deleted
// buckets only, carried as ?view=.
const (
	bucketViewKey         = "view"
	bucketViewLive        = "live"
	bucketViewSoftDeleted = "softdeleted"
	// softBucketPage addresses a soft-deleted bucket's page and its Restore,
	// [softBucketPage, generation, name]; no bucket name begins with "_".
	softBucketPage = "_softbucket"
)

// ScopeKey implements console.Scoper: the View options choice.
func (storageProvider) ScopeKey() string { return bucketViewKey }

func bucketViewScope(selected string) *console.Scope {
	return &console.Scope{Key: bucketViewKey, Label: "View options", Selected: selected, Default: bucketViewLive,
		Options: []console.ScopeOption{{ID: bucketViewLive, Label: "Live buckets"},
			{ID: bucketViewSoftDeleted, Label: "Soft deleted buckets only"}}, ViewOptions: true}
}

// softDeletedBuckets is the Buckets list showing soft-deleted buckets only,
// each with Restore and a page of its own.
func (p storageProvider) softDeletedBuckets(ctx context.Context, project string) (console.Listing, error) {
	out, err := storageDeletedProvider{endpoint: p.endpoint}.List(ctx, project)
	if err != nil {
		return out, err
	}
	for i := range out.Items {
		path := append([]string{softBucketPage}, out.Items[i].Target...)
		out.Items[i].Target, out.Items[i].Opens = path, path
	}
	out.Noun = "soft-deleted buckets"
	out.Scope = bucketViewScope(bucketViewSoftDeleted)
	out.Note = "Soft deleted buckets only: each can be restored until its hard delete time."
	return out, nil
}

// softBucketDetail is a soft-deleted bucket's page.
func (p storageProvider) softBucketDetail(ctx context.Context, path []string) (console.Detail, error) {
	name, gen, err := deletedBucketTarget(path[1:])
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	s, err := jsonAPI(ctx, p.endpoint)
	if err != nil {
		return console.Detail{}, err
	}
	b, err := s.Buckets.Get(name).SoftDeleted(true).Generation(gen).Context(ctx).Do()
	if err != nil {
		return console.Detail{Unavailable: fmt.Sprintf("%s has no soft-deleted generation %d: it was restored or hard deleted", name, gen)}, nil
	}
	props := []console.Property{
		{Label: "Bucket", Value: b.Name},
		{Label: "Generation", Value: strconv.FormatInt(b.Generation, 10)},
		{Label: "Location", Value: b.Location},
		{Label: "Default storage class", Value: storageClassLabel(b.StorageClass)},
		{Label: "Deleted", Value: shortTime(b.SoftDeleteTime)},
		{Label: "Hard delete", Value: shortTime(b.HardDeleteTime)},
	}
	return console.Detail{Summary: props, Title: b.Name + " (soft-deleted)",
		Trail: []console.Crumb{{Label: "Soft-deleted buckets", Path: nil}, {Label: b.Name}}}, nil
}

func (p storageProvider) softBucketActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) != 3 {
		return nil
	}
	return storageDeletedProvider{endpoint: p.endpoint}.DetailActions(ctx, project, path[1:])
}

func (p storageProvider) restoreSoftBucket(ctx context.Context, path []string, action string, values map[string]string) error {
	if len(path) != 3 {
		return errors.New("a soft-deleted bucket is addressed generation/name")
	}
	return storageDeletedProvider{endpoint: p.endpoint}.ActAt(ctx, "", path[1:], action, values)
}
