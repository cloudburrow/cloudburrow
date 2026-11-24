package iameval

import (
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/iamcheck"
)

// OAuth scopes (#2179). An access token minted with scopes (a Compute
// Engine VM's, which carries the instance's access scopes) may call only
// what one of them covers, as Google documents
// (https://docs.cloud.google.com/compute/docs/access/service-accounts#accesscopesiam,
// https://developers.google.com/identity/protocols/oauth2/scopes, read
// 2026-11-22): with IAM enforcement on, a call no scope covers is refused
// with 403 ACCESS_TOKEN_SCOPE_INSUFFICIENT before its permissions are
// checked, and a scope covers nothing the account's roles do not grant.

const scopePrefix = "https://www.googleapis.com/auth/"

// scopeRule is one scope of a service: readOnly scopes cover only
// methods that read; verbs, when set, the permissions whose last segment
// is one of them.
type scopeRule struct {
	scope    string
	readOnly bool
	perms    []string
}

// serviceScopes are each service's own scopes, besides cloud-platform
// (everything) and cloud-platform.read-only (every read).
var serviceScopes = map[string][]scopeRule{
	"storage.googleapis.com": {{scope: "devstorage.full_control"}, {scope: "devstorage.read_write"},
		{scope: "devstorage.read_only", readOnly: true}},
	"compute.googleapis.com": {{scope: "compute"}, {scope: "compute.readonly", readOnly: true}},
	"logging.googleapis.com": {{scope: "logging.admin"}, {scope: "logging.read", readOnly: true},
		{scope: "logging.write", perms: []string{"logging.logEntries.create", "logging.logEntries.route"}}},
	"monitoring.googleapis.com": {{scope: "monitoring"}, {scope: "monitoring.read", readOnly: true},
		{scope: "monitoring.write", perms: []string{"monitoring.timeSeries.create", "monitoring.metricDescriptors.create"}}},
	"pubsub.googleapis.com": {{scope: "pubsub"}},
	"bigquery.googleapis.com": {{scope: "bigquery"}, {scope: "bigquery.readonly", readOnly: true},
		{scope: "bigquery.insertdata", perms: []string{"bigquery.tables.updateData"}}},
	"datastore.googleapis.com":         {{scope: "datastore"}},
	"firestore.googleapis.com":         {{scope: "datastore"}},
	"spanner.googleapis.com":           {{scope: "spanner.admin"}, {scope: "spanner.data"}},
	"bigtable.googleapis.com":          {{scope: "bigtable.admin"}, {scope: "bigtable.data"}, {scope: "bigtable.data.readonly", readOnly: true}},
	"bigtableadmin.googleapis.com":     {{scope: "bigtable.admin"}},
	"sqladmin.googleapis.com":          {{scope: "sqlservice.admin"}},
	"cloudkms.googleapis.com":          {{scope: "cloudkms"}},
	"cloudtrace.googleapis.com":        {{scope: "trace.append", perms: []string{"cloudtrace.traces.patch"}}, {scope: "trace.readonly", readOnly: true}},
	"servicemanagement.googleapis.com": {{scope: "service.management"}, {scope: "service.management.readonly", readOnly: true}},
	"servicecontrol.googleapis.com":    {{scope: "servicecontrol"}},
	"source.googleapis.com":            {{scope: "source.full_control"}, {scope: "source.read_write"}, {scope: "source.read_only", readOnly: true}},
	"cloudresourcemanager.googleapis.com": {{scope: "cloudplatformprojects"},
		{scope: "cloudplatformprojects.readonly", readOnly: true}},
}

// readVerb reports whether permission only reads.
func readVerb(permission string) bool {
	verb := permission[strings.LastIndex(permission, ".")+1:]
	for _, p := range []string{"get", "list", "read", "search", "query", "lookup", "export", "view", "test", "aggregatedList"} {
		if strings.HasPrefix(verb, p) {
			return true
		}
	}
	return false
}

// ScopesAllow reports whether a token with scopes may call a method of
// service that needs permission; nil scopes are an unlimited token.
func ScopesAllow(service, permission string, scopes []string) bool {
	if scopes == nil {
		return true
	}
	for _, sc := range scopes {
		sc = strings.TrimPrefix(sc, scopePrefix)
		switch sc {
		case "cloud-platform":
			return true
		case "cloud-platform.read-only":
			if readVerb(permission) {
				return true
			}
			continue
		}
		for _, r := range serviceScopes[service] {
			if r.scope != sc {
				continue
			}
			switch {
			case r.readOnly && !readVerb(permission):
			case len(r.perms) > 0:
				for _, p := range r.perms {
					if p == permission {
						return true
					}
				}
			default:
				return true
			}
		}
	}
	return false
}

// InsufficientScopes is Google's refusal of a token whose scopes do not
// cover the call.
func InsufficientScopes(service, method string) error {
	return iamcheck.InsufficientScopes(service, method)
}
