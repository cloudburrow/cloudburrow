// Package logagent is Kubernetes Engine's node logging agent (#2100), the
// fluentbit-based agent GKE deploys on every node while a cluster's system
// or workload logs are on (re-read 2026-11-22:
// https://docs.cloud.google.com/kubernetes-engine/docs/concepts/about-logs).
//
// It runs in the cluster as the kube-system DaemonSet fluentbit-gke, one
// pod per node, built from cmd/cloudburrow-logging-agent and embedded in
// the CLI (internal/logagentimage). Each pod reads the container log files
// the container runtime writes under /var/log/pods on its node, labels
// each line with the pod's metadata from the Kubernetes API, and ships it
// to the instance's Cloud Logging with WriteLogEntries. Which logs are
// shipped is the fluentbit-gke-config ConfigMap the CLI writes from the
// cluster's logging configuration. How far each file has been shipped is
// kept in a position file on the node, so the agent, and the CLI, can stop
// and start without a line lost or shipped twice.
//
// It also holds what the CLI's own Cloud Run log feeder shares with it: the
// CRI log line format and the entry a line makes.
package logagent

import (
	"encoding/json"
	"net/url"
	"strings"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	ltype "google.golang.org/genproto/googleapis/logging/type"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ParseCRILine reads one line of a CRI container log file:
// "<RFC 3339 time> <stdout|stderr> <F|P> <text>", P marking a partial line
// the next one continues.
func ParseCRILine(l string) (ts time.Time, stream, text string, complete, ok bool) {
	parts := strings.SplitN(l, " ", 4)
	if len(parts) < 3 {
		return time.Time{}, "", "", false, false
	}
	t, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil || (parts[1] != "stdout" && parts[1] != "stderr") {
		return time.Time{}, "", "", false, false
	}
	if len(parts) == 4 {
		text = parts[3]
	}
	return t, parts[1], text, parts[2] != "P", true
}

// Entry is the entry of one line in log (URL-encoded into the log name) of
// project: a line that is a JSON object is a jsonPayload whose special
// fields are read as Google's structured logging documents them, any other
// a textPayload; severity DEFAULT unless the line names one.
func Entry(project, log string, res *monitoredres.MonitoredResource, labels map[string]string, ts time.Time, line string) *loggingpb.LogEntry {
	l := make(map[string]string, len(labels))
	for k, v := range labels {
		l[k] = v
	}
	e := &loggingpb.LogEntry{
		LogName:   "projects/" + project + "/logs/" + url.PathEscape(log),
		Resource:  res,
		Timestamp: timestamppb.New(ts),
		Labels:    l,
		Severity:  ltype.LogSeverity_DEFAULT,
	}
	var obj map[string]any
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "{") && json.Unmarshal([]byte(trimmed), &obj) == nil {
		structured(e, obj)
		if p, err := structpb.NewStruct(obj); err == nil {
			e.Payload = &loggingpb.LogEntry_JsonPayload{JsonPayload: p}
			return e
		}
	}
	e.Payload = &loggingpb.LogEntry_TextPayload{TextPayload: line}
	return e
}

// structured reads the special fields of Google's structured logging out of
// a JSON line into the entry, removing each it uses, as the logging agent
// does; message and every other field stay in the payload.
func structured(e *loggingpb.LogEntry, obj map[string]any) {
	if s, ok := obj["severity"].(string); ok {
		if v, ok := ltype.LogSeverity_value[strings.ToUpper(s)]; ok {
			e.Severity = ltype.LogSeverity(v)
			delete(obj, "severity")
		} else if strings.EqualFold(s, "WARN") {
			e.Severity = ltype.LogSeverity_WARNING
			delete(obj, "severity")
		}
	}
	if m, ok := obj["logging.googleapis.com/labels"].(map[string]any); ok {
		for k, v := range m {
			if s, ok := v.(string); ok {
				e.Labels[k] = s
			}
		}
		delete(obj, "logging.googleapis.com/labels")
	}
	if s, ok := obj["logging.googleapis.com/trace"].(string); ok {
		e.Trace = s
		delete(obj, "logging.googleapis.com/trace")
	}
	if s, ok := obj["logging.googleapis.com/spanId"].(string); ok {
		e.SpanId = s
		delete(obj, "logging.googleapis.com/spanId")
	}
	if b, ok := obj["logging.googleapis.com/trace_sampled"].(bool); ok {
		e.TraceSampled = b
		delete(obj, "logging.googleapis.com/trace_sampled")
	}
	if s, ok := obj["logging.googleapis.com/insertId"].(string); ok {
		e.InsertId = s
		delete(obj, "logging.googleapis.com/insertId")
	}
	for _, f := range []string{"time", "timestamp"} {
		if s, ok := obj[f].(string); ok {
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				e.Timestamp = timestamppb.New(t)
				delete(obj, f)
				break
			}
		}
	}
}
