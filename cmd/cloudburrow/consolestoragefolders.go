package main

// Hierarchical-namespace folders in the console (#1203).
//
// A bucket created with Hierarchical namespace on (the Create form, which
// turns uniform bucket-level access on with it and versioning off, as the
// API requires) has real folders, and its page shows them:
//
//   - The object browser lists the folders directly under the page's prefix
//     from folders.list, typed Folder, each opening its own page, whose
//     summary is folders.get's.
//   - Create folder, on the bucket's page and each folder's, is
//     folders.insert under the page's prefix, with recursive for the missing
//     parents of a nested name.
//   - On each folder's row: Rename folder is folders.rename, a long-running
//     operation whose result the dialog shows; Delete folder is
//     folders.delete, offered only on a folder with nothing in it, since the
//     API refuses any other; Delete folder and contents is
//     folders.deleteRecursive, confirmed by typing the folder's name.
//   - The Operations tab lists the bucket's long-running operations with
//     buckets.operations.list, each opening its own page, which is
//     buckets.operations.get.
//
// Every call goes through Google's generated JSON API client, since the
// official storage client has none of them; a refusal is the API's own
// message. The same folders are served on the Storage Control API, which
// the console does not call separately: it is the same resource.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	storagev1 "google.golang.org/api/storage/v1"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// folderPage is the first segment of an HNS folder's action path,
// [folderPage, bucket, name], its whole name ending in "/". Like objectPage
// it is never a bucket, whose name cannot begin with an underscore; the
// name is last, so a confirmation asks for it back.
const folderPage = "_folder"

// operationPage is the first segment of a bucket operation's page,
// [operationPage, bucket, id].
const operationPage = "_operation"

func folderPath(bucket, name string) []string { return []string{folderPage, bucket, name} }

func operationPath(bucket, id string) []string { return []string{operationPage, bucket, id} }

// operationsLimit is how many operations the Operations tab lists. Past it
// the tab says so rather than stopping silently.
const operationsLimit = 1000

// hnsFolders are the folders directly under a prefix, by their name there
// ("reports" for "logs/reports/" under "logs/"), from folders.list.
func (p storageProvider) hnsFolders(ctx context.Context, bucket, prefix string) ([]string, error) {
	s, err := jsonAPI(ctx, p.endpoint)
	if err != nil {
		return nil, err
	}
	var out []string
	err = s.Folders.List(bucket).Prefix(prefix).Delimiter("/").Context(ctx).Pages(ctx, func(page *storagev1.Folders) error {
		for _, f := range page.Items {
			rest := strings.TrimSuffix(strings.TrimPrefix(f.Name, prefix), "/")
			if rest != "" && !strings.Contains(rest, "/") {
				out = append(out, rest)
			}
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

// folderEmpty says whether a folder holds no object and no folder: one
// objects.list and one folders.list under its name, each a page of the
// least that answers it.
func folderEmpty(ctx context.Context, s *storagev1.Service, bucket, name string) (bool, error) {
	objs, err := s.Objects.List(bucket).Prefix(name).MaxResults(1).Context(ctx).Do()
	if err != nil {
		return false, err
	}
	if len(objs.Items) > 0 {
		return false, nil
	}
	// The folder itself is listed under its own name; a second is in it.
	fs, err := s.Folders.List(bucket).Prefix(name).PageSize(2).Context(ctx).Do()
	if err != nil {
		return false, err
	}
	for _, f := range fs.Items {
		if f.Name != name {
			return false, nil
		}
	}
	return true, nil
}

// createFolderAction is Create folder, on an HNS bucket's page and each
// folder's, made under the prefix the page shows.
func createFolderAction(bucket, prefix string) console.Action {
	where := "at the top of " + bucket
	if prefix != "" {
		where = "in " + prefix
	}
	return console.Action{
		ID: "createfolder", Label: "Create folder",
		Fields: []console.Field{
			{Name: "name", Label: "Folder name", Type: "text", Required: true,
				Help: "Made " + where + "; a / separates nested folders, and a trailing / is added."},
			{Name: "recursive", Label: "Create missing parent folders", Type: "checkbox", Default: "true",
				Help: "Sends recursive=true, so a nested name's missing folders are made with it. Off, a missing parent is refused."},
		},
	}
}

// folderRowActions are what can be done to one HNS folder from its row:
// Rename folder, Delete folder while it is empty, and Delete folder and
// contents.
func folderRowActions(name string, empty bool) []console.Action {
	out := []console.Action{{
		ID: "renamefolder", Label: "Rename folder",
		Fields: []console.Field{{
			Name: "destination", Label: "New folder name", Type: "text", Required: true, Default: name,
			Help: "The folder's whole new name in this bucket, parents included, such as logs/2026/. " +
				"Its folders and objects move with it, in one operation. The new name must not exist, " +
				"must not be inside the folder, and its parent must exist.",
		}},
	}}
	if empty {
		out = append(out, console.Action{
			ID: "deletefolder", Label: "Delete folder", Destructive: true,
			Confirm: fmt.Sprintf("The folder %s is empty and is deleted.", name),
		})
	}
	return append(out, console.Action{
		ID: "deletefolderrecursive", Label: "Delete folder and contents", Destructive: true,
		Confirm: fmt.Sprintf("The folder %s, every folder in it and every object in it are deleted, in one "+
			"operation. The objects are kept by the bucket's soft delete policy, as a single delete keeps "+
			"one; the folders are not.", name),
	})
}

// hnsFolderActions offers a folder's row actions on one that is there, read
// with folders.get, and Delete managed folder when it is also a managed
// folder (#828).
func (p storageProvider) hnsFolderActions(ctx context.Context, path []string) []console.Action {
	if len(path) != 3 || path[0] != folderPage {
		return nil
	}
	s, err := jsonAPI(ctx, p.endpoint)
	if err != nil {
		return nil
	}
	if _, err := s.Folders.Get(path[1], path[2]).Context(ctx).Do(); err != nil {
		return nil
	}
	empty, err := folderEmpty(ctx, s, path[1], path[2])
	actions := folderRowActions(path[2], err == nil && empty)
	if _, err := s.ManagedFolders.Get(path[1], path[2]).Context(ctx).Do(); err == nil {
		actions = append(actions, deleteManagedFolderAction(path[2]))
	}
	return actions
}

// hnsFolderRow is one folder row of an HNS bucket's browser.
func (p storageProvider) hnsFolderRow(ctx context.Context, s *storagev1.Service, bucket, prefix, name string, path []string, managed bool) console.Resource {
	full := prefix + name + "/"
	empty, err := folderEmpty(ctx, s, bucket, full)
	row := console.Resource{
		Name:    name + "/",
		Opens:   append(append([]string{}, path...), name),
		Fields:  map[string]string{"Type": "Folder", "Size": "—", "Updated": "—"},
		Target:  folderPath(bucket, full),
		Actions: folderRowActions(full, err == nil && empty),
	}
	if managed {
		row.Fields["Type"] = "Managed folder"
		row.Actions = append(row.Actions, deleteManagedFolderAction(full))
	}
	return row
}

// hnsFolderName reads a form's folder name under a prefix: trimmed, with the
// trailing slash the API adds anyway.
func hnsFolderName(prefix, raw string) (string, error) {
	name := strings.Trim(strings.TrimSpace(raw), "/")
	if name == "" {
		return "", errors.New("name the folder")
	}
	return prefix + name + "/", nil
}

// createFolder is folders.insert under the page's prefix.
func (p storageProvider) createFolder(ctx context.Context, path []string, values map[string]string) error {
	if len(path) == 0 || strings.HasPrefix(path[0], "_") {
		return errors.New("a folder is created on its bucket's page or a folder's")
	}
	prefix := ""
	if len(path) > 1 {
		prefix = strings.Join(path[1:], "/") + "/"
	}
	name, err := hnsFolderName(prefix, values["name"])
	if err != nil {
		return err
	}
	s, err := jsonAPI(ctx, p.endpoint)
	if err != nil {
		return err
	}
	_, err = s.Folders.Insert(path[0], &storagev1.Folder{Name: name}).
		Recursive(values["recursive"] == "true").Context(ctx).Do()
	return err
}

// actOnFolder performs a folder row's actions: rename and deleteRecursive
// answer with their operation.
func (p storageProvider) actOnFolder(ctx context.Context, path []string, action string, values map[string]string) (*console.Listing, error) {
	if len(path) != 3 || path[0] != folderPage {
		return nil, errors.New("a folder is addressed " + folderPage + "/bucket/name")
	}
	bucket, name := path[1], path[2]
	s, err := jsonAPI(ctx, p.endpoint)
	if err != nil {
		return nil, err
	}
	var op *storagev1.GoogleLongrunningOperation
	switch action {
	case "deletefolder":
		return nil, s.Folders.Delete(bucket, name).Context(ctx).Do()
	case "deletefolderrecursive":
		op, err = s.Folders.DeleteRecursive(bucket, name).Context(ctx).Do()
	case "renamefolder":
		dst, ferr := hnsFolderName("", values["destination"])
		if ferr != nil {
			return nil, ferr
		}
		op, err = s.Folders.Rename(bucket, name, dst).Context(ctx).Do()
	case "deletemanagedfolder":
		return nil, p.deleteManagedFolder(ctx, managedFolderPath(bucket, name))
	default:
		return nil, fmt.Errorf("%s is not a folder action", action)
	}
	if err != nil {
		return nil, err
	}
	return operationResult(op), nil
}

// operationMeta is the metadata of a folder operation, as the Storage
// Control API's RenameFolderMetadata and DeleteFolderRecursiveMetadata
// carry it.
type operationMeta struct {
	Common struct {
		Type       string `json:"type"`
		CreateTime string `json:"createTime"`
		EndTime    string `json:"endTime"`
		Progress   int    `json:"progressPercent"`
	} `json:"commonMetadata"`
	Source      string `json:"sourceFolderId"`
	Destination string `json:"destinationFolderId"`
	Folder      string `json:"folderId"`
}

func readOperationMeta(op *storagev1.GoogleLongrunningOperation) operationMeta {
	var m operationMeta
	_ = json.Unmarshal(op.Metadata, &m)
	if m.Source == "" {
		m.Source = m.Folder
	}
	return m
}

// operationID is an operation's ID, the last segment of its name.
func operationID(op *storagev1.GoogleLongrunningOperation) string {
	return op.Name[strings.LastIndex(op.Name, "/")+1:]
}

func operationState(op *storagev1.GoogleLongrunningOperation) string {
	switch {
	case op.Error != nil:
		return "Failed: " + op.Error.Message
	case op.Done:
		return "Done"
	}
	return "Running"
}

// operationResult is what Rename folder and Delete folder and contents
// answer with: their operation.
func operationResult(op *storagev1.GoogleLongrunningOperation) *console.Listing {
	m := readOperationMeta(op)
	row := func(label, value string) console.Resource {
		return console.Resource{Name: label, Fields: map[string]string{"Value": value}}
	}
	items := []console.Resource{
		row("Operation", op.Name),
		row("Type", orDash(m.Common.Type)),
		row("State", operationState(op)),
		row("Folder", orDash(m.Source)),
	}
	if m.Destination != "" {
		items = append(items, row("Destination", m.Destination))
	}
	return &console.Listing{Columns: []string{"Value"}, NameColumn: "Field", Noun: "fields",
		Items: items, Total: len(items)}
}

// operationsSection is the Operations tab of an HNS bucket's page, from
// buckets.operations.list.
func (p storageProvider) operationsSection(ctx context.Context, bucket string) console.Section {
	sec := console.Section{ID: "operations", Label: "Operations", Listing: console.Listing{
		Columns: []string{"Type", "State", "Folder", "Destination", "Created", "Ended"}, NameColumn: "Operation",
		Noun: "operations", Items: []console.Resource{},
	}}
	s, err := jsonAPI(ctx, p.endpoint)
	if err != nil {
		sec.Listing.Unavailable = err.Error()
		return sec
	}
	errFull := errors.New("full")
	err = s.Operations.List(bucket).Context(ctx).Pages(ctx, func(page *storagev1.GoogleLongrunningListOperationsResponse) error {
		for _, op := range page.Operations {
			if len(sec.Listing.Items) == operationsLimit {
				sec.Listing.Note = fmt.Sprintf("The first %d operations are listed; list the rest with buckets.operations.list.", operationsLimit)
				return errFull
			}
			m := readOperationMeta(op)
			id := operationID(op)
			sec.Listing.Items = append(sec.Listing.Items, console.Resource{
				Name: id,
				Fields: map[string]string{
					"Type": orDash(m.Common.Type), "State": operationState(op),
					"Folder": orDash(m.Source), "Destination": orDash(m.Destination),
					"Created": orDash(shortTime(m.Common.CreateTime)), "Ended": orDash(shortTime(m.Common.EndTime)),
				},
				Opens: operationPath(bucket, id),
			})
		}
		return nil
	})
	if err != nil && !errors.Is(err, errFull) {
		sec.Listing.Unavailable = "cannot list the operations: " + err.Error()
	}
	sec.Listing.Total = len(sec.Listing.Items)
	return sec
}

// operationDetail is one operation's page, from buckets.operations.get.
func (p storageProvider) operationDetail(ctx context.Context, path []string) (console.Detail, error) {
	if len(path) != 3 {
		return console.Detail{Unavailable: "an operation is addressed " + operationPage + "/bucket/id"}, nil
	}
	s, err := jsonAPI(ctx, p.endpoint)
	if err != nil {
		return console.Detail{}, err
	}
	op, err := s.Operations.Get(path[1], path[2]).Context(ctx).Do()
	if err != nil {
		return console.Detail{Unavailable: fmt.Sprintf("cannot read operation %s: %v", path[2], err)}, nil
	}
	m := readOperationMeta(op)
	props := []console.Property{
		{Label: "Name", Value: op.Name},
		{Label: "Type", Value: orDash(m.Common.Type)},
		{Label: "State", Value: operationState(op)},
		{Label: "Progress", Value: fmt.Sprintf("%d%%", m.Common.Progress)},
		{Label: "Folder", Value: orDash(m.Source)},
	}
	if m.Destination != "" {
		props = append(props, console.Property{Label: "Destination", Value: m.Destination})
	}
	props = append(props,
		console.Property{Label: "Created", Value: orDash(m.Common.CreateTime)},
		console.Property{Label: "Ended", Value: orDash(m.Common.EndTime)})
	return console.Detail{
		Summary: []console.Property{{Label: "Bucket", Value: path[1]}, {Label: "Operation", Value: path[2]}},
		Sections: []console.Section{{ID: "operation", Label: "Operation", Kind: console.KindProperties,
			Groups: []console.PropertyGroup{{Heading: "Operation", Properties: props}},
			Note: "Read with buckets.operations.get. A folder operation is done when it is returned here, " +
				"so there is nothing to cancel."}},
	}, nil
}

// folderSummary is what folders.get says about the folder a page shows, in
// an HNS bucket; nil when it is no folder.
func (p storageProvider) folderSummary(ctx context.Context, bucket, name string) []console.Property {
	s, err := jsonAPI(ctx, p.endpoint)
	if err != nil {
		return nil
	}
	f, err := s.Folders.Get(bucket, name).Context(ctx).Do()
	if err != nil {
		return nil
	}
	return []console.Property{
		{Label: "Type", Value: "Folder (hierarchical namespace)"},
		{Label: "Created", Value: shortTime(f.CreateTime)},
		{Label: "Metageneration", Value: fmt.Sprint(f.Metageneration)},
	}
}

// hnsObjects is one level of a bucket with hierarchical namespace: its
// folders from folders.list, each with its row actions, then the objects.
// A managed folder that is no folder (one made with nothing in it) is
// listed as the browser lists it elsewhere.
func (p storageProvider) hnsObjects(ctx context.Context, out console.Listing, bucket, prefix string, path []string, items []objectJSON, managed map[string]bool) (console.Listing, error) {
	names, err := p.hnsFolders(ctx, bucket, prefix)
	if err != nil {
		return out, fmt.Errorf("cannot list the folders of %s: %w", bucket, err)
	}
	s, err := jsonAPI(ctx, p.endpoint)
	if err != nil {
		return out, err
	}
	for _, name := range names {
		out.Items = append(out.Items, p.hnsFolderRow(ctx, s, bucket, prefix, name, path, managed[name]))
		delete(managed, name)
	}
	rest := make([]string, 0, len(managed))
	for name := range managed {
		rest = append(rest, name)
	}
	sort.Strings(rest)
	for _, name := range rest {
		full := prefix + name + "/"
		out.Items = append(out.Items, console.Resource{
			Name:    name + "/",
			Opens:   append(append([]string{}, path...), name),
			Fields:  map[string]string{"Type": "Managed folder", "Size": "—", "Updated": "—"},
			Target:  managedFolderPath(bucket, full),
			Actions: []console.Action{deleteManagedFolderAction(full)},
		})
	}
	out.Items = append(out.Items, objectRows(bucket, prefix, items)...)
	out.Total = len(out.Items)
	return out, nil
}
