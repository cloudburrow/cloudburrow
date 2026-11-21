package logging

// The _Required bucket and sink (#2108), as Google documents them
// (docs.cloud.google.com/logging/docs/routing/overview and
// /logging/docs/store-log-entries, read 2026-11-21):
//
//   - every project, folder, organization and billing account has a
//     _Required sink whose filter is the six LOG_IDs below (Admin Activity,
//     System Event and Access Transparency audit logs, Cloud's and external
//     ones), routing to its own global _Required bucket. "You can't modify or
//     delete the _Required log sink", so UpdateSink and DeleteSink of it are
//     refused, and CreateSink cannot take its ID;
//   - "The _Required log sink only matches log entries that originate in the
//     resource where the _Required log sink is defined": the bucket holds its
//     parent's own audit entries, never what another resource's sink routes
//     to it;
//   - "This sink doesn't specify any exclusion filters", and "the logs in
//     that bucket can't be excluded or disabled": a project exclusion never
//     keeps an entry out of _Required;
//   - an intercepting sink above the entry leaves it in _Required: "the log
//     entry is always sent to the _Required log sink in the resource where
//     the log entry originates";
//   - the _Default sink's exclusion is the same six LOG_IDs, so those entries
//     are not in _Default;
//   - "Log entries in this log bucket are retained for 400 days; you can't
//     change this retention period", and "you can't delete this log bucket"
//     (buckets.go).
//
// Reading a parent (resource_names projects/P) reads both of its buckets,
// _Default and _Required, as gcloud logging read of an audit log does.
//
// Google documents no error text or code for a change to the _Required
// sink or bucket: FAILED_PRECONDITION is this package's code for every
// undocumented refusal, and the wording is ours (UNVERIFIED (docs re-read
// 2026-11-21: https://docs.cloud.google.com/logging/docs/routing/overview)).

import (
	"net/url"
	"strings"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// sinkRequired is the _Required sink's ID.
const sinkRequired = "_Required"

// requiredLogIDs are the logs the _Required sink routes, in the order of
// Google's filter.
var requiredLogIDs = []string{
	"cloudaudit.googleapis.com/activity",
	"externalaudit.googleapis.com/activity",
	"cloudaudit.googleapis.com/system_event",
	"externalaudit.googleapis.com/system_event",
	"cloudaudit.googleapis.com/access_transparency",
	"externalaudit.googleapis.com/access_transparency",
}

// RequiredSinkFilter is the _Required sink's inclusion filter, Google's.
var RequiredSinkFilter = func() string {
	terms := make([]string, len(requiredLogIDs))
	for i, id := range requiredLogIDs {
		terms[i] = `LOG_ID("` + id + `")`
	}
	return strings.Join(terms, " OR ")
}()

// isRequiredLog is whether the _Required sink matches e: its log ID is one
// of requiredLogIDs.
func isRequiredLog(e *loggingpb.LogEntry) bool {
	_, enc, ok := strings.Cut(e.GetLogName(), "/logs/")
	if !ok {
		return false
	}
	if id, err := url.PathUnescape(enc); err == nil {
		enc = id
	}
	for _, id := range requiredLogIDs {
		if enc == id {
			return true
		}
	}
	return false
}

// ownBucket is the global built-in bucket of its own parent that holds e:
// _Required for the audit logs its sink matches, else _Default.
func ownBucket(e *loggingpb.LogEntry) bucketRef {
	if isRequiredLog(e) {
		return bucketRef{entryParent(e), "global", bucketRequired}
	}
	return bucketRef{entryParent(e), "global", bucketDefault}
}

// holdsOwn is whether ref is the built-in bucket of e's own parent that
// holds it: _Required always (no exclusion or intercepting sink keeps an
// entry out of it), _Default unless hidden (an exclusion or an intercepting
// sink kept it out).
func holdsOwn(ref bucketRef, e *loggingpb.LogEntry, hidden bool) bool {
	if ref.location != "global" || entryParent(e) != ref.parent {
		return false
	}
	switch ref.bucket {
	case bucketRequired:
		return isRequiredLog(e)
	case bucketDefault:
		return !hidden && !isRequiredLog(e)
	}
	return false
}

// requiredSink is p's _Required sink.
func requiredSink(p parent) *loggingpb.LogSink {
	epoch := timestamppb.New(time.Unix(0, 0))
	return &loggingpb.LogSink{
		Name:        sinkRequired,
		Destination: "logging.googleapis.com/" + (bucketRef{p, "global", bucketRequired}).name(),
		Filter:      RequiredSinkFilter,
		CreateTime:  epoch,
		UpdateTime:  epoch,
	}
}

// refuseRequiredSink refuses a change to the _Required sink.
func refuseRequiredSink(p parent, verb string) error {
	return apierror.FailedPrecondition("sink %s cannot be %s: the _Required sink can't be modified or deleted", kindSink.name(p, sinkRequired), verb)
}

// refuseRequiredBucket refuses an update of the _Required bucket: its
// retention can't be changed, and nothing else about it can be modified.
func refuseRequiredBucket(ref bucketRef, paths []string) error {
	for _, p := range paths {
		if p == "retention_days" || p == "retentionDays" {
			return apierror.FailedPrecondition("bucket %s: the retention period of the _Required bucket is %d days and can't be changed", ref.name(), requiredRetention)
		}
	}
	return apierror.FailedPrecondition("bucket %s cannot be modified: the _Required bucket can't be changed", ref.name())
}
