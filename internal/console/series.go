package console

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// A bounded, in-memory history of cluster readings.
//
// Nothing in this console retained more than one sample. The only thing that
// did was a browser tab polling every five seconds, which meant three things
// at once: the history died on navigation and on reload, the sampling rate
// was a function of how many tabs were open, and a chart had nothing to draw.
// A bar reading 0.097 cores cannot answer whether a deploy spiked memory.
//
// In memory and bounded, for the same reason the log recorder is: a
// development console that grew without limit would eventually be the reason
// the machine ran out of memory, and persisting metrics would make CloudBurrow
// responsible for data it never promised to keep. Restarting the instance
// clears it, and the screen says so rather than implying otherwise.

// SampleInterval is how often the sampler reads the cluster.
//
// Five seconds matches what the dashboard already polled at, so the rate no
// longer depends on who is looking.
const SampleInterval = 5 * time.Second

// SeriesLimit bounds the ring. At one sample per SampleInterval this is an
// hour of history, which is the span over which "did that deploy do this" is
// a question anyone asks.
const SeriesLimit = 720

// Sample is one reading, kept whole.
//
// The kubelet's own timestamp travels with it rather than the host's clock at
// decode: a rate divided by the wrong interval is wrong by however long the
// read took.
type Sample struct {
	At    time.Time     `json:"at"`
	Nodes []NodeMetrics `json:"nodes"`
	// Unavailable records a read that failed. It is kept in the series
	// rather than skipped, so a gap is drawable as a gap instead of
	// disappearing into a straight line between the readings either side.
	Unavailable string `json:"unavailable,omitempty"`
}

// Series holds the recent past.
type Series struct {
	mu      sync.RWMutex
	samples []Sample
	limit   int
	// started is when sampling began, so the screen can say "this instance
	// has been up for 30 seconds" rather than drawing 30 seconds of data as
	// if it were the whole story.
	started time.Time
	now     func() time.Time
}

// NewSeries returns an empty series.
func NewSeries(limit int, now func() time.Time) *Series {
	if limit <= 0 {
		limit = SeriesLimit
	}
	if now == nil {
		now = time.Now
	}
	return &Series{limit: limit, now: now, started: now().UTC()}
}

// Add records one reading.
func (s *Series) Add(m Metrics) {
	if s == nil {
		return
	}
	at := s.now().UTC()
	// The kubelet's timestamp when there is one. Collected is the console's
	// own, and is the fallback for a read that failed before any node
	// answered.
	if len(m.Nodes) > 0 && m.Nodes[0].At != "" {
		if parsed, err := time.Parse(time.RFC3339, m.Nodes[0].At); err == nil {
			at = parsed.UTC()
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples = append(s.samples, Sample{At: at, Nodes: m.Nodes, Unavailable: m.Unavailable})
	if len(s.samples) > s.limit {
		s.samples = s.samples[len(s.samples)-s.limit:]
	}
}

// Window returns the samples held, oldest first, and what the caller needs to
// tell a short history from a long one.
func (s *Series) Window() (samples []Sample, started time.Time, interval time.Duration) {
	if s == nil {
		return nil, time.Time{}, SampleInterval
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Sample, len(s.samples))
	copy(out, s.samples)
	return out, s.started, SampleInterval
}

// SetSeries installs the history the sampler writes to.
func (s *Server) SetSeries(series *Series) { s.series = series }

// SampleMetrics reads the cluster once and records it.
//
// Exported so the lifecycle component can drive it, and so a test can drive
// it with an injected clock rather than sleeping.
func (s *Server) SampleMetrics(ctx context.Context) {
	if s == nil || s.metrics == nil || s.series == nil {
		return
	}
	s.series.Add(s.metrics(ctx))
}

// handleSeries serves the retained history.
func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	if s.series == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"samples":     []Sample{},
			"unavailable": "this instance is not retaining metric history",
		})
		return
	}
	samples, started, interval := s.series.Window()
	if samples == nil {
		samples = []Sample{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"samples": samples,
		// The window and the interval travel with the data, so a client can
		// tell a gap from a flat line and a young instance from a quiet one.
		"startedAt":       started.Format(time.RFC3339),
		"intervalSeconds": int(interval / time.Second),
		"limit":           s.series.limit,
		// Said out loud rather than implied: this is not persisted, and a
		// restart begins again from nothing.
		"retention": "in memory only; a restart clears it",
	})
}

// Sampler drives the series on a fixed interval, independent of any request.
//
// A lifecycle component rather than a goroutine beside one, so it starts and
// stops with the instance and a shutdown does not leave it reading a cluster
// that is going away.
type Sampler struct {
	server   *Server
	interval time.Duration
	stop     chan struct{}
	done     chan struct{}
}

// NewSampler returns a sampler for a server's metrics source.
func NewSampler(s *Server, interval time.Duration) *Sampler {
	if interval <= 0 {
		interval = SampleInterval
	}
	return &Sampler{server: s, interval: interval,
		stop: make(chan struct{}), done: make(chan struct{})}
}

// Name implements lifecycle.Component.
func (s *Sampler) Name() string { return "metrics sampler" }

// Start begins sampling. It does not block.
func (s *Sampler) Start(ctx context.Context) error {
	// One reading immediately, so a dashboard opened straight after startup
	// has something rather than an empty chart for the first interval.
	s.server.SampleMetrics(ctx)

	ticker := time.NewTicker(s.interval)
	go func() {
		defer close(s.done)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ticker.C:
				// A fresh context per read: the start context is cancelled on
				// shutdown, and a read that used it would fail on the way out
				// and record a spurious gap.
				readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), readBudget)
				s.server.SampleMetrics(readCtx)
				cancel()
			}
		}
	}()
	return nil
}

// Stop ends sampling.
func (s *Sampler) Stop(ctx context.Context) error {
	select {
	case <-s.stop:
		return nil // already stopped
	default:
		close(s.stop)
	}
	select {
	case <-s.done:
	case <-ctx.Done():
	}
	return nil
}
