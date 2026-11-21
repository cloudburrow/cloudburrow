package main

import (
	"context"
	"testing"

	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// Every queue and task action is confirmed as Google's prompt is, from
// https://docs.cloud.google.com/tasks/docs/deleting-appengine-queues-and-tasks
// (#1848): Pause queue by Pause, Purge queue by Purge, Delete task by
// Delete, with nothing typed; Delete queue by the name typed back, then
// Confirm; Resume queue at once.
func TestTasksActionsConfirmAsGooglesPrompts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, name := newTasksEditProvider(t)
	type want struct{ submit, style string }
	click := func(submit string) want { return want{submit, console.DeleteConfirmClick} }
	check := func(where string, acts []console.Action, wants map[string]want) {
		t.Helper()
		seen := map[string]bool{}
		for _, a := range acts {
			w, ok := wants[a.ID]
			if !ok {
				continue
			}
			seen[a.ID] = true
			if a.Submit != w.submit || a.ConfirmStyle != w.style {
				t.Errorf("%s %s (%s): submit %q style %q; want %q %q", where, a.ID, a.Label, a.Submit, a.ConfirmStyle, w.submit, w.style)
			}
			if w.submit != "" && a.Confirm == "" {
				t.Errorf("%s %s has no prompt", where, a.ID)
			}
		}
		for id := range wants {
			if !seen[id] {
				t.Errorf("%s offers no %s: %+v", where, id, acts)
			}
		}
	}
	check("the list's header", p.ListActions(ctx, "edit-proj"), map[string]want{
		tasksPauseSelected: click("Pause"), tasksResumeSelected: {}, tasksPurgeSelected: click("Purge")})
	for _, a := range p.ListActions(ctx, "edit-proj") {
		for _, f := range a.Fields {
			if f.Type != "hidden" {
				t.Errorf("the header's %s opens a form with %s; Google's acts on the checked queues", a.Label, f.Name)
			}
		}
	}
	check("a running queue's row", tasksQueueActions("RUNNING"), map[string]want{"pause": click("Pause"), "purge": click("Purge")})
	check("a paused queue's row", tasksQueueActions("PAUSED"), map[string]want{"resume": {}})
	check("the queue page", p.DetailActions(ctx, "edit-proj", []string{name}), map[string]want{
		"pausequeue": click("Pause"), "purgequeue": click("Purge"), "deletequeue": {"Confirm", ""},
		tasksDeleteSelected: click("Delete")})
	check("a task", tasksTaskActions(), map[string]want{"runtask": {}, "deletetask": click("Delete")})

	w := p.DeleteWords()
	if w.Selected != "Delete queue" || w.Submit != "Confirm" {
		t.Errorf("the list's delete words = %+v; want Delete queue, then Confirm", w)
	}
}

// The Tasks tab's checkbox on each task and its header Delete task (#1848):
// the rows are selectable, the listing carries Delete task, and it deletes
// the checked tasks, by ID or full name, as the official API's GetTask then
// reads; a task of another queue is refused and nothing of it is deleted.
func TestTasksDeleteCheckedTasks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p, name := newTasksEditProvider(t)
	for _, id := range []string{"a", "b", "c"} {
		if err := p.ActAt(ctx, "edit-proj", []string{name}, "createtask", map[string]string{
			"url": "http://127.0.0.1:9/hook", "httpMethod": "POST", "taskId": id,
			"scheduleTime": "2099-01-01T00:00:00Z",
		}); err != nil {
			t.Fatalf("createtask %s: %v", id, err)
		}
	}
	d, err := p.Detail(ctx, "edit-proj", []string{name})
	if err != nil {
		t.Fatal(err)
	}
	var listing *console.Listing
	for i := range d.Sections {
		if d.Sections[i].ID == "tasks" {
			listing = &d.Sections[i].Listing
		}
	}
	if listing == nil || len(listing.SelectActions) != 1 || listing.SelectActions[0].ID != tasksDeleteSelected ||
		listing.SelectActions[0].Label != "Delete task" {
		t.Fatalf("the Tasks tab's selection actions = %+v; want Delete task", listing)
	}
	if len(listing.Items) != 3 {
		t.Fatalf("the Tasks tab lists %d tasks", len(listing.Items))
	}
	for _, it := range listing.Items {
		if !it.Selectable {
			t.Errorf("task %s has no checkbox", it.Name)
		}
	}
	for _, a := range d.Actions {
		if a.ID == tasksDeleteSelected {
			t.Errorf("Delete task over the checked tasks is beside the title: %+v", d.Actions)
		}
	}

	other, err := p.Create(ctx, "edit-proj", map[string]string{"name": "other-q", "location": "us-central1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.ActAt(ctx, "edit-proj", []string{name}, tasksDeleteSelected,
		map[string]string{"tasks": other + "/tasks/a"}); err == nil {
		t.Error("deleting another queue's task through this queue's Delete task was accepted")
	}
	if err := p.ActAt(ctx, "edit-proj", []string{name}, tasksDeleteSelected, map[string]string{"tasks": ""}); err == nil {
		t.Error("Delete task with nothing checked was accepted")
	}
	if err := p.ActAt(ctx, "edit-proj", []string{name}, tasksDeleteSelected,
		map[string]string{"tasks": "a\n" + name + "/tasks/b"}); err != nil {
		t.Fatalf("Delete task: %v", err)
	}
	for id, kept := range map[string]bool{"a": false, "b": false, "c": true} {
		_, err := p.api().GetTask(ctx, &cloudtaskspb.GetTaskRequest{Name: name + "/tasks/" + id})
		if (err == nil) != kept {
			t.Errorf("task %s: GetTask = %v; want kept %v", id, err, kept)
		}
	}
}
