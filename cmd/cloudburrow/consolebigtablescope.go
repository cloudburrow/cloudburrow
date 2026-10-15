package main

import (
	"context"
	"fmt"
	"net/url"
	"sort"

	"cloud.google.com/go/bigtable"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The Bigtable screen's instance picker (#1360).
//
// The screen read its tables from bigtableInstance alone, so a table made in
// an instance created through the instance admin API (cbt, the SDK,
// Terraform's google_bigtable_table, or the instances screen) could not be
// browsed. The instance now travels in the URL as ?instance=, the way the
// project does, and every read and change the screen makes addresses it:
// tables, rows, column families and each table and cell action.

// bigtableScopeKey is the query parameter the chosen instance travels in.
const bigtableScopeKey = "instance"

// ScopeKey implements console.Scoper.
func (bigtableProvider) ScopeKey() string { return bigtableScopeKey }

// bigtableInstanceIn is the instance ctx's request names, or bigtableInstance
// when it names none.
func bigtableInstanceIn(ctx context.Context) string {
	if v := console.ScopeValue(ctx, bigtableScopeKey); v != "" {
		return v
	}
	return bigtableInstance
}

// bigtableTablesHref is the Bigtable screen's address for an instance's
// tables, or for one table when table is not empty.
func bigtableTablesHref(project, instance, table string) string {
	path := "/bigtable"
	if table != "" {
		path += "/" + url.PathEscape(table)
	}
	q := url.Values{}
	if project != "" {
		q.Set("project", project)
	}
	if instance != "" && instance != bigtableInstance {
		q.Set(bigtableScopeKey, instance)
	}
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}

// instanceOptions are the instances the picker offers: bigtableInstance,
// which the emulator serves without it being created, then the instances
// the admin API lists, by ID.
func (p bigtableProvider) instanceOptions(ctx context.Context, project string) ([]console.ScopeOption, error) {
	opts := []console.ScopeOption{{ID: bigtableInstance, Label: bigtableInstance + " (default)"}}
	iac, err := bigtable.NewInstanceAdminClient(ctx, project, localOpts(p.endpoint)...)
	if err != nil {
		return opts, fmt.Errorf("cannot list instances: %w", err)
	}
	defer iac.Close()
	list, err := iac.Instances(ctx)
	if err != nil {
		return opts, fmt.Errorf("listing instances: %w", err)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	for _, in := range list {
		if in.Name == bigtableInstance {
			opts[0].Label = in.Name
			if in.DisplayName != "" && in.DisplayName != in.Name {
				opts[0].Label += " (" + in.DisplayName + ")"
			}
			continue
		}
		label := in.Name
		if in.DisplayName != "" && in.DisplayName != in.Name {
			label += " (" + in.DisplayName + ")"
		}
		opts = append(opts, console.ScopeOption{ID: in.Name, Label: label})
	}
	return opts, nil
}

// instance is the instance ctx's request addresses, refused when it is
// neither bigtableInstance nor one the admin API lists. The emulator serves
// any instance name, so an instance mistyped in the address, or deleted,
// would otherwise show an empty instance and take a table created in it.
func (p bigtableProvider) instance(ctx context.Context, project string) (string, error) {
	name := bigtableInstanceIn(ctx)
	if name == bigtableInstance {
		return name, nil
	}
	opts, err := p.instanceOptions(ctx, project)
	if err != nil {
		return "", err
	}
	for _, o := range opts {
		if o.ID == name {
			return name, nil
		}
	}
	return "", fmt.Errorf("no Bigtable instance %q in project %q: create it on the Bigtable instances screen, or choose another", name, project)
}

// newAdmin is a table admin client on the instance ctx's request addresses.
func (p bigtableProvider) newAdmin(ctx context.Context, project string) (*bigtable.AdminClient, error) {
	instance, err := p.instance(ctx, project)
	if err != nil {
		return nil, err
	}
	return bigtable.NewAdminClient(ctx, project, instance, localOpts(p.endpoint)...)
}

// newData is a data client on the instance ctx's request addresses.
func (p bigtableProvider) newData(ctx context.Context, project string) (*bigtable.Client, error) {
	instance, err := p.instance(ctx, project)
	if err != nil {
		return nil, err
	}
	return bigtable.NewClient(ctx, project, instance, localOpts(p.endpoint)...)
}

// scope is the list screen's picker, with the instance in force selected.
// The instance is offered even when the listing failed, so the picker can
// always lead back to one that works.
func (p bigtableProvider) scope(ctx context.Context, project string) *console.Scope {
	opts, _ := p.instanceOptions(ctx, project)
	selected := bigtableInstanceIn(ctx)
	found := false
	for _, o := range opts {
		found = found || o.ID == selected
	}
	if !found {
		opts = append(opts, console.ScopeOption{ID: selected, Label: selected + " (not found)"})
	}
	return &console.Scope{Key: bigtableScopeKey, Label: "Instance", Selected: selected,
		Default: bigtableInstance, Options: opts}
}

var _ console.Scoper = bigtableProvider{}
