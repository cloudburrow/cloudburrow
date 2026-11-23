package sparkproc

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/cmek"
)

type fakeEngine struct {
	got Job
	res Result
	err error
}

func (f *fakeEngine) Run(_ context.Context, job Job) (Result, error) {
	f.got = job
	return f.res, f.err
}

// TestClientServer: a job and its result cross the runner's HTTP API; a
// runner that cannot run Spark answers an *Unavailable.
func TestClientServer(t *testing.T) {
	e := &fakeEngine{res: Result{Out: map[string]string{"n": "3"}}}
	srv := httptest.NewServer(NewServer(e))
	defer srv.Close()
	c := NewClient(srv.URL, nil)
	res, err := c.Run(context.Background(), Job{SparkJobID: "j", Code: "print(1)", Params: map[string]string{"x": `"a"`},
		PyFiles: []File{{URI: "gs://b/h.py", Data: []byte("x = 1")}}})
	if err != nil || res.Out["n"] != "3" || e.got.Code != "print(1)" || string(e.got.PyFiles[0].Data) != "x = 1" {
		t.Fatalf("run: %+v %v (engine saw %+v)", res, err, e.got)
	}
	e.err = &Unavailable{Msg: "no docker"}
	_, err = c.Run(context.Background(), Job{})
	var u *Unavailable
	if !errors.As(err, &u) || u.Msg != "no docker" {
		t.Errorf("unavailable: %v", err)
	}
	if _, err := NewClient("", nil).Run(context.Background(), Job{}); !errors.As(err, &u) {
		t.Errorf("no address: %v", err)
	}
}

// TestWriteJobAndSubmitArgs: the job's files are written where
// spark-submit is told they are, the connector's endpoints are the relay's
// names, and properties are passed as --conf.
func TestWriteJobAndSubmitArgs(t *testing.T) {
	dir := t.TempDir()
	job := Job{SparkJobID: "bqsp-1", Project: "p", Language: "PYTHON", Main: &File{URI: "gs://b/dir/main.py", Data: []byte("main")},
		PyFiles: []File{{URI: "gs://b/h.py", Data: []byte("h")}}, Files: []File{{URI: "gs://b/f.txt", Data: []byte("f")}},
		Properties: map[string]string{"spark.cb.marker": "m1"}}
	a, err := writeJob(dir, job)
	if err != nil {
		t.Fatal(err)
	}
	if a.main != "/cb/main/main.py" || len(a.pyFiles) != 1 || a.pyFiles[0] != "/cb/py/0/h.py" || a.files[0] != "/cb/files/0/f.txt" {
		t.Errorf("args %+v", a)
	}
	for _, rel := range []string{"main/main.py", "py/0/h.py", "lib/bigquery/spark/procedure.py", "relay.py", "run.sh"} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s: %v", rel, err)
		}
	}
	args := strings.Join(submitArgs(job, a, "secret-tok-123", map[string]string{"spark.cb.marker": "overridden", "spark.cb.extra": "x"}), " ")
	for _, want := range []string{"--master local[*]", "--py-files /cb/py/0/h.py", "--conf spark.cb.marker=m1",
		"bigQueryStorageGrpcEndpoint=bigquerystorage.cloudburrow.internal:8443", "bigQueryHttpEndpoint=http://host.docker.internal:8080",
		"--jars /cb-connector/spark-bigquery.jar", "/cb/main/main.py", "--conf spark.cb.extra=x", "--properties-file /cb/token/spark.conf"} {
		if !strings.Contains(args, want) {
			t.Errorf("spark-submit %s: no %q", args, want)
		}
	}
	if strings.Contains(args, "secret-tok-123") {
		t.Errorf("spark-submit %s: the token is on the command line", args)
	}
	if err := writeToken(dir, "tok"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "token", "spark.conf")); string(b) != "spark.datasource.bigquery.gcpAccessToken tok\n" {
		t.Errorf("token file %q", b)
	}
	jar := Job{SparkJobID: "j", Language: "JAVA", MainClass: "com.example.Main", Jars: []File{{URI: "gs://b/app.jar", Data: []byte("j")}}}
	a, _ = writeJob(t.TempDir(), jar)
	if args := submitArgs(jar, a, accessToken, nil); args[len(args)-1] != "/cb/jars/0/app.jar" || !strings.Contains(strings.Join(args, " "), "--class com.example.Main") {
		t.Errorf("java: %v", args)
	}
	if err := writeTLS(filepath.Join(dir, "tls")); err != nil {
		t.Fatal(err)
	}
}

// TestFailure: the error of a failed application is its traceback's last
// line, or the Java exception py4j carries.
func TestFailure(t *testing.T) {
	py := []string{"Traceback (most recent call last):", `  File "/cb/main/main.py", line 1, in <module>`, "ValueError: kaboom",
		"26/11/21 09:12:01 INFO ShutdownHookManager: Shutdown hook called"}
	if got := failure(py); got != "ValueError: kaboom" {
		t.Errorf("python: %q", got)
	}
	java := []string{"py4j.protocol.Py4JJavaError: An error occurred while calling o34.load.", ": com.example.BigQueryException: 404 Not Found",
		"\tat com.example.X(X.java:1)"}
	if got := failure(java); got != "com.example.BigQueryException: 404 Not Found" {
		t.Errorf("py4j: %q", got)
	}
	jvm := []string{`Exception in thread "main" org.apache.hadoop.security.KerberosAuthException: failure to login`, "\tat x.y(Z.java:1)"}
	if got := failure(jvm); got != "org.apache.hadoop.security.KerberosAuthException: failure to login" {
		t.Errorf("jvm: %q", got)
	}
	if got := failure(nil); got == "" {
		t.Error("no lines: empty")
	}
}

// TestRuntimeAndRegistryPath: a job's runtime is its runtime_version's
// MAJOR.MINOR or the default; container_image is an Artifact Registry
// path.
func TestRuntimeAndRegistryPath(t *testing.T) {
	conn := func(context.Context) (string, error) { return "", nil }
	d := &Docker{Default: "2.2", Runtimes: map[string]Runtime{"2.2": {Image: "a", Connector: conn}, "3.0": {Image: "b", Connector: conn}}}
	for _, tc := range []struct{ v, want string }{{"", "a"}, {"3.0", "b"}, {"2.2.30", "a"}} {
		rt, _, ok := d.runtime(Job{RuntimeVersion: tc.v})
		if !ok || rt.Image != tc.want {
			t.Errorf("runtime %q: %+v %v, want %s", tc.v, rt, ok, tc.want)
		}
	}
	if _, _, ok := d.runtime(Job{RuntimeVersion: "9.9"}); ok {
		t.Error("runtime 9.9 served")
	}
	if loc, rest, err := registryPath("us-docker.pkg.dev/p/r/img:1"); err != nil || loc != "us" || rest != "p/r/img:1" {
		t.Errorf("registryPath: %q %q %v", loc, rest, err)
	}
	for _, bad := range []string{"docker.io/library/python:3", "us-docker.pkg.dev/p/img", "gcr.io/p/r/i"} {
		if _, _, err := registryPath(bad); err == nil || !strings.Contains(err.Error(), "cannot fetch containers from other registries") {
			t.Errorf("registryPath(%s): %v", bad, err)
		}
	}
}

// TestKeyCheck (#2104): a job's customer-managed key is checked before
// anything runs; a KeyOnly job is the check alone.
func TestKeyCheck(t *testing.T) {
	ctx := context.Background()
	job := Job{Project: "p", KMSKey: "projects/p/locations/us-central1/keyRings/r/cryptoKeys/k", KeyAgent: "bq-1@bigquery-encryption.iam.gserviceaccount.com", KeyOnly: true}
	if res, err := (&Docker{}).Run(ctx, job); err != nil || res.KeyKind != "noKMS" {
		t.Errorf("no Cloud KMS: %+v, %v", res, err)
	}
	var seen string
	d := &Docker{KeyCheck: func(_ context.Context, project, agent, key string) error {
		seen = project + " " + agent + " " + key
		return &cmek.Error{Kind: cmek.PermissionDenied, Key: key, Msg: "no role"}
	}}
	res, err := d.Run(ctx, job)
	if err != nil || res.KeyKind != "permissionDenied" || !strings.Contains(res.KeyRefused, "no role") {
		t.Errorf("refused key: %+v, %v", res, err)
	}
	if seen != "p "+job.KeyAgent+" "+job.KMSKey {
		t.Errorf("checked %q", seen)
	}
	d.KeyCheck = func(context.Context, string, string, string) error { return nil }
	if res, err := d.Run(ctx, job); err != nil || res.KeyRefused != "" || res.Failed {
		t.Errorf("usable key, KeyOnly: %+v, %v", res, err)
	}
}

// TestSolrPointSchema (#2162): Ranger's Solr 8 audit schema, its Trie
// fields the Point fields Solr 9 has in their place.
func TestSolrPointSchema(t *testing.T) {
	in := `<schema name="ranger-audit-schema">
<fieldType name="date" class="solr.TrieDateField" docValues="true" precisionStep="0" positionIncrementGap="0"/>
<fieldType name="tlongs" class="solr.TrieLongField" docValues="true" precisionStep="8" multiValued="true" positionIncrementGap="0"/>
<fieldType name="booleans" class="solr.BoolField" multiValued="true" sortMissingLast="true"/>
</schema>`
	got := string(solrPointSchema([]byte(in)))
	for _, want := range []string{`<fieldType name="date" class="solr.DatePointField" docValues="true" positionIncrementGap="0"/>`,
		`<fieldType name="tlongs" class="solr.LongPointField" docValues="true" multiValued="true" positionIncrementGap="0"/>`,
		`<fieldType name="text_general" class="solr.TextField"`, `<fieldType name="pdates"`, `<dynamicField name="*_str"`} {
		if !strings.Contains(got, want) {
			t.Errorf("solrPointSchema has no %s:\n%s", want, got)
		}
	}
	if strings.Count(got, `name="booleans"`) != 1 || strings.Contains(got, "Trie") || strings.Contains(got, "precisionStep") || !strings.HasSuffix(got, "</schema>") {
		t.Errorf("solrPointSchema =\n%s", got)
	}
}

// TestRangerHiveConf (#2162): HiveServer2 authorizes with Ranger's Hive
// plugin against the hive-dataproc service on the master's Admin server,
// auditing to Solr, in files HiveServer2 alone reads.
func TestRangerHiveConf(t *testing.T) {
	c := &Cluster{spec: ClusterSpec{Name: "rg"}}
	conf := c.rangerHiveConf()
	if got := conf["hiveserver2-site.xml"]["hive.security.authorization.manager"]; got != "org.apache.ranger.authorization.hive.authorizer.RangerHiveAuthorizerFactory" {
		t.Errorf("authorization manager = %q", got)
	}
	sec := conf["ranger-hive-security.xml"]
	if sec["ranger.plugin.hive.service.name"] != "hive-dataproc" || sec["ranger.plugin.hive.policy.rest.url"] != "http://rg-m:6080" {
		t.Errorf("ranger-hive-security.xml = %v", sec)
	}
	if conf["ranger-hive-audit.xml"]["xasecure.audit.destination.solr.urls"] != "http://localhost:8983/solr/ranger_audits" {
		t.Errorf("ranger-hive-audit.xml = %v", conf["ranger-hive-audit.xml"])
	}
	if _, ok := conf["hive-site.xml"]; ok {
		t.Error("the authorizer must not be in hive-site.xml, which Spark and the drivers read")
	}
}

// TestRangerYarnConf (#2162): with RANGER, the ResourceManager authorizes
// queue access with Ranger's YARN plugin against the yarn-dataproc
// service on the master's Admin server, auditing to Solr; without it,
// YARN's ACLs stay off.
func TestRangerYarnConf(t *testing.T) {
	c := &Cluster{spec: ClusterSpec{Name: "rg", Components: []string{ComponentSolr, ComponentRanger}}}
	yarn, _ := c.yarnConf(nil)
	if yarn["yarn.acl.enable"] != "true" || yarn["yarn.authorization-provider"] != "org.apache.ranger.authorization.yarn.authorizer.RangerYarnAuthorizer" {
		t.Errorf("yarn-site.xml with RANGER = %v", yarn)
	}
	conf := c.rangerYarnConf()
	sec := conf["ranger-yarn-security.xml"]
	if sec["ranger.plugin.yarn.service.name"] != "yarn-dataproc" || sec["ranger.plugin.yarn.policy.rest.url"] != "http://rg-m:6080" {
		t.Errorf("ranger-yarn-security.xml = %v", sec)
	}
	if conf["ranger-yarn-audit.xml"]["xasecure.audit.destination.solr.urls"] != "http://localhost:8983/solr/ranger_audits" {
		t.Errorf("ranger-yarn-audit.xml = %v", conf["ranger-yarn-audit.xml"])
	}
	plain := &Cluster{spec: ClusterSpec{Name: "pl"}}
	if y, _ := plain.yarnConf(nil); y["yarn.acl.enable"] != "" || y["yarn.authorization-provider"] != "" {
		t.Errorf("yarn-site.xml without RANGER = %v", y)
	}
}
