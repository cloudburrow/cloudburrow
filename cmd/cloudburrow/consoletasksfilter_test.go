package main

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// TestConsoleQueueListFilter (#1255): the queue list's filter is ListQueues',
// evaluated by the same code; a refused filter is reported on the box with
// no rows.
func TestConsoleQueueListFilter(t *testing.T) {
	ctx := context.Background()
	p := tasksProvider{svc: &tasksService{store: tasks.NewStore(store.NewMemory())}}
	queues := map[string]string{}
	for _, name := range []string{"orders", "emails", "orders-slow"} {
		q, err := p.Create(ctx, "demo-project", map[string]string{"name": name})
		if err != nil {
			t.Fatal(err)
		}
		queues[name] = q
	}
	if err := p.Act(ctx, "demo-project", queues["orders-slow"], "pause"); err != nil {
		t.Fatal(err)
	}
	l, err := p.ListFiltered(ctx, "demo-project", "name: orders AND NOT state: PAUSED")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range l.Items {
		got = append(got, lastSegment(r.Name))
	}
	if strings.Join(got, " ") != "orders" {
		t.Errorf("filtered rows = %v", got)
	}
	if l.ServerFilter == nil || l.ServerFilter.Value != "name: orders AND NOT state: PAUSED" || l.ServerFilter.Help == "" {
		t.Errorf("serverFilter = %+v", l.ServerFilter)
	}
	l, err = p.ListFiltered(ctx, "demo-project", "state: SLEEPING")
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Items) != 0 || l.ServerFilter == nil || !strings.Contains(l.ServerFilter.Error, "SLEEPING") {
		t.Errorf("a refused filter = %d rows, %+v", len(l.Items), l.ServerFilter)
	}
	l, _ = p.List(ctx, "demo-project")
	if len(l.Items) != 3 || l.ServerFilter == nil {
		t.Errorf("unfiltered list = %d rows, filter box %v", len(l.Items), l.ServerFilter)
	}
}
