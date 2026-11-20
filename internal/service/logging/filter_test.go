package logging

import (
	"testing"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/genproto/googleapis/cloud/audit"
	ltype "google.golang.org/genproto/googleapis/logging/type"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestFilterOverLogEntries (#2026): the query language over real LogEntry
// messages, the fields read straight from the message and those read
// through its JSON form (httpRequest, operation, protoPayload) alike.
func TestFilterOverLogEntries(t *testing.T) {
	at := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	payload, _ := structpb.NewStruct(map[string]any{"event": "boot", "ms": 900, "user": map[string]any{"id": "u1"}})
	audited, _ := anypb.New(&audit.AuditLog{MethodName: "v1.compute.instances.insert", ServiceName: "compute.googleapis.com"})
	json := &loggingpb.LogEntry{LogName: "projects/p1/logs/app", InsertId: "i1", Severity: ltype.LogSeverity_WARNING,
		Timestamp: timestamppb.New(at), Resource: &monitoredres.MonitoredResource{Type: "global", Labels: map[string]string{"project_id": "p1"}},
		Labels:      map[string]string{"env": "prod"},
		HttpRequest: &ltype.HttpRequest{RequestMethod: "POST", Status: 503, RemoteIp: "10.0.0.7", Latency: durationpb.New(2 * time.Second)},
		Operation:   &loggingpb.LogEntryOperation{Id: "op-1", First: true},
		Payload:     &loggingpb.LogEntry_JsonPayload{JsonPayload: payload}}
	audit := &loggingpb.LogEntry{LogName: "projects/p1/logs/cloudaudit.googleapis.com%2Factivity", InsertId: "i2",
		Severity: ltype.LogSeverity_NOTICE, Timestamp: timestamppb.New(at.Add(time.Hour)),
		Resource: &monitoredres.MonitoredResource{Type: "gce_instance"},
		Payload:  &loggingpb.LogEntry_ProtoPayload{ProtoPayload: audited}}
	text := &loggingpb.LogEntry{LogName: "projects/proj-2/logs/stdout", InsertId: "i3", Timestamp: timestamppb.New(at.Add(2 * time.Hour)),
		Payload: &loggingpb.LogEntry_TextPayload{TextPayload: "listening on 8080"}}
	for filter, want := range map[string]string{
		`logName:"/logs/app" OR logName:"/logs/stdout"`:                          "i1,i3",
		`NOT severity >= WARNING`:                                                "i2,i3",
		`severity = DEFAULT`:                                                     "i3",
		`jsonPayload.event = boot AND jsonPayload.ms > 500`:                      "i1",
		`jsonPayload.user.id = U1`:                                               "i1",
		`httpRequest.status >= 500 AND httpRequest.latency > "1s"`:               "i1",
		`ip_in_net(httpRequest.remoteIp, "10.0.0.0/24")`:                         "i1",
		`operation.first = true`:                                                 "i1",
		`operation.id:*`:                                                         "i1",
		`protoPayload.@type = "type.googleapis.com/google.cloud.audit.AuditLog"`: "i2",
		`protoPayload.methodName:"instances.insert"`:                             "i2",
		`log_id("cloudaudit.googleapis.com/activity")`:                           "i2",
		`textPayload:8080`:                                                       "i3",
		`"8080" OR "compute.googleapis.com"`:                                     "i2,i3",
		`SEARCH("listening")`:                                                    "i3",
		`labels.env = prod OR resource.type = (gce_instance OR k8s_node)`:        "i1,i2",
		`timestamp >= "2026-11-20T10:30:00Z" timestamp < "2026-11-20T12:00:00Z"`: "i2",
		`source("projects/proj-2")`:                                              "i3",
		`-resource.type = global`:                                                "i2,i3",
	} {
		match, err := compile(filter)
		if err != nil {
			t.Errorf("%q refused: %v", filter, err)
			continue
		}
		got := ""
		for _, e := range []*loggingpb.LogEntry{json, audit, text} {
			if match(e) {
				if got != "" {
					got += ","
				}
				got += e.GetInsertId()
			}
		}
		if got != want {
			t.Errorf("%q = [%s], want [%s]", filter, got, want)
		}
	}
}
