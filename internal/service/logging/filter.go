package logging

import (
	"encoding/json"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"cloud.google.com/go/logging/apiv2/loggingpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/logfilter"
)

// Filters — ListLogEntries, TailLogEntries, sinks, exclusions, log views,
// log-based metrics and CopyLogEntries — are Cloud Logging's query language
// in full (#2026), parsed and evaluated by internal/logfilter, which the
// console's Logs Explorer also uses: comparisons (= != < <= > >= : =~ !~ and
// :*), NOT, OR and AND with the language's precedence (NOT, then OR, then
// AND; adjacent terms ANDed), parentheses, field paths over every LogEntry
// field (labels."a/b", jsonPayload.x.y, protoPayload.@type), global
// restrictions, SEARCH, log_id, sample, ip_in_net, cast, regexp_extract and
// source (docs.cloud.google.com/logging/docs/view/logging-query-language).
//
// source("projects/P") matches that parent's own entries; in a log view
// filter (compileWith, which has the hierarchy) a folder or organization
// also takes what Resource Manager holds under it (#1410), as log views
// document (docs.cloud.google.com/logging/docs/logs-views).
//
// A filter the language refuses — a syntax error, an undefined field, a bad
// regular expression or function argument — is INVALID_ARGUMENT with the
// language's message.

type pred func(*loggingpb.LogEntry) bool

// compile turns a filter into a predicate.
func compile(filter string) (pred, error) { return compileWith(filter, nil) }

// compileWith compiles a log view filter: the restricted set log views take
// (logfilter's CheckView), with source() taking a folder's or organization's
// descendants, ancestors answering what a project or folder sits under,
// nearest first. compile passes nil ancestors: any filter, and source()
// matching the parent's own entries only.
func compileWith(filter string, ancestors func(parent) []parent) (pred, error) {
	f, err := logfilter.Parse(filter, logfilter.Options{Source: func(arg string) (func(logfilter.Record) bool, error) {
		return sourcePred(arg, ancestors)
	}})
	if err == nil && ancestors != nil {
		err = f.CheckView()
	}
	if err != nil {
		return nil, apierror.InvalidArgument("%s", err.Error())
	}
	if f == nil {
		return func(*loggingpb.LogEntry) bool { return true }, nil
	}
	return func(e *loggingpb.LogEntry) bool { return f.Match(&entryRecord{e: e}) }, nil
}

func sourcePred(arg string, ancestors func(parent) []parent) (func(logfilter.Record) bool, error) {
	want, rest, err := cutParent("SOURCE", arg)
	if err != nil || rest != "" {
		return nil, &logfilter.Error{Msg: "Invalid filter: source(\"" + arg + "\") does not name a project, folder, organization or billing account"}
	}
	return func(r logfilter.Record) bool {
		p := entryParent(r.(*entryRecord).e)
		if p == want {
			return true
		}
		if ancestors == nil || !want.hasChildren() {
			return false
		}
		for _, a := range ancestors(p) {
			if a == want {
				return true
			}
		}
		return false
	}, nil
}

// entryRecord is a LogEntry as the query language sees it: its JSON form.
// The fields filters name most are read straight from the message; the
// rest, and global restrictions, use its protojson form, built once.
type entryRecord struct {
	e    *loggingpb.LogEntry
	root map[string]any
}

func (r *entryRecord) Field(name string) (any, bool) {
	e := r.e
	switch name {
	case "logName":
		return e.GetLogName(), e.GetLogName() != ""
	case "severity":
		return int(e.GetSeverity()), e.GetSeverity() != 0
	case "timestamp":
		if e.GetTimestamp() == nil {
			return nil, false
		}
		return e.GetTimestamp().AsTime(), true
	case "receiveTimestamp":
		if e.GetReceiveTimestamp() == nil {
			return nil, false
		}
		return e.GetReceiveTimestamp().AsTime(), true
	case "insertId":
		return e.GetInsertId(), e.GetInsertId() != ""
	case "trace":
		return e.GetTrace(), e.GetTrace() != ""
	case "spanId":
		return e.GetSpanId(), e.GetSpanId() != ""
	case "traceSampled":
		return e.GetTraceSampled(), e.GetTraceSampled()
	case "textPayload":
		p, ok := e.GetPayload().(*loggingpb.LogEntry_TextPayload)
		if !ok {
			return nil, false
		}
		return p.TextPayload, true
	case "labels":
		if len(e.GetLabels()) == 0 {
			return nil, false
		}
		return e.GetLabels(), true
	case "resource":
		if e.GetResource() == nil {
			return nil, false
		}
		m := map[string]any{"type": e.GetResource().GetType()}
		if l := e.GetResource().GetLabels(); len(l) > 0 {
			m["labels"] = l
		}
		return m, true
	case "jsonPayload":
		if e.GetJsonPayload() == nil {
			return nil, false
		}
		return e.GetJsonPayload().AsMap(), true
	}
	v, ok := r.Root()[name]
	return v, ok
}

func (r *entryRecord) Root() map[string]any {
	if r.root != nil {
		return r.root
	}
	m := map[string]any{}
	b, err := protojson.Marshal(r.e)
	if err != nil {
		// A protoPayload whose type is not linked in: everything else, and
		// the payload's @type.
		c := proto.Clone(r.e).(*loggingpb.LogEntry)
		c.Payload = nil
		b, _ = protojson.Marshal(c)
	}
	_ = json.Unmarshal(b, &m)
	if pp := r.e.GetProtoPayload(); err != nil && pp != nil {
		m["protoPayload"] = map[string]any{"@type": pp.GetTypeUrl()}
	}
	r.root = m
	return m
}
