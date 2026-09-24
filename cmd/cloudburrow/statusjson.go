package main

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/config"
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
	Error      string          `json:"error,omitempty"`
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
		return []string{"components", "forward:storage", "storage-notify"}
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
		code, r.State = statusExitNotReady, "starting"
		if rd := live.readiness; rd != nil {
			ready, r.Components, r.Error = rd.Components, rd.Components, rd.Error
			switch {
			case rd.Ready:
				code, r.State = statusExitReady, "ready"
			case rd.State == "failed":
				r.State = "failed"
			}
		}
	}

	for _, s := range cfg.EnabledServices() {
		svc := statusService{ID: string(s), Persistence: string(s.Persistence()), EnvVar: netfwd.EnvVarFor(string(s))}
		if s == config.ServiceBigQuery {
			svc.EnvVar = "CLOUDBURROW_BIGQUERY_ENDPOINT"
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
