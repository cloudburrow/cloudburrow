package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/images"
	"github.com/cloudburrow/cloudburrow/internal/metrics"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	gcsbuiltin "github.com/cloudburrow/cloudburrow/internal/service/storage"
	"github.com/cloudburrow/cloudburrow/internal/storageimage"
)

// storageImageComponent builds the builtin Cloud Storage server's image and
// loads it into the cluster (#514), between the cluster and the components
// that deploy it. The binary is embedded in this CLI and the base pinned by
// digest (internal/storageimage), so nothing is published or pulled but the
// base; an image already built for this binary is reused.
type storageImageComponent struct {
	kubeconfig string
	loader     *images.Loader
	build      func(ctx context.Context, arch string) (string, error)
	comps      *components.LifecycleComponent
	out        io.Writer
}

func newStorageImageComponent(kubeconfig, cluster string, comps *components.LifecycleComponent, out io.Writer) *storageImageComponent {
	r := images.ExecRunner{}
	return &storageImageComponent{
		kubeconfig: kubeconfig,
		loader:     &images.Loader{ClusterName: cluster, Runner: r},
		build:      func(ctx context.Context, arch string) (string, error) { return storageimage.Build(ctx, r, arch) },
		comps:      comps,
		out:        out,
	}
}

func (s *storageImageComponent) Name() string { return "storage-image" }

func (s *storageImageComponent) Start(ctx context.Context) error {
	arch, err := s.loader.NodeArchitecture(ctx, s.kubeconfig)
	if err != nil {
		return fmt.Errorf("the cluster's node architecture: %w", err)
	}
	ref, err := s.build(ctx, arch)
	if err != nil {
		return fmt.Errorf("the builtin storage image: %w", err)
	}
	if err := s.loader.Load(ctx, ref, s.kubeconfig); err != nil {
		return fmt.Errorf("load %s: %w", ref, err)
	}
	fmt.Fprintf(s.out, "  storage: builtin server image %s (linux/%s)\n", ref, arch)
	s.comps.SetBuiltinStorageImage(ref)
	return nil
}

func (s *storageImageComponent) Stop(context.Context) error { return nil }

// storageEventScraper feeds the in-cluster builtin server's calls into
// /admin/events and /metrics (#513, #514). The server keeps its last calls
// at /_cloudburrow/events; the CLI polls it through the tunnel, since a pod
// cannot reach the CLI, which binds loopback (ADR-0004).
type storageEventScraper struct {
	tunnel   *netfwd.Forwarder
	record   func(gcsbuiltin.Call)
	interval time.Duration

	mu     sync.Mutex
	after  uint64
	cancel context.CancelFunc
	done   chan struct{}
}

func newStorageEventScraper(tunnel *netfwd.Forwarder, rec *admin.Recorder, reg *metrics.Registry) *storageEventScraper {
	return &storageEventScraper{tunnel: tunnel, record: storageEvents(rec, reg), interval: 2 * time.Second}
}

func (s *storageEventScraper) Name() string { return "storage-events" }

func (s *storageEventScraper) Start(context.Context) error {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel, s.done = cancel, make(chan struct{})
	go func() {
		defer close(s.done)
		t := time.NewTicker(s.interval)
		defer t.Stop()
		for {
			s.scrape(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return nil
}

func (s *storageEventScraper) Stop(ctx context.Context) error {
	if s.cancel == nil {
		return nil
	}
	s.cancel()
	select {
	case <-s.done:
	case <-ctx.Done():
	}
	return nil
}

// scrape reads the calls after the last one seen. A server that restarted
// numbers from 1 again, so a sequence that went back resets the cursor.
func (s *storageEventScraper) scrape(ctx context.Context) {
	if s.tunnel == nil || s.tunnel.HostAddr() == "" || s.record == nil {
		return
	}
	s.mu.Lock()
	after := s.after
	s.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+s.tunnel.HostAddr()+"/_cloudburrow/events?after="+strconv.FormatUint(after, 10), nil)
	if err != nil {
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var page struct {
		Calls []gcsbuiltin.Call `json:"calls"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&page) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// The server returns only calls after the cursor, oldest first.
	for _, c := range page.Calls {
		s.record(c)
		s.after = c.Seq
	}
	if len(page.Calls) == 0 && after > 0 {
		// Nothing after our cursor: if the server restarted, its sequence
		// is below it; the next page from 0 finds the new calls.
		s.after = s.probeRestart(ctx, after)
	}
}

// probeRestart returns 0 when the server's newest call is older than after
// (it restarted), else after.
func (s *storageEventScraper) probeRestart(ctx context.Context, after uint64) uint64 {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+s.tunnel.HostAddr()+"/_cloudburrow/events?after=0", nil)
	if err != nil {
		return after
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return after
	}
	defer resp.Body.Close()
	var page struct {
		Calls []gcsbuiltin.Call `json:"calls"`
	}
	if json.NewDecoder(resp.Body).Decode(&page) != nil || len(page.Calls) == 0 {
		return after
	}
	if page.Calls[len(page.Calls)-1].Seq < after {
		return 0
	}
	return after
}
