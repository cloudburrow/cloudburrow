package logging

// A sink's custom writer identity (#1879). sinks.create, sinks.update and
// sinks.patch take the query parameter customWriterIdentity: "The service
// account provided by the caller that will be used to write the log
// entries. The format must be serviceAccount:some@email. This field can
// only be specified when you are routing logs to a log bucket that is in a
// different project than the sink. When not specified, a Logging service
// account will automatically be generated." (the v2 REST reference,
// https://docs.cloud.google.com/logging/docs/reference/v2/rest/v2/projects.sinks/create,
// read 2026-11-20). cloud.google.com/go/logging v1.20.0's CreateSinkRequest
// and UpdateSinkRequest have no such field, so it is served over JSON
// alone: serveSinkJSON takes it out of the query (the transcoder refuses
// unknown parameters) and hands it to CreateSink and UpdateSink here. The
// sink keeps it as its writer_identity until an update names another.
// With up --iam-enforcement=on the caller needs iam.serviceAccounts.actAs
// on the account ("Configure log sinks with user-managed service
// accounts", https://docs.cloud.google.com/logging/docs/routing/user-managed-service-accounts),
// which internal/iameval checks before the request reaches the server.

import (
	"context"
	"net/mail"
	"strings"

	"cloud.google.com/go/logging/apiv2/loggingpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// CustomWriterParam is the query parameter naming a sink's custom writer
// identity.
const CustomWriterParam = "customWriterIdentity"

type writerCtxKey struct{}

// requestWriter is the custom writer identity a JSON request named, "" for
// none.
func requestWriter(ctx context.Context) string {
	s, _ := ctx.Value(writerCtxKey{}).(string)
	return s
}

// checkCustomWriter is a custom writer identity's documented restrictions:
// "serviceAccount:EMAIL", and a sink that routes to a log bucket in
// another project than its own (any project, for a sink of a folder, the
// organization or a billing account). Google's wording of either refusal
// is UNVERIFIED (docs re-read 2026-11-20:
// https://docs.cloud.google.com/logging/docs/reference/v2/rest/v2/projects.sinks/create
// gives none).
func checkCustomWriter(p parent, sk *loggingpb.LogSink, writer string) error {
	email, ok := strings.CutPrefix(writer, "serviceAccount:")
	if !ok || email == "" {
		return apierror.InvalidArgument("customWriterIdentity %q must be serviceAccount:{email}", writer)
	}
	if _, err := mail.ParseAddress(email); err != nil || !strings.HasSuffix(strings.ToLower(email), ".gserviceaccount.com") {
		return apierror.InvalidArgument("customWriterIdentity %q does not name a service account", writer)
	}
	parts := strings.Split(sk.GetDestination(), "/")
	if len(parts) != 7 || parts[0] != "logging.googleapis.com" || parts[1] != "projects" || parts[3] != "locations" || parts[5] != "buckets" {
		return apierror.InvalidArgument("customWriterIdentity can only be specified when the sink routes logs to a log bucket, not %q", sk.GetDestination())
	}
	if p.isProject() && parts[2] == p.id {
		return apierror.InvalidArgument("customWriterIdentity can only be specified when the log bucket %s is in a different project than the sink", sk.GetDestination())
	}
	return nil
}

// customWriter is the writer identity sk keeps from an earlier request's
// customWriterIdentity, "" when it has Logging's own (or none).
func customWriter(p parent, sk *loggingpb.LogSink) string {
	w := sk.GetWriterIdentity()
	if w == "" || w == "serviceAccount:"+serviceAccount(p) {
		return ""
	}
	return w
}

// sinkBucketDestination is whether a sink of p routing to d may keep a
// custom writer identity: d is a log bucket in another project. An update
// that routes elsewhere drops the one the sink kept.
func sinkBucketDestination(p parent, d string) bool {
	return checkCustomWriter(p, &loggingpb.LogSink{Destination: d}, "serviceAccount:x@x.iam.gserviceaccount.com") == nil
}
