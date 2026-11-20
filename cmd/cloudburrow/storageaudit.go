package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	auditpb "google.golang.org/genproto/googleapis/cloud/audit"
	ltype "google.golang.org/genproto/googleapis/logging/type"
	"google.golang.org/genproto/googleapis/rpc/context/attribute_context"
	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	gcsbuiltin "github.com/cloudburrow/cloudburrow/internal/service/storage"
	"github.com/cloudburrow/cloudburrow/internal/service/storagejobs"
)

// Cloud Storage's audit logs and request activity (#1979). The in-cluster
// server keeps one record per request (internal/service/storage/requestlog.go);
// the CLI reads them through the tunnel, as it reads the calls, and from
// each record:
//
//   - writes Cloud Storage's audit log entries to the instance's Cloud
//     Logging, as Google does (docs.cloud.google.com/storage/docs/audit-logging):
//     Admin Activity entries (cloudaudit.googleapis.com%2Factivity) always,
//     and Data Access entries (cloudaudit.googleapis.com%2Fdata_access) of
//     type ADMIN_READ, DATA_READ or DATA_WRITE only when the project's IAM
//     policy, its folders' or its organization's enable that type for
//     storage.googleapis.com or allServices; they are off by default. The
//     type is the permission each request is checked for, so a copy or a
//     compose writes two entries, the source's read and the destination's
//     write. No caller is authenticated (ADR-0006), so authenticationInfo
//     names no principal and no exemptedMembers can match;
//   - hands the records to the Storage Insights activity views
//     (internal/service/storagejobs/activity.go).

// storageLogType is the audit log type a permission's check writes:
// ADMIN_READ, DATA_READ, DATA_WRITE, or "" for Admin Activity.
func storageLogType(perm string) string {
	switch perm {
	case "storage.objects.get", "storage.objects.list", "storage.folders.get", "storage.folders.list",
		"storage.multipartUploads.list", "storage.multipartUploads.listParts", "storage.objects.viewFullContext":
		return "DATA_READ"
	case "storage.objects.create", "storage.objects.delete", "storage.objects.update", "storage.objects.restore",
		"storage.objects.move", "storage.folders.create", "storage.folders.delete", "storage.folders.rename",
		"storage.multipartUploads.create", "storage.multipartUploads.abort":
		return "DATA_WRITE"
	case "storage.buckets.get", "storage.buckets.list", "storage.buckets.getIamPolicy", "storage.objects.getIamPolicy",
		"storage.managedFolders.get", "storage.managedFolders.list", "storage.managedFolders.getIamPolicy",
		"storage.hmacKeys.get", "storage.hmacKeys.list", "storage.anywhereCaches.get", "storage.anywhereCaches.list",
		"resourcemanager.projects.get":
		return "ADMIN_READ"
	}
	return ""
}

// storageAudit turns the request records into audit log entries and
// activity.
type storageAudit struct {
	registry func() *resourcemanager.Registry
	write    func(ctx context.Context, entries []*loggingpb.LogEntry) error
	jobs     func() *storagejobs.Runtime
}

// auditTypes is the Data Access log types a project's effective policy
// enables for Cloud Storage: the union over the project, its folders and
// its organization, of storage.googleapis.com's and allServices' configs.
func (a *storageAudit) auditTypes(r *resourcemanager.Registry, project string) map[string]bool {
	out := map[string]bool{}
	if r == nil || project == "" {
		return out
	}
	resources := []string{"projects/" + project}
	if anc, err := r.Ancestry(project); err == nil {
		for _, x := range anc {
			switch x.Type {
			case "folder":
				resources = append(resources, "folders/"+x.ID)
			case "organization":
				resources = append(resources, "organizations/"+x.ID)
			}
		}
	}
	for _, res := range resources {
		p, err := r.IAMPolicy(res)
		if err != nil || p == nil {
			continue
		}
		for _, ac := range p.AuditConfigs {
			if ac.Service != "storage.googleapis.com" && ac.Service != "allServices" {
				continue
			}
			for _, l := range ac.LogConfigs {
				out[l.LogType] = true
			}
		}
	}
	return out
}

// handle writes the batch's audit entries and records its activity.
func (a *storageAudit) handle(ctx context.Context, reqs []gcsbuiltin.Request) {
	keep := reqs[:0:0]
	for _, r := range reqs {
		if r.UserAgent != storagejobs.SnapshotUserAgent {
			keep = append(keep, r)
		}
	}
	reqs = keep
	if len(reqs) == 0 {
		return
	}
	if a.jobs != nil {
		if rt := a.jobs(); rt != nil {
			act := make([]storagejobs.ActivityRequest, 0, len(reqs))
			for _, r := range reqs {
				act = append(act, storagejobs.ActivityRequest{Time: r.Time, Method: r.Method, Bucket: r.Bucket, Object: r.Object,
					ProjectNumber: r.ProjectNumber, Location: r.Location, BucketGeneration: r.BucketGeneration, Status: r.Status,
					DurationMicros: r.DurationMicros, RequestBytes: r.RequestBytes, ResponseBytes: r.ResponseBytes,
					RequestID: r.RequestID, Host: r.Host, HTTPMethod: r.HTTPMethod, Referrer: r.Referrer})
			}
			rt.RecordActivity(act)
		}
	}
	if a.write == nil {
		return
	}
	var reg *resourcemanager.Registry
	if a.registry != nil {
		reg = a.registry()
	}
	types := map[string]map[string]bool{}
	var entries []*loggingpb.LogEntry
	for _, r := range reqs {
		if r.Project == "" {
			continue
		}
		enabled, ok := types[r.Project]
		if !ok {
			enabled = a.auditTypes(reg, r.Project)
			types[r.Project] = enabled
		}
		for i, perm := range r.Permissions {
			lt := storageLogType(perm)
			if lt != "" && !enabled[lt] {
				continue
			}
			if e := storageAuditEntry(r, i, perm, lt); e != nil {
				entries = append(entries, e)
			}
		}
	}
	if len(entries) == 0 {
		return
	}
	if err := a.write(ctx, entries); err != nil {
		slog.Warn("Cloud Storage could not write its audit log entries", "entries", len(entries), "err", err)
	}
}

// storageAuditEntry is the entry of one permission check of a request: the
// i-th of its permissions, of log type lt ("" for Admin Activity).
func storageAuditEntry(r gcsbuiltin.Request, i int, perm, lt string) *loggingpb.LogEntry {
	bucket, object := r.Bucket, r.Object
	if i > 0 && r.DestBucket != "" {
		// A copy's or rewrite's second check is its destination's.
		bucket, object = r.DestBucket, r.DestObject
	}
	resource := "projects/_"
	switch {
	case bucket != "" && object != "":
		resource = "projects/_/buckets/" + bucket + "/objects/" + object
	case bucket != "":
		resource = "projects/_/buckets/" + bucket
	case strings.HasPrefix(perm, "storage.buckets."):
		resource = "projects/_/buckets"
	}
	st := &spb.Status{}
	if r.Status >= 400 {
		st = &spb.Status{Code: int32(httpToRPC(r.Status)), Message: http.StatusText(r.Status)}
	}
	md := &auditpb.RequestMetadata{CallerIp: r.CallerIP, CallerSuppliedUserAgent: r.UserAgent,
		RequestAttributes: &attribute_context.AttributeContext_Request{Time: timestamppb.New(r.Time),
			Auth: &attribute_context.AttributeContext_Auth{}},
		DestinationAttributes: &attribute_context.AttributeContext_Peer{}}
	payload := &auditpb.AuditLog{
		ServiceName:        "storage.googleapis.com",
		MethodName:         r.Method,
		ResourceName:       resource,
		Status:             st,
		AuthenticationInfo: &auditpb.AuthenticationInfo{},
		AuthorizationInfo:  []*auditpb.AuthorizationInfo{{Resource: resource, Permission: perm, Granted: r.Status != 401 && r.Status != 403}},
		RequestMetadata:    md,
	}
	if r.Location != "" {
		payload.ResourceLocation = &auditpb.ResourceLocation{CurrentLocations: []string{strings.ToLower(r.Location)}}
	}
	anyp, err := anypb.New(payload)
	if err != nil {
		return nil
	}
	logID := "cloudaudit.googleapis.com%2Factivity"
	sev := ltype.LogSeverity_NOTICE
	if lt != "" {
		logID, sev = "cloudaudit.googleapis.com%2Fdata_access", ltype.LogSeverity_INFO
	}
	if r.Status >= 400 {
		sev = ltype.LogSeverity_ERROR
	}
	labels := map[string]string{"project_id": r.Project}
	if bucket != "" {
		labels["bucket_name"] = bucket
	}
	if r.Location != "" {
		labels["location"] = strings.ToLower(r.Location)
	}
	return &loggingpb.LogEntry{
		LogName:   "projects/" + r.Project + "/logs/" + logID,
		Resource:  &monitoredres.MonitoredResource{Type: "gcs_bucket", Labels: labels},
		Timestamp: timestamppb.New(r.Time),
		Severity:  sev,
		InsertId:  r.RequestID + "-" + strconv.Itoa(i),
		Payload:   &loggingpb.LogEntry_ProtoPayload{ProtoPayload: anyp},
	}
}

// httpToRPC is the google.rpc.Code of an HTTP status, as Google maps them.
func httpToRPC(s int) int {
	switch s {
	case 400:
		return 3
	case 401:
		return 16
	case 403:
		return 7
	case 404:
		return 5
	case 409:
		return 10
	case 412:
		return 9
	case 416:
		return 11
	case 429:
		return 8
	case 499:
		return 1
	case 501:
		return 12
	case 503:
		return 14
	case 504:
		return 4
	}
	if s >= 500 {
		return 13
	}
	return 2
}

// storageRequestScraper reads the server's request records through the
// tunnel, every interval and on Flush (before a Storage Insights
// snapshot), and hands them to the audit log and activity.
type storageRequestScraper struct {
	tunnel *netfwd.Forwarder
	audit  *storageAudit

	mu    sync.Mutex
	after uint64
}

// Flush reads every record the server has not handed over yet.
func (s *storageRequestScraper) Flush(ctx context.Context) {
	if s == nil || s.tunnel == nil || s.tunnel.HostAddr() == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 0; i < 8; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+s.tunnel.HostAddr()+"/_cloudburrow/events?requests=1&after="+
			strconv.FormatUint(s.after, 10), nil)
		if err != nil {
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		var page struct {
			Requests []gcsbuiltin.Request `json:"requests"`
		}
		err = json.NewDecoder(resp.Body).Decode(&page)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || err != nil {
			return
		}
		if len(page.Requests) == 0 {
			if s.after > 0 {
				s.after = s.restarted(ctx, s.after)
			}
			return
		}
		if page.Requests[0].Seq <= s.after {
			// Never: the server answers only records after the cursor.
			return
		}
		s.after = page.Requests[len(page.Requests)-1].Seq
		s.audit.handle(ctx, page.Requests)
	}
}

// restarted returns 0 when the server's newest record is older than after
// (it restarted and numbers from 1 again), else after.
func (s *storageRequestScraper) restarted(ctx context.Context, after uint64) uint64 {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+s.tunnel.HostAddr()+"/_cloudburrow/events?requests=1&after=0", nil)
	if err != nil {
		return after
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return after
	}
	defer resp.Body.Close()
	var page struct {
		Requests []gcsbuiltin.Request `json:"requests"`
	}
	if json.NewDecoder(resp.Body).Decode(&page) != nil || len(page.Requests) == 0 {
		return after
	}
	if page.Requests[len(page.Requests)-1].Seq < after {
		return 0
	}
	return after
}
