//go:build compat

package compat

import (
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/iterator"
)

// queryResultsDataset is the hidden dataset the BigQuery front has the
// emulator write query results to (internal/bigqueryfront/results.go).
const queryResultsDataset = "_cloudburrow_query_results"

// TestBigQueryQueryResultsInOneHiddenDataset (#1017): a query job's result
// is in a table named after the job, in one hidden dataset: the job's
// configuration names it, its rows read back through jobs.getQueryResults
// and through tabledata.list of that table, an earlier job's still do
// after later ones, and datasets.list leaves the dataset out unless hidden
// datasets are asked for. Measured first: the emulator made a dataset of
// its own for each such job, named after it, which datasets.list listed,
// and the engine's memory grew with each (docs/compatibility.md).
func TestBigQueryQueryResultsInOneHiddenDataset(t *testing.T) {
	h := New(t)
	c, project := bigqueryClient(t, h)
	ctx := h.Context()
	want := [][]bigquery.Value{{int64(1), "a"}, {int64(2), "b"}}
	readAll := func(what string, it *bigquery.RowIterator, err error) [][]bigquery.Value {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		var out [][]bigquery.Value
		for {
			var row []bigquery.Value
			err := it.Next(&row)
			if errors.Is(err, iterator.Done) {
				return out
			}
			if err != nil {
				t.Fatalf("%s: %v", what, err)
			}
			out = append(out, row)
		}
	}
	var jobs []*bigquery.Job
	for i := 0; i < 3; i++ {
		job, err := c.Query("SELECT x, s FROM UNNEST([STRUCT(1 AS x, 'a' AS s), (2, 'b')]) ORDER BY x").Run(ctx)
		if err != nil {
			t.Fatalf("query job: %v", err)
		}
		if _, err := job.Wait(ctx); err != nil {
			t.Fatalf("query job %s: %v", job.ID(), err)
		}
		jobs = append(jobs, job)
	}
	for _, job := range jobs {
		cfg, err := job.Config()
		if err != nil {
			t.Fatalf("job %s: %v", job.ID(), err)
		}
		dst := cfg.(*bigquery.QueryConfig).Dst
		if dst == nil || dst.ProjectID != project || dst.DatasetID != queryResultsDataset || dst.TableID != job.ID() {
			t.Fatalf("job %s: destination %+v, want %s.%s", job.ID(), dst, queryResultsDataset, job.ID())
		}
		it, err := job.Read(ctx)
		if got := readAll("jobs.getQueryResults of "+job.ID(), it, err); !reflect.DeepEqual(got, want) {
			t.Errorf("jobs.getQueryResults of %s: %v, want %v", job.ID(), got, want)
		}
		if got := readAll("tabledata.list of "+dst.TableID, dst.Read(ctx), nil); !reflect.DeepEqual(got, want) {
			t.Errorf("tabledata.list of %s's destination: %v, want %v", job.ID(), got, want)
		}
	}
	listed := func(hidden bool) map[string]bool {
		it := c.Datasets(ctx)
		it.ListHidden = hidden
		out := map[string]bool{}
		for {
			ds, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return out
			}
			if err != nil {
				t.Fatalf("datasets.list: %v", err)
			}
			out[ds.DatasetID] = true
		}
	}
	if all := listed(false); all[queryResultsDataset] {
		t.Errorf("datasets.list lists %s", queryResultsDataset)
	}
	all := listed(true)
	if !all[queryResultsDataset] {
		t.Errorf("datasets.list with all=true: %v, want %s in it", all, queryResultsDataset)
	}
	for _, job := range jobs {
		if all[job.ID()] {
			t.Errorf("a dataset named after job %s", job.ID())
		}
	}
}

// TestBigQueryJobsAcrossAnEmulatorRestart (#1016): the emulator keeps its
// data in memory, and Kubernetes restarts its container alone (the front's
// goes on); once it has, a job the front had failed is not reported
// failed to a client that runs a job of the same ID on the new emulator,
// and jobs.list lists it once, as it now is. The restart is the node's
// container runtime stopping the emulator's container, as a crash does.
// Measured first (unit TestFrontDropsJobsAtRestart): the front answered the
// new job with the old one's failure and times.
func TestBigQueryJobsAcrossAnEmulatorRestart(t *testing.T) {
	h := New(t)
	c, _ := bigqueryClient(t, h)
	restart := bigQueryEmulatorRestarter(t)
	ctx := h.Context()
	id := "restart_" + strings.ReplaceAll(h.Project(), "-", "_")
	load := func(data string, nullMarker string) error {
		src := bigquery.NewReaderSource(strings.NewReader(data))
		src.Schema = loadSchema
		src.NullMarker = nullMarker
		l := validationDataset(t, h, c).Table("t").LoaderFrom(src)
		l.JobID = id
		return runLoad(ctx, l)
	}
	// An empty INTEGER value with a nullMarker: the front fails the job
	// the emulator ran (#945).
	if err := load(",x\n", `\N`); err == nil {
		t.Fatal("the load the front fails succeeded")
	}
	if st, err := c.JobFromID(ctx, id); err != nil || st.LastStatus().Err() == nil {
		t.Fatalf("before the restart: %v, status %+v, want the job failed", err, st)
	}
	restart()
	if err := load("1,x\n", ""); err != nil {
		t.Fatalf("the load on the restarted emulator, job %s: %v", id, err)
	}
	if job, err := c.JobFromID(ctx, id); err != nil {
		t.Errorf("after the restart, jobs.get: %v", err)
	} else if err := job.LastStatus().Err(); err != nil {
		t.Errorf("after the restart, jobs.get: the job failed: %v, want it done", err)
	}
	n := 0
	it := c.Jobs(ctx)
	for {
		j, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			t.Fatalf("jobs.list: %v", err)
		}
		if j.ID() == id {
			n++
			if err := j.LastStatus().Err(); err != nil {
				t.Errorf("jobs.list: job %s failed: %v", id, err)
			}
		}
	}
	if n != 1 {
		t.Errorf("jobs.list lists job %s %d times, want once", id, n)
	}
}

// bigQueryEmulatorRestarter returns a function that restarts the BigQuery
// emulator's container, alone, through the kind node's container runtime,
// and waits until it is ready again; the test is skipped without the
// instance's kubeconfig, cluster, kubectl and docker.
func bigQueryEmulatorRestarter(t *testing.T) func() {
	t.Helper()
	kubeconfig := strings.TrimSpace(os.Getenv(envKubeconfig))
	cluster := strings.TrimSpace(os.Getenv("CLOUDBURROW_TEST_CLUSTER"))
	if kubeconfig == "" || cluster == "" {
		t.Skip("CLOUDBURROW_TEST_KUBECONFIG and CLOUDBURROW_TEST_CLUSTER are required to restart the emulator's container")
	}
	for _, bin := range []string{"docker", "kubectl"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is required to restart the emulator's container", bin)
		}
	}
	kubectl := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("kubectl", append([]string{"--kubeconfig", kubeconfig}, args...)...).Output()
		if err != nil {
			t.Fatalf("kubectl %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	pods := strings.Fields(kubectl("get", "pods", "-A", "-l", "app=bigquery,cloudburrow.dev/owned=true",
		"-o", "jsonpath={.items[0].metadata.namespace} {.items[0].metadata.name} {.items[0].spec.nodeName}"))
	if len(pods) != 3 {
		t.Fatalf("the BigQuery pod: %v", pods)
	}
	ns, pod, node := pods[0], pods[1], pods[2]
	status := func(field string) string {
		return kubectl("get", "-n", ns, "pod/"+pod, "-o", `jsonpath={.status.containerStatuses[?(@.name=="bigquery")].`+field+`}`)
	}
	return func() {
		t.Helper()
		before := status("restartCount")
		id := strings.TrimPrefix(status("containerID"), "containerd://")
		if out, err := exec.Command("docker", "exec", node, "crictl", "stop", id).CombinedOutput(); err != nil {
			t.Fatalf("stop the emulator's container on %s: %v\n%s", node, err, out)
		}
		for deadline := time.Now().Add(3 * time.Minute); ; time.Sleep(time.Second) {
			if status("restartCount") != before && status("ready") == "true" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the emulator's container was not ready again within 3 minutes")
			}
		}
		// The front sees the emulator back within its poll (restart.go).
		time.Sleep(time.Second)
	}
}
