package netfwd

import (
	"fmt"
	"io"
	"sort"
)

// Endpoint is one published API surface, in both the forms a caller may need.
type Endpoint struct {
	Service   string // "storage", "pubsub", ...
	Host      string // 127.0.0.1:PORT — for clients on the developer's machine
	InCluster string // name.namespace.svc.cluster.local:PORT — for workloads
	// EnvVar is the official SDK environment variable that redirects this
	// service, or "" when no such variable exists.
	EnvVar string
	// EnvValue is the value that variable takes for host clients. It is not
	// always the bare address: the Python storage client requires a scheme.
	EnvValue string
}

// EnvVarFor returns the official emulator environment variable for a service.
//
// This is the one table of those variables. `up` prints its endpoints from it
// and `cloudburrow env` exports from it; a second copy used to exist in
// internal/components, used by nothing, which is how two answers to one
// question start.
//
// Cloud Tasks and Cloud Run have none. That is not an omission on our side:
// no such variable exists in the official clients, so those services can only
// be redirected by explicit client options in application code.
func EnvVarFor(service string) string {
	switch service {
	case "storage":
		return "STORAGE_EMULATOR_HOST"
	case "pubsub":
		return "PUBSUB_EMULATOR_HOST"
	// The optional emulators each have an official variable, which is what
	// makes them cheap to support: an application needs no code change.
	case "firestore":
		return "FIRESTORE_EMULATOR_HOST"
	case "datastore":
		return "DATASTORE_EMULATOR_HOST"
	case "bigtable":
		return "BIGTABLE_EMULATOR_HOST"
	case "spanner":
		return "SPANNER_EMULATOR_HOST"
	default:
		return ""
	}
}

// EnvValueFor returns the value the variable should take.
//
// Storage carries a scheme because the Go and Python clients disagree: Python
// uses the value verbatim and needs one, Go prepends http:// when absent. The
// form with a scheme is accepted by both. Pub/Sub takes a bare host:port.
func EnvValueFor(service, addr string) string {
	switch service {
	case "storage":
		return "http://" + addr
	case "pubsub", "firestore", "datastore", "bigtable", "spanner":
		// All of these take a bare host:port. Storage is the odd one out.
		return addr
	default:
		return ""
	}
}

// ScopeNoteFor returns the one thing a service's endpoint does not give you.
//
// An address on this list reads as "the service is here", and for every
// emulator that is true. Cloud SQL is the exception: there is no Cloud SQL
// emulator to run, so what is behind that address is a real PostgreSQL and
// the management API is simply absent. Saying so beside the address is the
// only place a reader is guaranteed to be looking.
func ScopeNoteFor(service string) string {
	switch service {
	case "cloudsql":
		return "a local SQL database, not the Cloud SQL Admin API — " +
			"connect with an ordinary driver; there is no sqladmin endpoint"
	case "bigquery":
		return "goccy/bigquery-emulator, not Google's; serves this instance's " +
			"project only, and keeps nothing across a restart"
	case "bigquery-storage":
		return "the BigQuery Storage Read API (gRPC) for the endpoint above"
	case "cloudsql-mysql":
		return "a local MySQL 8.4, not the Cloud SQL Admin API — " +
			"connect with an ordinary driver; there is no sqladmin endpoint"
	case "memorystore":
		return "a real Valkey (Redis-compatible) server, not the Memorystore admin API — " +
			"connect with any Redis client; there is no redis.googleapis.com endpoint"
	default:
		return ""
	}
}

// NewEndpoint builds an Endpoint for a service and its two addresses.
func NewEndpoint(service, host, inCluster string) Endpoint {
	return Endpoint{
		Service:   service,
		Host:      host,
		InCluster: inCluster,
		EnvVar:    EnvVarFor(service),
		EnvValue:  EnvValueFor(service, host),
	}
}

// PrintEndpoints writes the endpoint table and the per-SDK configuration.
//
// Both address forms are always shown, because handing a workload the host
// address is the most common way this goes wrong.
func PrintEndpoints(w io.Writer, endpoints []Endpoint) {
	if len(endpoints) == 0 {
		return
	}
	sorted := append([]Endpoint(nil), endpoints...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Service < sorted[j].Service })

	fmt.Fprintln(w, "\n  endpoints:")
	for _, e := range sorted {
		fmt.Fprintf(w, "    %-8s host %-22s in-cluster %s\n", e.Service, e.Host, e.InCluster)
		if note := ScopeNoteFor(e.Service); note != "" {
			fmt.Fprintf(w, "             %s\n", note)
		}
	}

	fmt.Fprintln(w, "\n  configure official SDKs on this machine:")
	var manual []Endpoint
	for _, e := range sorted {
		if e.EnvVar == "" {
			manual = append(manual, e)
			continue
		}
		fmt.Fprintf(w, "    export %s=%s\n", e.EnvVar, e.EnvValue)
	}
	for _, e := range manual {
		fmt.Fprintf(w, "    %-8s no emulator environment variable exists; pass an explicit\n", e.Service)
		fmt.Fprintf(w, "             endpoint (%s) in client options\n", e.Host)
	}
	fmt.Fprintln(w, "\n  workloads inside the cluster must use the in-cluster address:")
	fmt.Fprintln(w, "    a pod's loopback is the pod itself, not your machine.")
}
