package storage

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Cloud Build's default logs bucket (#1430). A build without logsBucket
// writes its log to gs://{project number}.cloudbuild-logs.googleusercontent.com,
// a bucket Google owns and creates, in a project of its own: its name is
// one no user may create (it contains "google", and is a domain name only
// Google can verify), it is not in the user's project's bucket list, and a
// user reads the build's log there but writes nothing.
//
// Here the bucket is kept beside the user's buckets, recorded under the
// build's project, and so:
//   - Cloud Build writes a log through CloudBuildLogsPath, which makes the
//     bucket on the first log and bypasses the reserved-name check; it is
//     the only way to create the bucket or write to it;
//   - every request through the APIs (JSON, upload, XML, Storage Control,
//     batch, and gRPC, which is served by the JSON handlers in process) that
//     would change the bucket or an object in it is 403 Forbidden, as a
//     write to a bucket the caller does not own is on Google; reads
//     (buckets.get, objects.list, objects.get, a download) are served;
//   - buckets.list leaves it out.

// CloudBuildLogsPath is where Cloud Build writes a build's log to its
// default logs bucket: POST ?project=&bucket=&name=, the body the log. It
// cannot collide with the XML API since no bucket name begins with "_".
const CloudBuildLogsPath = "/_cloudburrow/cloudbuild-logs"

var cloudBuildLogsBucketRE = regexp.MustCompile(`^[0-9]+\.cloudbuild-logs\.googleusercontent\.com$`)

// IsCloudBuildLogsBucket reports whether name is a Cloud Build default logs
// bucket, {project number}.cloudbuild-logs.googleusercontent.com.
func IsCloudBuildLogsBucket(name string) bool { return cloudBuildLogsBucketRE.MatchString(name) }

// serveCloudBuildLog writes one log object to a default logs bucket,
// making the bucket in project first if it does not exist.
func (s *Server) serveCloudBuildLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, errorf(http.StatusMethodNotAllowed, "methodNotAllowed", "POST a build log to %s", CloudBuildLogsPath))
		return
	}
	q := r.URL.Query()
	project, bucket, name := q.Get("project"), q.Get("bucket"), q.Get("name")
	switch {
	case project == "":
		writeError(w, required("project"))
		return
	case !IsCloudBuildLogsBucket(bucket):
		writeError(w, badRequest("%q is not a Cloud Build default logs bucket, {project number}.cloudbuild-logs.googleusercontent.com", bucket))
		return
	case name == "":
		writeError(w, required("name"))
		return
	}
	now := s.now()
	err := s.meta.Update(func(tx Tx) error {
		if _, exists, err := s.getBucket(tx, bucket); err != nil || exists {
			return err
		}
		b := bucketRecord{Name: bucket, Project: project, Created: now, Updated: now, Metageneration: 1,
			Generation: now.UnixMicro(), Fields: map[string]any{}}
		defaultBucketFields(b.Fields, now)
		return putBucket(tx, b)
	})
	if err != nil {
		writeError(w, err)
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, badRequest("the log could not be read"))
		return
	}
	// The JSON API's media upload, called in process past the guard.
	u := uploadPrefix + "b/" + url.PathEscape(bucket) + "/o?" + url.Values{"uploadType": {"media"}, "name": {name}}.Encode()
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, "http://localhost"+u, bytes.NewReader(data))
	if err != nil {
		writeError(w, badRequest("%v", err))
		return
	}
	req.RemoteAddr = r.RemoteAddr
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	s.objectsInsertUpload(w, req)
}

// cloudBuildLogsGuard refuses a request through the APIs that would change
// a default logs bucket or an object in it, and reports whether it did.
func (s *Server) cloudBuildLogsGuard(w http.ResponseWriter, r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	path := r.URL.EscapedPath()
	var bucket string
	xml := false
	switch {
	case strings.HasPrefix(path, controlPrefix):
		rest := strings.TrimPrefix(path, controlPrefix+"projects/_/buckets/")
		bucket, _, _ = strings.Cut(rest, "/")
	case strings.HasPrefix(path, jsonPrefix):
		bucket = jsonTargetBucket(strings.TrimPrefix(path, jsonPrefix))
	case strings.HasPrefix(path, resumablePrefix):
		bucket = jsonTargetBucket(strings.TrimPrefix(path, resumablePrefix))
	case strings.HasPrefix(path, uploadPrefix):
		bucket = jsonTargetBucket(strings.TrimPrefix(path, uploadPrefix))
	case strings.HasPrefix(path, adminPathStart), strings.HasPrefix(path, downloadPrefix), path == batchPath,
		strings.HasPrefix(path, batchPath+"/"):
		// Admin paths, downloads, and a batch, whose parts each come back
		// through route.
		return false
	default:
		bucket, _ = s.xmlTarget(r)
		xml = true
	}
	if b, err := url.PathUnescape(bucket); err == nil {
		bucket = b
	}
	if !IsCloudBuildLogsBucket(bucket) {
		return false
	}
	msg := "The caller does not have permission to change gs://" + bucket + ": it is Cloud Build's default logs bucket, " +
		"which Google owns; builds write their logs there, and you may only read them."
	if xml {
		writeXMLError(w, http.StatusForbidden, "AccessDenied", msg)
	} else {
		writeError(w, errorf(http.StatusForbidden, "forbidden", "%s", msg))
	}
	return true
}

// jsonTargetBucket is the bucket a JSON API path changes: b/{bucket}/...,
// or the destination of a copyTo or rewriteTo.
func jsonTargetBucket(rest string) string {
	segs := strings.Split(rest, "/")
	if len(segs) < 2 || segs[0] != "b" {
		return ""
	}
	bucket := segs[1]
	for i := 2; i+2 < len(segs); i++ {
		if (segs[i] == "copyTo" || segs[i] == "rewriteTo") && segs[i+1] == "b" {
			return segs[i+2]
		}
	}
	return bucket
}
