package main

import (
	functionspb "cloud.google.com/go/functions/apiv2/functionspb"
	"cloud.google.com/go/kms/apiv1/kmspb"
	"google.golang.org/grpc"
	"time"

	cbpb "cloud.google.com/go/cloudbuild/apiv1/v2/cloudbuildpb"
	ssmpb "cloud.google.com/go/securesourcemanager/apiv1/securesourcemanagerpb"
	"context"
	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"cloud.google.com/go/bigtable"
	"cloud.google.com/go/bigtable/bttest"
	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"

	arpb "cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/registry"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/secrets"
	"github.com/cloudburrow/cloudburrow/internal/service/storage"
	"github.com/cloudburrow/cloudburrow/internal/service/tasks"
	"github.com/cloudburrow/cloudburrow/internal/store"
	"github.com/google/go-containerregistry/pkg/v1/random"
)

// blankProject is the project every seeded resource belongs to, fresh on
// each run (freshProject) because some of them live in process-wide
// stores.
var blankProject = "blank-check"

// emulatorOnly are the screens whose backend is a container image with no
// in-process equivalent, so this test cannot open their rows. Each is still
// built from the registry, so a provider cannot leave this list by being
// renamed, and a new one is not exempt unless it is added here with a reason.
//
// What they return is still covered at the one place every Detail passes
// through: the server drops a blank Property before it is served
// (TestTheServerDropsBlankProperties).
var emulatorOnly = map[string]string{
	"firestore":            "the Firestore emulator is a Java container; there is no in-process fake",
	"firestore-databases":  "Firestore's databases screen reads the Admin API the emulator pod's call front serves",
	"bigtable-instances":   "the instance admin API is the Bigtable front's, which bttest does not serve (TestConsoleBigtableInstancesScreen drives the front)",
	"datastore":            "the Datastore emulator is a Java container; there is no in-process fake",
	"spanner":              "the screens read the instance and database admin APIs, which spannertest does not serve",
	"cloudsql":             "a PostgreSQL server; there is no in-process one",
	"cloudsql-mysql":       "a MySQL server; there is no in-process one",
	"memorystore":          "a Valkey server per instance; there is no in-process one (unit-tested in consolememorystore_test.go)",
	"memorystore-clusters": "a Redis Cluster of Valkey servers; there is no in-process one (TestConsoleRedisCluster drives the page)",
	"memorystore-valkey":   "Valkey servers per instance; there is no in-process one (TestConsoleValkeyInstances drives the page over fake nodes)",
	"bigquery":             "goccy/bigquery-emulator is a container built on ZetaSQL; there is no in-process fake",
	"bigquery-jobs":        "BigQuery's Job history reads the same emulator, through the validating front",
	"cloudsql-instances":   "the Cloud SQL Admin API over PostgreSQL and MySQL servers; there is no in-process one",
}

// TestNoDetailShowsABlankProperty enforces console parity §4.4: "Only fields
// CloudBurrow actually stores. A field the backend does not hold is absent,
// not blank."
//
// It builds the providers from consoleProviders, the same registry a running
// instance serves, with every service enabled and every tunnel present, so a
// screen added there is walked here without anyone remembering to add it. Each
// provider's backend is its real in-process server or its official fake,
// seeded with the sparsest resources the API accepts, because the sparse ones
// are where an unset field turns into an empty cell. Every row is opened, and
// every row of every section of that page that opens is opened in turn.
//
// A Property whose Value is empty or whitespace fails, naming the provider,
// the resource path and the label. An unset field is either left out or given
// words that say what unset means ("Default (100)", "None"), the pattern
// TestUnsetScalingSettingsSayWhatTheyMean set.
func TestNoDetailShowsABlankProperty(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the cluster screens are driven through a shell-script kubectl")
	}
	ctx := context.Background()
	blankProject = freshProject("blank-check")
	d := blankCheckDeps(t)

	providers := consoleProviders(d, clusterMetrics(d.cfg.KubeconfigPath()), console.NewSeries(console.SeriesLimit, nil))
	seen := map[string]bool{}
	for _, p := range providers {
		seen[p.ID()] = true
		if reason, ok := emulatorOnly[p.ID()]; ok {
			t.Logf("%s: not opened: %s", p.ID(), reason)
			continue
		}
		driller, ok := p.(console.Driller)
		if !ok {
			continue
		}
		if o, ok := p.(console.OptionalDriller); ok && !o.CanDrill() {
			continue
		}
		t.Run(p.ID(), func(t *testing.T) {
			list, err := p.List(ctx, blankProject)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if list.Unavailable != "" {
				t.Fatalf("list unavailable: %s", list.Unavailable)
			}
			if len(list.Items) == 0 {
				t.Fatalf("no rows, so no Detail was opened: seed this provider's backend in blankCheckDeps")
			}
			w := blankWalker{t: t, ctx: ctx, id: p.ID(), driller: driller}
			for _, row := range list.Items {
				path := row.Opens
				if path == nil {
					path = []string{row.Name}
				}
				w.walk(path)
			}
			t.Logf("opened %d pages", w.opened)
		})
	}
	for id := range emulatorOnly {
		if !seen[id] {
			t.Errorf("%s is exempted but is not in the registry; remove it from emulatorOnly", id)
		}
	}
}

// blankWalker opens a page and everything below it that opens.
type blankWalker struct {
	t       *testing.T
	ctx     context.Context
	id      string
	driller console.Driller
	opened  int
}

// walkLimit bounds the pages opened per provider. The seeds are small, so
// reaching it means a listing that opens onto itself.
const walkLimit = 200

func (w *blankWalker) walk(path []string) {
	if w.opened++; w.opened > walkLimit {
		w.t.Fatalf("more than %d pages under %s: a listing opens onto itself", walkLimit, w.id)
	}
	where := w.id + " " + strings.Join(path, "/")
	if testing.Verbose() {
		w.t.Logf("open %s", where)
	}
	d, err := w.driller.Detail(w.ctx, blankProject, path)
	if err != nil {
		w.t.Errorf("%s: %v", where, err)
		return
	}
	if d.Unavailable != "" {
		w.t.Errorf("%s: unavailable: %s", where, d.Unavailable)
		return
	}
	for _, prop := range d.Summary {
		if strings.TrimSpace(prop.Value) == "" {
			w.t.Errorf("%s: summary property %q is blank", where, prop.Label)
		}
	}
	for _, sec := range d.Sections {
		for _, g := range sec.Groups {
			for _, prop := range g.Properties {
				if strings.TrimSpace(prop.Value) == "" {
					w.t.Errorf("%s: tab %q, group %q: property %q is blank", where, sec.Label, g.Heading, prop.Label)
				}
			}
		}
		for _, row := range sec.Listing.Items {
			switch {
			case row.Opens != nil:
				w.walk(row.Opens)
			case sec.Listing.RowsOpenable:
				w.walk(append(append([]string{}, path...), row.Name))
			}
		}
	}
}

// blankCheckDeps is an instance with every service enabled, each backed by
// its in-process server or official fake, and each holding a few resources.
func blankCheckDeps(t *testing.T) consoleDeps {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	cfg := config.Config{
		Name: "blank-check", Project: blankProject, BindAddress: "127.0.0.1",
		StateDir: dir, Mode: config.ModeEphemeral,
		// Every service, the opt-in ones included, so every screen is built.
		Services: config.KnownServices(),
	}
	d := consoleDeps{cfg: cfg}

	// A tunnel for every service, so every provider that is built from one is
	// in the registry. The ones replaced below are served; the rest point at
	// a port nothing listens on, and are the emulatorOnly screens.
	tunnels := map[string]string{}
	for _, s := range config.KnownServices() {
		tunnels[string(s)] = "127.0.0.1:1"
	}

	// Resource Manager: a project with nothing but its ID, and one carrying
	// a label whose value is empty, which the API permits.
	d.projects = resourcemanager.New(store.NewMemory())
	if _, err := d.projects.EnsureExists(blankProject); err != nil {
		t.Fatal(err)
	}
	if _, err := d.projects.Create(resourcemanager.Project{
		ProjectID: "labelled-project", Labels: map[string]string{"team": "", "env": "dev"},
	}); err != nil {
		t.Fatal(err)
	}
	// The resource hierarchy (#1294): a folder under the organization.
	if _, err := d.projects.CreateFolder(resourcemanager.OrganizationName, "Blank folder"); err != nil {
		t.Fatal(err)
	}
	// Tags (#1295): a key with a value, bound to the project.
	tk, err := d.projects.CreateTagKey(&rmpb.TagKey{Parent: resourcemanager.OrganizationName, ShortName: "blank-key", Description: "Blank key"}, false)
	if err != nil {
		t.Fatal(err)
	}
	tv, err := d.projects.CreateTagValue(&rmpb.TagValue{Parent: tk.GetName(), ShortName: "blank-value", Description: "Blank value"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.projects.CreateTagBinding(&rmpb.TagBinding{Parent: projectTarget(blankProject), TagValue: tv.GetName()}, false); err != nil {
		t.Fatal(err)
	}

	// Cloud Storage: the built-in server, a bucket, and an object under a
	// prefix so the folder level opens too.
	gcs, err := storage.NewServer(storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	gcsSrv := httptest.NewServer(gcs)
	t.Cleanup(gcsSrv.Close)
	tunnels["storage"] = strings.TrimPrefix(gcsSrv.URL, "http://")
	if _, err := (storageProvider{endpoint: tunnels["storage"]}).Create(ctx, blankProject, map[string]string{"name": "blank-bucket"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"top.txt", "folder%2Finner.txt"} {
		resp, err := http.Post(gcsSrv.URL+"/upload/storage/v1/b/blank-bucket/o?uploadType=media&name="+name,
			"text/plain", strings.NewReader("x"))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("upload %s: %s", name, resp.Status)
		}
	}

	// Pub/Sub: a topic with a bare pull subscription, and a push one whose
	// optional parts are unset; and a snapshot of the pull one (#787), from
	// pstest with the snapshots it does not serve.
	psAddr, _, psc := newSnapshotFake(t, blankProject)
	tunnels["pubsub"] = psAddr
	topic := "projects/" + blankProject + "/topics/blank-topic"
	if _, err := psc.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topic}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*pubsubpb.Subscription{
		{Name: "projects/" + blankProject + "/subscriptions/blank-pull", Topic: topic},
		{Name: "projects/" + blankProject + "/subscriptions/blank-push", Topic: topic,
			PushConfig: &pubsubpb.PushConfig{PushEndpoint: "https://example.invalid/push",
				Attributes: map[string]string{"x-empty": ""}}},
	} {
		if _, err := psc.SubscriptionAdminClient.CreateSubscription(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := psc.SubscriptionAdminClient.CreateSnapshot(ctx, &pubsubpb.CreateSnapshotRequest{
		Name: "projects/" + blankProject + "/snapshots/blank-snap", Subscription: "projects/" + blankProject + "/subscriptions/blank-pull",
	}); err != nil {
		t.Fatal(err)
	}
	// And a schema with one revision (#788), whose page is opened too.
	schemas, err := (pubsubSchemasProvider{endpoint: psAddr}).client(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = schemas.Close() })
	if _, err := schemas.CreateSchema(ctx, &pubsubpb.CreateSchemaRequest{Parent: "projects/" + blankProject, SchemaId: "blank-schema",
		Schema: &pubsubpb.Schema{Type: pubsubpb.Schema_AVRO, Definition: `{"type":"record","name":"Blank","fields":[]}`}}); err != nil {
		t.Fatal(err)
	}

	// Cloud Tasks: a queue with the service's defaults, and a task with the
	// fewest fields CreateTask accepts plus a header whose value is empty.
	d.tasks = &tasksService{store: tasks.NewStore(store.NewMemory())}
	queue, err := (tasksProvider{svc: d.tasks}).Create(ctx, blankProject, map[string]string{"name": "blank-queue"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.tasks.Store().CreateTask(tasks.Task{
		Name: queue + "/tasks/bare", Queue: queue,
		HTTPRequest: &tasks.HTTPRequest{URL: "http://127.0.0.1:1/", Headers: map[string]string{"X-Empty": ""}},
	}); err != nil {
		t.Fatal(err)
	}

	// Secret Manager: a secret with one version, and a label whose value is
	// empty.
	d.secrets = &secretsService{store: secrets.NewStore(store.NewMemory())}
	if _, err := (secretsProvider{svc: d.secrets}).Create(ctx, blankProject, map[string]string{
		"secretId": "blank-secret", "payload": "x", "labels": `{"team":""}`,
	}); err != nil {
		t.Fatal(err)
	}

	// Cloud KMS: a key ring holding one key, through the running API.
	d.kms = &kmsService{cfg: cfg, db: store.NewMemory()}
	if err := d.kms.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.kms.Stop(context.Background()) })
	kp := kmsProvider{svc: d.kms}
	ring, err := kp.Create(ctx, blankProject, map[string]string{"keyRingId": "blank-ring", "location": "global"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kp.ActAtResult(ctx, blankProject, []string{ring}, "createkey", map[string]string{"cryptoKeyId": "k1"}); err != nil {
		t.Fatal(err)
	}
	// The sparsest EKM connection the API accepts: no service resolver
	// (#1452).
	if _, err := d.kms.API().Ekm().CreateEkmConnection(ctx, &kmspb.CreateEkmConnectionRequest{Parent: "projects/" + blankProject + "/locations/global",
		EkmConnectionId: "blank-ekm", EkmConnection: &kmspb.EkmConnection{}}); err != nil {
		t.Fatal(err)
	}
	// A single-tenant HSM instance before its keys are registered (#1469).
	if _, err := d.kms.API().HsmManagement().CreateSingleTenantHsmInstance(ctx, &kmspb.CreateSingleTenantHsmInstanceRequest{Parent: "projects/" + blankProject + "/locations/global",
		SingleTenantHsmInstanceId: "blank-hsm", SingleTenantHsmInstance: &kmspb.SingleTenantHsmInstance{QuorumAuth: &kmspb.SingleTenantHsmInstance_QuorumAuth{TotalApproverCount: 3}}}); err != nil {
		t.Fatal(err)
	}

	// Cloud Scheduler: a job with no description, through the running API.
	d.scheduler = &schedulerService{cfg: cfg}
	if err := d.scheduler.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.scheduler.Stop(context.Background()) })
	if _, err := (schedulerProvider{svc: d.scheduler}).Create(ctx, blankProject, map[string]string{
		"name": "blank-job", "location": "us-central1", "schedule": "*/5 * * * *",
		"timeZone": "Etc/UTC", "uri": "http://127.0.0.1:1/",
	}); err != nil {
		t.Fatal(err)
	}

	// Bigtable: the official in-memory server, a table with one family and
	// one row.
	bt, err := bttest.NewServer("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bt.Close)
	tunnels["bigtable"] = bt.Addr
	if _, err := (bigtableProvider{endpoint: bt.Addr}).Create(ctx, blankProject, map[string]string{
		"table": "blank-table", "families": "cf",
	}); err != nil {
		t.Fatal(err)
	}
	btc, err := bigtable.NewClient(ctx, blankProject, bigtableInstance, localOpts(bt.Addr)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = btc.Close() })
	mut := bigtable.NewMutation()
	mut.Set("cf", "col", bigtable.Now(), []byte("v"))
	if err := btc.Open("blank-table").Apply(ctx, "row-1", mut); err != nil {
		t.Fatal(err)
	}

	// Cloud Run jobs (#785): the Jobs and Executions API, with a job never
	// executed and one whose execution carries only its name and job.
	jobs := newFakeRunJobs()
	// Cloud Build on the same port, for a trigger's page, with the
	// sparsest trigger CreateBuildTrigger takes: one step, nothing else.
	cb := runadapter.NewServer(&runadapter.Knative{}, "t", time.Minute).Builds(stubSource{}, stubBuilder{}).CloudBuild()
	// Secure Source Manager on the same port (#1527): the sparsest instance
	// and repository on it.
	if err := runadapter.UseSourceManagerRoot(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	ssm := runadapter.NewServer(&runadapter.Knative{}, "t", time.Minute).SecureSourceManager(nil)
	jobs.also = func(g *grpc.Server) { cb.Register(g); ssm.Register(g) }
	ssmParent := "projects/" + blankProject + "/locations/us-central1"
	if _, err := ssm.CreateInstance(ctx, &ssmpb.CreateInstanceRequest{Parent: ssmParent, InstanceId: "blank-instance"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ssm.CreateRepository(ctx, &ssmpb.CreateRepositoryRequest{Parent: ssmParent, RepositoryId: "blank-repo",
		Repository: &ssmpb.Repository{Instance: ssmParent + "/instances/blank-instance"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = ssm.DeleteInstance(context.Background(), &ssmpb.DeleteInstanceRequest{Name: ssmParent + "/instances/blank-instance", Force: true})
	})
	trig, err := cb.CreateBuildTrigger(ctx, &cbpb.CreateBuildTriggerRequest{ProjectId: blankProject,
		Parent: "projects/" + blankProject + "/locations/global", Trigger: &cbpb.BuildTrigger{Name: "blank-trigger",
			BuildTemplate: &cbpb.BuildTrigger_Build{Build: &cbpb.Build{Steps: []*cbpb.BuildStep{{Name: "alpine:3.20"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = cb.DeleteBuildTrigger(context.Background(), &cbpb.DeleteBuildTriggerRequest{Name: trig.GetResourceName(), ProjectId: blankProject, TriggerId: trig.GetId()})
	})
	// And the sparsest worker pool CreateWorkerPool takes, for a pool's
	// page (#1491).
	if _, err := cb.CreateWorkerPool(ctx, &cbpb.CreateWorkerPoolRequest{Parent: "projects/" + blankProject + "/locations/us-central1",
		WorkerPoolId: "blank-pool", WorkerPool: &cbpb.WorkerPool{}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = cb.DeleteWorkerPool(context.Background(), &cbpb.DeleteWorkerPoolRequest{Name: "projects/" + blankProject + "/locations/us-central1/workerPools/blank-pool"})
	})
	d.run = fixedRunAdapter(jobs.serve(t))
	for _, id := range []string{"blank-job", "blank-ran"} {
		name := "projects/" + blankProject + "/locations/us-central1/jobs/" + id
		jobs.jobs[name] = &runpb.Job{Name: name, Template: &runpb.ExecutionTemplate{
			Template: &runpb.TaskTemplate{Containers: []*runpb.Container{{Image: "example.invalid/job:1"}}}}}
	}
	ran := "projects/" + blankProject + "/locations/us-central1/jobs/blank-ran"
	jobs.executions[ran+"/executions/blank-ran-x1"] = &runpb.Execution{Name: ran + "/executions/blank-ran-x1", Job: ran}
	jobs.order = append(jobs.order, ran+"/executions/blank-ran-x1")
	// Worker pools and instances (#1300, #1299): the sparsest each returns.
	pool := "projects/" + blankProject + "/locations/us-central1/workerPools/blank-pool"
	jobs.pools.pools = map[string]*runpb.WorkerPool{pool: {Name: pool, Template: &runpb.WorkerPoolRevisionTemplate{
		Containers: []*runpb.Container{{Image: "example.invalid/pool:1"}}}}}
	inst := "projects/" + blankProject + "/locations/us-central1/instances/blank-instance"
	jobs.instances.instances = map[string]*runpb.Instance{inst: {Name: inst}}
	// A Cloud Build build (#1347), the sparsest CreateBuild runs: one
	// push step, which fails, and no source.
	if _, err := runadapter.NewServer(&runadapter.Knative{}, "t", time.Minute).Builds(stubSource{}, stubBuilder{}).CloudBuild().
		CreateBuild(ctx, &cbpb.CreateBuildRequest{ProjectId: blankProject, Build: &cbpb.Build{
			Steps: []*cbpb.BuildStep{{Name: "gcr.io/cloud-builders/docker", Args: []string{"push", "example.invalid/never"}}}}}); err != nil {
		t.Fatal(err)
	}
	// A Cloud Functions function (#1377), the sparsest CreateFunction
	// takes, whose build fails.
	fnSrv := runadapter.NewServer(&runadapter.Knative{}, "t", time.Minute)
	if _, err := fnSrv.Functions(fnSrv.Builds(stubSource{}, failingBuilder{}), func() string { return "" }).CreateFunction(ctx, &functionspb.CreateFunctionRequest{
		Parent: "projects/" + blankProject + "/locations/us-central1", FunctionId: "blank-fn", Function: &functionspb.Function{
			BuildConfig: &functionspb.BuildConfig{Runtime: "nodejs22",
				Source: &functionspb.Source{Source: &functionspb.Source_StorageSource{StorageSource: &functionspb.StorageSource{Bucket: "b", Object: "o"}}}}}}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if fns := runadapter.Functions(blankProject); len(fns) == 1 && fns[0].GetState() == functionspb.Function_FAILED {
			break
		}
	}

	// An Artifact Registry repository, the sparsest CreateRepository takes,
	// holding one untagged image pushed to its registry (#1383).
	regStore, err := registry.Open(filepath.Join(dir, "artifactregistry"))
	if err != nil {
		t.Fatal(err)
	}
	runadapter.UseRegistry(regStore)
	t.Cleanup(func() { runadapter.UseRegistry(nil) })
	if _, err := fnSrv.ArtifactRegistry().CreateRepository(ctx, &arpb.CreateRepositoryRequest{
		Parent: "projects/" + blankProject + "/locations/us-central1", RepositoryId: "blank-repo",
		Repository: &arpb.Repository{Format: arpb.Repository_DOCKER}}); err != nil {
		t.Fatal(err)
	}
	blankImage, _ := random.Image(64, 1)
	if _, err := regStore.WriteImage(registry.Repo{Location: "us-central1", Project: blankProject, Repository: "blank-repo"},
		"blank-image", "latest", blankImage); err != nil {
		t.Fatal(err)
	}

	// Cloud Run and the cluster screens read through kubectl, so a kubectl
	// that answers from fixtures stands in for the cluster.
	fakeKubectl(t, dir)
	d.cfg.Cluster.Kubeconfig = filepath.Join(dir, "kubeconfig")

	for name, addr := range tunnels {
		d.forwarders = append(d.forwarders, namedForwarder(t, name, addr))
	}
	return d
}

// fixedRunAdapter is a Cloud Run adapter already bound at addr.
type fixedRunAdapter string

func (a fixedRunAdapter) Addr() string { return string(a) }

// namedForwarder is a never-started Forwarder carrying a tunnel's name and
// host address, which is all the registry reads from one.
func namedForwarder(t *testing.T, name, addr string) *netfwd.Forwarder {
	t.Helper()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return netfwd.New(netfwd.Target{Name: name, HostPort: p}, "", host)
}

// fakeKubectl puts a kubectl on PATH that answers `get <kind> -o json` from
// the fixtures below and fails anything else, the way a cluster that refuses
// a request does.
func fakeKubectl(t *testing.T, dir string) {
	t.Helper()
	bin := filepath.Join(dir, "bin")
	fixtures := filepath.Join(dir, "fixtures")
	for _, d := range []string{bin, fixtures} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for kind, body := range kubeFixtures {
		if err := os.WriteFile(filepath.Join(fixtures, kind+".json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := `#!/bin/sh
kind=""
while [ $# -gt 0 ]; do
  if [ "$1" = "get" ]; then kind="$2"; break; fi
  shift
done
[ -n "$kind" ] || { echo "fake kubectl: only get is served" >&2; exit 1; }
f="` + fixtures + `/$kind.json"
if [ -f "$f" ]; then cat "$f"; else echo '{"items":[]}'; fi
`
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// kubeFixtures are the cluster objects, by the kind argument kubectl is
// given. Each is as sparse as the API server would return for a freshly
// created object that has not started yet, because that is when the most
// fields are unset.
var kubeFixtures = map[string]string{
	"ksvc": `{"items":[
	  {"metadata":{"name":"pending-svc","creationTimestamp":"2026-01-01T00:00:00Z"},
	   "spec":{"template":{"spec":{"containers":[{"image":"example.invalid/app:1"}]}}},
	   "status":{"latestCreatedRevisionName":"pending-svc-00001",
	     "conditions":[{"type":"Ready","status":"Unknown"}]}},
	  {"metadata":{"name":"ready-svc","creationTimestamp":"2026-01-01T00:00:00Z"},
	   "spec":{"template":{"spec":{"containers":[{"image":"example.invalid/app:1","ports":[{"containerPort":8080}]}]}}},
	   "status":{"url":"http://ready-svc.example.invalid","latestReadyRevisionName":"ready-svc-00001",
	     "latestCreatedRevisionName":"ready-svc-00001",
	     "conditions":[{"type":"Ready","status":"True"}],
	     "traffic":[{"revisionName":"ready-svc-00001","percent":100,"latestRevision":true}]}}]}`,
	"revisions": `{"items":[
	  {"metadata":{"name":"ready-svc-00001","creationTimestamp":"2026-01-01T00:00:00Z",
	     "labels":{"serving.knative.dev/service":"ready-svc"}},
	   "spec":{"containers":[{"image":"example.invalid/app:1"}]},
	   "status":{"conditions":[{"type":"Ready","status":"True"}]}},
	  {"metadata":{"name":"pending-svc-00001","creationTimestamp":"2026-01-01T00:00:00Z",
	     "labels":{"serving.knative.dev/service":"pending-svc"}},
	   "spec":{"containers":[{"image":"example.invalid/app:1"}]},
	   "status":{}}]}`,
	"deployments,statefulsets,daemonsets,replicasets": `{"items":[
	  {"kind":"Deployment","metadata":{"name":"web","namespace":"default","creationTimestamp":"2026-01-01T00:00:00Z"},
	   "spec":{"replicas":1,"strategy":{"type":"RollingUpdate"},"selector":{"matchLabels":{"app":"web"}},
	     "template":{"spec":{"containers":[{"name":"web","image":"example.invalid/web:1"}]}}},
	   "status":{}},
	  {"kind":"StatefulSet","metadata":{"name":"db","namespace":"default","creationTimestamp":"2026-01-01T00:00:00Z"},
	   "spec":{"replicas":1,"selector":{"matchLabels":{"app":"db"}},
	     "template":{"spec":{"containers":[{"name":"db","image":"example.invalid/db:1"}]}}},
	   "status":{}}]}`,
	"pods": `{"items":[
	  {"kind":"Pod","metadata":{"name":"web-1","namespace":"default","creationTimestamp":"2026-01-01T00:00:00Z",
	     "labels":{"app":"web"}},
	   "spec":{"containers":[{"name":"web","image":"example.invalid/web:1"}]},
	   "status":{"phase":"Pending"}}]}`,
	"services": `{"items":[
	  {"kind":"Service","metadata":{"name":"web","namespace":"default","creationTimestamp":"2026-01-01T00:00:00Z"},
	   "spec":{"type":"ClusterIP","selector":{"app":"web"},"ports":[{"port":80}]},
	   "status":{}}]}`,
	"jobs": `{"items":[
	  {"kind":"Job","metadata":{"name":"migrate","namespace":"default","creationTimestamp":"2026-01-01T00:00:00Z"},
	   "spec":{"template":{"spec":{"containers":[{"name":"migrate","image":"example.invalid/migrate:1"}]}}},
	   "status":{}}]}`,
	"nodes": `{"items":[
	  {"kind":"Node","metadata":{"name":"node-1","creationTimestamp":"2026-01-01T00:00:00Z"},
	   "spec":{},"status":{}}]}`,
	"persistentvolumeclaims": `{"items":[
	  {"kind":"PersistentVolumeClaim","metadata":{"name":"data","namespace":"default","creationTimestamp":"2026-01-01T00:00:00Z"},
	   "spec":{"accessModes":["ReadWriteOnce"],"resources":{"requests":{"storage":"1Gi"}}},
	   "status":{"phase":"Pending"}}]}`,
	"events": `{"items":[
	  {"kind":"Event","metadata":{"name":"web-1.1","namespace":"default","creationTimestamp":"2026-01-01T00:00:00Z"},
	   "involvedObject":{"kind":"Pod","name":"web-1","namespace":"default"},
	   "reason":"Scheduled","type":"Normal"}]}`,
}
