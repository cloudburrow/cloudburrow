package logging

import (
	"errors"
	"io"
	"sort"
	"sync"
	"time"

	"cloud.google.com/go/logging/apiv2/loggingpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// TailLogEntries limits, from the TailLogEntriesRequest reference.
const (
	// DefaultTailBufferWindow is buffer_window when the request sets none.
	DefaultTailBufferWindow = 2 * time.Second
	// MaxTailBufferWindow is the longest buffer_window accepted (0-60000 ms).
	MaxTailBufferWindow = 60 * time.Second
	// MaxTailFilterLength is the longest filter accepted.
	MaxTailFilterLength = 20000
)

// TailLogEntries streams entries as they are written (#1215).
//
// The first request on the stream sets resource_names (parents, whose own
// entries are sent, #1395: a bucket view is INVALID_ARGUMENT here), filter (the same subset as
// ListLogEntries, filter.go) and buffer_window. Only entries written after
// the stream opened are sent: a tail is not a read of history. Matching
// entries are held for buffer_window and sent as one response ordered by
// timestamp; an entry arriving later than that goes in a later response,
// and ordering between responses is not guaranteed, as Google documents. A
// buffer_window of 0 sends each write's entries as they arrive.
//
// Not modelled: suppression_info. CloudBurrow never rate-limits or drops a
// tail, so there is nothing to report. Later requests on the stream are
// read and ignored. The stream ends when the client cancels it or the
// server stops; the client closing its send side does not end it, since
// the tail has nothing more to receive but still has entries to send.
func (s *Server) TailLogEntries(stream loggingpb.LoggingServiceV2_TailLogEntriesServer) error {
	req, err := stream.Recv()
	if err != nil {
		return err
	}
	if len(req.GetResourceNames()) == 0 {
		return apierror.Wrap(apierror.InvalidArgument("resource_names is required"))
	}
	projects := map[parent]bool{}
	for _, r := range req.GetResourceNames() {
		p, err := s.admin.resolve("resource name", r)
		if err != nil {
			return apierror.Wrap(err)
		}
		projects[p] = true
	}
	if len(req.GetFilter()) > MaxTailFilterLength {
		return apierror.Wrap(apierror.InvalidArgument("filter is %d characters, over the %d-character limit",
			len(req.GetFilter()), MaxTailFilterLength))
	}
	match, err := compile(req.GetFilter())
	if err != nil {
		return apierror.Wrap(err)
	}
	window := DefaultTailBufferWindow
	if bw := req.GetBufferWindow(); bw != nil {
		if err := bw.CheckValid(); err != nil {
			return apierror.Wrap(apierror.InvalidArgument("buffer_window: %v", err))
		}
		window = bw.AsDuration()
		if window < 0 || window > MaxTailBufferWindow {
			return apierror.Wrap(apierror.InvalidArgument("buffer_window %s must be between 0 and 60s", window))
		}
	}

	ctx := stream.Context()
	var mu sync.Mutex
	var pending []*loggingpb.LogEntry
	wake := make(chan struct{}, 1)
	cancel := s.store.subscribe(func(e *loggingpb.LogEntry) {
		if !projects[entryParent(e)] || !match(e) {
			return
		}
		mu.Lock()
		pending = append(pending, e)
		mu.Unlock()
		select {
		case wake <- struct{}{}:
		default:
		}
	})
	defer cancel()

	// Later requests carry nothing the tail uses; reading them notices a
	// stream the client has broken.
	broken := make(chan error, 1)
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				if ctx.Err() == nil && !errors.Is(err, io.EOF) {
					broken <- err
				}
				return
			}
		}
	}()

	flush := func() error {
		mu.Lock()
		out := pending
		pending = nil
		mu.Unlock()
		if len(out) == 0 {
			return nil
		}
		sort.SliceStable(out, func(i, j int) bool { return cursorOf(out[i]).compare(cursorOf(out[j])) < 0 })
		return stream.Send(&loggingpb.TailLogEntriesResponse{Entries: out})
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-broken:
			return err
		case <-wake:
		}
		// The first entry of a batch opens the window; whatever matches
		// before it closes is sent with it, in timestamp order.
		if window > 0 {
			t := time.NewTimer(window)
			select {
			case <-ctx.Done():
				t.Stop()
				return nil
			case <-t.C:
			}
		}
		if err := flush(); err != nil {
			return err
		}
	}
}
