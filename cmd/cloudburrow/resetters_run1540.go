package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"github.com/cloudburrow/cloudburrow/internal/netfwd"
)

// Per-project resets for BigQuery and Memorystore (#1540), so a project's
// purge, and `/admin/reset?project=`, drop what Google drops with a deleted
// project. Cloud Run's is runVolumesResetter's.

// bigqueryResetter deletes a project's datasets, with their tables, views,
// models and routines, through the BigQuery front, marked as a purge's
// (bigqueryPurgeHeader), so the front keeps nothing of them for
// undelete and forgets what it kept of the project's deleted tables.
//
// The emulator serves one project, the instance's own (docs/bigquery.md):
// another project has no datasets, and its reset has nothing to do. A full
// reset resets every project CloudBurrow knows of.
type bigqueryResetter struct {
	tunnel   func() *netfwd.Forwarder
	projects func() []string
}

func (b *bigqueryResetter) Name() string { return "bigquery" }

func (b *bigqueryResetter) Reset(ctx context.Context) error {
	var errs []error
	for _, p := range b.projects() {
		if err := b.ResetProject(ctx, p); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
		}
	}
	return errors.Join(errs...)
}

func (b *bigqueryResetter) ResetProject(ctx context.Context, project string) error {
	f := b.tunnel()
	if f == nil || f.HostAddr() == "" {
		return errors.New("the BigQuery tunnel is not running")
	}
	// The linked writer's header (loggingsinks.go), so a Log Analytics
	// linked dataset, which the front keeps read-only, can go too; and the
	// purge's, so the front keeps nothing of a dataset for undelete.
	hc := &http.Client{Transport: purgeTransport{next: linkedTransport{next: http.DefaultTransport}}, Timeout: 2 * time.Minute}
	c, err := bigquery.NewClient(ctx, project, option.WithEndpoint("http://"+f.HostAddr()), option.WithHTTPClient(hc))
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	it := c.Datasets(ctx)
	it.ListHidden = true
	var errs []error
	for {
		ds, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if isHTTPStatus(err, http.StatusNotFound) {
			return nil // a project the emulator does not serve has none
		}
		if err != nil {
			return fmt.Errorf("list datasets: %w", err)
		}
		if err := ds.DeleteWithContents(ctx); err != nil && !isHTTPStatus(err, http.StatusNotFound) {
			errs = append(errs, fmt.Errorf("delete dataset %s: %w", ds.DatasetID, err))
		}
	}
	// The copies the front kept of tables deleted before, for undelete.
	if err := c.Dataset(bigqueryDroppedDataset).DeleteWithContents(ctx); err != nil && !isHTTPStatus(err, http.StatusNotFound) {
		errs = append(errs, fmt.Errorf("delete the front's deleted tables: %w", err))
	}
	return errors.Join(errs...)
}

// bigqueryPurgeHeader is bigqueryfront.PurgeHeader and
// bigqueryDroppedDataset bigqueryfront.DroppedDatasetID: the front runs in
// the BigQuery pod, and is not linked into the CLI.
const (
	bigqueryPurgeHeader    = "X-Cloudburrow-Purge"
	bigqueryDroppedDataset = "_cloudburrow_dropped"
)

// purgeTransport marks each request as a project's purge's.
type purgeTransport struct{ next http.RoundTripper }

func (t purgeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(bigqueryPurgeHeader, "1")
	return t.next.RoundTrip(r)
}

func isHTTPStatus(err error, code int) bool {
	var ge *googleapi.Error
	return errors.As(err, &ge) && ge.Code == code
}

// memorystoreResetter deletes the Memorystore instances CreateInstance made
// in a project, with their Redis servers and kept records. The instance
// `cloudburrow up` runs is the instance's own and stays. A full reset
// resets every project CloudBurrow knows of.
type memorystoreResetter struct {
	svc      *memorystoreAdminService
	projects func() []string
}

func (m *memorystoreResetter) Name() string { return "memorystore" }

func (m *memorystoreResetter) Reset(ctx context.Context) error {
	var errs []error
	for _, p := range m.projects() {
		if err := m.ResetProject(ctx, p); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p, err))
		}
	}
	return errors.Join(errs...)
}

func (m *memorystoreResetter) ResetProject(ctx context.Context, project string) error {
	if m.svc == nil || m.svc.api == nil {
		return nil
	}
	return m.svc.api.ResetProject(ctx, project)
}
