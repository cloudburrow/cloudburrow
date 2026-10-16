// Command coverage generates per-service API coverage from the proto surface
// (#288).
//
// Every RPC of the services CloudBurrow serves is listed from the Go proto
// descriptors, not from a hand-written table, and classified:
//
//   - Verified: a compat test annotated `// covers: <Service>/<Method>`
//     exercises it through an official SDK.
//   - Unimplemented: the registry says so, and servers_test.go proves the
//     in-process server returns codes.Unimplemented for it.
//   - Implemented: the registry says CloudBurrow implements it, and no
//     official-SDK test proves it yet.
//   - Not served: the service is not registered on any port.
//   - Unknown: an upstream emulator's method no annotated test covers.
//
// Firestore, Datastore, Bigtable and Spanner are the opt-in emulators, which
// are upstream-backed like Pub/Sub (#717).
//
// Storage's JSON API has no proto the Go client exposes, so its methods come
// from the discovery document's method table the server itself is built
// from (internal/service/storage), which also says which are built (#520).
//
// BigQuery's REST API has no proto either. Its methods are the official
// generated client's (google.golang.org/api/bigquery/v2), read from its
// Service by reflection, so a method Google adds appears as Unknown. The
// emulator behind the validating front is upstream-backed, so a BigQuery
// method is Verified only by an annotated official-client test (#993).
//
//	go run ./tools/coverage          # write docs/coverage
//	go run ./tools/coverage -check   # fail if docs/coverage is stale or an annotation is wrong
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	rpccode "google.golang.org/genproto/googleapis/rpc/code"

	gcs "github.com/cloudburrow/cloudburrow/internal/service/storage"
	bqv2 "google.golang.org/api/bigquery/v2"
	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	runv1 "google.golang.org/api/run/v1"

	serviceusagesvc "github.com/cloudburrow/cloudburrow/internal/service/serviceusage"
	sqladminsvc "github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"

	_ "cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	_ "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	_ "cloud.google.com/go/bigtable/admin/apiv2/adminpb"
	_ "cloud.google.com/go/bigtable/apiv2/bigtablepb"
	_ "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	_ "cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	_ "cloud.google.com/go/datastore/apiv1/datastorepb"
	_ "cloud.google.com/go/firestore/apiv1/admin/adminpb"
	_ "cloud.google.com/go/firestore/apiv1/firestorepb"
	_ "cloud.google.com/go/functions/apiv1/functionspb"
	_ "cloud.google.com/go/functions/apiv2/functionspb"
	_ "cloud.google.com/go/iam/apiv1/iampb"
	_ "cloud.google.com/go/kms/apiv1/kmspb"
	_ "cloud.google.com/go/logging/apiv2/loggingpb"
	_ "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	_ "cloud.google.com/go/redis/apiv1/redispb"
	_ "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	_ "cloud.google.com/go/run/apiv2/runpb"
	_ "cloud.google.com/go/scheduler/apiv1/schedulerpb"
	_ "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	_ "cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	_ "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	_ "cloud.google.com/go/spanner/apiv1/spannerpb"
	_ "google.golang.org/genproto/googleapis/cloud/location"
)

// area is one generated page.
type area struct {
	Key, Title string
	// Own is true for services CloudBurrow implements: every method must
	// have a registry entry. Upstream-backed ones are classified by
	// annotations alone.
	Own bool
	// Services are fully qualified proto service names, or, with Package,
	// every service in that package.
	Services []string
	Package  string
	// Methods, for an area with no proto (Storage's JSON API).
	Methods []string
	// Extra are methods beside its Services' that an area serves under
	// another name, such as a mixin another area lists.
	Extra []string
}

var areas = []area{
	{Key: "tasks", Title: "Cloud Tasks", Own: true, Services: []string{"google.cloud.tasks.v2.CloudTasks"}},
	{Key: "secretmanager", Title: "Secret Manager", Own: true, Services: []string{"google.cloud.secretmanager.v1.SecretManagerService"}},
	{Key: "run", Title: "Cloud Run v2", Own: true, Package: "google.cloud.run.v2"},
	// Served on the Run port for `gcloud run deploy --source` (#1347): the
	// repository calls and the builds SubmitBuild runs.
	{Key: "artifactregistry", Title: "Artifact Registry", Own: true, Services: []string{"google.devtools.artifactregistry.v1.ArtifactRegistry"}},
	{Key: "cloudbuild", Title: "Cloud Build", Own: true, Services: []string{"google.devtools.cloudbuild.v1.CloudBuild"}},
	// Cloud Functions v2 (#1377), on the Run port: a function is a
	// SubmitBuild buildpacks build and a Cloud Run service. Its IAM and
	// Locations mixins are KMS's keys, which are global.
	{Key: "functions", Title: "Cloud Functions v2", Own: true, Services: []string{"google.cloud.functions.v2.FunctionService"}},
	{Key: "functionsv1", Title: "Cloud Functions v1", Own: true, Services: []string{"google.cloud.functions.v1.CloudFunctionsService"}},
	// The v1 (Knative-shaped) API gcloud run calls (#1293): no proto, so
	// its methods are the official generated client's, and a method is
	// Verified only by an annotated official-client test.
	{Key: "runv1", Title: "Cloud Run v1 (Knative API)", Methods: discoveryMethods("run", reflect.TypeOf(runv1.APIService{}))},
	// google.iam.v1.IAMPolicy is the mixin cloudkms.googleapis.com serves
	// (#428). Mixin keys are global, so only one area may list it.
	// google.cloud.location.Locations too (#1291); Cloud Scheduler lists it
	// as well, and both serve it, so the one registry status is true of both.
	{Key: "kms", Title: "Cloud KMS", Own: true, Services: []string{"google.cloud.kms.v1.KeyManagementService", "google.iam.v1.IAMPolicy", "google.cloud.location.Locations"}},
	// Liens (#1295) have no gRPC service: they are on resourcemanagerv1.
	{Key: "resourcemanager", Title: "Resource Manager v3 (Projects, Folders, Organizations, Tags)", Own: true, Services: []string{"google.cloud.resourcemanager.v3.Projects",
		"google.cloud.resourcemanager.v3.Folders", "google.cloud.resourcemanager.v3.Organizations", "google.cloud.resourcemanager.v3.TagKeys",
		"google.cloud.resourcemanager.v3.TagValues", "google.cloud.resourcemanager.v3.TagBindings", "google.cloud.resourcemanager.v3.TagHolds"}},
	// Resource Manager's JSON-only surface: v1 REST, which gcloud projects
	// and Terraform's google_project and google_project_organization_policy
	// call (#301, #1296), and liens (#1295), which have no gRPC service. A
	// lien's v3 REST method has the same discovery ID as v1's
	// (cloudresourcemanager.liens.create) and is served by the same handler
	// on both paths, so v3's liens are these rows too. No proto, so the
	// methods are the official generated client's, and a method is Verified
	// only by an annotated official-client test, as runv1's are.
	{Key: "resourcemanagerv1", Title: "Resource Manager v1 REST (projects, org policies; liens on v1 and v3)",
		Methods: discoveryMethods("cloudresourcemanager", reflect.TypeOf(crmv1.Service{}))},
	// google.cloud.location.Locations is the mixin cloudscheduler.googleapis.com
	// serves (#1237); Cloud KMS serves it too (#1291).
	{Key: "scheduler", Title: "Cloud Scheduler", Own: true, Services: []string{"google.cloud.scheduler.v1.CloudScheduler", "google.cloud.location.Locations"}},
	// MetricsServiceV2 and ConfigServiceV2 since #1226: metrics, and project
	// sinks and exclusions, are served; buckets and views since #1264, the
	// Async bucket calls, links and settings since #1292, and CopyLogEntries since #1394.
	{Key: "logging", Title: "Cloud Logging", Own: true, Services: []string{"google.logging.v2.LoggingServiceV2", "google.logging.v2.MetricsServiceV2", "google.logging.v2.ConfigServiceV2"}},
	// The local AI endpoint's PredictionService (#1193).
	{Key: "vertexai", Title: "Vertex AI PredictionService", Own: true, Services: []string{"google.cloud.aiplatform.v1.PredictionService"}},
	// The Memorystore for Redis admin API (#1302), over the one Valkey
	// server --services memorystore runs.
	{Key: "memorystore", Title: "Memorystore for Redis", Own: true, Services: []string{"google.cloud.redis.v1.CloudRedis"}},
	// Pub/Sub's IAM (#1367) is the google.iam.v1.IAMPolicy mixin, which the
	// kms area lists (mixin keys are global), served by the Pub/Sub front
	// over gRPC and REST: its rows are named by the REST methods' discovery
	// IDs, one per resource kind.
	{Key: "pubsub", Title: "Pub/Sub", Services: []string{"google.pubsub.v1.Publisher", "google.pubsub.v1.Subscriber", "google.pubsub.v1.SchemaService"},
		Extra: pubsubIAMMethods()},
	{Key: "storage", Title: "Cloud Storage (JSON API)", Own: true, Methods: storageMethods()},
	// The gRPC API (#1321), served from the JSON API's handlers. Its proto
	// is registered under CloudBurrow's own name, since the CLI links the Go
	// client's copy too, so its methods come from the server's service
	// descriptor, under the name clients call.
	{Key: "storagegrpc", Title: "Cloud Storage (gRPC API v2)", Own: true, Methods: gcs.GRPCMethods()},
	// The opt-in emulators (#717) are Google's, like Pub/Sub's: a method is
	// Verified only by an annotated official-client test, and Unknown
	// otherwise. Bigtable's instance admin service (#1331) is the Bigtable
	// front's own (internal/bigtablefront), classified by annotations too.
	{Key: "firestore", Title: "Firestore", Services: []string{"google.firestore.v1.Firestore"}},
	// The Firestore Admin API (#1331) is CloudBurrow's own, served by the
	// Firestore pod's call front (internal/firestoreadmin): the emulator
	// serves the documents API alone.
	{Key: "firestoreadmin", Title: "Firestore Admin API", Own: true, Services: []string{"google.firestore.admin.v1.FirestoreAdmin"}},
	{Key: "datastore", Title: "Datastore", Services: []string{"google.datastore.v1.Datastore"}},
	{Key: "bigtable", Title: "Bigtable", Services: []string{"google.bigtable.v2.Bigtable", "google.bigtable.admin.v2.BigtableTableAdmin", "google.bigtable.admin.v2.BigtableInstanceAdmin"}},
	{Key: "spanner", Title: "Spanner", Services: []string{"google.spanner.v1.Spanner", "google.spanner.admin.database.v1.DatabaseAdmin", "google.spanner.admin.instance.v1.InstanceAdmin"}},
	// BigQuery (#993): the community emulator behind CloudBurrow's
	// validating front, classified by annotations alone, as the other
	// opt-in emulators are.
	{Key: "bigquery", Title: "BigQuery (REST API v2)", Methods: bigqueryMethods()},
	// The Cloud SQL Admin API (#1301): CloudBurrow's own, over the
	// PostgreSQL and MySQL servers it runs. Its methods are the discovery
	// document's, which the server's own table is (internal/service/sqladmin).
	{Key: "sqladmin", Title: "Cloud SQL Admin API (v1)", Own: true, Methods: sqladminMethods()},
	// The Service Usage API (#1352): CloudBurrow's own, JSON only, as
	// Terraform and gcloud call it. No proto, so its methods are the
	// official generated client's.
	{Key: "serviceusage", Title: "Service Usage API (v1)", Own: true,
		Methods: serviceusageMethods()},
}

// pubsubIAMMethods are Pub/Sub's IAM methods, as discovery IDs:
// pubsub.projects.topics.getIamPolicy and so on.
func pubsubIAMMethods() []string {
	var out []string
	for _, kind := range []string{"schemas", "snapshots", "subscriptions", "topics"} {
		for _, m := range []string{"getIamPolicy", "setIamPolicy", "testIamPermissions"} {
			out = append(out, "pubsub.projects."+kind+"."+m)
		}
	}
	return out
}

// serviceusageMethods is every method of the serviceusage v1 discovery
// document, from the server's own table (internal/service/serviceusage).
func serviceusageMethods() []string {
	var out []string
	for id := range serviceusagesvc.MethodStatuses() {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// sqladminMethods is every method of the sqladmin v1 discovery document.
func sqladminMethods() []string {
	var out []string
	for id := range sqladminsvc.MethodStatuses() {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// bigqueryMethods is every method of the official generated BigQuery client,
// as its discovery ID: bigquery.<resource>.<method>, such as
// bigquery.jobs.insert. Each resource is a *XService field of bqv2.Service,
// and each of its exported methods is one API method.
func bigqueryMethods() []string {
	lower := func(s string) string { return strings.ToLower(s[:1]) + s[1:] }
	var out []string
	svc := reflect.TypeOf(bqv2.Service{})
	for i := 0; i < svc.NumField(); i++ {
		f := svc.Field(i)
		if !f.IsExported() || f.Type.Kind() != reflect.Pointer || !strings.HasSuffix(f.Type.Elem().Name(), "Service") {
			continue
		}
		for j := 0; j < f.Type.NumMethod(); j++ {
			out = append(out, "bigquery."+lower(f.Name)+"."+lower(f.Type.Method(j).Name))
		}
	}
	sort.Strings(out)
	return out
}

// discoveryMethods is every method of a generated google.golang.org/api
// client, as its discovery ID: <api>.<resource>[.<resource>...].<method>,
// such as run.namespaces.services.list. Resources nest as *XService fields.
func discoveryMethods(api string, svc reflect.Type) []string {
	lower := func(s string) string { return strings.ToLower(s[:1]) + s[1:] }
	var out []string
	var walk func(prefix string, t reflect.Type)
	walk = func(prefix string, t reflect.Type) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() || f.Type.Kind() != reflect.Pointer || !strings.HasSuffix(f.Type.Elem().Name(), "Service") {
				continue
			}
			p := prefix + "." + lower(f.Name)
			for j := 0; j < f.Type.NumMethod(); j++ {
				out = append(out, p+"."+lower(f.Type.Method(j).Name))
			}
			walk(p, f.Type.Elem())
		}
	}
	walk(api, svc)
	sort.Strings(out)
	return out
}

// storageMethods is every JSON API method of the discovery document.
func storageMethods() []string {
	var out []string
	for id := range gcs.MethodStatuses() {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Registry statuses.
const (
	regImplemented   = "implemented"
	regUnimplemented = "unimplemented"
	regNotServed     = "not-served"
)

// Row is one method's classification.
type Row struct {
	Method string   `json:"method"`
	Status string   `json:"status"`
	Tests  []string `json:"tests,omitempty"` // file.go:line FuncName
	Note   string   `json:"note,omitempty"`
	// Unverified are the error codes a test asserts for this method that
	// Google does not document: implemented as the most plausible code,
	// and labelled until a recorded observation pins them.
	Unverified []Unverified `json:"unverified,omitempty"`
}

// Unverified is one `// unverified:` claim: a code asserted, not known.
type Unverified struct {
	Code string `json:"code"`
	Case string `json:"case"`
	Test string `json:"test"` // path:line FuncName
}

// Page is one area's rows.
type Page struct {
	Key, Title string
	Rows       []Row
}

func main() {
	check := flag.Bool("check", false, "fail instead of writing when docs/coverage is stale")
	root := flag.String("root", ".", "repository root")
	flag.Parse()
	pages, err := build(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "coverage:", err)
		os.Exit(1)
	}
	files := render(pages)
	stale := 0
	for name, want := range files {
		path := filepath.Join(*root, "docs", "coverage", name)
		if *check {
			if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
				fmt.Fprintf(os.Stderr, "coverage: %s is stale; run `go run ./tools/coverage` and commit it\n", path)
				stale++
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "coverage:", err)
			os.Exit(1)
		}
		if err := os.WriteFile(path, want, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "coverage:", err)
			os.Exit(1)
		}
	}
	if stale > 0 {
		os.Exit(1)
	}
}

// methodsOf lists an area's methods, as Service/Method.
func methodsOf(a area) ([]string, error) {
	if a.Methods != nil {
		return append([]string(nil), a.Methods...), nil
	}
	var sds []protoreflect.ServiceDescriptor
	for _, name := range a.Services {
		d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(name))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		sds = append(sds, d.(protoreflect.ServiceDescriptor))
	}
	if a.Package != "" {
		protoregistry.GlobalFiles.RangeFilesByPackage(protoreflect.FullName(a.Package), func(f protoreflect.FileDescriptor) bool {
			for i := 0; i < f.Services().Len(); i++ {
				sds = append(sds, f.Services().Get(i))
			}
			return true
		})
	}
	out := append([]string(nil), a.Extra...)
	for _, sd := range sds {
		for i := 0; i < sd.Methods().Len(); i++ {
			out = append(out, string(sd.FullName())+"/"+string(sd.Methods().Get(i).Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

// annotation is one `// covers:` claim.
type annotation struct {
	method, test string
	refused      bool // "(unimplemented)": the test proves it is refused
}

var coversLine = regexp.MustCompile(`^//\s*covers:\s*(.+)$`)

// unverifiedLine is `// unverified: <Service>/<Method> <CODE>: <case>` for a
// gRPC method, or `// unverified: <method-id> <HTTP status>: <case>` for a
// Cloud Storage JSON API method (storage.buckets.delete 409: ...), whose
// errors are HTTP statuses, not gRPC codes (#490).
var unverifiedLine = regexp.MustCompile(`^//\s*unverified:\s*(.*)$`)
var unverifiedBody = regexp.MustCompile(`^(\S+/\S+)\s+([A-Z_]+):\s*(\S.*)$`)
var unverifiedHTTPBody = regexp.MustCompile(`^(storage\.[A-Za-z.]+)\s+([0-9]{3}):\s*(\S.*)$`)

// unverifiedAnn is one parsed `// unverified:` line.
type unverifiedAnn struct {
	method string
	Unverified
}

// unverifiedAnnotations reads every `// unverified:` line in the compat
// tests and in the in-process tests under internal/service, where a case
// that needs a fake clock can only be reached. Like covers:, each must be in
// a Test function's doc comment and name a real gRPC code.
func unverifiedAnnotations(root string) ([]unverifiedAnn, error) {
	files, err := filepath.Glob(filepath.Join(root, "test", "compat", "*_test.go"))
	if err != nil {
		return nil, err
	}
	_ = filepath.WalkDir(filepath.Join(root, "internal", "service"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, "_test.go") {
			files = append(files, p)
		}
		return nil
	})
	var out []unverifiedAnn
	var problems []string
	for _, f := range files {
		rel, _ := filepath.Rel(root, f)
		rel = filepath.ToSlash(rel)
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, f, nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		attached := map[*ast.Comment]bool{}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Doc == nil {
				continue
			}
			for _, c := range fn.Doc.List {
				m := unverifiedLine.FindStringSubmatch(c.Text)
				if m == nil {
					continue
				}
				attached[c] = true
				at := fset.Position(c.Pos())
				if !strings.HasPrefix(fn.Name.Name, "Test") {
					problems = append(problems, fmt.Sprintf("%s: unverified: on %s, which is not a test", at, fn.Name.Name))
					continue
				}
				body := strings.TrimSpace(m[1])
				b := unverifiedBody.FindStringSubmatch(body)
				switch {
				case b != nil:
					if _, ok := rpccode.Code_value[b[2]]; !ok {
						problems = append(problems, fmt.Sprintf("%s: unverified: %q is not a gRPC code name", at, b[2]))
						continue
					}
				case unverifiedHTTPBody.MatchString(body):
					b = unverifiedHTTPBody.FindStringSubmatch(body)
					if n, _ := strconv.Atoi(b[2]); http.StatusText(n) == "" {
						problems = append(problems, fmt.Sprintf("%s: unverified: %s is not an HTTP status", at, b[2]))
						continue
					}
				default:
					problems = append(problems, fmt.Sprintf("%s: unverified: must be \"<Service>/<Method> <CODE>: <case>\" or \"storage.<resource>.<method> <HTTP status>: <case>\"", at))
					continue
				}
				pos := fset.Position(fn.Pos())
				out = append(out, unverifiedAnn{method: b[1], Unverified: Unverified{
					Code: b[2], Case: b[3], Test: fmt.Sprintf("%s:%d %s", rel, pos.Line, fn.Name.Name)}})
			}
		}
		for _, g := range file.Comments {
			for _, c := range g.List {
				if unverifiedLine.MatchString(c.Text) && !attached[c] {
					problems = append(problems, fmt.Sprintf("%s: unverified: outside a test function's doc comment", fset.Position(c.Pos())))
				}
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("bad annotations:\n  %s", strings.Join(problems, "\n  "))
	}
	return out, nil
}

// annotations reads every `// covers:` line in test/compat. Each must be in
// the doc comment of a Test function: one anywhere else is an error, since
// it would claim coverage for no test at all.
func annotations(root string) ([]annotation, error) {
	files, err := filepath.Glob(filepath.Join(root, "test", "compat", "*_test.go"))
	if err != nil {
		return nil, err
	}
	var out []annotation
	var problems []string
	for _, f := range files {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, f, nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		attached := map[*ast.Comment]bool{}
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Doc == nil {
				continue
			}
			for _, c := range fn.Doc.List {
				m := coversLine.FindStringSubmatch(c.Text)
				if m == nil {
					continue
				}
				attached[c] = true
				if !strings.HasPrefix(fn.Name.Name, "Test") {
					problems = append(problems, fmt.Sprintf("%s: covers: on %s, which is not a test", fset.Position(c.Pos()), fn.Name.Name))
					continue
				}
				pos := fset.Position(fn.Pos())
				ref := fmt.Sprintf("test/compat/%s:%d %s", filepath.Base(f), pos.Line, fn.Name.Name)
				for _, item := range strings.Split(m[1], ",") {
					item = strings.TrimSpace(item)
					refused := strings.HasSuffix(item, "(unimplemented)")
					item = strings.TrimSpace(strings.TrimSuffix(item, "(unimplemented)"))
					out = append(out, annotation{method: item, test: ref, refused: refused})
				}
			}
		}
		for _, g := range file.Comments {
			for _, c := range g.List {
				if coversLine.MatchString(c.Text) && !attached[c] {
					problems = append(problems, fmt.Sprintf("%s: covers: outside a test function's doc comment", fset.Position(c.Pos())))
				}
			}
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("bad annotations:\n  %s", strings.Join(problems, "\n  "))
	}
	return out, nil
}

func loadRegistry(root string) (map[string]string, error) {
	b, err := os.ReadFile(filepath.Join(root, "tools", "coverage", "registry.json"))
	if err != nil {
		return nil, err
	}
	var r map[string]string
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("registry.json: %w", err)
	}
	// Storage's registry is the server's own method table, so a method moves
	// to implemented in the same change that builds it.
	for id, isBuilt := range gcs.MethodStatuses() {
		if isBuilt {
			r[id] = regImplemented
		} else {
			r[id] = regUnimplemented
		}
	}
	// The Cloud SQL Admin API's too (#1301), and Service Usage's (#1352).
	for _, statuses := range []map[string]bool{sqladminsvc.MethodStatuses(), serviceusagesvc.MethodStatuses()} {
		for id, isBuilt := range statuses {
			if isBuilt {
				r[id] = regImplemented
			} else {
				r[id] = regUnimplemented
			}
		}
	}
	return r, nil
}

func build(root string) ([]Page, error) {
	reg, err := loadRegistry(root)
	if err != nil {
		return nil, err
	}
	anns, err := annotations(root)
	if err != nil {
		return nil, err
	}
	byMethod := map[string][]annotation{}
	for _, a := range anns {
		byMethod[a.method] = append(byMethod[a.method], a)
	}
	unv, err := unverifiedAnnotations(root)
	if err != nil {
		return nil, err
	}
	unverifiedBy := map[string][]Unverified{}
	for _, u := range unv {
		unverifiedBy[u.method] = append(unverifiedBy[u.method], u.Unverified)
	}

	var problems []string
	known := map[string]bool{}
	var pages []Page
	for _, a := range areas {
		methods, err := methodsOf(a)
		if err != nil {
			return nil, err
		}
		p := Page{Key: a.Key, Title: a.Title}
		for _, m := range methods {
			known[m] = true
			row := Row{Method: m}
			var tests []string
			refusedOnly := true
			for _, an := range byMethod[m] {
				tests = append(tests, an.test)
				refusedOnly = refusedOnly && an.refused
			}
			sort.Strings(tests)
			row.Tests = tests
			row.Unverified = unverifiedBy[m]
			sort.Slice(row.Unverified, func(i, j int) bool {
				a, b := row.Unverified[i], row.Unverified[j]
				return a.Code+a.Case+a.Test < b.Code+b.Case+b.Test
			})
			status, inReg := reg[m]
			switch {
			case a.Own && !inReg:
				problems = append(problems, fmt.Sprintf("%s has no registry entry: classify it in tools/coverage/registry.json", m))
			case !a.Own && inReg:
				problems = append(problems, fmt.Sprintf("%s is upstream-backed: only an annotated compat test can classify it", m))
			}
			switch {
			case status == regNotServed:
				row.Status, row.Note = "Not served", "the service is not registered on any port"
			case status == regUnimplemented:
				row.Status, row.Note = "Unimplemented", "returns UNIMPLEMENTED (in-process check)"
				for _, an := range byMethod[m] {
					if !an.refused {
						problems = append(problems, fmt.Sprintf("%s: %s claims to cover %s, which is unimplemented; mark it (unimplemented)", an.test, an.test, m))
					}
				}
			case len(tests) > 0 && refusedOnly:
				row.Status, row.Note = "Refused", "an official-SDK test proves it is refused"
				if a.Own {
					// Only an upstream emulator refuses what the registry
					// cannot speak for; CloudBurrow's own refusals are
					// registry entries, checked in-process.
					problems = append(problems, fmt.Sprintf("%s marks %s (unimplemented), but the registry says implemented", tests[0], m))
				}
			case len(tests) > 0:
				row.Status = "Verified"
				for _, an := range byMethod[m] {
					if an.refused {
						problems = append(problems, fmt.Sprintf("%s marks %s (unimplemented), but the registry says implemented", an.test, m))
					}
				}
			case status == regImplemented:
				row.Status, row.Note = "Implemented", "no official-SDK test proves it yet"
			default:
				row.Status = "Unknown"
			}
			p.Rows = append(p.Rows, row)
		}
		pages = append(pages, p)
	}
	for m := range reg {
		if !known[m] {
			problems = append(problems, fmt.Sprintf("registry entry %s names no method in the proto surface", m))
		}
	}
	for m, as := range byMethod {
		if !known[m] {
			problems = append(problems, fmt.Sprintf("%s covers %s, which is not a method of any listed service", as[0].test, m))
		}
	}
	for m, us := range unverifiedBy {
		if !known[m] {
			problems = append(problems, fmt.Sprintf("%s: unverified: %s is not a method of any listed service", us[0].Test, m))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, fmt.Errorf("%d problem(s):\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	return pages, nil
}

var statusOrder = []string{"Verified", "Refused", "Unimplemented", "Implemented", "Not served", "Unknown"}

func unverifiedCount(p Page) int {
	n := 0
	for _, r := range p.Rows {
		n += len(r.Unverified)
	}
	return n
}

func counts(p Page) map[string]int {
	c := map[string]int{}
	for _, r := range p.Rows {
		c[r.Status]++
	}
	return c
}

func render(pages []Page) map[string][]byte {
	out := map[string][]byte{}
	var readme bytes.Buffer
	readme.WriteString("# API coverage\n\n")
	readme.WriteString("Generated by `go run ./tools/coverage` from the Go proto descriptors, a checked-in registry\n" +
		"(`tools/coverage/registry.json`) and `// covers:` annotations on `test/compat` tests. CI runs\n" +
		"`go run ./tools/coverage -check` and fails when this is stale or an annotation is wrong, and\n" +
		"`tools/coverage/servers_test.go` proves every Unimplemented method returns `codes.Unimplemented`.\n" +
		"Do not edit these files by hand.\n\n")
	readme.WriteString("| Service | RPCs | " + strings.Join(statusOrder, " | ") + " | Unverified codes |\n|---|---|" + strings.Repeat("---|", len(statusOrder)+1) + "\n")
	for _, p := range pages {
		c := counts(p)
		fmt.Fprintf(&readme, "| [%s](%s.md) | %d |", p.Title, p.Key, len(p.Rows))
		for _, s := range statusOrder {
			fmt.Fprintf(&readme, " %d |", c[s])
		}
		fmt.Fprintf(&readme, " %d |\n", unverifiedCount(p))

		var b bytes.Buffer
		fmt.Fprintf(&b, "# %s\n\n", p.Title)
		b.WriteString("Generated by `go run ./tools/coverage`; see [the summary](README.md) for what each status means.\n\n")
		var parts []string
		for _, s := range statusOrder {
			if c[s] > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", c[s], strings.ToLower(s)))
			}
		}
		fmt.Fprintf(&b, "%d methods: %s.\n\n", len(p.Rows), strings.Join(parts, ", "))
		b.WriteString("| Method | Status | Evidence |\n|---|---|---|\n")
		for _, r := range p.Rows {
			var ev []string
			for _, t := range r.Tests {
				fileLine, fn, _ := strings.Cut(t, " ")
				file, line, _ := strings.Cut(fileLine, ":")
				ev = append(ev, fmt.Sprintf("[`%s`](../../%s#L%s)", fn, file, line))
			}
			if r.Note != "" {
				ev = append(ev, r.Note)
			}
			// The call is verified; the code it answers with is not.
			if n := len(r.Unverified); n > 0 && r.Status == "Verified" {
				ev = append(ev, fmt.Sprintf("%d error code(s) UNVERIFIED", n))
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s |\n", r.Method, r.Status, strings.Join(ev, "; "))
		}
		if unverifiedCount(p) > 0 {
			b.WriteString("\n## Unverified error codes\n\n" +
				"Codes these tests assert that Google does not document. Each is the most plausible code, and\n" +
				"stays UNVERIFIED until a recorded observation pins it (see test/compat/README.md).\n\n" +
				"| Method | Code | Case | Test |\n|---|---|---|---|\n")
			for _, r := range p.Rows {
				for _, u := range r.Unverified {
					fileLine, fn, _ := strings.Cut(u.Test, " ")
					file, line, _ := strings.Cut(fileLine, ":")
					fmt.Fprintf(&b, "| `%s` | `%s` | %s | [`%s`](../../%s#L%s) |\n", r.Method, u.Code, u.Case, fn, file, line)
				}
			}
		}
		out[p.Key+".md"] = b.Bytes()
	}
	readme.WriteString("\n**Statuses.** *Verified*: an official-SDK compat test, linked, exercises it. *Refused*: a compat\n" +
		"test proves the upstream emulator refuses it. *Unimplemented*: CloudBurrow returns `UNIMPLEMENTED`, proven\n" +
		"in-process. *Implemented*: served, with no official-SDK test yet. *Not served*: not registered on any port.\n" +
		"*Unknown*: an upstream-backed method no test covers, so nothing is claimed. *Unverified codes*: error codes a\n" +
		"test asserts that Google does not document (`// unverified:`), listed on each service's page.\n")
	out["README.md"] = readme.Bytes()

	type jsonPage struct {
		Key, Title string
		Counts     map[string]int `json:"counts"`
		Rows       []Row          `json:"rows"`
	}
	var js []jsonPage
	for _, p := range pages {
		js = append(js, jsonPage{p.Key, p.Title, counts(p), p.Rows})
	}
	b, _ := json.MarshalIndent(js, "", "  ")
	out["coverage.json"] = append(b, '\n')
	return out
}
