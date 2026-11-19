package apienable

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Path is the host's answer, on the Resource Manager port: every project
// with a disabled API. `up` always serves it, and always gives the servers
// in the cluster its address (--resourcemanager).
const Path = "/cloudburrow/v1/services:disabled"

// CacheFor is how long Remote keeps an answer. A disable reaches the
// servers in the cluster within it, as Google's reaches its servers after
// a while ("wait a few minutes for the action to propagate"); the
// in-process services see it at once.
const CacheFor = time.Second

// Project is one project's disabled APIs.
type Project struct {
	ID       string   `json:"id"`
	Number   int64    `json:"number"`
	Disabled []string `json:"disabled"`
}

// Snapshot is Path's answer.
type Snapshot struct {
	Projects []Project         `json:"projects"`
	Titles   map[string]string `json:"titles,omitempty"`
}

// Source is what the host serves Path from.
type Source interface {
	DisabledSnapshot() (Snapshot, error)
}

// Handler serves Path from src.
func Handler(src Source) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "GET only", http.StatusMethodNotAllowed)
			return
		}
		snap, err := src.DisabledSnapshot()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(snap)
	})
}

// Decide answers from a snapshot.
func (s Snapshot) Decide(project, service string) (Denial, bool) {
	for _, p := range s.Projects {
		if p.ID != project && strconv.FormatInt(p.Number, 10) != project {
			continue
		}
		for _, d := range p.Disabled {
			if d == service {
				return Denial{Service: service, Title: s.Titles[service], Number: p.Number}, true
			}
		}
	}
	return Denial{}, false
}

// Remote asks the host at Base ("http://host:port"), keeping each answer
// for CacheFor. When the host does not answer, the last answer stands
// (none: every API enabled), so a host restart does not fail every call.
type Remote struct {
	Base   string
	Client *http.Client

	mu      sync.Mutex
	snap    Snapshot
	fetched time.Time
}

var remoteClient = &http.Client{Timeout: 5 * time.Second}

// Disabled implements Decider.
func (c *Remote) Disabled(ctx context.Context, project, service string) (Denial, bool) {
	return c.snapshot(ctx).Decide(project, service)
}

func (c *Remote) snapshot(ctx context.Context) Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.fetched.IsZero() && time.Since(c.fetched) < CacheFor {
		return c.snap
	}
	if snap, err := c.fetch(ctx); err == nil {
		c.snap = snap
	}
	c.fetched = time.Now()
	return c.snap
}

func (c *Remote) fetch(ctx context.Context) (Snapshot, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(c.Base, "/")+Path, nil)
	if err != nil {
		return Snapshot{}, err
	}
	hc := c.Client
	if hc == nil {
		hc = remoteClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return Snapshot{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Snapshot{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return Snapshot{}, fmt.Errorf("apienable: %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}
