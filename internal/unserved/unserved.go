// Package unserved is the local-only guard (#1975): the inventory of every
// endpoint setting the pinned Google tools have, and the local refusal a
// tool configuration points each one at that this instance does not serve.
//
// A tool configuration that leaves an endpoint unset sends that API's calls
// to real Google: the google provider falls back to its default base path,
// gcloud to the API's googleapis.com host. So `cloudburrow terraform`, `env
// --format terraform` and `gcloud-setup` set every endpoint the pinned tool
// has: the served ones at this instance, every other one at URL, a path on
// the Resource Manager port (always served) whose Handler answers every
// request with a FAILED_PRECONDITION naming the API, so the call fails
// locally with a CloudBurrow message and nothing leaves the machine.
//
// The inventories are snapshots of the pinned versions dependencies.json
// records (realTools), taken by scripts/endpoint-inventory.sh. A repo test
// (TestEndpointInventoriesMatchThePins) fails when a pin moves without them,
// and the compat test TestPinnedToolsGainNoUnmappedEndpoint re-reads the
// real tools and fails when one has a setting the snapshot does not.
package unserved

import (
	_ "embed"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// The pinned tool versions the inventories were taken from. They must equal
// dependencies.json's realTools entries.
const (
	TerraformProviderVersion = "8.4.0"
	GcloudVersion            = "593.0.0"
)

//go:embed terraform-google-8.4.0.txt
var terraformGoogle string

//go:embed terraform-google-beta-8.4.0.txt
var terraformGoogleBeta string

//go:embed gcloud-593.0.0.txt
var gcloudOverrides string

// TerraformNoEnv are the endpoint arguments of the pinned providers that
// read no GOOGLE_*_CUSTOM_ENDPOINT environment variable (measured in the
// 8.4.0 binaries, #1975), so a refusal for one must be written in the
// provider block. Every other argument's refusal is passed in the
// environment.
var TerraformNoEnv = map[string]bool{
	"billing_custom_endpoint": true, "cloud_resource_manager_custom_endpoint": true,
	"core_billing_custom_endpoint": true, "iam_custom_endpoint": true,
	"resource_manager3_custom_endpoint": true, "runtimeconfig_custom_endpoint": true,
}

// GcloudUnoverridable are the APIs gcloud 593.0.0 has a client for
// (generated_clients/apis/apis_map.py) but no api_endpoint_overrides
// property: gcloud resolves their endpoint with no override to read, so no
// configuration can point them anywhere but googleapis.com. gcloud-setup
// names them; only an egress block (a proxy or firewall) stops them.
var GcloudUnoverridable = []string{
	"admin", "agenticapplications", "artifactscanguard", "bigquerydatatransfer", "billingbudgets",
	"blockchainnodeengine", "blockchainvalidatormanager", "connectors", "dataform", "dialogflow", "dlp",
	"essentialcontacts", "gsuiteaddons", "hstprof", "microservices", "serviceconsumermanagement",
	"servicenetworking", "telemetrymanagement", "translate", "videointelligence", "websecurityscanner",
}

// GcloudHardcoded are gcloud 593.0.0 commands that name an API by a name
// with no property even though its alias has one, so the alias's override
// does not reach them either (read in the SDK's sources, #1975).
var GcloudHardcoded = map[string]string{
	// surface/transfer/authorize.py: apis.GetClientInstance('storagetransfer',
	// 'v1'). The property is `transfer` (api_lib/util/apis_util.py aliases
	// it); a lookup of `storagetransfer` finds no property, so it calls
	// storagetransfer.googleapis.com whatever the configuration says. Every
	// other gcloud transfer command asks for 'transfer' and honours it.
	"gcloud transfer authorize": "storagetransfer.googleapis.com",
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

// TerraformEndpoints are the *_custom_endpoint arguments of the pinned
// hashicorp/google (beta false) or hashicorp/google-beta (beta true).
func TerraformEndpoints(beta bool) []string {
	if beta {
		return lines(terraformGoogleBeta)
	}
	return lines(terraformGoogle)
}

// AllTerraformEndpoints is the union of both providers' arguments, sorted.
func AllTerraformEndpoints() []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range append(TerraformEndpoints(false), TerraformEndpoints(true)...) {
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	sort.Strings(out)
	return out
}

// GcloudOverrides are the api_endpoint_overrides properties of the pinned
// gcloud.
func GcloudOverrides() []string { return lines(gcloudOverrides) }

// TerraformEnv is the environment variable the provider reads an argument
// from: GOOGLE_ and the argument in capitals.
func TerraformEnv(attr string) string { return "GOOGLE_" + strings.ToUpper(attr) }

// PathPrefix heads every refusal path.
const PathPrefix = "/_cloudburrow/unserved/"

// URL is the refusal endpoint for the setting name on the server at base
// (http://host:port): base + PathPrefix + name + "/". A tool appends its own
// version and resource path, which the Handler refuses whatever it is.
func URL(base, name string) string {
	return strings.TrimSuffix(base, "/") + PathPrefix + name + "/"
}

// Match reports whether r is for a refusal path.
func Match(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, PathPrefix) }

// Message is what the refusal says for the setting name.
func Message(name string) string {
	return fmt.Sprintf("CloudBurrow refused this request locally: %s is not served by this instance, "+
		"so its endpoint points here and nothing was sent to Google (#1975). "+
		"If CloudBurrow emulates this API, enable its service with `cloudburrow up --services`; "+
		"see docs/security.md, \"Nothing reaches Google\"", name)
}

// Handler refuses every request with FAILED_PRECONDITION (400), Google's
// JSON error shape, which no tool retries. It changes and reveals nothing,
// so it is served before the Host check.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, PathPrefix), "/")
		if name == "" || strings.Trim(name, "abcdefghijklmnopqrstuvwxyz0123456789_-") != "" || len(name) > 80 {
			name = "this API"
		}
		apierror.WriteJSON(w, apierror.FailedPrecondition("%s", Message(name)))
	})
}
