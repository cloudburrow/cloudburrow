package workflows

import (
	"errors"
	"strings"
	"time"
)

// Call logging, as https://docs.cloud.google.com/workflows/docs/log-workflow
// documents it: with call logging on, each explicit call step (a call to a
// subworkflow, a standard library function, an http.* function or a
// connector) is logged; calls from within expressions, and the requests a
// connector makes inside itself, are not. The level is the execution's
// call_log_level, or the workflow's when the execution does not set one:
//
//   - LOG_ALL_CALLS logs each call step's start (BEGUN, with its
//     arguments), its return (SUCCEEDED, with the response), an exception
//     it raised (EXCEPTION_RAISED) and an exception handled inside a
//     called subworkflow (EXCEPTION_HANDLED);
//   - LOG_ERRORS_ONLY logs only the exceptions call steps raise;
//   - LOG_NONE, or no level, logs nothing.
//
// An entry's jsonPayload is google.cloud.workflows.type.EngineCallLog
// (googleapis' google/cloud/workflows/type/engine_call.proto, which the
// page links to) in its JSON form, with its @type. HTTP Authorization
// headers are redacted. Google does not document the entries' log name or
// severity; CloudBurrow writes them to
// workflows.googleapis.com/executions_system_log, beside the
// ExecutionsSystemLog entries of each execution's start and end, INFO, and
// ERROR for a raised exception.

// Log IDs of an execution's entries.
const (
	// SysLogID is sys.log's log ("Workflows").
	SysLogID = "Workflows"
	// SystemLogID is the executions' start and end, and call logs.
	SystemLogID = "workflows.googleapis.com/executions_system_log"
)

const (
	engineCallLogType = "type.googleapis.com/google.cloud.workflows.type.EngineCallLog"
	systemLogType     = "type.googleapis.com/google.cloud.workflows.type.ExecutionsSystemLog"
)

// loggedCall is a call step under call logging.
type loggedCall struct {
	step, callee string
	start        time.Time
}

func (x *machine) logsAll() bool    { return x.callLevel == "LOG_ALL_CALLS" }
func (x *machine) logsErrors() bool { return x.logsAll() || x.callLevel == "LOG_ERRORS_ONLY" }

// callLog writes one EngineCallLog entry, once: a resumed execution does
// not write it again.
func (f *frame) callLog(lc *loggedCall, state, severity string, details map[string]any) {
	x := f.x
	if x.emit == nil {
		return
	}
	payload := map[string]any{"@type": engineCallLogType, "executionId": x.execID,
		"activityTime": x.now().UTC().Format(time.RFC3339Nano), "state": state, "step": lc.step, "callee": lc.callee}
	for k, v := range details {
		payload[k] = v
	}
	_, _ = x.effect(f, "calllog", func() (any, error) {
		x.emit(SystemLogID, severity, payload)
		return nil, nil
	})
}

// logBegun starts a call step's logging; nil when its calls are not logged.
func (f *frame) logBegun(s *step, named map[string]any) *loggedCall {
	if !f.x.logsErrors() {
		return nil
	}
	lc := &loggedCall{step: s.name, callee: s.call, start: f.x.now()}
	if f.x.logsAll() {
		args := map[string]any{}
		for k, v := range named {
			args[k] = redact(k, v)
		}
		f.callLog(lc, "BEGUN", "INFO", map[string]any{"begun": map[string]any{"namedArgs": jsonable(args)}})
	}
	return lc
}

// logEnd logs a call step's return or exception.
func (f *frame) logEnd(lc *loggedCall, v any, err error) {
	if lc == nil {
		return
	}
	start := lc.start.UTC().Format(time.RFC3339Nano)
	if err == nil {
		if f.x.logsAll() {
			f.callLog(lc, "SUCCEEDED", "INFO", map[string]any{"succeeded": map[string]any{"callStartTime": start, "response": serialize(v)}})
		}
		return
	}
	var we *wfError
	if !errors.As(err, &we) {
		return
	}
	origin := we.step
	if origin == "" {
		origin = lc.step
	}
	f.callLog(lc, "EXCEPTION_RAISED", "ERROR", map[string]any{"exceptionRaised": map[string]any{"callStartTime": start,
		"exception": serialize(we.val), "origin": origin}})
}

// logHandled logs an exception an except handles inside a call-logged
// subworkflow, which then carries on.
func (f *frame) logHandled(we *wfError) {
	lc := f.caller
	if lc == nil || !f.x.logsAll() {
		return
	}
	origin := we.step
	if origin == "" {
		origin = lc.step
	}
	f.callLog(lc, "EXCEPTION_HANDLED", "INFO", map[string]any{"exceptionHandled": map[string]any{
		"callStartTime": lc.start.UTC().Format(time.RFC3339Nano), "exception": serialize(we.val), "origin": origin}})
}

// redact hides HTTP Authorization headers.
func redact(arg string, v any) any {
	if arg != "headers" {
		return v
	}
	h, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := map[string]any{}
	for k, w := range h {
		if strings.EqualFold(k, "Authorization") {
			w = "<redacted>"
		}
		out[k] = w
	}
	return out
}

// serialize is a value as the call log's strings carry it.
func serialize(v any) string {
	b, err := encodeJSON(jsonable(v), false)
	if err != nil {
		return typeName(v)
	}
	return string(b)
}
