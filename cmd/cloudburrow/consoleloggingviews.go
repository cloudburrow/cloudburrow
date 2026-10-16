package main

// Cloud Logging: creating, editing and deleting a bucket's log views (#1283),
// on the bucket's page of the Logs Storage screen, through the same
// ConfigServiceV2 the views are listed by.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

const (
	viewCreate = "create-view"
	viewEdit   = "edit-view"
	viewDelete = "delete-view"
)

// viewsChangeable reports whether a bucket's views can be created, changed
// or deleted: never _Required's, and only while the bucket is active.
func viewsChangeable(b *loggingpb.LogBucket) bool {
	return !strings.HasSuffix(b.GetName(), "/buckets/_Required") && b.GetLifecycleState() == loggingpb.LifecycleState_ACTIVE
}

// viewChangeable reports whether one view can be edited or deleted: not
// _AllLogs, which the API keeps unchanged on every bucket.
func viewChangeable(b *loggingpb.LogBucket, id string) bool {
	return viewsChangeable(b) && id != "_AllLogs"
}

func viewFilterField(def string) console.Field {
	return console.Field{Name: "filter", Label: "Filter", Type: "textarea", Default: def,
		Help: `Optional. Which entries the view shows, such as SOURCE("projects/my-project") AND resource.type = "gce_instance" AND LOG_ID("stdout"). Empty shows every entry.`}
}

func viewDescriptionField(def string) console.Field {
	return console.Field{Name: "description", Label: "Description", Type: "text", Default: def}
}

func createViewAction() console.Action {
	return console.Action{ID: viewCreate, Label: "Create view", Fields: []console.Field{
		{Name: "id", Label: "View ID", Type: "text", Required: true, Pattern: `^[A-Za-z0-9][A-Za-z0-9_\-.]{0,99}$`,
			Help: "Up to 100 letters, digits, _, - and ., not starting with _."},
		viewFilterField(""),
		viewDescriptionField(""),
	}}
}

func viewRowActions(v *loggingpb.LogView) []console.Action {
	return []console.Action{
		{ID: viewEdit, Label: "Edit", Fields: []console.Field{viewFilterField(v.GetFilter()), viewDescriptionField(v.GetDescription())}},
		{ID: viewDelete, Label: "Delete", Destructive: true},
	}
}

// DetailActions implements console.PathActor: Create view, the link
// actions (#1292, consoleloggingbuckets.go) and Copy log entries (#1394,
// consoleloggingcopy.go) on a bucket's page, and Edit and
// Delete on a view.
func (p loggingBucketsProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) < 1 || len(path) > 2 {
		return nil
	}
	b, err := p.get(ctx, project, path[0])
	if err != nil {
		return nil
	}
	if len(path) == 1 {
		var out []console.Action
		if viewsChangeable(b) {
			out = append(out, createViewAction())
		}
		out = append(out, p.linkActions(ctx, project, path)...)
		return append(out, p.copyActions(ctx, project, path)...) // #1394
	}
	if !viewsChangeable(b) {
		return nil
	}
	if !viewChangeable(b, path[1]) {
		return nil
	}
	v, err := p.view(ctx, path[0]+"/views/"+path[1])
	if err != nil {
		return nil
	}
	return viewRowActions(v)
}

func (p loggingBucketsProvider) view(ctx context.Context, name string) (*loggingpb.LogView, error) {
	a, err := p.admin()
	if err != nil {
		return nil, err
	}
	if err := p.fault(ctx, loggingpb.ConfigServiceV2_GetView_FullMethodName, name); err != nil {
		return nil, err
	}
	return a.ConfigServer().GetView(ctx, &loggingpb.GetViewRequest{Name: name})
}

// ActAt implements console.PathActor through CreateView, UpdateView and
// DeleteView, and CreateLink and DeleteLink (#1292).
func (p loggingBucketsProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if len(path) == 0 {
		return errors.New("no bucket named")
	}
	if action == "create-link" || action == "delete-link" {
		return p.actOnLink(ctx, project, path, action, values)
	}
	if action == copyEntriesAction || action == cancelCopyAction {
		return p.actOnCopy(ctx, project, path, action, values)
	}
	if err := bucketOfProject(project, path[0]); err != nil {
		return err
	}
	a, err := p.admin()
	if err != nil {
		return err
	}
	switch {
	case action == viewCreate && len(path) == 1:
		if err := p.fault(ctx, loggingpb.ConfigServiceV2_CreateView_FullMethodName, path[0]); err != nil {
			return err
		}
		_, err = a.ConfigServer().CreateView(ctx, &loggingpb.CreateViewRequest{Parent: path[0], ViewId: strings.TrimSpace(values["id"]),
			View: &loggingpb.LogView{Filter: strings.TrimSpace(values["filter"]), Description: values["description"]}})
		return err
	case action == viewEdit && len(path) == 2:
		name := path[0] + "/views/" + path[1]
		if err := p.fault(ctx, loggingpb.ConfigServiceV2_UpdateView_FullMethodName, name); err != nil {
			return err
		}
		_, err = a.ConfigServer().UpdateView(ctx, &loggingpb.UpdateViewRequest{Name: name,
			View:       &loggingpb.LogView{Filter: strings.TrimSpace(values["filter"]), Description: values["description"]},
			UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"filter", "description"}}})
		return err
	case action == viewDelete && len(path) == 2:
		name := path[0] + "/views/" + path[1]
		if err := p.fault(ctx, loggingpb.ConfigServiceV2_DeleteView_FullMethodName, name); err != nil {
			return err
		}
		_, err = a.ConfigServer().DeleteView(ctx, &loggingpb.DeleteViewRequest{Name: name})
		return err
	}
	return fmt.Errorf("unknown action %q", action)
}

var _ console.PathActor = loggingBucketsProvider{}
