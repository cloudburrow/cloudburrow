package main

// Google's Ops Agent in VM images (#2139): the agent (Fluent Bit and the
// OpenTelemetry Collector, installed at a pinned version from Google's
// package repository, internal/service/compute/guest/Dockerfile.*) writes
// a VM's logs and metrics to this instance's Cloud Logging and Cloud
// Monitoring. The guest is told where they are by two variables of its
// pod, each an in-cluster host:port of the cloudburrow-host Service
// (clusterHost.InCluster); the guest's fence (usr/lib/cloudburrow/ops-agent)
// resolves them and lets the agent's processes send to those two
// addresses and nothing else, and the agent does not start without them.
// Both are published to the cluster whenever Compute Engine is served
// (up.go).

const (
	// opsAgentLoggingEnv and opsAgentMonitoringEnv name the variables.
	opsAgentLoggingEnv    = "CLOUDBURROW_LOGGING_HOST"
	opsAgentMonitoringEnv = "CLOUDBURROW_MONITORING_HOST"
)

// telemetryHosts are the in-cluster Logging and Monitoring addresses, ""
// for one not served.
func (s *computeService) telemetryHosts() (logging, monitoring string) {
	if s == nil || s.opsAgentHosts == nil {
		return "", ""
	}
	return s.opsAgentHosts()
}

// opsAgentEnv is the pod's environment for the guest's Ops Agent: the
// variable of each endpoint that is served, none when neither is.
func (b *computeVMs) opsAgentEnv() []any {
	if b.opsAgentHosts == nil {
		return nil
	}
	logging, monitoring := b.opsAgentHosts()
	var env []any
	if logging != "" {
		env = append(env, map[string]string{"name": opsAgentLoggingEnv, "value": logging})
	}
	if monitoring != "" {
		env = append(env, map[string]string{"name": opsAgentMonitoringEnv, "value": monitoring})
	}
	return env
}
