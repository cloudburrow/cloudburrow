package logging

// Log Analytics over a bucket with no link (#2021). Google's Log
// Analytics page queries a log bucket upgraded to use Log Analytics
// directly: "You don't need a linked BigQuery dataset when you want to
// query your log data" (docs.cloud.google.com/logging/docs/analyze/
// query-and-view, read 2026-11-20); a link is for BigQuery Studio, joins
// with other datasets and the like. Google documents no API for the page's
// queries.
//
// Here such a query runs in the instance's BigQuery over a query dataset:
// AnalyticsDataset makes, for one bucket, a dataset as a link makes
// (analytics.go: the bucket's entries in Log Analytics' schema, inside its
// retention, and a view per log view), named with a leading underscore,
// which BigQuery's console treats as a hidden dataset, marked read-only
// with LinkedDatasetLabel, and deleted by the drop it returns once the
// query has run. It is never a link: ListLinks does not name it, and no
// write after it is made reaches it.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// AnalyticsDatasetPrefix begins every query dataset's ID.
const AnalyticsDatasetPrefix = "_cloudburrow_log_analytics_"

// AnalyticsDataset makes a query dataset of bucket (projects/P/locations/
// L/buckets/B), which must be upgraded to use Log Analytics, in project
// (the bucket's own, or, when BigQuery does not serve it, fallback). It
// returns the project and dataset it made, and drop, which deletes it.
func (a *Admin) AnalyticsDataset(ctx context.Context, bucket, fallback string) (project, dataset string, drop func(), err error) {
	ref, err := parseBucket("bucket", bucket)
	if err != nil {
		return "", "", nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.linked == nil {
		return "", "", nil, apierror.FailedPrecondition("Log Analytics runs its queries in BigQuery, which is not served on this instance: start it with bigquery among --services")
	}
	b, err := a.getBucket(ref)
	if err != nil {
		return "", "", nil, err
	}
	if !b.GetAnalyticsEnabled() {
		return "", "", nil, apierror.FailedPrecondition("log bucket %s is not upgraded to use Log Analytics: upgrade it in Logs Storage first", ref.bucket)
	}
	views, err := a.bucketViews(ref)
	if err != nil {
		return "", "", nil, err
	}
	days := a.retentionDays(ref)
	var suffix [6]byte
	_, _ = rand.Read(suffix[:])
	dataset = AnalyticsDatasetPrefix + hex.EncodeToString(suffix[:])
	skew, _ := a.clockSkew(ctx, a.linked)
	a.copies.mu.Lock()
	entriesOf := a.copies.entries
	a.copies.mu.Unlock()
	project = ref.parent.id
	desc := "Log Analytics query dataset of the log bucket " + ref.name() + "; read-only, deleted after the query"
	create := func(p string) error {
		return a.linked.Create(ctx, p, dataset, desc, linkedSchema(), viewQueries(p, dataset, views, days, skew))
	}
	err = create(project)
	if err != nil && fallback != "" && fallback != project && isNotServed(err) {
		project = fallback
		err = create(project)
	}
	if err != nil {
		return "", "", nil, linkError(project, dataset, err)
	}
	l, p := a.linked, project
	drop = func() { _ = l.Delete(context.WithoutCancel(ctx), p, dataset) }
	if entriesOf != nil {
		es, err := entriesOf(ref)
		if err != nil {
			drop()
			return "", "", nil, err
		}
		rows, ids := linkedRows(es, views, a.logNow(), days)
		if err := appendRows(ctx, a.linked, project, dataset, rows, ids); err != nil {
			drop()
			return "", "", nil, linkError(project, dataset, err)
		}
	}
	return project, dataset, drop, nil
}

func isNotServed(err error) bool { return errors.Is(err, ErrProjectNotServed) }
