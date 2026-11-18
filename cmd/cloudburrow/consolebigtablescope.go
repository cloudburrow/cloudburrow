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
	return newConsoleBigtableClient(ctx, p.endpoint, project, instance)
}

// consoleBigtableConfig is the configuration of every data client the
// console makes (#1820).
//
// The console makes a client for each request and closes it when the
// request is done, so each of its point reads and writes would be the first
// call on a cold session pool: the front's configuration sends every
// ReadRow and Apply through a session (sessions.go). The Go client's
// session pool (cloud.google.com/go/bigtable v1.58.0,
// internal/transport/session_pool.go, CheckoutSession) can lose the wake-up
// of a caller that finds no ready session: when the pool's first session
// turns active after the caller looked for one but before it queued as a
// waiter, the one signal finds no waiter, and the caller waits out its
// whole deadline beside an idle session ("no sessions available: context
// deadline exceeded"). Under the race detector and load about one cold read
// in a thousand did. The console's calls are one-offs, so it uses the
// classic data plane, which is what the front runs each session vRPC as.
//
// Its clients also export no client-side metrics: the built-in exporter
// drops the endpoint option and would send them to Cloud Monitoring. And
// they do not try Direct Access, which is on by default: the channel pool
// probes it with a dial of its own, and when that dial is not ALTS (it never
// is here) investigateDirectAccessFailure asks metadata.OnGCE, an HTTP
// request to 169.254.169.254 and a DNS lookup of metadata.google.internal
// (internal/transport/direct_access_checker.go, #1825).
var consoleBigtableConfig = bigtable.ClientConfig{
	DisableSession:      true,
	DisableDirectAccess: true,
	MetricsProvider:     bigtable.NoopMetricsProvider{},
}

// newConsoleBigtableClient is a data client on instance at endpoint, made
// with consoleBigtableConfig.
func newConsoleBigtableClient(ctx context.Context, endpoint, project, instance string) (*bigtable.Client, error) {
	return bigtable.NewClientWithConfig(ctx, project, instance, consoleBigtableConfig, localOpts(endpoint)...)
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
