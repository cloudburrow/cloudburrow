package monitoring

import (
	"context"
	"strings"
	"time"

	"cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	"google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Uptime checks' Log check failures and disabled (#2146). Monitoring's
// REST reference (docs.cloud.google.com/monitoring/api/ref_v3/rest/v3/
// projects.uptimeCheckConfigs, read 2026-11-22) and its discovery document
// (google.golang.org/api/monitoring/v3, v0.314.0) publish two
// UptimeCheckConfig fields no monitoringpb has (cloud.google.com/go/
// monitoring through v1.32.1, googleapis master's uptime.proto of
// 2026-11-22):
//
//   - logCheckFailures: "To specify whether to log the results of failed
//     probes to Cloud Logging.";
//   - disabled: "Whether the check is disabled or not."
//
// They are served over REST through the transcoder's overlay
// (rest.Overlay), as Cloud Run's worker pool scaling is (#1788): the JSON
// API accepts, stores and returns them by those names; gRPC answers as the
// proto defines it, the fields riding as unknown fields under the
// overlay's own numbers. A check with logCheckFailures writes each failed
// run to Cloud Logging (SetUptimeLogger), as the console's "Log check
// failures" does: an entry on the uptime_url resource, labelled with the
// check's ID, whose payload is an UptimeCheckResult
// (docs.cloud.google.com/monitoring/uptime-checks/troubleshoot: query
// resource.type="uptime_url", "UptimeCheckResult" and labels.check_id).
// The log name and the payload's fields beyond those are not documented:
// UNVERIFIED (console-parity §1; the troubleshoot page, re-read 2026-11-22,
// names neither).

const (
	fieldLogCheckFailures = "log_check_failures"
	fieldUptimeDisabled   = "disabled"
	// The overlay's own numbers, far above Google's, never stored as
	// wire bytes (the state file holds overlay JSON).
	numLogCheckFailures = 1000001
	numUptimeDisabled   = 1000002
)

// UptimeLogID is the log uptime check failures are written to.
const UptimeLogID = "monitoring.googleapis.com/uptime_checks"

// monitoringOverlay is google.monitoring.v3 with the fields the discovery
// document publishes and monitoringpb lacks.
var monitoringOverlay = rest.MustOverlay("google.monitoring.v3", addUptimeFields)

// addUptimeFields adds them to UptimeCheckConfig; a field the proto
// already has (once Google publishes it) is left as published.
func addUptimeFields(f *descriptorpb.FileDescriptorProto) {
	for _, m := range f.GetMessageType() {
		if m.GetName() != "UptimeCheckConfig" {
			continue
		}
		has := map[string]bool{}
		for _, fd := range m.GetField() {
			has[fd.GetName()] = true
		}
		for _, a := range []struct {
			name, json string
			num        int32
		}{{fieldLogCheckFailures, "logCheckFailures", numLogCheckFailures}, {fieldUptimeDisabled, "disabled", numUptimeDisabled}} {
			if has[a.name] {
				continue
			}
			m.Field = append(m.Field, &descriptorpb.FieldDescriptorProto{Name: proto.String(a.name), JsonName: proto.String(a.json),
				Number: proto.Int32(a.num), Label: descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Type: descriptorpb.FieldDescriptorProto_TYPE_BOOL.Enum()})
		}
		return
	}
}

func overlayBool(c *monitoringpb.UptimeCheckConfig, field protoreflect.Name) bool {
	v, ok := monitoringOverlay.Get(c, field)
	return ok && v.Bool()
}

// LogCheckFailures is whether a check logs its failed runs.
func LogCheckFailures(c *monitoringpb.UptimeCheckConfig) bool {
	return overlayBool(c, fieldLogCheckFailures)
}

// UptimeDisabled is whether a check is disabled.
func UptimeDisabled(c *monitoringpb.UptimeCheckConfig) bool {
	return overlayBool(c, fieldUptimeDisabled)
}

// SetLogCheckFailures sets logCheckFailures on c (false clears it, as
// proto3 does a bool).
func SetLogCheckFailures(c *monitoringpb.UptimeCheckConfig, on bool) error {
	return setOverlayBool(c, fieldLogCheckFailures, on)
}

// SetUptimeDisabled sets disabled on c.
func SetUptimeDisabled(c *monitoringpb.UptimeCheckConfig, on bool) error {
	return setOverlayBool(c, fieldUptimeDisabled, on)
}

func setOverlayBool(c *monitoringpb.UptimeCheckConfig, field protoreflect.Name, on bool) error {
	v := protoreflect.Value{}
	if on {
		v = protoreflect.ValueOfBool(true)
	}
	return monitoringOverlay.Set(c, field, v)
}

// overlayMask splits an update mask into the overlay's fields, which it
// copies from src to dst, and the rest, which it returns.
func overlayMask(dst, src *monitoringpb.UptimeCheckConfig, mask *fieldmaskpb.FieldMask) (*fieldmaskpb.FieldMask, error) {
	if mask == nil {
		return nil, nil
	}
	rest := &fieldmaskpb.FieldMask{}
	for _, p := range mask.GetPaths() {
		switch p {
		case "logCheckFailures", fieldLogCheckFailures:
			if err := SetLogCheckFailures(dst, LogCheckFailures(src)); err != nil {
				return nil, err
			}
		case fieldUptimeDisabled:
			if err := SetUptimeDisabled(dst, UptimeDisabled(src)); err != nil {
				return nil, err
			}
		default:
			rest.Paths = append(rest.Paths, p)
		}
	}
	return rest, nil
}

// UptimeLogEntry is one failed run, for Cloud Logging.
type UptimeLogEntry struct {
	Time     time.Time
	Resource *monitoredres.MonitoredResource
	// Labels are the entry's: check_id.
	Labels map[string]string
	// Payload is the UptimeCheckResult, as a JSON object.
	Payload map[string]any
}

// UptimeLogger writes failed runs to a project's Cloud Logging.
type UptimeLogger func(ctx context.Context, project string, e UptimeLogEntry) error

// SetUptimeLogger sets where Log check failures go; nil is nowhere.
func (s *Server) SetUptimeLogger(l UptimeLogger) {
	s.mu.Lock()
	s.uptimeLog = l
	s.mu.Unlock()
}

// logUptimeFailure writes a failed run of c against host, when c logs
// its failures.
func (s *Server) logUptimeFailure(ctx context.Context, c *monitoringpb.UptimeCheckConfig, host string, r UptimeResult) {
	if r.Passed || !LogCheckFailures(c) {
		return
	}
	s.mu.Lock()
	l := s.uptimeLog
	s.mu.Unlock()
	if l == nil {
		return
	}
	project := projectOfName(c.GetName())
	id := c.GetName()[strings.LastIndex(c.GetName(), "/")+1:]
	payload := map[string]any{
		"@type":               "type.googleapis.com/google.monitoring.uptime.UptimeCheckResult",
		"checkId":             id,
		"checkDisplayName":    c.GetDisplayName(),
		"checkerLocation":     "local",
		"requestStartTime":    r.Time.UTC().Format(time.RFC3339Nano),
		"requestLatency":      r.Latency.String(),
		"checkPassed":         false,
		"errorMessage":        r.Error,
		"httpResponseCode":    r.Status,
		"target":              host,
		"uptimeCheckConfigId": c.GetName(),
	}
	_ = l(ctx, project, UptimeLogEntry{Time: r.Time,
		Resource: &monitoredres.MonitoredResource{Type: "uptime_url", Labels: map[string]string{"project_id": project, "host": host}},
		Labels:   map[string]string{"check_id": id}, Payload: payload})
}
