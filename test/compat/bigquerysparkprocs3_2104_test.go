//go:build compat

package compat

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	artifactregistry "cloud.google.com/go/artifactregistry/apiv1"
	"cloud.google.com/go/artifactregistry/apiv1/artifactregistrypb"
	"cloud.google.com/go/bigquery"
	"github.com/google/go-containerregistry/pkg/crane"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"golang.org/x/oauth2"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// sparkDataset makes a dataset in US for a Spark procedure test.
func sparkDataset(t *testing.T, c *bigquery.Client, name string) string {
	t.Helper()
	ctx := context.Background()
	if err := c.Dataset(name).Create(ctx, &bigquery.DatasetMetadata{Location: "US"}); err != nil {
		t.Fatalf("Dataset.Create %s: %v", name, err)
	}
	t.Cleanup(func() { _ = c.Dataset(name).DeleteWithContents(context.Background()) })
	return name
}

// sparkQueryRow runs sql with cl and returns its one row.
func sparkQueryRow(ctx context.Context, cl *bigquery.Client, sql string) ([]bigquery.Value, error) {
	q := cl.Query(sql)
	q.Location = "US"
	it, err := q.Read(ctx)
	if err != nil {
		return nil, err
	}
	var row []bigquery.Value
	if err := it.Next(&row); err != nil {
		return nil, err
	}
	return row, nil
}

// sparkTypes is a PySpark procedure that reports the Spark it runs on and
// the Python types of its IN arguments, and sets the page's ARRAY of
// STRUCT OUT argument, a STRUCT, BYTES, a DATETIME and JSON.
const sparkTypes = `
import datetime, pyspark
from pyspark.sql import SparkSession
from bigquery.spark.procedure import SparkProcParamContext
spark = SparkSession.builder.getOrCreate()
ctx = SparkProcParamContext.getOrCreate(spark)
ctx.version = spark.version
ctx.kinds = ",".join(type(v).__name__ for v in [ctx.d, ctx.ts, ctx.b, ctx.arr, ctx.st, ctx.arr[0]])
ctx.info = [{"a": 2, "b": "dd"}, {"a": ctx.st["n"], "b": ctx.st["s"]}]
ctx.pair = {"x": [1.5, 2.5], "y": {"z": True}}
ctx.bs = ctx.b + b"!"
ctx.dt = datetime.datetime(2026, 11, 21, 12, 0, 0, tzinfo=datetime.timezone(datetime.timedelta(hours=2)))
ctx.j = {"k": [1, 2], "when": str(ctx.d + datetime.timedelta(days=1))}
`

// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.routines.get
//
// TestBigQuerySparkProcedureRuntimesAndTypes (#2104, round 3): a PySpark
// stored procedure on each runtime version Managed Service for Apache
// Spark lists runs on that runtime's Spark (runtime pages, read
// 2026-11-21): 2.2 (the default) Spark 3.5, 3.0 Spark 4.0 and 4.0 Spark
// 4.2. Its IN arguments are the page's Python types (DATE datetime.date,
// TIMESTAMP datetime.datetime, BYTES bytes, ARRAY a list, STRUCT a dict),
// and its OUT arguments of the page's example, ARRAY<STRUCT<a INT64, b
// STRING>> set as a list of dicts, and a STRUCT, BYTES, a DATETIME
// (returned in UTC) and JSON, are the caller's variables after the CALL,
// through the official cloud.google.com/go/bigquery client. An argument of
// a type the library does not map (NUMERIC) is refused.
func TestBigQuerySparkProcedureRuntimesAndTypes(t *testing.T) {
	h, c, project := sparkEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	under := strings.ReplaceAll(h.Project(), "-", "_")
	_, conn := sparkConnection(t, h, project, under+"_sparkrt", nil)
	ds := sparkDataset(t, c, "compat_sparkrt_"+under)
	for _, tc := range []struct{ runtime, spark string }{{"", "3.5."}, {"3.0", "4.0."}, {"4.0", "4.2."}} {
		opts := `engine="SPARK"`
		if tc.runtime != "" {
			opts += fmt.Sprintf(`, runtime_version="%s"`, tc.runtime)
		}
		name := "types_" + strings.ReplaceAll(tc.runtime, ".", "_")
		create := fmt.Sprintf("CREATE OR REPLACE PROCEDURE `%s.%s`(d DATE, ts TIMESTAMP, b BYTES, arr ARRAY<INT64>, st STRUCT<n INT64, s STRING>, "+
			"OUT version STRING, OUT kinds STRING, OUT info ARRAY<STRUCT<a INT64, b STRING>>, OUT pair STRUCT<x ARRAY<FLOAT64>, y STRUCT<z BOOL>>, "+
			"OUT bs BYTES, OUT dt DATETIME, OUT j JSON)\nWITH CONNECTION `%s` OPTIONS(%s)\nLANGUAGE PYTHON AS r\"\"\"%s\"\"\"",
			ds, name, conn, opts, sparkTypes)
		if err := mlRunIn(ctx, c, create); err != nil {
			t.Fatalf("CREATE PROCEDURE (runtime %q): %v", tc.runtime, err)
		}
		row, err := sparkQueryRow(ctx, c, fmt.Sprintf("DECLARE version, kinds STRING; DECLARE info ARRAY<STRUCT<a INT64, b STRING>>; "+
			"DECLARE pair STRUCT<x ARRAY<FLOAT64>, y STRUCT<z BOOL>>; DECLARE bs BYTES; DECLARE dt DATETIME; DECLARE j JSON;\n"+
			"CALL `%s.%s`(DATE '2026-11-21', TIMESTAMP '2026-11-21 10:00:00+00', b'hi', [7, 8], STRUCT(5 AS n, 'five' AS s), "+
			"version, kinds, info, pair, bs, dt, j);\n"+
			"SELECT version, kinds, TO_JSON_STRING(info), TO_JSON_STRING(pair), bs, dt, TO_JSON_STRING(j)", ds, name))
		if err != nil {
			t.Fatalf("CALL (runtime %q): %v", tc.runtime, err)
		}
		if v, _ := row[0].(string); !strings.HasPrefix(v, tc.spark) {
			t.Errorf("runtime %q runs Spark %v, want %s*", tc.runtime, row[0], tc.spark)
		}
		if row[1] != "date,datetime,bytes,list,dict,int" {
			t.Errorf("runtime %q: the IN arguments' Python types %v", tc.runtime, row[1])
		}
		if row[2] != `[{"a":2,"b":"dd"},{"a":5,"b":"five"}]` {
			t.Errorf("runtime %q: OUT info %v", tc.runtime, row[2])
		}
		if row[3] != `{"x":[1.5,2.5],"y":{"z":true}}` {
			t.Errorf("runtime %q: OUT pair %v", tc.runtime, row[3])
		}
		if b, _ := row[4].([]byte); string(b) != "hi!" {
			t.Errorf("runtime %q: OUT bs %v", tc.runtime, row[4])
		}
		if fmt.Sprint(row[5]) != "2026-11-21T10:00:00" {
			t.Errorf("runtime %q: OUT dt %v, want 2026-11-21T10:00:00 (UTC)", tc.runtime, row[5])
		}
		if row[6] != `{"k":[1,2],"when":"2026-11-22"}` {
			t.Errorf("runtime %q: OUT j %v", tc.runtime, row[6])
		}
	}
	for _, tc := range []struct{ sql, want string }{
		{fmt.Sprintf("CREATE PROCEDURE `%s.bad1`(n NUMERIC) WITH CONNECTION `%s` OPTIONS(engine=\"SPARK\") LANGUAGE PYTHON AS r'print(1)'", ds, conn), "NUMERIC"},
		{fmt.Sprintf("CREATE PROCEDURE `%s.bad2`() WITH CONNECTION `%s` OPTIONS(engine=\"SPARK\", runtime_version=\"9.9\") LANGUAGE PYTHON AS r'print(1)'", ds, conn),
			"1.2, 2.2, 2.3, 3.0 and 4.0"},
	} {
		if err := mlRunIn(ctx, c, tc.sql); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want an error naming %q", tc.sql, err, tc.want)
		}
	}
}

// sparkCustomImage is a custom container image as Google's custom
// containers guide has it, built on the pinned python:3.12-slim (Debian)
// with no Spark and no Java: the spark user and group 1099, a Python
// package of its own under PYTHONPATH, and PYSPARK_PYTHON.
func sparkCustomImage(t *testing.T) v1.Image {
	t.Helper()
	base, err := crane.Pull("python@sha256:5e2bdb1b0d542d5039cffb8181c65930093949194a65c365dfb1b0fe13a4fa5b",
		crane.WithPlatform(&v1.Platform{OS: "linux", Architecture: runtime.GOARCH}))
	if err != nil {
		t.Fatalf("pull python:3.12-slim: %v", err)
	}
	// World-readable, as a Dockerfile's COPY makes them: the container
	// runs as UID 1099.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range []struct{ name, body string }{
		{"etc/passwd", "root:x:0:0:root:/root:/bin/bash\nspark:x:1099:1099::/home/spark:/bin/bash\n"},
		{"etc/group", "root:x:0:\nspark:x:1099:\n"},
		{"opt/python/packages/cbmarker.py", "MARK = 'from-the-custom-container'\n"},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	layer, err := tarball.LayerFromReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	img, err := mutate.AppendLayers(base, layer)
	if err != nil {
		t.Fatal(err)
	}
	cf, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	cf = cf.DeepCopy()
	cf.Config.Env = append(cf.Config.Env, "PYTHONPATH=/opt/python/packages", "PYSPARK_PYTHON=/usr/local/bin/python3")
	img, err = mutate.ConfigFile(img, cf)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.routines.get, google.devtools.artifactregistry.v1.ArtifactRegistry/CreateRepository
//
// TestBigQuerySparkProcedureContainerImage (#2104, round 3): a stored
// procedure for Apache Spark with container_image, "path of image in
// Artifact Registry", runs in that image, pushed to the instance's
// Artifact Registry with go-containerregistry: the image holds no Spark or
// Java, as Google's custom containers guide requires, and its own Python
// package and PYSPARK_PYTHON are used, Spark's runtime mounted into it.
// routines.get shows sparkOptions.containerImage. A path outside
// Artifact Registry is refused when the procedure is made; one the
// registry does not hold fails the CALL.
func TestBigQuerySparkProcedureContainerImage(t *testing.T) {
	h, c, project := sparkEnv(t)
	h.Endpoint(EnvRun)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	ar, err := artifactregistry.NewClient(ctx, runClientOptions(h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ar.Close() })
	repoID := "spark-images"
	parent := fmt.Sprintf("projects/%s/locations/us", project)
	op, err := ar.CreateRepository(ctx, &artifactregistrypb.CreateRepositoryRequest{Parent: parent, RepositoryId: repoID,
		Repository: &artifactregistrypb.Repository{Format: artifactregistrypb.Repository_DOCKER}})
	if err == nil {
		_, err = op.Wait(ctx)
	}
	if err != nil {
		t.Fatalf("CreateRepository: %v", err)
	}
	t.Cleanup(func() {
		if op, err := ar.DeleteRepository(context.Background(), &artifactregistrypb.DeleteRepositoryRequest{Name: parent + "/repositories/" + repoID}); err == nil {
			_ = op.Wait(context.Background())
		}
	})
	_, port, _ := splitHostPort(h.Endpoint(EnvRun))
	if err := crane.Push(sparkCustomImage(t), "us-docker.pkg.localhost:"+port+"/"+project+"/"+repoID+"/pyspark:1", registryOptions(h)...); err != nil {
		t.Fatalf("push the custom image: %v", err)
	}
	image := "us-docker.pkg.dev/" + project + "/" + repoID + "/pyspark:1"

	under := strings.ReplaceAll(h.Project(), "-", "_")
	_, conn := sparkConnection(t, h, project, under+"_sparkimg", nil)
	ds := sparkDataset(t, c, "compat_sparkimg_"+under)
	code := `
import os, sys, cbmarker
from pyspark.sql import SparkSession
from bigquery.spark.procedure import SparkProcParamContext
spark = SparkSession.builder.getOrCreate()
ctx = SparkProcParamContext.getOrCreate(spark)
ctx.mark = cbmarker.MARK + ":" + sys.executable + ":" + str(os.getuid()) + ":" + str(spark.range(5).count())
`
	if err := mlRunIn(ctx, c, fmt.Sprintf("CREATE PROCEDURE `%s.custom`(OUT mark STRING) WITH CONNECTION `%s` "+
		"OPTIONS(engine=\"SPARK\", runtime_version=\"3.0\", container_image=\"%s\") LANGUAGE PYTHON AS r\"\"\"%s\"\"\"", ds, conn, image, code)); err != nil {
		t.Fatalf("CREATE PROCEDURE (container_image): %v", err)
	}
	r, err := c.Dataset(ds).Routine("custom").Metadata(ctx)
	if err != nil {
		t.Fatalf("routines.get: %v", err)
	}
	if r.Language != "PYTHON" {
		t.Errorf("routine language %q", r.Language)
	}
	row, err := sparkQueryRow(ctx, c, fmt.Sprintf("DECLARE mark STRING; CALL `%s.custom`(mark); SELECT mark", ds))
	if err != nil {
		t.Fatalf("CALL (container_image): %v", err)
	}
	if row[0] != "from-the-custom-container:/usr/local/bin/python3:1099:5" {
		t.Errorf("mark %v, want the custom container's package, its PYSPARK_PYTHON, UID 1099 and a Spark count", row[0])
	}

	if err := mlRunIn(ctx, c, fmt.Sprintf("CREATE PROCEDURE `%s.elsewhere`() WITH CONNECTION `%s` "+
		"OPTIONS(engine=\"SPARK\", container_image=\"docker.io/library/python:3\") LANGUAGE PYTHON AS r'print(1)'", ds, conn)); err == nil ||
		!strings.Contains(err.Error(), "Artifact Registry") {
		t.Errorf("container_image outside Artifact Registry: %v", err)
	}
	if err := mlRunIn(ctx, c, fmt.Sprintf("CREATE PROCEDURE `%s.missing`() WITH CONNECTION `%s` "+
		"OPTIONS(engine=\"SPARK\", container_image=\"us-docker.pkg.dev/%s/%s/nothing:1\") LANGUAGE PYTHON AS r'print(1)'", ds, conn, project, repoID)); err != nil {
		t.Fatalf("CREATE PROCEDURE missing: %v", err)
	}
	if err := mlRunIn(ctx, c, fmt.Sprintf("CALL `%s.missing`()", ds)); err == nil || !strings.Contains(err.Error(), "cannot be read from Artifact Registry") {
		t.Errorf("CALL with an image the registry does not hold: %v", err)
	}
}

// splitHostPort is net.SplitHostPort of an endpoint that may have a
// scheme.
func splitHostPort(ep string) (string, string, error) {
	ep = strings.TrimPrefix(strings.TrimPrefix(ep, "http://"), "https://")
	i := strings.LastIndex(ep, ":")
	if i < 0 {
		return ep, "", errors.New("no port in " + ep)
	}
	return ep[:i], ep[i+1:], nil
}

// covers: bigquery.jobs.query, bigquery.jobs.insert, bigquery.datasets.update, bigquery.routines.get
//
// TestBigQuerySparkProcedureInvoker (#2104, round 3): EXTERNAL SECURITY
// INVOKER, "The procedure to be executed with the privileges of the user
// that calls it" (the DDL reference). The procedure reads a table of
// another dataset through Google's BigQuery connector. Called with an
// access token the instance minted for a principal (IAM Credentials
// GenerateAccessToken) that may read the procedure's dataset but not the
// table's, the job is refused the table (the connector's tables.get is
// checked as the caller's: bigquery.tables.get), and the CALL fails; once
// the table's dataset grants the caller READER, it runs. The same code
// made DEFINER (the default) runs as its connection's service account
// whatever the caller may read. With SET
// @@spark_proc_properties.service_account (the page's custom service
// account), the job runs as that account, held to its own access; one
// that does not exist fails the CALL. routines.get shows securityMode
// INVOKER.
func TestBigQuerySparkProcedureInvoker(t *testing.T) {
	h, c, project := sparkEnv(t)
	if h.Endpoint(EnvMetadata) == "" {
		t.Skip("no metadata endpoint: IAM Credentials mints the caller's token")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	under := strings.ReplaceAll(h.Project(), "-", "_")
	_, conn := sparkConnection(t, h, project, under+"_sparkinv", nil)
	procs := sparkDataset(t, c, "compat_sparkinv_procs_"+under)
	data := sparkDataset(t, c, "compat_sparkinv_data_"+under)
	if err := mlRunIn(ctx, c, fmt.Sprintf("CREATE TABLE `%s.words` AS SELECT * FROM UNNEST(['a', 'b', 'c']) AS word", data)); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	code := fmt.Sprintf(`
from pyspark.sql import SparkSession
from bigquery.spark.procedure import SparkProcParamContext
spark = SparkSession.builder.getOrCreate()
ctx = SparkProcParamContext.getOrCreate(spark)
ctx.n = spark.read.format("bigquery").load("%s.%s.words").count()
`, project, data)
	for name, security := range map[string]string{"as_invoker": "EXTERNAL SECURITY INVOKER ", "as_definer": ""} {
		if err := mlRunIn(ctx, c, fmt.Sprintf("CREATE PROCEDURE `%s.%s`(OUT n INT64) %sWITH CONNECTION `%s` OPTIONS(engine=\"SPARK\") "+
			"LANGUAGE PYTHON AS r\"\"\"%s\"\"\"", procs, name, security, conn, code)); err != nil {
			t.Fatalf("CREATE PROCEDURE %s: %v", name, err)
		}
	}
	svc, err := bq.NewService(ctx, option.WithEndpoint(h.Endpoint(EnvBigQuery)), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	r, err := svc.Routines.Get(project, procs, "as_invoker").Context(ctx).Do()
	if err != nil {
		t.Fatalf("routines.get: %v", err)
	}
	if r.SecurityMode != "INVOKER" {
		t.Errorf("securityMode %q, want INVOKER", r.SecurityMode)
	}

	grantReader := func(dataset, email string) {
		t.Helper()
		md, err := c.Dataset(dataset).Metadata(ctx)
		if err != nil {
			t.Fatal(err)
		}
		access := append(md.Access, &bigquery.AccessEntry{Role: bigquery.ReaderRole, EntityType: bigquery.UserEmailEntity, Entity: email})
		if _, err := c.Dataset(dataset).Update(ctx, bigquery.DatasetMetadataToUpdate{Access: access}, md.ETag); err != nil {
			t.Fatalf("grant READER on %s: %v", dataset, err)
		}
	}
	until := func(what string, f func() (bool, string)) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Minute)
		for {
			ok, got := f()
			if ok {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: %s", what, got)
			}
			time.Sleep(2 * time.Second)
		}
	}

	// The caller: a principal with a minted token.
	p := newIAMPrincipalIn(t, h, "CLOUDBURROW_TEST_BIGQUERY_RESOURCEMANAGER", project, "sparkinv")
	p.grant("roles/bigquery.jobUser")
	grantReader(procs, p.email)
	pc, err := bigquery.NewClient(ctx, project, option.WithEndpoint(h.Endpoint(EnvBigQuery)),
		option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: p.token, TokenType: "Bearer"})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	call := func(cl *bigquery.Client, prefix, name string) (int64, error) {
		row, err := sparkQueryRow(ctx, cl, fmt.Sprintf("%sDECLARE n INT64; CALL `%s.%s`(n); SELECT n", prefix, procs, name))
		if err != nil {
			return 0, err
		}
		n, _ := row[0].(int64)
		return n, nil
	}
	// DEFINER: the connection's identity reads the table (once the
	// principal's roles have reached the front).
	until("DEFINER called by the principal", func() (bool, string) {
		n, err := call(pc, "", "as_definer")
		return err == nil && n == 3, fmt.Sprintf("%d, %v", n, err)
	})
	// INVOKER: the caller's own access, which lacks the table's dataset.
	n, err := call(pc, "", "as_invoker")
	var ge *googleapi.Error
	if err == nil || !strings.Contains(err.Error(), "bigquery.tables.get") {
		t.Errorf("INVOKER called by a principal that cannot read %s: %d, %v; want the job refused bigquery.tables.get", data, n, err)
	} else if errors.As(err, &ge) && ge.Code == http.StatusServiceUnavailable {
		t.Errorf("INVOKER: the runner was unavailable: %v", err)
	}
	grantReader(data, p.email)
	until("INVOKER once the caller may read the table", func() (bool, string) {
		n, err := call(pc, "", "as_invoker")
		return err == nil && n == 3, fmt.Sprintf("%d, %v", n, err)
	})

	// A custom service account set before the CALL: its own access.
	sa := newServiceAccount(t, h, project, "sparkrunas")
	set := fmt.Sprintf("SET @@spark_proc_properties.service_account = '%s';\n", sa.GetEmail())
	if n, err := call(c, set, "as_invoker"); err == nil || !strings.Contains(err.Error(), "bigquery.tables.get") {
		t.Errorf("INVOKER as a service account that cannot read %s: %d, %v; want bigquery.tables.get refused", data, n, err)
	}
	// The page's custom service account permissions: the tables', and,
	// as Google's connector runs a query to count, bigquery.jobs.create.
	saRoles := projectRoles(t, h, project, "serviceAccount:"+sa.GetEmail())
	saRoles("roles/bigquery.jobUser", true)
	t.Cleanup(func() { saRoles("roles/bigquery.jobUser", false) })
	grantReader(procs, sa.GetEmail())
	grantReader(data, sa.GetEmail())
	until("INVOKER as a service account that may read the table", func() (bool, string) {
		n, err := call(c, set, "as_invoker")
		return err == nil && n == 3, fmt.Sprintf("%d, %v", n, err)
	})
	missing := "SET @@spark_proc_properties.service_account = 'nobody-here@" + project + ".iam.gserviceaccount.com';\n"
	if _, err := call(c, missing, "as_invoker"); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("INVOKER as a service account that does not exist: %v", err)
	}
	if _, err := call(c, "SET @@spark_proc_properties.no_such_field = 'x';\n", "as_invoker"); err == nil ||
		!strings.Contains(err.Error(), "spark_proc_properties") {
		t.Errorf("an unknown @@spark_proc_properties field: %v", err)
	}
}
