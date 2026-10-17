package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// `cloudburrow terraform` (#283): run Terraform or OpenTofu with the Google
// provider pointed at this instance.
//
// It writes a provider configuration next to the module, runs the binary with
// the remaining arguments, and removes the file however the run ends. Only
// services whose Terraform support is Verified in docs/compatibility.md get
// an endpoint; the rest are named in a warning rather than silently sent to
// real Google. A module declaring resources it cannot point here at all
// (unpointedResources, #1331) is refused outright.

// terraformEndpoint is one provider endpoint attribute.
type terraformEndpoint struct {
	service config.Service
	attr    string // the google provider's *_custom_endpoint argument
	env     string // the environment variable the provider reads it from
	path    string // appended to http://host:port
	// verified is true only for rows docs/compatibility.md marks Verified
	// with a test behind them. `env --format terraform` reads this table too.
	verified bool
	// regional names the provider's base paths that carry {{location}}
	// (Cloud Run v1's is https://{{location}}-run.googleapis.com/): on
	// loopback the host is {{location}}-<regional>.localhost, which the
	// provider fills in per resource and which resolves to loopback, so the
	// server reads the region from the host as it does for gcloud.
	regional string
}

var terraformEndpoints = []terraformEndpoint{
	{config.ServiceStorage, "storage_custom_endpoint", "GOOGLE_STORAGE_CUSTOM_ENDPOINT", "/storage/v1/", true, ""},
	{config.ServicePubSub, "pubsub_custom_endpoint", "GOOGLE_PUBSUB_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// Cloud Tasks (#366): a queue and its *_iam_member apply, plan clean and
	// destroy through the JSON API in TestTerraformAppliesAndDestroysThroughTheWrapper.
	{config.ServiceTasks, "cloud_tasks_custom_endpoint", "GOOGLE_CLOUD_TASKS_CUSTOM_ENDPOINT", "/v2/", true, ""},
	// Secret Manager (#365): a secret and its *_iam_member apply, plan clean
	// and destroy in TestTerraformAppliesAndDestroysThroughTheWrapper.
	{config.ServiceSecrets, "secret_manager_custom_endpoint", "GOOGLE_SECRET_MANAGER_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// Regional secrets (#1519): the provider's base path is
	// secretmanager.{{location}}.rep.googleapis.com, and the resources'
	// URLs carry projects/{project}/locations/{location}, so the one port
	// serves every location. google_secret_manager_regional_secret, its
	// _version and _iam_member, _binding and _policy apply, plan clean,
	// update and destroy in TestTerraformRegionalSecret.
	{config.ServiceSecrets, "secret_manager_regional_custom_endpoint", "GOOGLE_SECRET_MANAGER_REGIONAL_CUSTOM_ENDPOINT", "/v1/", true, ""},
	{config.ServiceRun, "cloud_run_v2_custom_endpoint", "GOOGLE_CLOUD_RUN_V2_CUSTOM_ENDPOINT", "/v2/", true, ""},
	// Cloud Run v1 (#1293, #1304), the Knative-style API on the same port:
	// google_cloud_run_domain_mapping applies, becomes Ready, plans clean
	// and destroys in TestTerraformCloudRunDomainMapping. Its URLs are
	// regional, {{location}}-run.googleapis.com, and a v1 write needs a
	// region, so the host is {{location}}-run.localhost on loopback.
	{config.ServiceRun, "cloud_run_custom_endpoint", "GOOGLE_CLOUD_RUN_CUSTOM_ENDPOINT", "/", true, "run"},
	// Artifact Registry and Cloud Build v1 (#1304) are served on the Run
	// port beside Cloud Run (#1347, SourceDeployHandler).
	// google_artifact_registry_repository applies, plans clean, updates in
	// place and destroys in TestTerraformArtifactRegistryRepository;
	// google_cloudbuild_trigger (manual or webhook) and _worker_pool do in
	// TestTerraformCloudBuildTriggerAndWorkerPool (#1422).
	{config.ServiceRun, "artifact_registry_custom_endpoint", "GOOGLE_ARTIFACT_REGISTRY_CUSTOM_ENDPOINT", "/v1/", true, ""},
	{config.ServiceRun, "cloud_build_custom_endpoint", "GOOGLE_CLOUD_BUILD_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// Cloud Functions v2 (#1377), on the Run port: a function is a
	// buildpacks build and a Cloud Run service. google_cloudfunctions2_function
	// applies, plans clean, updates in place and destroys in
	// TestTerraformCloudFunctions2Function, gated as the Functions tests are.
	{config.ServiceRun, "cloudfunctions2_custom_endpoint", "GOOGLE_CLOUDFUNCTIONS2_CUSTOM_ENDPOINT", "/v2/", true, ""},
	// Cloud Functions v1 (#1428), on the same port over the same machinery:
	// google_cloudfunctions_function reaches this instance, never Google:
	// TestTerraformCloudFunctionsFunction applies an HTTP and an event
	// function, gated as the Functions tests are.
	{config.ServiceRun, "cloud_functions_custom_endpoint", "GOOGLE_CLOUD_FUNCTIONS_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// Cloud KMS (#425): a key ring, a key and a version apply, plan clean,
	// update their labels and destroy in TestTerraformKMS, over the JSON API
	// (#422-#424). The provider's discovery client strips the version from
	// the base path and appends v1/ itself, so both styles land on /v1/.
	{config.ServiceKMS, "kms_custom_endpoint", "GOOGLE_KMS_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// Cloud Scheduler (#591): a google_cloud_scheduler_job applies, plans
	// clean, updates (a PATCH, then :pause) and destroys in
	// TestTerraformSchedulerAndSubscription, over the JSON API on the gRPC
	// port.
	{config.ServiceScheduler, "cloud_scheduler_custom_endpoint", "GOOGLE_CLOUD_SCHEDULER_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// BigQuery (#1236): a dataset, a table, a view and a routine, with the
	// table, view and routine *_iam_member, *_iam_binding and *_iam_policy
	// (ADR-0006, stored, never enforced), apply, plan clean and destroy in
	// TestTerraformBigQueryIAM, through the front on the REST port. The
	// provider's argument is big_query_, not bigquery_.
	{config.ServiceBigQuery, "big_query_custom_endpoint", "GOOGLE_BIG_QUERY_CUSTOM_ENDPOINT", "/bigquery/v2/", true, ""},
	// Cloud Logging (#1246): a google_logging_metric, a
	// google_logging_project_sink and a google_logging_project_exclusion
	// apply, plan clean, update and destroy in TestTerraformLogging, over the
	// JSON API on the gRPC port (#591), against the metrics, sinks and
	// exclusions #1226 serves. Buckets, views, links and settings stay 501.
	{config.ServiceLogging, "logging_custom_endpoint", "GOOGLE_LOGGING_CUSTOM_ENDPOINT", "/v2/", true, ""},
	// google_project (#301): Resource Manager v1, and the Cloud Billing read
	// the provider makes on every refresh, both served on the Resource
	// Manager port.
	// The Cloud SQL Admin API (#1301): a google_sql_database and a
	// google_sql_user on an instance `up` runs apply, plan clean and destroy
	// in TestTerraformSQLDatabaseAndUser. The provider calls v1beta4.
	{sqlAdminEndpoint, "sql_custom_endpoint", "GOOGLE_SQL_CUSTOM_ENDPOINT", "/sql/v1beta4/", true, ""},
	// The Backup and DR Service (#1433): google_backup_dr_backup_vault,
	// _backup_plan and _backup_plan_association apply, plan clean and
	// destroy in TestTerraformBackupDR.
	{config.ServiceBackupDR, "backup_dr_custom_endpoint", "GOOGLE_BACKUP_DR_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// Firestore (#1331): google_firestore_document applies, plans clean,
	// updates its fields and destroys in TestTerraformFirestoreDocument,
	// over the documents REST API the emulator serves on its port. On the
	// same port the call front serves the Firestore Admin API itself
	// (internal/firestoreadmin): google_firestore_database, _index and
	// _field apply, plan clean, update and destroy in
	// TestTerraformFirestoreDatabaseIndexAndField; _backup_schedule answers
	// 501, locally, never at Google.
	{config.ServiceFirestore, "firestore_custom_endpoint", "GOOGLE_FIRESTORE_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// Spanner (#1331): google_spanner_instance and _database apply, read
	// back through the official admin clients, plan clean, update in place
	// and destroy in TestTerraformSpannerInstanceAndDatabase, over the
	// emulator's REST API, Spanner's second port (Endpoints.SpannerREST),
	// which the call front in its pod serves.
	{config.ServiceSpanner, "spanner_custom_endpoint", "GOOGLE_SPANNER_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// Bigtable's admin REST API (#1440), on the Bigtable port beside gRPC
	// (internal/bigtablefront/rest.go): google_bigtable_app_profile and
	// google_bigtable_instance_iam_member, _binding and _policy apply, plan
	// clean, update and destroy in TestTerraformBigtableAppProfileAndIAM.
	// The provider's instance, table and GC policy resources use the gRPC
	// clients, which BIGTABLE_EMULATOR_HOST points here (bigtableHost).
	{config.ServiceBigtable, "bigtable_custom_endpoint", "GOOGLE_BIGTABLE_CUSTOM_ENDPOINT", "/v2/", true, ""},
	// Memorystore for Redis (#1302): a google_redis_instance applies, is
	// reached with its AUTH string, plans clean, updates its size and labels
	// in place and destroys in TestTerraformRedisInstance, over the admin
	// API's JSON on its port (Endpoints.MemorystoreAdmin), each instance a
	// Valkey server of its own.
	{memorystoreAdminEndpoint, "redis_custom_endpoint", "GOOGLE_REDIS_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// Memorystore for Valkey (#1519): google_memorystore_instance applies,
	// plans clean, changes and destroys in TestTerraformMemorystoreInstance,
	// over memorystore.googleapis.com's JSON, served under /memorystore/ on
	// the same port.
	{memorystoreAdminEndpoint, "memorystore_custom_endpoint", "GOOGLE_MEMORYSTORE_CUSTOM_ENDPOINT", "/memorystore/v1/", true, ""},
	// google_project_service (#1352): the Service Usage API v1 on its own
	// port, in TestTerraformProjectService.
	{serviceUsageService, "service_usage_custom_endpoint", "GOOGLE_SERVICE_USAGE_CUSTOM_ENDPOINT", "/v1/", true, ""},
	{resourceManagerEndpoint, "resource_manager_custom_endpoint", "GOOGLE_RESOURCE_MANAGER_CUSTOM_ENDPOINT", "/v1/", true, ""},
	{resourceManagerEndpoint, "cloud_billing_custom_endpoint", "GOOGLE_CLOUD_BILLING_CUSTOM_ENDPOINT", "/v1/", true, ""},
	// google_folder (#1294): Resource Manager v3 Folders, on the same port,
	// in TestTerraformGoogleFolder.
	{resourceManagerEndpoint, "resource_manager_v3_custom_endpoint", "GOOGLE_RESOURCE_MANAGER_V3_CUSTOM_ENDPOINT", "/v3/", true, ""},
	// google_tags_tag_key, _tag_value and _tag_binding (#1295): the tags
	// services are Resource Manager v3, which the provider reaches through
	// its own tags_ endpoint, in TestTerraformTagsAndLien.
	{resourceManagerEndpoint, "tags_custom_endpoint", "GOOGLE_TAGS_CUSTOM_ENDPOINT", "/v3/", true, ""},
}

// resourceManagerEndpoint keys the Resource Manager rows. It is not a
// selectable service: the project registry, and so its API, always runs.
const resourceManagerEndpoint config.Service = "resourcemanager"

// sqlAdminEndpoint keys the Cloud SQL Admin API row: it is served when
// either Cloud SQL service is (#1301), and is not a service of its own.
const sqlAdminEndpoint config.Service = "sqladmin"

// memorystoreAdminEndpoint keys the Memorystore admin API's rows (#1302),
// apart from the RESP server's "memorystore": it is the CLI's own port,
// published to pods as cloudburrow-host's "memorystore-admin".
const memorystoreAdminEndpoint config.Service = "memorystore-admin"

// terraformAccessToken is what the provider presents. CloudBurrow
// authenticates nothing, so any token works for its own APIs; a fixed one
// keeps the provider from looking for credentials of its own.
//
// It is "owner" (#1331) because the Firestore emulator does read the token:
// any other bearer must be a JWT, or it answers 400 "invalid jwt". "owner"
// is the emulator's documented admin token, the one the official clients
// send it when FIRESTORE_EMULATOR_HOST is set. It is no Google credential.
const terraformAccessToken = "owner"

// generatedMarker heads every file this writes, so one left by a killed run
// is recognised as ours and a user's file of the same name never is.
const generatedMarker = "# Generated by `cloudburrow terraform`. Removed when it exits; do not edit."

// terraformURL is one endpoint the provider is given.
type terraformURL struct {
	terraformEndpoint
	url string
}

// terraformSettings resolves the table against an instance: the endpoints to
// set, and the enabled services deliberately left out. Both the provider file
// and `env --format terraform` are rendered from this, so they cannot differ.
func terraformSettings(cfg config.Config) ([]terraformURL, []config.Service) {
	host := func(port int) string { return net.JoinHostPort(cfg.BindAddress, strconv.Itoa(port)) }
	ports := map[config.Service]int{
		config.ServiceStorage: cfg.Endpoints.Storage, config.ServicePubSub: cfg.Endpoints.PubSub,
		config.ServiceTasks: cfg.Endpoints.Tasks, config.ServiceSecrets: cfg.Endpoints.Secrets, config.ServiceRun: cfg.Endpoints.Run,
		config.ServiceKMS: cfg.Endpoints.KMS, resourceManagerEndpoint: cfg.Endpoints.ResourceManager,
		serviceUsageService:     cfg.Endpoints.ServiceUsage,
		config.ServiceScheduler: cfg.Endpoints.Scheduler, config.ServiceLogging: cfg.Endpoints.Logging,
		config.ServiceBigQuery: cfg.Endpoints.BigQuery, sqlAdminEndpoint: cfg.Endpoints.SQLAdmin,
		config.ServiceFirestore: cfg.Endpoints.Firestore,
		// The REST port: the provider's Spanner resources use REST, the
		// client libraries the gRPC one.
		config.ServiceSpanner: cfg.Endpoints.SpannerREST,
		// The front serves the admin REST API on the gRPC port.
		config.ServiceBigtable: cfg.Endpoints.Bigtable,
		// The admin API's port, not the RESP one.
		memorystoreAdminEndpoint: cfg.Endpoints.MemorystoreAdmin,
		config.ServiceBackupDR:   cfg.Endpoints.BackupDR,
	}
	enabled := map[config.Service]bool{resourceManagerEndpoint: true, serviceUsageService: true, sqlAdminEndpoint: sqlAdminEnabled(cfg),
		memorystoreAdminEndpoint: serviceEnabled(cfg, config.ServiceMemorystore)}
	for _, s := range cfg.EnabledServices() {
		enabled[s] = true
	}
	enabled[config.ServiceBackupDR] = backupDREnabled(cfg)
	var set []terraformURL
	var skipped []config.Service
	for _, e := range terraformEndpoints {
		if !enabled[e.service] {
			continue
		}
		if !e.verified || ports[e.service] == 0 {
			skipped = append(skipped, e.service)
			continue
		}
		addr := host(ports[e.service])
		if ip := net.ParseIP(cfg.BindAddress); e.regional != "" && ip != nil && ip.IsLoopback() {
			addr = net.JoinHostPort("{{location}}-"+e.regional+".localhost", strconv.Itoa(ports[e.service]))
		}
		set = append(set, terraformURL{e, "http://" + addr + e.path})
	}
	return set, skipped
}

// terraformProvider renders the provider configuration and names the
// enabled services it deliberately left out.
func terraformProvider(cfg config.Config, override bool) (string, []config.Service) {
	set, skipped := terraformSettings(cfg)
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n# Instance %q.\n", generatedMarker, cfg.Name)
	b.WriteString("provider \"google\" {\n")
	fmt.Fprintf(&b, "  project      = %q\n", cfg.DefaultProject())
	fmt.Fprintf(&b, "  access_token = %q\n", terraformAccessToken)
	if override {
		// An override merges into the module's own block, and the provider
		// refuses credentials alongside access_token.
		b.WriteString("  credentials  = null\n")
	}
	for _, e := range set {
		fmt.Fprintf(&b, "  %s = %q\n", e.attr, e.url)
	}
	b.WriteString("}\n")
	return b.String(), skipped
}

// unpointedPrefix is a family of the provider's resources and data sources
// the wrapper cannot point at the instance (#1331).
type unpointedPrefix struct{ prefix, why string }

// unpointedResources are the families the provider would send to real
// Google for this instance, whatever is enabled, so the wrapper refuses to
// run a module that declares one rather than warn and let it through.
//
// Firestore, Spanner and Bigtable, and Artifact Registry and Cloud Build,
// when the instance does not serve them (Spanner's REST port among it),
// since nothing then points them here; Cloud Build v2 and the
// google_storage_buckets, google_projects and google_folders data sources
// always.
func unpointedResources(cfg config.Config) []unpointedPrefix {
	var out []unpointedPrefix
	set, _ := terraformSettings(cfg)
	firestore, spanner := false, false
	for _, e := range set {
		firestore = firestore || e.service == config.ServiceFirestore
		spanner = spanner || e.service == config.ServiceSpanner
	}
	if !firestore {
		out = append(out, unpointedPrefix{"google_firestore_", "this instance does not serve Firestore"})
	}
	if !spanner {
		out = append(out, unpointedPrefix{"google_spanner_", "this instance does not serve Spanner's REST API"})
	}
	if bigtableHost(cfg) == "" {
		out = append(out, unpointedPrefix{"google_bigtable_", "this instance does not serve Bigtable"})
	}
	run := false
	for _, e := range set {
		run = run || e.service == config.ServiceRun
	}
	if !run {
		out = append(out,
			unpointedPrefix{"google_cloud_run_", "this instance does not serve Cloud Run"},
			unpointedPrefix{"google_artifact_registry_", "this instance does not serve Artifact Registry (it is on the Run port)"},
			unpointedPrefix{"google_cloudbuild_", "this instance does not serve Cloud Build (it is on the Run port)"},
			unpointedPrefix{"google_cloudfunctions2_", "this instance does not serve Cloud Functions (it is on the Run port)"},
			unpointedPrefix{"google_cloudfunctions_", "this instance does not serve Cloud Functions (it is on the Run port)"})
	}
	// Whatever is enabled: no instance serves Cloud Build v2's repository
	// connections, so the provider has no local endpoint for them (#1304).
	// Cloud Functions v2 (#1377) and v1 (#1428) are served on the Run port.
	// Three list data sources call a fixed Google URL whatever the
	// provider's custom endpoints say (measured with hashicorp/google 8.4.0,
	// #1304): google_storage_buckets lists at
	// https://storage.googleapis.com/storage/v1/b, google_projects at
	// https://cloudresourcemanager.googleapis.com/v1/projects and
	// google_folders at https://cloudresourcemanager.googleapis.com/v3/folders,
	// so no setting points them here.
	out = append(out,
		unpointedPrefix{"google_cloudbuildv2_", "CloudBurrow does not serve Cloud Build v2 connections"},
		unpointedPrefix{"google_storage_buckets", "the provider lists buckets at storage.googleapis.com, ignoring storage_custom_endpoint"},
		unpointedPrefix{"google_projects", "the provider lists projects at cloudresourcemanager.googleapis.com, ignoring resource_manager_custom_endpoint"},
		unpointedPrefix{"google_folders", "the provider lists folders at cloudresourcemanager.googleapis.com, ignoring resource_manager_v3_custom_endpoint"})
	return out
}

// bigtableHost is the address the provider's Bigtable clients are given, or
// "" when the instance does not serve Bigtable. The provider's admin and
// data clients are the Go bigtable clients, which read
// BIGTABLE_EMULATOR_HOST and then dial it in plaintext, without
// credentials, as they dial the emulator for any program; the wrapper sets
// it for the binary it runs.
func bigtableHost(cfg config.Config) string {
	for _, s := range cfg.EnabledServices() {
		if s == config.ServiceBigtable && cfg.Endpoints.Bigtable != 0 {
			return net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Endpoints.Bigtable))
		}
	}
	return ""
}

var declaredResource = regexp.MustCompile(`(?m)^\s*(?:resource|data)\s+"(google_[a-z0-9_]+)"`)

// unpointed names the resources and data sources in the module's *.tf files
// that would reach real Google, each with the reason.
func unpointed(dir string, families []unpointedPrefix) []string {
	files, _ := filepath.Glob(filepath.Join(dir, "*.tf"))
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, m := range declaredResource.FindAllSubmatch(b, -1) {
			typ := string(m[1])
			for _, u := range families {
				if strings.HasPrefix(typ, u.prefix) && !seen[typ] {
					seen[typ] = true
					out = append(out, typ+" ("+u.why+")")
				}
			}
		}
	}
	return out
}

// offlineSubcommands never call a provider's API, so a module with an
// unpointed resource may still run them.
var offlineSubcommands = map[string]bool{"init": true, "fmt": true, "validate": true, "version": true, "providers": true, "get": true}

// subcommand is the first argument that is not a global flag.
func subcommand(theirs []string) string {
	for _, a := range theirs {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

var googleProviderBlock = regexp.MustCompile(`(?m)^\s*provider\s+"google"\s*\{`)

// moduleDeclaresGoogle reports whether the module already configures the
// google provider. An override file needs a block to merge into, and a plain
// one would duplicate an existing block, so which file is written depends on
// the answer.
func moduleDeclaresGoogle(dir string) bool {
	files, _ := filepath.Glob(filepath.Join(dir, "*.tf"))
	for _, f := range files {
		if strings.HasPrefix(filepath.Base(f), "cloudburrow_providers") {
			continue
		}
		if b, err := os.ReadFile(f); err == nil && googleProviderBlock.Match(b) {
			return true
		}
	}
	return false
}

// terraformArgs splits `cloudburrow terraform [flags] [--] args...`.
// CloudBurrow's own flags come before "--". Without one, every argument is
// the binary's, and the instance is chosen by the environment and config
// file, as for any command.
func terraformArgs(args []string) (ours, theirs []string, binary string, err error) {
	for i, a := range args {
		if a == "--" {
			ours, theirs = args[:i], args[i+1:]
			break
		}
	}
	if theirs == nil {
		theirs, ours = args, nil
	}
	v, found, ours, err := splitFlag(ours, "binary", false)
	if err != nil {
		return nil, nil, "", err
	}
	binary = "terraform"
	if found {
		binary = v
	}
	return ours, theirs, binary, nil
}

// moduleDir is the directory Terraform will read, honouring -chdir.
func moduleDir(theirs []string) string {
	for _, a := range theirs {
		if dir, ok := strings.CutPrefix(a, "-chdir="); ok {
			return dir
		}
		if !strings.HasPrefix(a, "-") {
			break // the subcommand: -chdir must come before it
		}
	}
	return "."
}

func runTerraform(args []string, stdout, stderr io.Writer) error {
	ours, theirs, binary, err := terraformArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return errUsage
	}
	cfg, err := config.Load(config.Options{Args: ours, Output: stderr})
	if err != nil {
		return err
	}
	if info, ok := running(cfg); ok {
		// The services the instance serves, as `env` takes them (#652):
		// `--name x` alone would otherwise give the default services, and
		// an opt-in one such as BigQuery would get no endpoint (#1236).
		cfg = withLivePorts(servedConfig(cfg, info, true), info.Endpoints)
	}

	dir := moduleDir(theirs)
	if bad := unpointed(dir, unpointedResources(cfg)); len(bad) != 0 && !offlineSubcommands[subcommand(theirs)] {
		return fmt.Errorf("refusing to run %s %s: the module declares resources cloudburrow cannot point at this instance, "+
			"which the provider would send to real Google:\n  %s", binary, subcommand(theirs), strings.Join(bad, "\n  "))
	}
	override := moduleDeclaresGoogle(dir)
	name := "cloudburrow_providers.tf"
	if override {
		name = "cloudburrow_providers_override.tf"
	}
	path := filepath.Join(dir, name)
	if b, err := os.ReadFile(path); err == nil && !strings.HasPrefix(string(b), generatedMarker) {
		return fmt.Errorf("%s exists and was not written by cloudburrow; refusing to replace it", path)
	}
	body, skipped := terraformProvider(cfg, override)
	for _, s := range skipped {
		fmt.Fprintf(stderr, "cloudburrow terraform: %s is enabled but has no Verified Terraform support, "+
			"so no endpoint is set for it; resources for it would go to real Google\n", s)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return err
	}
	defer func() { _ = os.Remove(path) }()

	cmd := exec.Command(binary, theirs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
	if h := bigtableHost(cfg); h != "" {
		cmd.Env = append(os.Environ(), "BIGTABLE_EMULATOR_HOST="+h)
	}
	// Terraform handles Ctrl-C itself: the terminal delivers SIGINT to it
	// directly, and it stops cleanly and saves state. This process only has
	// to outlive it, so the file is removed afterwards rather than under a
	// run still reading it. SIGTERM, sent to this process alone, is passed on.
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("run %s: %w", binary, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case s := <-sigs:
			if s == syscall.SIGTERM {
				_ = cmd.Process.Signal(syscall.SIGTERM)
			}
		case err := <-done:
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				// The binary's own status, and its own message already shown.
				return &exitError{code: exit.ExitCode(), err: err, quiet: true}
			}
			return err
		}
	}
}
