package main

import (
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// hostPorts are the fixed host ports this instance binds, keyed by the suffix
// of the --port-* flag that sets each one. OS-assigned ports (0) are left out:
// nothing can already hold a port the OS has yet to choose.
//
// It is the whole set: the always-on surfaces, every enabled service, the
// console, the local AI endpoint when a model is configured, and the ingress
// the cluster publishes. `up` checks it before creating anything and `doctor`
// reports on it, so neither can pass a port the other would trip over.
func hostPorts(cfg config.Config) map[string]int {
	e := cfg.Endpoints
	ports := map[string]int{
		"control":         e.Control,
		"metadata":        e.Metadata,
		"resourcemanager": e.ResourceManager,
		"console":         e.Console,
		"ingress":         e.Ingress,
	}
	if strings.TrimSpace(cfg.LocalAI.ModelPath) != "" {
		ports["localai"] = e.LocalAI
	}
	for _, s := range cfg.EnabledServices() {
		switch s {
		case config.ServiceStorage:
			ports["storage"] = e.Storage
		case config.ServicePubSub:
			ports["pubsub"] = e.PubSub
		case config.ServiceTasks:
			ports["tasks"] = e.Tasks
		case config.ServiceRun:
			ports["run"] = e.Run
		case config.ServiceSecrets:
			ports["secrets"] = e.Secrets
		case config.ServiceKMS:
			ports["kms"] = e.KMS
		case config.ServiceScheduler:
			ports["scheduler"] = e.Scheduler
		case config.ServiceLogging:
			ports["logging"] = e.Logging
		case config.ServiceBigQuery:
			ports["bigquery"] = e.BigQuery
			ports["bigquery-storage"] = e.BigQueryStorage
		default:
			ports[string(s)] = e.OptionalPort(s)
		}
	}
	for name, port := range ports {
		// 0 is OS-assigned; a negative console port disables the console.
		if port <= 0 {
			delete(ports, name)
		}
	}
	return ports
}

// checkHostPorts probes every port in hostPorts and fails naming each one
// that is already taken, before `up` creates a cluster or binds anything.
//
// Without it the first taken port was found by whichever component happened
// to bind it first, one at a time and part-way through startup: a second
// instance at the default ports got as far as "start tasks" and stopped
// there, naming one port of the dozen that collided (#584).
//
// ingressOurs is asked only when the ingress port is taken: an instance whose
// cluster is already running holds its own ingress, and that is not a
// collision. It is a callback because answering it shells out to kind.
func checkHostPorts(cfg config.Config, portFree func(host string, port int) error, ingressOurs func() bool) error {
	ports := hostPorts(cfg)
	names := make([]string, 0, len(ports))
	for name := range ports {
		names = append(names, name)
	}
	// Sorted by port, so the message reads like the layout.
	slices.SortFunc(names, func(a, b string) int { return ports[a] - ports[b] })

	var taken []string
	for _, name := range names {
		if portFree(cfg.BindAddress, ports[name]) == nil {
			continue
		}
		if name == "ingress" && ingressOurs() {
			continue
		}
		taken = append(taken, fmt.Sprintf("  - %s (--port-%s)",
			net.JoinHostPort(cfg.BindAddress, strconv.Itoa(ports[name])), name))
	}
	if len(taken) == 0 {
		return nil
	}
	return fmt.Errorf("%d host port(s) instance %q needs are already in use:\n%s\n\n"+
		"Another instance, or another program, holds them. To run beside it, move every port at once "+
		"with --port-base (for example --port-base %d), or set the ports above individually.",
		len(taken), cfg.Name, strings.Join(taken, "\n"), suggestPortBase(cfg))
}

// suggestPortBase is a base clear of this instance's current layout: the next
// block of 100 above it.
func suggestPortBase(cfg config.Config) int {
	base := cfg.PortBase
	if base == 0 {
		base = config.DefaultPortBase
	}
	next := (base/100 + 1) * 100
	if next > config.MaxPortBase() {
		return config.DefaultPortBase + 100
	}
	return next
}
