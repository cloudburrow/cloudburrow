//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestBqSparkStoredProcedure (#2104), through the bq CLI (`bq query
// --nouse_legacy_sql`, the configuration `cloudburrow env` exports, egress
// blocked), as https://docs.cloud.google.com/bigquery/docs/spark-procedures
// walks it: a PySpark procedure written inline is made and CALLed, `bq
// show --routine` shows it as language PYTHON with its sparkOptions, `bq ls
// -j --parent_job_id` lists the CALL's child job and `bq show
// --format=prettyjson --job` of it has statistics.query.sparkStatistics;
// the page's dry run of a procedure with a YARN property fails with the
// page's error.
//
// covers: bigquery.jobs.insert, bigquery.jobs.getQueryResults, bigquery.jobs.list, bigquery.jobs.get, bigquery.routines.get
func TestBqSparkStoredProcedure(t *testing.T) {
	h, c, project := sparkEnv(t)
	vars := bqEnv(t, h)
	b := newBqSession(t, vars, "--nouse_google_auth", bqToken)
	_, conn := sparkConnection(t, h, project, strings.ReplaceAll(h.Project(), "-", "_")+"_bqspark", nil)
	ds := strings.ReplaceAll(h.Project(), "-", "_") + "_bqspark2104"
	b.must("mk", "--dataset", "--location=US", project+":"+ds)
	t.Cleanup(func() { _ = c.Dataset(ds).DeleteWithContents(context.Background()) })
	b.must("query", "--nouse_legacy_sql", "CREATE PROCEDURE `"+ds+".hello`(OUT greeting STRING) WITH CONNECTION `"+conn+
		"` OPTIONS(engine=\"SPARK\", runtime_version=\"1.2\") LANGUAGE PYTHON AS r\"\"\"\n"+
		"from pyspark.sql import SparkSession\nfrom bigquery.spark.procedure import SparkProcParamContext\n"+
		"spark = SparkSession.builder.getOrCreate()\nctx = SparkProcParamContext.getOrCreate(spark)\n"+
		"ctx.greeting = 'hello from spark ' + spark.version[:3]\n\"\"\"")
	shown := b.must("show", "--format=prettyjson", "--routine", project+":"+ds+".hello")
	for _, want := range []string{`"language": "PYTHON"`, `"sparkOptions"`, `"runtimeVersion": "1.2"`, `"routineType": "PROCEDURE"`} {
		if !strings.Contains(shown, want) {
			t.Errorf("bq show --routine: no %s in\n%s", want, shown)
		}
	}
	out := b.must("query", "--nouse_legacy_sql", "--format=json", "--job_id=bqspark_"+strings.ReplaceAll(h.Project(), "-", "_"),
		"DECLARE greeting STRING; CALL `"+ds+".hello`(greeting); SELECT greeting")
	if !strings.Contains(out, "hello from spark 3.5") {
		t.Errorf("the CALL's OUT argument: %s", out)
	}
	kids := b.must("ls", "-j", "--format=json", "--parent_job_id=bqspark_"+strings.ReplaceAll(h.Project(), "-", "_"), project+":")
	var jobs []struct {
		JobReference struct {
			JobID    string `json:"jobId"`
			Location string `json:"location"`
		} `json:"jobReference"`
	}
	if err := json.Unmarshal([]byte(kids), &jobs); err != nil || len(jobs) == 0 {
		t.Fatalf("bq ls -j --parent_job_id: %v\n%s", err, kids)
	}
	found := false
	for _, j := range jobs {
		job := b.must("show", "--format=prettyjson", "--job", project+":"+j.JobReference.JobID)
		if strings.Contains(job, `"sparkStatistics"`) {
			found = true
			for _, want := range []string{`"sparkJobId"`, `"sparkJobLocation"`, `"resourceType": "bigquery.googleapis.com/SparkJob"`} {
				if !strings.Contains(job, want) {
					t.Errorf("bq show --job of the CALL's child: no %s in\n%s", want, job)
				}
			}
		}
	}
	if !found {
		t.Errorf("no child job of the CALL has sparkStatistics (%d children)", len(jobs))
	}
	o, e, err := b.run(nil, "query", "--nouse_legacy_sql", "--dry_run", "CREATE PROCEDURE `"+ds+".yarn`() WITH CONNECTION `"+conn+
		"` OPTIONS(engine=\"SPARK\", main_file_uri=\"gs://b/my-pyspark-main.py\", properties=[(\"spark.executor.instances\", \"3\"), "+
		"(\"spark.yarn.am.memory\", \"3g\")]) LANGUAGE PYTHON")
	// bq wraps its error's lines; the words are the page's.
	if err == nil || !strings.Contains(strings.Join(strings.Fields(o+e), " "), "Attempted to set unsupported properties: [spark:spark.yarn.am.memory]") {
		t.Errorf("bq query --dry_run with a YARN property: %v\n%s%s", err, o, e)
	}
}
