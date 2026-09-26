package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/hooks"
	"github.com/cloudburrow/cloudburrow/internal/lifecycle"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
)

// `status --format json` (#281): the instance's state as a versioned object,
// with exit statuses a script can branch on. The human form is unchanged,
// output and exit status both.

// statusSchemaVersion is bumped on any change a consumer could notice. The
// golden file in testdata pins the shape it names.
const statusSchemaVersion = 1

// Exit statuses of `status --format json`. 2 is a usage error, as for every
// command.
const (
	statusExitReady      = 0
	statusExitNotRunning = 3
	statusExitNotReady   = 4
)

type statusReport struct {
	SchemaVersion int    `json:"schema_version"`
	Instance      string `json:"instance"`
	Project       string `json:"project"`
	Mode          string `json:"mode"`
	Bind          string `json:"bind"`
	// State is not_running, starting, ready or failed.
	State      string          `json:"state"`
	ControlURL string          `json:"control_url,omitempty"`
	ConsoleURL string          `json:"console_url,omitempty"`
	IngressURL string          `json:"ingress_url,omitempty"`
	Cluster    statusCluster   `json:"cluster"`
	Services   []statusService `json:"services"`
	// Components is /readyz's per-component readiness, when there is a
	// running instance to ask.
	Components map[string]bool `json:"components,omitempty"`
	// Timing is when each component started and how long it took, from
	// /readyz (#312).
	Timing map[string]componentTiming `json:"timing,omitempty"`
	Error  string                     `json:"error,omitempty"`
	// Hooks are the lifecycle hooks' outcomes by stage, when any ran.
	Hooks map[string][]hooks.Result `json:"hooks,omitempty"`
}

type statusCluster struct {
	Name       string `json:"name"`
	State      string `json:"state"`
	Kubernetes string `json:"kubernetes,omitempty"`
}

type statusService struct {
	ID string `json:"id"`
	// Endpoint is the host address: the one bound, when the instance is
	// running, else the configured one; empty when neither is known.
	Endpoint    string `json:"endpoint,omitempty"`
	EnvVar      string `json:"env_var,omitempty"`
	Persistence string `json:"persistence"`
	Ready       bool   `json:"ready"`
	Reason      string `json:"reason,omitempty"`
}

// componentsOf names the readiness components a service depends on, beyond
// the cluster every one of them needs. A component the running instance did
// not register is not held against the service.
func componentsOf(s config.Service) []string {
	switch s {
	case config.ServiceStorage:
		// One Deployment from a locally built image (#514): storage waits for
		// the image, then the Deployment and its tunnel.
		return []string{"storage-image", "components", "forward:storage"}
	case config.ServiceTasks, config.ServiceSecrets:
		return []string{string(s)}
	case config.ServiceRun:
		return []string{"components", "run"}
	case config.ServiceBigQuery:
		return []string{"components", "forward:bigquery", "forward:bigquery-storage"}
	default:
		return []string{"components", "forward:" + string(s)}
	}
}

// liveState is what a running instance reported, when there is one.
type liveState struct {
	info      runtimeInfo
	readiness *readiness // nil when /readyz did not answer
}

// buildStatusReport assembles the report. It does no I/O, so a golden file
// can pin its output.
func buildStatusReport(cfg config.Config, live *liveState, clusterState, kubernetes string) (statusReport, int) {
	r := statusReport{
		SchemaVersion: statusSchemaVersion,
		Instance:      cfg.Name,
		Project:       cfg.DefaultProject(),
		Mode:          string(cfg.Mode),
		Bind:          cfg.BindAddress,
		Cluster:       statusCluster{Name: cfg.ClusterName(), State: clusterState, Kubernetes: kubernetes},
	}
	host := func(port int) string { return net.JoinHostPort(cfg.BindAddress, strconv.Itoa(port)) }
	if cfg.Endpoints.Ingress != 0 {
		r.IngressURL = "http://" + host(cfg.Endpoints.Ingress)
	}

	var ready map[string]bool
	code := statusExitNotRunning
	r.State = "not_running"
	if live != nil {
		if a := live.info.Endpoints["control"]; a != "" {
			r.ControlURL = "http://" + a
		} else if live.info.Control != "" {
			r.ControlURL = "http://" + live.info.Control
		}
		if a := live.info.Endpoints["console"]; a != "" {
			r.ConsoleURL = "http://" + a
		}
		r.Hooks = live.info.Hooks
		code, r.State = statusExitNotReady, "starting"
		if rd := live.readiness; rd != nil {
			ready, r.Components, r.Error = rd.Components, rd.Components, rd.Error
			r.Timing = rd.Timing
			switch {
			case rd.Ready:
				code, r.State = statusExitReady, "ready"
			case rd.State == "failed":
				r.State = "failed"
			}
		}
	}

	for _, s := range cfg.EnabledServices() {
		// What this instance keeps, not what the backend could: in ephemeral
		// mode no volume is provisioned for anything (#307).
		persistence := s.Persistence()
		if cfg.Mode == config.ModeEphemeral {
			persistence = config.PersistenceNone
		}
		svc := statusService{ID: string(s), Persistence: string(persistence), EnvVar: netfwd.EnvVarFor(string(s))}
		switch s {
		case config.ServiceBigQuery:
			svc.EnvVar = "CLOUDBURROW_BIGQUERY_ENDPOINT"
		case config.ServiceMemorystore:
			svc.EnvVar = "REDIS_PORT"
		case config.ServiceCloudSQLMySQL:
			svc.EnvVar = "MYSQL_PORT"
		}
		if live != nil && live.info.Endpoints[string(s)] != "" {
			svc.Endpoint = live.info.Endpoints[string(s)]
		} else if eps := configuredEndpoints(cfg, s); len(eps) > 0 && eps[0].port != 0 {
			svc.Endpoint = host(eps[0].port)
		}
		switch {
		case live == nil:
			svc.Reason = "the instance is not running"
		case ready == nil:
			svc.Reason = "the instance is starting; readiness is not answering yet"
		default:
			var waiting []string
			for _, c := range append([]string{"cluster"}, componentsOf(s)...) {
				if ok, known := ready[c]; known && !ok {
					waiting = append(waiting, c)
				}
			}
			svc.Ready = len(waiting) == 0
			if !svc.Ready {
				svc.Reason = "not ready: " + strings.Join(waiting, ", ")
			}
		}
		r.Services = append(r.Services, svc)
	}
	sort.Slice(r.Services, func(i, j int) bool { return r.Services[i].ID < r.Services[j].ID })
	return r, code
}

// liveStatus asks the running instance, if there is one, for its readiness.
func liveStatus(cfg config.Config) *liveState {
	info, ok := running(cfg)
	if !ok {
		return nil
	}
	live := &liveState{info: info}
	c := &http.Client{Timeout: 3 * time.Second}
	if resp, err := c.Get("http://" + info.Control + "/readyz"); err == nil {
		var rd readiness
		if json.NewDecoder(resp.Body).Decode(&rd) == nil {
			live.readiness = &rd
		}
		_ = resp.Body.Close()
	}
	return live
}

func writeStatusJSON(w io.Writer, r statusReport, code int) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return err
	}
	if code == statusExitReady {
		return nil
	}
	// The report already says why; the status is for the caller to branch on.
	return &exitError{code: code, err: errors.New(r.State), quiet: true}
}

// readySummary is the one line `up` prints when ready: the total and the
// components that took at least a second, slowest first (#312).
func readySummary(c *lifecycle.Coordinator) string {
	timings := c.Timings()
	var first, last time.Time
	type part struct {
		name string
		d    time.Duration
	}
	var parts []part
	for _, name := range c.TimingOrder() {
		t := timings[name]
		if first.IsZero() || t.StartedAt.Before(first) {
			first = t.StartedAt
		}
		if end := t.StartedAt.Add(t.ReadyAfter); end.After(last) {
			last = end
		}
		if t.ReadyAfter >= time.Second {
			parts = append(parts, part{name, t.ReadyAfter})
		}
	}
	sort.SliceStable(parts, func(i, j int) bool { return parts[i].d > parts[j].d })
	var b strings.Builder
	fmt.Fprintf(&b, "ready in %s", last.Sub(first).Round(time.Second))
	for i, p := range parts {
		sep := ", "
		if i == 0 {
			sep = ": "
		}
		fmt.Fprintf(&b, "%s%s %s", sep, p.name, p.d.Round(time.Second))
	}
	return b.String()
}
