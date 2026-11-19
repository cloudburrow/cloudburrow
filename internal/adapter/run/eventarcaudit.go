package run

// Cloud Audit Logs events (#1877): google.cloud.audit.log.v1.written, for
// the calls this instance's services serve. Google writes an audit log
// entry for each call a service records, and an Eventarc trigger whose
// serviceName and methodName (and optionally resourceName, exactly or by
// path pattern) match it receives the entry as a CloudEvent, over a Pub/Sub
// topic Eventarc makes. Here the CLI hands each call its front recorded
// (/admin/events, the console's Request Log) to EmitAuditLog, which builds
// the entry Google's LogEntryData documents, with its AuditLog payload, and
// publishes it to each matching trigger's topic; the trigger's push
// subscription then delivers it as every other trigger's events are.
//
// Like Google's defaults, only Admin Activity entries are written: Data
// Access audit logs are off unless a project enables them, so reads (Get,
// List, ...) and data writes (Publish, Commit, Encrypt, ...) write no entry
// and fire no trigger. The read/write split is by method name
// (UNVERIFIED, re-read 2026-11-19: Google classifies each method in its service's
// audit logging page; the prefixes below follow those pages' pattern).

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// auditLogWritten is the audit-log event type.
const auditLogWritten = "google.cloud.audit.log.v1.written"

// EventPublisher publishes a message to a topic; the CLI's EventTransport
// implements it, and events Eventarc sources itself (audit logs, Firestore)
// are published through it to their trigger's topic.
type EventPublisher interface {
	Publish(ctx context.Context, topic string, data []byte, attributes map[string]string) error
}

// AuditCall is one call a service served, as its front recorded it.
type AuditCall struct {
	// Project is the project the call named.
	Project string
	// ServiceName is the API's host, secretmanager.googleapis.com.
	ServiceName string
	// MethodName is the full method, google.cloud.secretmanager.v1.SecretManagerService.CreateSecret.
	MethodName string
	// ResourceName is the resource the call named.
	ResourceName string
	// Code is the call's google.rpc.Code, and Message its error.
	Code    int32
	Message string
	Time    time.Time
}

// auditServiceNames maps a gRPC package to its API's service name, for the
// services this instance serves over gRPC.
var auditServiceNames = map[string]string{
	"google.cloud.secretmanager.v1":       "secretmanager.googleapis.com",
	"google.cloud.kms.v1":                 "cloudkms.googleapis.com",
	"google.cloud.tasks.v2":               "cloudtasks.googleapis.com",
	"google.cloud.tasks.v2beta3":          "cloudtasks.googleapis.com",
	"google.cloud.scheduler.v1":           "cloudscheduler.googleapis.com",
	"google.cloud.run.v2":                 "run.googleapis.com",
	"google.cloud.functions.v2":           "cloudfunctions.googleapis.com",
	"google.cloud.eventarc.v1":            "eventarc.googleapis.com",
	"google.devtools.cloudbuild.v1":       "cloudbuild.googleapis.com",
	"google.devtools.artifactregistry.v1": "artifactregistry.googleapis.com",
	"google.cloud.securesourcemanager.v1": "securesourcemanager.googleapis.com",
	"google.cloud.developerconnect.v1":    "developerconnect.googleapis.com",
	"google.logging.v2":                   "logging.googleapis.com",
	"google.pubsub.v1":                    "pubsub.googleapis.com",
	"google.firestore.v1":                 "firestore.googleapis.com",
	"google.firestore.admin.v1":           "firestore.googleapis.com",
	"google.datastore.v1":                 "datastore.googleapis.com",
	"google.bigtable.admin.v2":            "bigtableadmin.googleapis.com",
	"google.bigtable.v2":                  "bigtable.googleapis.com",
	"google.spanner.admin.instance.v1":    "spanner.googleapis.com",
	"google.spanner.admin.database.v1":    "spanner.googleapis.com",
	"google.spanner.v1":                   "spanner.googleapis.com",
	"google.cloud.redis.v1":               "redis.googleapis.com",
	"google.cloud.memorystore.v1":         "memorystore.googleapis.com",
	"google.cloud.resourcemanager.v3":     "cloudresourcemanager.googleapis.com",
	"google.iam.admin.v1":                 "iam.googleapis.com",
	"google.cloud.backupdr.v1":            "backupdr.googleapis.com",
}

// AuditServiceNames are the service names audit-log triggers can name,
// sorted, for ListProviders.
func AuditServiceNames() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range auditServiceNames {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// GRPCAuditCall is the audit call of a gRPC method, /pkg.Service/Method;
// false when the method's service is not one this instance's audit logs
// name.
func GRPCAuditCall(fullMethod string) (serviceName, methodName string, ok bool) {
	m := strings.TrimPrefix(fullMethod, "/")
	svc, method, ok := strings.Cut(m, "/")
	if !ok {
		return "", "", false
	}
	i := strings.LastIndex(svc, ".")
	if i < 0 {
		return "", "", false
	}
	name, ok := auditServiceNames[svc[:i]]
	if !ok {
		return "", "", false
	}
	return name, svc + "." + method, true
}

// dataAccessPrefixes are the methods whose entries are Data Access audit
// logs (DATA_READ or DATA_WRITE), which Google writes only when a project
// enables them.
var dataAccessPrefixes = []string{"Get", "List", "Lookup", "Search", "Read", "BatchGet", "Access", "Pull", "StreamingPull",
	"Acknowledge", "ModifyAck", "ModifyPushConfig", "TestIamPermissions", "Watch", "Listen", "RunQuery", "RunAggregationQuery",
	"Fetch", "Check", "Query", "Publish", "Commit", "Write", "BatchWrite", "BeginTransaction", "Rollback", "Mutate",
	"ReadModifyWrite", "ExecuteSql", "ExecuteStreamingSql", "ExecuteBatchDml", "PartitionQuery", "PartitionRead",
	"StreamingRead", "Encrypt", "Decrypt", "AsymmetricSign", "AsymmetricDecrypt", "MacSign", "MacVerify", "RawEncrypt",
	"RawDecrypt", "GenerateRandomBytes", "Tail", "Seek", "Wait", "Batch", "SampleRowKeys", "Ping"}

// isAdminActivity reports whether a method writes an Admin Activity entry.
func isAdminActivity(methodName string) bool {
	short := methodName[strings.LastIndex(methodName, ".")+1:]
	for _, p := range dataAccessPrefixes {
		if strings.HasPrefix(short, p) {
			return false
		}
	}
	return true
}

// auditEntry is the LogEntryData of a call, as Cloud Logging writes an
// Admin Activity entry: its AuditLog in protoPayload.
func auditEntry(c AuditCall, insertID string) map[string]any {
	ts := c.Time.UTC().Format(time.RFC3339Nano)
	status := map[string]any{}
	severity := "NOTICE"
	if c.Code != 0 {
		status = map[string]any{"code": c.Code, "message": c.Message}
		severity = "ERROR"
	}
	payload := map[string]any{
		"@type":        "type.googleapis.com/google.cloud.audit.AuditLog",
		"status":       status,
		"serviceName":  c.ServiceName,
		"methodName":   c.MethodName,
		"resourceName": c.ResourceName,
		// No principal: this instance authenticates no caller (ADR-0006).
		"authenticationInfo": map[string]any{},
		"requestMetadata":    map[string]any{"callerIp": "127.0.0.1", "callerSuppliedUserAgent": "cloudburrow"},
	}
	return map[string]any{
		"protoPayload": payload,
		"insertId":     insertID,
		"resource": map[string]any{"type": "audited_resource", "labels": map[string]string{
			"service": c.ServiceName, "method": c.MethodName, "project_id": c.Project}},
		"timestamp":        ts,
		"severity":         severity,
		"logName":          "projects/" + c.Project + "/logs/cloudaudit.googleapis.com%2Factivity",
		"receiveTimestamp": ts,
	}
}

// auditFilterValue is the attribute an audit-log trigger's filter reads.
func auditFilterValue(c AuditCall, attribute string) (string, bool) {
	switch attribute {
	case "serviceName":
		return c.ServiceName, true
	case "methodName":
		return c.MethodName, true
	case "resourceName":
		return c.ResourceName, true
	}
	return "", false
}

// EmitAuditLog writes the audit-log entry of one call to every trigger it
// matches, and reports how many.
func (e *EventarcServer) EmitAuditLog(ctx context.Context, c AuditCall) int {
	if c.Project == "" || c.ServiceName == "" || c.MethodName == "" || !isAdminActivity(c.MethodName) {
		return 0
	}
	if c.Time.IsZero() {
		c.Time = time.Now()
	}
	matched := eaMatching(c.Project, []string{auditLogWritten}, func(attr string) (string, bool) { return auditFilterValue(c, attr) })
	if len(matched) == 0 {
		return 0
	}
	insertID := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	entry := auditEntry(c, insertID)
	data, err := json.Marshal(entry)
	if err != nil {
		return 0
	}
	ts := c.Time.UTC().Format(time.RFC3339Nano)
	attrs := map[string]string{
		"ce-id":           fmt.Sprintf("%s%s%s", entry["logName"], insertID, ts),
		"ce-source":       "//cloudaudit.googleapis.com/projects/" + c.Project + "/logs/activity",
		"ce-subject":      c.ServiceName + "/" + strings.TrimPrefix(c.ResourceName, "/"),
		"ce-time":         ts,
		"ce-methodname":   c.MethodName,
		"ce-servicename":  c.ServiceName,
		"ce-resourcename": c.ResourceName,
		"ce-recordedtime": ts,
		"content-type":    "application/json; charset=utf-8",
	}
	return e.f.publishSourced(ctx, matched, data, attrs)
}

// publishSourced publishes one sourced event to each trigger's topic.
func (f *FunctionsServer) publishSourced(ctx context.Context, topics []string, data []byte, attrs map[string]string) int {
	pub, ok := f.publisher()
	if !ok {
		return 0
	}
	n := 0
	for _, topic := range topics {
		if err := pub.Publish(ctx, topic, data, attrs); err != nil {
			slog.Warn("eventarc: publish an event to its trigger's topic", "topic", topic, "type", attrs["ce-type"], "err", err)
			continue
		}
		n++
	}
	return n
}

func (f *FunctionsServer) publisher() (EventPublisher, bool) {
	if f.events == nil {
		return nil, false
	}
	p, ok := f.events.Transport.(EventPublisher)
	return p, ok
}
