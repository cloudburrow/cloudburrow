package main

// The Logs Explorer's read through a log view (#1410): ListLogEntries and
// TailLogEntries on {bucket}/views/{view}, in process on the one
// LoggingServiceV2 server the gRPC and JSON transports serve, so the view's
// filter and the bucket's retention apply as they do to an SDK's read.

import (
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"

	"cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

type consoleLogViews struct {
	svc    *loggingService
	faults *admin.Faults
}

// logViewReader is the Explorer's view reads, or nil when Logging is not
// served.
func (s *loggingService) logViewReader(faults *admin.Faults) console.LogViewReader {
	if s == nil {
		return nil
	}
	return consoleLogViews{svc: s, faults: faults}
}

// consoleEntry is a Logging entry as the Explorer shows it.
func consoleEntry(e *loggingpb.LogEntry) console.Entry {
	// A folder's, organization's or billing account's entry (#1395) has
	// no project; its Resource is its log name, which names the parent.
	project := ""
	if rest, ok := strings.CutPrefix(e.GetLogName(), "projects/"); ok {
		project, _, _ = strings.Cut(rest, "/")
	}
	out := console.Entry{
		Timestamp: e.GetTimestamp().AsTime(),
		Severity:  consoleSeverity(e.GetSeverity().String()),
		Source:    console.LoggingSourcePrefix + e.GetLogName()[strings.LastIndex(e.GetLogName(), "/")+1:],
		Project:   project,
		Resource:  e.GetLogName(),
		Message:   console.Redact(entryMessage(e)),
		// The monitored resource, for resource.type and resource.labels
		// queries (#1849).
		ResourceType:   e.GetResource().GetType(),
		ResourceLabels: e.GetResource().GetLabels(),
	}
	// The jsonPayload itself, for the query editor and the Fields pane's
	// Frequent fields (#2031).
	if p, ok := e.GetPayload().(*loggingpb.LogEntry_JsonPayload); ok {
		if b, err := protojson.Marshal(p.JsonPayload); err == nil {
			out.JSONPayload = console.RedactJSON(b)
		}
	}
	// Its labels, for labels.KEY and the Fields pane's label dimensions
	// (#2032).
	if l := e.GetLabels(); len(l) > 0 {
		out.Labels = make(map[string]string, len(l))
		for k, v := range l {
			out.Labels[k] = console.Redact(v)
		}
	}
	return out
}

func (v consoleLogViews) Buckets(ctx context.Context, parent string) ([]console.LogBucketViews, error) {
	a := v.svc.Admin()
	if a == nil {
		return nil, errors.New(loggingNotStarted)
	}
	if err := v.faults.Apply(ctx, "logging", loggingpb.ConfigServiceV2_ListBuckets_FullMethodName, parent); err != nil {
		return nil, err
	}
	resp, err := a.ConfigServer().ListBuckets(ctx, &loggingpb.ListBucketsRequest{Parent: strings.TrimSuffix(parent, "/") + "/locations/-", PageSize: 1000})
	if err != nil {
		return nil, err
	}
	var out []console.LogBucketViews
	for _, b := range resp.GetBuckets() {
		bv := console.LogBucketViews{Name: b.GetName(), RetentionDays: b.GetRetentionDays(), Views: []console.LogViewInfo{}}
		if err := v.faults.Apply(ctx, "logging", loggingpb.ConfigServiceV2_ListViews_FullMethodName, b.GetName()); err != nil {
			return nil, err
		}
		views, err := a.ConfigServer().ListViews(ctx, &loggingpb.ListViewsRequest{Parent: b.GetName(), PageSize: 1000})
		if err != nil {
			return nil, err
		}
		for _, w := range views.GetViews() {
			bv.Views = append(bv.Views, console.LogViewInfo{Name: w.GetName(), Filter: w.GetFilter()})
		}
		out = append(out, bv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (v consoleLogViews) Read(ctx context.Context, names []string, limit int) ([]console.Entry, error) {
	// ListLogEntriesRequest has no name or parent, so the interceptor
	// names no resource for it; neither does this.
	if err := v.faults.Apply(ctx, "logging", loggingpb.LoggingServiceV2_ListLogEntries_FullMethodName, ""); err != nil {
		return nil, err
	}
	resp, err := v.svc.api.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{ResourceNames: names,
		OrderBy: "timestamp desc", PageSize: int32(limit)})
	if err != nil {
		return nil, err
	}
	es := resp.GetEntries()
	out := make([]console.Entry, len(es))
	for i, e := range es {
		out[len(es)-1-i] = consoleEntry(e)
	}
	return out, nil
}

func (v consoleLogViews) Tail(ctx context.Context, names []string, send func(console.Entry)) error {
	if err := v.faults.Apply(ctx, "logging", loggingpb.LoggingServiceV2_TailLogEntries_FullMethodName, ""); err != nil {
		return err
	}
	return v.svc.api.TailLogEntries(&inProcessTail{ctx: ctx,
		req: &loggingpb.TailLogEntriesRequest{ResourceNames: names}, send: send})
}

// LogScopes is the project's log scopes, _Default first, through
// logScopes.list, and its default one through the Observability API's
// scopes.get (#2020): what the Explorer's Refine scope offers and opens on.
func (v consoleLogViews) LogScopes(_ context.Context, project string) (console.LogScopeSet, error) {
	a := v.svc.Admin()
	if a == nil {
		return console.LogScopeSet{}, errors.New(loggingNotStarted)
	}
	all, err := loggingLogScopesProvider{loggingAdminScreen: loggingAdminScreen{svc: v.svc, faults: v.faults}}.scopes(project)
	if err != nil {
		return console.LogScopeSet{}, err
	}
	def, err := projectDefaultLogScope(a, project)
	if err != nil {
		return console.LogScopeSet{}, err
	}
	out := console.LogScopeSet{Default: def}
	for _, s := range all {
		out.Scopes = append(out.Scopes, console.LogScopeInfo{Name: s.Name, ResourceNames: s.ResourceNames, Description: s.Description})
	}
	return out, nil
}

// inProcessTail is a TailLogEntries stream whose one request is req and
// whose responses go to send.
type inProcessTail struct {
	ctx  context.Context
	req  *loggingpb.TailLogEntriesRequest
	send func(console.Entry)
	mu   sync.Mutex
	read bool
}

func (t *inProcessTail) Context() context.Context     { return t.ctx }
func (t *inProcessTail) SetHeader(metadata.MD) error  { return nil }
func (t *inProcessTail) SendHeader(metadata.MD) error { return nil }
func (t *inProcessTail) SetTrailer(metadata.MD)       {}
func (t *inProcessTail) SendMsg(m any) error {
	return t.Send(m.(*loggingpb.TailLogEntriesResponse))
}
func (t *inProcessTail) RecvMsg(any) error { return io.EOF }

func (t *inProcessTail) Recv() (*loggingpb.TailLogEntriesRequest, error) {
	t.mu.Lock()
	first := !t.read
	t.read = true
	t.mu.Unlock()
	if first {
		return t.req, nil
	}
	<-t.ctx.Done()
	return nil, io.EOF
}

func (t *inProcessTail) Send(resp *loggingpb.TailLogEntriesResponse) error {
	for _, e := range resp.GetEntries() {
		t.send(consoleEntry(e))
	}
	return nil
}

// consoleLogNames is the Explorer's Log name picker (#1554): ListLogs on the
// same LoggingServiceV2 server, with the instance's fault rules applied
// first, as the service's interceptor applies them to an SDK's call.
type consoleLogNames struct {
	svc    *loggingService
	faults *admin.Faults
}

// logNameLister is the Explorer's ListLogs, or nil when Logging is not
// served.
func (s *loggingService) logNameLister(faults *admin.Faults) console.LogNameLister {
	if s == nil {
		return nil
	}
	return consoleLogNames{svc: s, faults: faults}
}

func (n consoleLogNames) ListLogs(ctx context.Context, parent string) ([]string, error) {
	if n.svc.api == nil {
		return nil, errors.New(loggingNotStarted)
	}
	if err := n.faults.Apply(ctx, "logging", loggingpb.LoggingServiceV2_ListLogs_FullMethodName, parent); err != nil {
		return nil, err
	}
	var out []string
	req := &loggingpb.ListLogsRequest{Parent: parent, PageSize: 1000}
	for {
		resp, err := n.svc.api.ListLogs(ctx, req)
		if err != nil {
			return nil, err
		}
		out = append(out, resp.GetLogNames()...)
		if resp.GetNextPageToken() == "" {
			return out, nil
		}
		req.PageToken = resp.GetNextPageToken()
	}
}
