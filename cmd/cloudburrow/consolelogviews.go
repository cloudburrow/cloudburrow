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
	return console.Entry{
		Timestamp: e.GetTimestamp().AsTime(),
		Severity:  consoleSeverity(e.GetSeverity().String()),
		Source:    console.LoggingSourcePrefix + e.GetLogName()[strings.LastIndex(e.GetLogName(), "/")+1:],
		Project:   project,
		Resource:  e.GetLogName(),
		Message:   console.Redact(entryMessage(e)),
	}
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

func (v consoleLogViews) Read(ctx context.Context, view string, limit int) ([]console.Entry, error) {
	// ListLogEntriesRequest has no name or parent, so the interceptor
	// names no resource for it; neither does this.
	if err := v.faults.Apply(ctx, "logging", loggingpb.LoggingServiceV2_ListLogEntries_FullMethodName, ""); err != nil {
		return nil, err
	}
	resp, err := v.svc.api.ListLogEntries(ctx, &loggingpb.ListLogEntriesRequest{ResourceNames: []string{view},
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

func (v consoleLogViews) Tail(ctx context.Context, view string, send func(console.Entry)) error {
	if err := v.faults.Apply(ctx, "logging", loggingpb.LoggingServiceV2_TailLogEntries_FullMethodName, ""); err != nil {
		return err
	}
	return v.svc.api.TailLogEntries(&inProcessTail{ctx: ctx,
		req: &loggingpb.TailLogEntriesRequest{ResourceNames: []string{view}}, send: send})
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
