package main

// Cloud Storage pages laid out as Google's console lays them out (#1843),
// from Google's public documentation only (creating-buckets,
// managing-lifecycles, using-object-versioning, use-soft-deleted-objects,
// discover-object-storage-console):
//
//   - the Buckets list's columns: Created, Location type, Location, Default
//     storage class, Last modified, Public access, Access control,
//     Protection, Lifecycle, Hierarchical namespace and Labels, with a row
//     menu holding Create restore job;
//   - Create a bucket as five numbered steps, in Google's order with
//     Google's defaults, ending in Create;
//   - the bucket's Protection and Lifecycle tabs, Lifecycle with Add a rule
//     and a per-rule Delete rule;
//   - Create folder on a bucket without hierarchical namespace, which makes
//     the zero-byte "name/" object Google's console makes.
//
// Every change goes through the storage server's JSON API, as before.

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

	"cloud.google.com/go/storage"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// bucketListColumns are the Buckets list's columns, in Google's order.
var bucketListColumns = []string{
	"Created", "Location type", "Location", "Default storage class", "Last modified",
	"Public access", "Access control", "Protection", "Lifecycle", "Hierarchical namespace", "Labels",
}

// locationTypeLabel is the API's locationType as Google's console words it.
func locationTypeLabel(t string) string {
	switch strings.ToLower(t) {
	case "multi-region":
		return "Multi-region"
	case "dual-region":
		return "Dual-region"
	case "region":
		return "Region"
	case "zone":
		return "Zone"
	}
	return orDash(t)
}

// storageClassLabel is a storage class as the console words it.
func storageClassLabel(c string) string {
	if c == "" {
		return "—"
	}
	return strings.ToUpper(c[:1]) + strings.ToLower(c[1:])
}

func (b bucketMeta) accessControl() string {
	if b.IAMConfiguration.UniformBucketLevelAccess.Enabled {
		return "Uniform"
	}
	return "Fine-grained"
}

// publicAccess is Google's Public access column: "Not public" when public
// access prevention is enforced, "Subject to object ACLs" on a fine-grained
// bucket, and "Not public" otherwise, since this instance grants allUsers
// nothing unless a policy says so.
func (b bucketMeta) publicAccess() string {
	if b.IAMConfiguration.PublicAccessPrevention == "enforced" {
		return "Not public"
	}
	if !b.IAMConfiguration.UniformBucketLevelAccess.Enabled {
		return "Subject to object ACLs"
	}
	return "Not public"
}

// protection is the Protection column: the protections the bucket has.
func (b bucketMeta) protection() string {
	var out []string
	if b.softDeleteSeconds() != "0" {
		out = append(out, "Soft Delete")
	}
	if b.Versioning.Enabled {
		out = append(out, "Object versioning")
	}
	if b.retentionSeconds() != "" {
		out = append(out, "Bucket retention policy")
	}
	if b.objectRetentionEnabled() {
		out = append(out, "Object retention")
	}
	if len(out) == 0 {
		return "None"
	}
	return strings.Join(out, ", ")
}

func (b bucketMeta) lifecycleSummary() string {
	switch n := len(b.Lifecycle.Rule); n {
	case 0:
		return "None"
	case 1:
		return "1 rule"
	default:
		return fmt.Sprintf("%d rules", n)
	}
}

func (b bucketMeta) labelSummary() string {
	if len(b.Labels) == 0 {
		return "—"
	}
	keys := make([]string, 0, len(b.Labels))
	for k := range b.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+": "+b.Labels[k])
	}
	return strings.Join(parts, ", ")
}

// bucketListRow is one bucket on the Buckets list. Its menu has Create
// restore job, which Google's bucket overflow menu holds, addressed at the
// bucket's page, which offers it.
func bucketListRow(b bucketMeta) console.Resource {
	hns := "Disabled"
	if b.hierarchical() {
		hns = "Enabled"
	}
	return console.Resource{
		Name: b.Name,
		Fields: map[string]string{
			"Created":                shortTime(b.TimeCreated),
			"Location type":          locationTypeLabel(b.LocationType),
			"Location":               strings.ToLower(b.Location),
			"Default storage class":  storageClassLabel(b.StorageClass),
			"Last modified":          shortTime(b.Updated),
			"Public access":          b.publicAccess(),
			"Access control":         b.accessControl(),
			"Protection":             b.protection(),
			"Lifecycle":              b.lifecycleSummary(),
			"Hierarchical namespace": hns,
			"Labels":                 b.labelSummary(),
		},
		Target:  []string{b.Name},
		Actions: []console.Action{bulkRestoreAction()},
	}
}

// CreateTitle implements console.CreateTitler: the page is "Create a
// bucket" and its button Create, as Google's are.
func (storageProvider) CreateTitle() string { return "Create a bucket" }

// CreateOnPage implements console.PageCreator: Create a bucket is a full page
// of numbered steps.
func (storageProvider) CreateOnPage() bool { return true }

// Google's access control choice, on the create form and the Permissions
// tab's Switch to (#1843), and its storage class choice.
const (
	accessUniform        = "Uniform"
	accessFineGrained    = "Fine-grained"
	classChoiceDefault   = "Set a default class"
	classChoiceAutoclass = "Autoclass"
)

// The location types Google's step 2 offers, and the default location of
// each: US multi-region is Google's default.
const (
	locMulti  = "Multi-region"
	locDual   = "Dual-region"
	locRegion = "Region"
)

// softDeleteSecondsFrom is the soft delete policy the Create form asks for:
// its checkbox, and its duration in days when "Set custom retention
// duration" is given; an older client's softDeleteSeconds is kept.
func softDeleteSecondsFrom(values map[string]string) (string, bool, error) {
	if v := strings.TrimSpace(values["softDeleteSeconds"]); v != "" {
		return v, true, nil
	}
	on, ok := values["softDelete"]
	if !ok {
		return "", false, nil
	}
	if on != "true" {
		return "0", true, nil
	}
	days := strings.TrimSpace(values["softDeleteDays"])
	if days == "" {
		return "", false, nil
	}
	n, err := strconv.ParseInt(days, 10, 64)
	if err != nil || n < 0 {
		return "", false, fmt.Errorf("soft delete retention duration: %q is not a number of days", days)
	}
	return strconv.FormatInt(n*86400, 10), true, nil
}

// --- Protection and Lifecycle tabs -------------------------------------

// protectionSection is the Protection tab: soft delete, object versioning,
// the bucket retention policy, object retention and the default hold, as
// Google's Protection tab groups them. Lock retention policy is on it.
func protectionSection(b bucketMeta) console.Section {
	onOff := func(v bool) string {
		if v {
			return "On"
		}
		return "Off"
	}
	softDelete := "Off"
	if s := b.softDeleteSeconds(); s != "0" {
		softDelete = "On"
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			if n%86400 == 0 {
				softDelete += fmt.Sprintf(", retained for %d days", n/86400)
			} else {
				softDelete += fmt.Sprintf(", retained for %d seconds", n)
			}
		}
	}
	retention := "None"
	if rp := b.RetentionPolicy; rp != nil && rp.RetentionPeriod != "" {
		retention = rp.RetentionPeriod + " seconds"
		if rp.IsLocked {
			retention += " (locked)"
		}
	}
	hardDeletePause := "Off"
	if b.hardDeletePaused() {
		hardDeletePause = "On"
	}
	retentionActions := []console.Action{}
	for _, a := range lockRetentionAction(b) {
		if a.ID == "lockretention" {
			retentionActions = append(retentionActions, a)
		}
	}
	return console.Section{
		ID: "protection", Label: "Protection", Kind: console.KindProperties,
		Groups: []console.PropertyGroup{
			{Heading: "Soft delete policy (for data recovery)", Properties: []console.Property{
				{Label: "Soft delete policy", Value: softDelete},
				{Label: "Hard delete pause", Value: hardDeletePause},
			}},
			{Heading: "Object versioning", Properties: []console.Property{
				{Label: "Object versioning", Value: onOff(b.Versioning.Enabled)},
			}},
			{Heading: "Bucket retention policy", Properties: []console.Property{
				{Label: "Retention period", Value: retention},
			}},
			{Heading: "Object retention", Properties: []console.Property{
				{Label: "Object retention", Value: onOff(b.objectRetentionEnabled())},
			}},
			{Heading: "Default event-based hold", Properties: []console.Property{
				{Label: "Default event-based hold", Value: onOff(b.DefaultEventBasedHold)},
			}},
		},
		Actions: retentionActions,
		Note:    "Edit bucket changes these through buckets.patch; object retention can be enabled only when a bucket is created.",
	}
}

// lifecycleRule is a rule as the API returns it.
type lifecycleRule struct {
	Action struct {
		Type         string `json:"type"`
		StorageClass string `json:"storageClass"`
	} `json:"action"`
	Condition map[string]any `json:"condition"`
}

func lifecycleActionLabel(r lifecycleRule) string {
	switch r.Action.Type {
	case "Delete":
		return "Delete object"
	case "SetStorageClass":
		return "Set to " + storageClassLabel(r.Action.StorageClass)
	case "AbortIncompleteMultipartUpload":
		return "Delete incomplete multipart upload"
	}
	return r.Action.Type
}

// lifecycleConditionLabels names each documented condition as Google's rule
// editor does.
var lifecycleConditionLabels = map[string]string{
	"age":                     "Age",
	"createdBefore":           "Created before",
	"customTimeBefore":        "Custom-Time before",
	"daysSinceCustomTime":     "Days since custom time",
	"daysSinceNoncurrentTime": "Days since becoming noncurrent",
	"isLive":                  "Live state",
	"matchesStorageClass":     "Storage class matches",
	"matchesPrefix":           "Name matches prefix",
	"matchesSuffix":           "Name matches suffix",
	"noncurrentTimeBefore":    "Became noncurrent before",
	"numNewerVersions":        "Number of newer versions",
}

func lifecycleConditionText(c map[string]any) string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		label := lifecycleConditionLabels[k]
		if label == "" {
			label = k
		}
		var v string
		switch x := c[k].(type) {
		case []any:
			s := make([]string, 0, len(x))
			for _, e := range x {
				s = append(s, fmt.Sprint(e))
			}
			v = strings.Join(s, ", ")
		case float64:
			v = strconv.FormatFloat(x, 'f', -1, 64)
		case bool:
			if k == "isLive" {
				v = map[bool]string{true: "Live", false: "Noncurrent"}[x]
			} else {
				v = strconv.FormatBool(x)
			}
		default:
			v = fmt.Sprint(x)
		}
		if k == "age" || k == "daysSinceNoncurrentTime" || k == "daysSinceCustomTime" {
			v += " days"
		}
		parts = append(parts, label+": "+v)
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, "; ")
}

// lifecycleRulePage is the first segment of a lifecycle rule's action path,
// [lifecycleRulePage, bucket, index]; no bucket begins with an underscore.
const lifecycleRulePage = "_lifecyclerule"

// lifecycleSection is the Lifecycle tab: the rules, one per row with Delete
// rule, and Add a rule above them.
func lifecycleSection(b bucketMeta) console.Section {
	list := console.Listing{
		Columns:    []string{"Action", "Object condition"},
		NameColumn: "Rule",
		Noun:       "lifecycle rules",
		Items:      []console.Resource{},
		Note:       "No rules yet. Add a rule deletes or reclasses objects that meet its conditions.",
	}
	for i, raw := range b.Lifecycle.Rule {
		var r lifecycleRule
		_ = json.Unmarshal(raw, &r)
		list.Items = append(list.Items, console.Resource{
			Name: fmt.Sprintf("Rule %d", i+1),
			Fields: map[string]string{
				"Action":           lifecycleActionLabel(r),
				"Object condition": lifecycleConditionText(r.Condition),
			},
			// The rule's own name, "Rule N", last: Delete rule's confirmation
			// asks for what the row shows.
			Target:  []string{lifecycleRulePage, b.Name, fmt.Sprintf("Rule %d", i+1)},
			Actions: []console.Action{deleteLifecycleRuleAction(b.Metageneration)},
		})
	}
	list.Total = len(list.Items)
	return console.Section{
		ID: "lifecycle", Label: "Lifecycle", Listing: list,
		Actions: []console.Action{addLifecycleRuleAction(b.Metageneration), runLifecycleAction()},
	}
}

// addLifecycleRuleAction is Add a rule: Google's two steps, "Select an
// action" and "Select object conditions", as the form's two sections.
func addLifecycleRuleAction(metageneration string) console.Action {
	const act, cond = "Select an action", "Select object conditions"
	return console.Action{
		ID: "addlifecyclerule", Label: "Add a rule",
		Fields: []console.Field{
			{Name: "metageneration", Type: "hidden", Default: metageneration},
			{Name: "action", Label: "Action", Type: "select", Section: act, Default: "Delete object",
				Options: []string{"Set storage class to Nearline", "Set storage class to Coldline",
					"Set storage class to Archive", "Set storage class to Standard", "Delete object",
					"Delete incomplete multipart upload"}},
			{Name: "age", Label: "Age (days)", Type: "text", Section: cond, Pattern: `^[0-9]*$`,
				Help: "Days since the object was created."},
			{Name: "createdBefore", Label: "Created before", Type: "text", Section: cond,
				Pattern: `^([0-9]{4}-[0-9]{2}-[0-9]{2})?$`, Help: "A date, YYYY-MM-DD."},
			{Name: "matchesStorageClass", Label: "Set storage class matches", Type: "text", Section: cond,
				Help: "Comma-separated storage classes, such as STANDARD, NEARLINE."},
			{Name: "numNewerVersions", Label: "Number of newer versions", Type: "text", Section: cond, Pattern: `^[0-9]*$`},
			{Name: "daysSinceNoncurrentTime", Label: "Days since becoming noncurrent", Type: "text", Section: cond, Pattern: `^[0-9]*$`},
			{Name: "isLive", Label: "Live state", Type: "select", Section: cond, Default: "Any",
				Options: []string{"Any", "Live", "Noncurrent"}},
			{Name: "matchesPrefix", Label: "Object name matches prefix", Type: "text", Section: cond,
				Help: "Comma-separated prefixes."},
			{Name: "matchesSuffix", Label: "Object name matches suffix", Type: "text", Section: cond,
				Help: "Comma-separated suffixes."},
		},
	}
}

func deleteLifecycleRuleAction(metageneration string) console.Action {
	return console.Action{
		ID: "deletelifecyclerule", Label: "Delete rule", Destructive: true,
		Fields: []console.Field{{Name: "metageneration", Type: "hidden", Default: metageneration}},
	}
}

// lifecycleRuleFrom is the rule Add a rule describes.
func lifecycleRuleFrom(values map[string]string) (map[string]any, error) {
	action := map[string]any{}
	switch a := values["action"]; {
	case a == "Delete object":
		action["type"] = "Delete"
	case a == "Delete incomplete multipart upload":
		action["type"] = "AbortIncompleteMultipartUpload"
	case strings.HasPrefix(a, "Set storage class to "):
		action["type"] = "SetStorageClass"
		action["storageClass"] = strings.ToUpper(strings.TrimPrefix(a, "Set storage class to "))
	default:
		return nil, fmt.Errorf("choose an action")
	}
	cond := map[string]any{}
	for _, k := range []string{"age", "numNewerVersions", "daysSinceNoncurrentTime"} {
		if v := strings.TrimSpace(values[k]); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return nil, fmt.Errorf("%s: %q is not a whole number", lifecycleConditionLabels[k], v)
			}
			cond[k] = n
		}
	}
	if v := strings.TrimSpace(values["createdBefore"]); v != "" {
		cond["createdBefore"] = v
	}
	for _, k := range []string{"matchesStorageClass", "matchesPrefix", "matchesSuffix"} {
		if l := splitList(values[k]); len(l) > 0 {
			if k == "matchesStorageClass" {
				for i := range l {
					l[i] = strings.ToUpper(l[i])
				}
			}
			cond[k] = l
		}
	}
	switch values["isLive"] {
	case "Live":
		cond["isLive"] = true
	case "Noncurrent":
		cond["isLive"] = false
	}
	if len(cond) == 0 {
		return nil, errors.New("select at least one object condition")
	}
	return map[string]any{"action": action, "condition": cond}, nil
}

// patchBucket sends buckets.patch against the metageneration the page read,
// so a bucket changed since is refused rather than overwritten.
func (p storageProvider) patchBucket(ctx context.Context, bucket, metageneration string, body map[string]any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	q := urlpkg.Values{}
	if metageneration != "" {
		q.Set("ifMetagenerationMatch", metageneration)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, p.bucketURL(bucket)+"?"+q.Encode(), bytes.NewReader(raw))
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

// actOnLifecycle performs Add a rule (on the bucket's page) and Delete rule
// (on a rule's row).
func (p storageProvider) actOnLifecycle(ctx context.Context, path []string, action string, values map[string]string) (bool, error) {
	switch action {
	case "addlifecyclerule":
		if len(path) != 1 || strings.HasPrefix(path[0], "_") {
			return true, errors.New("a lifecycle rule is added on its bucket's page")
		}
		b, err := p.readBucket(ctx, path[0])
		if err != nil {
			return true, err
		}
		rule, err := lifecycleRuleFrom(values)
		if err != nil {
			return true, err
		}
		rules := make([]any, 0, len(b.Lifecycle.Rule)+1)
		for _, r := range b.Lifecycle.Rule {
			rules = append(rules, r)
		}
		rules = append(rules, rule)
		return true, p.patchBucket(ctx, path[0], values["metageneration"],
			map[string]any{"lifecycle": map[string]any{"rule": rules}})
	case "deletelifecyclerule":
		if len(path) != 3 || path[0] != lifecycleRulePage {
			return true, errors.New("a lifecycle rule is deleted from its row")
		}
		b, err := p.readBucket(ctx, path[1])
		if err != nil {
			return true, err
		}
		i, err := strconv.Atoi(strings.TrimPrefix(path[2], "Rule "))
		i-- // rules are numbered from 1, as the tab lists them
		if err != nil || i < 0 || i >= len(b.Lifecycle.Rule) {
			return true, fmt.Errorf("%s has no rule %s", path[1], path[2])
		}
		rules := make([]any, 0, len(b.Lifecycle.Rule))
		for j, r := range b.Lifecycle.Rule {
			if j != i {
				rules = append(rules, r)
			}
		}
		return true, p.patchBucket(ctx, path[1], values["metageneration"],
			map[string]any{"lifecycle": map[string]any{"rule": rules}})
	}
	return false, nil
}

// lifecycleRuleActions are what a rule's row offers.
func (p storageProvider) lifecycleRuleActions(ctx context.Context, path []string) ([]console.Action, bool) {
	if len(path) == 0 || path[0] != lifecycleRulePage {
		return nil, false
	}
	if len(path) != 3 {
		return nil, true
	}
	b, err := p.readBucket(ctx, path[1])
	if err != nil {
		return nil, true
	}
	return []console.Action{deleteLifecycleRuleAction(b.Metageneration)}, true
}

// --- Create folder on a flat bucket ------------------------------------

// createSimpleFolderAction is Create folder on a bucket without
// hierarchical namespace: Google's console makes a zero-byte object whose
// name ends in "/", which the browser lists as a folder.
func createSimpleFolderAction(bucket, prefix string) console.Action {
	where := "at the top of " + bucket
	if prefix != "" {
		where = "in " + prefix
	}
	return console.Action{
		ID: "createsimplefolder", Label: "Create folder",
		Fields: []console.Field{
			{Name: "name", Label: "Name", Type: "text", Required: true, Pattern: `^[^\r\n#\[\]*?]+$`,
				Help: "Made " + where + " as an empty object whose name ends in /."},
		},
	}
}

func (p storageProvider) createSimpleFolder(ctx context.Context, path []string, values map[string]string) error {
	if len(path) == 0 || strings.HasPrefix(path[0], "_") {
		return errors.New("a folder is created on its bucket's page or a folder's")
	}
	name := strings.Trim(strings.TrimSpace(values["name"]), "/")
	if name == "" {
		return errors.New("name the folder")
	}
	prefix := ""
	if len(path) > 1 {
		prefix = strings.Join(path[1:], "/") + "/"
	}
	c, err := p.storageClient(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	w := c.Bucket(path[0]).Object(prefix + name + "/").If(storage.Conditions{DoesNotExist: true}).NewWriter(ctx)
	w.ContentType = "text/plain"
	return w.Close()
}

// objectsTabActions are the Objects tab's own buttons after Upload: Create
// folder (a real folder in a bucket with hierarchical namespace, an empty
// "name/" object otherwise) and Create managed folder.
func objectsTabActions(bucket, prefix string, hns bool) []console.Action {
	folder := createSimpleFolderAction(bucket, prefix)
	if hns {
		folder = createFolderAction(bucket, prefix)
	}
	return []console.Action{folder, createManagedFolderAction(bucket, prefix)}
}

// bucketObservability is a bucket's Observability tab, in Google's place
// (#1843): its dashboards read Cloud Monitoring, which this instance does
// not run, so it says so, as the Observability page does.
func bucketObservability() console.Section {
	return console.Section{ID: "observability", Label: "Observability",
		Unavailable: "Not available locally: a bucket's Observability charts read Cloud Monitoring metrics, which CloudBurrow does not " +
			"emulate. The bucket's requests are in Logs Explorer and in the console's Requests page."}
}

// inventoryReportsSection is a bucket's Inventory reports tab (#1843): the
// Storage Insights inventory report configurations whose source is this
// bucket, each opening its page under Inventory reports, with Create
// inventory report there with the bucket filled in.
func (p storageProvider) inventoryReportsSection(ctx context.Context, project, bucket string) console.Section {
	sec := console.Section{ID: "inventory", Label: "Inventory reports", Listing: console.Listing{
		Columns: []string{"Display name", "Destination", "Format", "Frequency", "Created"}, NameColumn: "Configuration ID",
		Noun: "inventory report configurations", Items: []console.Resource{}}}
	if p.insights == "" {
		sec.Unavailable = "Storage Insights is not served by this instance, so no inventory report can be configured."
		return sec
	}
	q := urlpkg.Values{"sourceBucket": {bucket}}
	if project != "" {
		q.Set("project", project)
	}
	sec.Actions = []console.Action{{ID: "create-inventory-report", Label: "Create inventory report configuration",
		Href: "/storage-inventory-reports/create?" + q.Encode()}}
	if project == "" {
		return sec
	}
	inv := storageInventoryProvider{endpoint: p.insights, storage: p.endpoint}
	cs, err := inv.configs(ctx, project)
	if err != nil {
		sec.Listing.Unavailable = "cannot list the inventory report configurations: " + err.Error()
		return sec
	}
	for _, c := range cs {
		if c.source() != bucket {
			continue
		}
		freq, _, _ := c.frequency()
		id := lastSegment(c.Name)
		sec.Listing.Items = append(sec.Listing.Items, console.Resource{Name: id,
			Link: "/storage-inventory-reports/" + urlpkg.PathEscape(id) + "?project=" + urlpkg.QueryEscape(project),
			Fields: map[string]string{"Display name": c.DisplayName, "Destination": c.destination(),
				"Format": c.format(), "Frequency": freq, "Created": shortTime(c.CreateTime)}})
	}
	sec.Listing.Total = len(sec.Listing.Items)
	return sec
}
