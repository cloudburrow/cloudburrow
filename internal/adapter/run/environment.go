package run

import (
	runpb "cloud.google.com/go/run/apiv2/runpb"
)

// InjectedEnvAnnotation is the revision template annotation recording what
// was injected, as a JSON object of name to value, for a reader of the
// Knative Service that wants to tell the injected variables apart — the
// console's editor, which edits only the service's own.
const InjectedEnvAnnotation = annInjectedEnv

// Environment returns the variables CloudBurrow gives every revision and
// job task (#576), by name: the in-cluster *_EMULATOR_HOST addresses of the
// enabled emulators, CLOUDBURROW_*_ENDPOINT for the services the CLI serves
// and GCE_METADATA_HOST, as a pod reaches them. An application that builds
// its clients with no options, as it would on Cloud Run, then reaches this
// instance rather than googleapis.com.
//
// It is a function, called each time a service or execution is rendered,
// because the adapter starts before everything it names: the CLI-hosted
// services are published to the cluster only once they are bound. A name
// with an empty value is left out, never injected empty, since most clients
// read an empty variable as unset and fall back to Google.
//
// The values must be addresses a pod can use and nothing else. Credentials
// never belong here: the host's credentials fixture is a host path, and a
// pod reaches the metadata server for a token instead.
type Environment func() map[string]string

// WithEnvironment sets what every revision and job task is given.
func (s *Server) WithEnvironment(env Environment) *Server {
	s.env = env
	return s
}

// injectedFor is the environment a workload in project is given, in name
// order, so the same service renders identically. GOOGLE_CLOUD_PROJECT is
// the workload's own project, which is what an application reading it
// expects its resources to live under.
func (s *Server) injectedFor(project string) []injectedEnv {
	vars := map[string]string{}
	if s.env != nil {
		for k, v := range s.env() {
			if k != "" && v != "" {
				vars[k] = v
			}
		}
	}
	if project != "" {
		vars["GOOGLE_CLOUD_PROJECT"] = project
	}
	out := make([]injectedEnv, 0, len(vars))
	for _, k := range sortedKeys(vars) {
		out = append(out, injectedEnv{name: k, value: vars[k]})
	}
	return out
}

// injectedRecord is what was injected into a revision, for its annotation:
// every injected literal no container set itself. `kubectl get ksvc -o yaml`
// shows it, and the pod spec holds the variables themselves.
func injectedRecord(containers []*runpb.Container, injected []injectedEnv) map[string]string {
	set := map[string]bool{}
	for _, c := range containers {
		for _, e := range c.GetEnv() {
			set[e.GetName()] = true
		}
	}
	out := map[string]string{}
	for _, e := range injected {
		if e.fieldPath == "" && !set[e.name] {
			out[e.name] = e.value
		}
	}
	return out
}

// injectedNames is the set of variables a revision's annotations record as
// injected. The Cloud Run API reports only the caller's env: the injected
// ones are left out of GetService, ListServices and GetRevision, as Cloud Run
// leaves its own platform variables (K_SERVICE, PORT) out of the resource,
// so a declarative client such as Terraform compares exactly what it set.
// The record never names a variable the caller set, so the caller's always
// reads back.
func injectedNames(annotations map[string]string) map[string]bool {
	record := map[string]string{}
	readJSONAnnotation(annotations, annInjectedEnv, &record)
	out := make(map[string]bool, len(record))
	for k := range record {
		out[k] = true
	}
	return out
}
