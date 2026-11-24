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
	if conf["ranger-yarn-audit.xml"]["xasecure.audit.destination.solr.urls"] != "http://rg-m:8983/solr/ranger_audits" {
		t.Errorf("ranger-yarn-audit.xml = %v", conf["ranger-yarn-audit.xml"])
	}
	plain := &Cluster{spec: ClusterSpec{Name: "pl"}}
	if y, _ := plain.yarnConf(nil); y["yarn.acl.enable"] != "" || y["yarn.authorization-provider"] != "" {
		t.Errorf("yarn-site.xml without RANGER = %v", y)
	}
}

// TestHDFSConf (#2186): a YARN cluster's core-site.xml names its HDFS
// (hdfs://CLUSTER-m), its hdfs-site.xml the NameNode on the master,
// dfs.replication for the worker count and the cluster's hdfs:
// properties, and with RANGER the NameNode's Ranger HDFS plugin.
func TestHDFSConf(t *testing.T) {
	dir := t.TempDir()
	c := &Cluster{d: &Docker{Ecosystem: &Ecosystem{Hive: "hive", GCS: func(context.Context) (string, error) { return "", nil }}},
		dir: dir, spec: ClusterSpec{Name: "hd", Workers: 3, Components: []string{ComponentSolr, ComponentRanger},
			ConfProperties: map[string]string{"hdfs:dfs.blocksize": "64m", "hdfs:dfs.replication": "3"}}}
	if err := c.writeConf(filepath.Join(dir, "conf"), nil, nil); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, "conf", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for name, want := range map[string][]string{
		"core-site.xml": {"<name>fs.defaultFS</name><value>hdfs://hd-m</value>"},
		"hdfs-site.xml": {"<name>dfs.namenode.rpc-address</name><value>hd-m:8020</value>", "<name>dfs.blocksize</name><value>64m</value>",
			"<name>dfs.replication</name><value>3</value>", "<name>dfs.namenode.http-address</name><value>hd-m:9870</value>",
			"<name>dfs.namenode.inode.attributes.provider.class</name><value>org.apache.ranger.authorization.hadoop.RangerHdfsAuthorizer</value>",
			"<name>dfs.permissions.enabled</name><value>true</value>"},
		"hive-site.xml":            {"<name>hive.metastore.warehouse.dir</name><value>/user/hive/warehouse</value>"},
		"mapred-site.xml":          {"<name>yarn.app.mapreduce.am.staging-dir</name><value>/tmp/hadoop-yarn/staging</value>"},
		"ranger-hdfs-security.xml": {"<name>ranger.plugin.hdfs.service.name</name><value>hadoop-dataproc</value>"},
		"ranger-hdfs-audit.xml":    {"<name>xasecure.audit.destination.solr.urls</name><value>http://hd-m:8983/solr/ranger_audits</value>"},
	} {
		got := read(name)
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("%s lacks %s:\n%s", name, w, got)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "conf", "nodes_exclude")); err != nil {
		t.Errorf("nodes_exclude: %v", err)
	}
	for workers, want := range map[int]int{0: 1, 1: 1, 2: 2, 5: 2} {
		if got := (&Cluster{spec: ClusterSpec{Workers: workers}}).replication(); got != want {
			t.Errorf("replication with %d workers = %d; want %d", workers, got, want)
		}
	}
	plain := &Cluster{d: &Docker{}, spec: ClusterSpec{Name: "pl"}}
	if plain.defaultFS() != "file:///" {
		t.Errorf("defaultFS without YARN = %q", plain.defaultFS())
	}
}

// TestAllDecommissioned reads dfsadmin -report's per-node status.
func TestAllDecommissioned(t *testing.T) {
	report := "Live datanodes (2):\n\nName: 10.0.0.3:9866 (hd-w-0)\nHostname: hd-w-0\nDecommission Status : Normal\n\n" +
		"Name: 10.0.0.4:9866 (hd-w-1)\nHostname: hd-w-1\nDecommission Status : Decommission in progress\n"
	if allDecommissioned(report, []string{"hd-w-1"}) {
		t.Error("in progress counted as decommissioned")
	}
	done := strings.Replace(report, "Decommission in progress", "Decommissioned", 1)
	if !allDecommissioned(done, []string{"hd-w-1"}) || !allDecommissioned(report, []string{"hd-w-9"}) {
		t.Error("decommissioned or absent host not counted")
	}
}

// TestHDFSHighAvailabilityConf (#2188): a High Availability cluster's
// core-site.xml names its nameservice (hdfs://CLUSTER) and the masters'
// ZooKeeper ensemble, its hdfs-site.xml the NameNodes nn0 on -m-0 and nn1
// on -m-1, the three masters' JournalNodes and automatic failover; and
// HDFS checks no permissions without Ranger.
func TestHDFSHighAvailabilityConf(t *testing.T) {
	dir := t.TempDir()
	c := &Cluster{d: &Docker{Ecosystem: &Ecosystem{Hive: "hive", GCS: func(context.Context) (string, error) { return "", nil }}},
		dir: dir, spec: ClusterSpec{Name: "ha", Workers: 2, Masters: 3}}
	if err := c.writeConf(filepath.Join(dir, "conf"), nil, nil); err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, "conf", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for name, want := range map[string][]string{
		"core-site.xml": {"<name>fs.defaultFS</name><value>hdfs://ha</value>",
			"<name>ha.zookeeper.quorum</name><value>ha-m-0:2181,ha-m-1:2181,ha-m-2:2181</value>"},
		"hdfs-site.xml": {"<name>dfs.nameservices</name><value>ha</value>", "<name>dfs.ha.namenodes.ha</name><value>nn0,nn1</value>",
			"<name>dfs.namenode.rpc-address.ha.nn0</name><value>ha-m-0:8020</value>",
			"<name>dfs.namenode.rpc-address.ha.nn1</name><value>ha-m-1:8020</value>",
			"<name>dfs.namenode.http-address.ha.nn1</name><value>ha-m-1:9870</value>",
			"<name>dfs.namenode.shared.edits.dir</name><value>qjournal://ha-m-0:8485;ha-m-1:8485;ha-m-2:8485/ha</value>",
			"<name>dfs.ha.automatic-failover.enabled</name><value>true</value>",
			"<name>dfs.client.failover.proxy.provider.ha</name><value>org.apache.hadoop.hdfs.server.namenode.ha.ConfiguredFailoverProxyProvider</value>",
			"<name>dfs.permissions.enabled</name><value>false</value>"},
		"yarn-site.xml": {"<name>yarn.resourcemanager.ha.rm-ids</name><value>rm0,rm1,rm2</value>", "<name>yarn.resourcemanager.hostname.rm1</name><value>ha-m-1</value>"},
	} {
		got := read(name)
		for _, w := range want {
			if !strings.Contains(got, w) {
				t.Errorf("%s lacks %s:\n%s", name, w, got)
			}
		}
	}
	if strings.Contains(read("hdfs-site.xml"), "<name>dfs.namenode.rpc-address</name>") {
		t.Error("a High Availability cluster's hdfs-site.xml names one NameNode")
	}
	if got := c.masterNode(2); got != c.master+"-2" {
		t.Errorf("masterNode(2) = %q", got)
	}
	if got := (&Cluster{spec: ClusterSpec{Name: "one", Masters: 1}}).nodeAlias(-1); got != "one-m" {
		t.Errorf("a standard cluster's master = %q", got)
	}
}

// TestLocalSSDs (#2188): a node's local SSDs are mounted at /mnt/N from
// its disk's directory, and the NodeManager's scratch is on them, unless
// dataproc:dataproc.localssd.mount.enable is false.
func TestLocalSSDs(t *testing.T) {
	root := t.TempDir()
	c := &Cluster{d: &Docker{Disks: root}, spec: ClusterSpec{ID: "uuid-1", Name: "ssd", LocalSSDs: 2}}
	m := c.ssdMounts("ssd-w-0")
	want := []string{"-v", filepath.Join(root, "uuid-1", "ssd-w-0", "local-ssd-1") + ":/mnt/1",
		"-v", filepath.Join(root, "uuid-1", "ssd-w-0", "local-ssd-2") + ":/mnt/2"}
	if strings.Join(m, " ") != strings.Join(want, " ") {
		t.Errorf("ssdMounts = %v; want %v", m, want)
	}
	if got := c.nmLocalDirs(); got != "/mnt/1/hadoop/yarn/nm-local-dir,/mnt/2/hadoop/yarn/nm-local-dir" {
		t.Errorf("nmLocalDirs = %q", got)
	}
	c.spec.LocalSSDsUnmounted = true
	if got := c.nmLocalDirs(); got != "/tmp/yarn-local" || len(c.ssdMounts("ssd-w-0")) != 0 {
		t.Errorf("unmounted SSDs: %q %v", got, c.ssdMounts("ssd-w-0"))
	}
	c.d.RemoveDisks("uuid-1")
	if _, err := os.Stat(filepath.Join(root, "uuid-1")); !os.IsNotExist(err) {
		t.Errorf("the cluster's disks after RemoveDisks: %v", err)
	}
}

// TestYarnHAConf (#2189): a High Availability cluster's yarn-site.xml
// names a ResourceManager on each master (rm0, rm1, rm2), electing the
// active one and keeping the applications' state in the masters'
// ZooKeeper ensemble; with RANGER each loads Ranger's YARN plugin and
// audits to the Solr of -m-0. A standard cluster's has the one
// ResourceManager on the master.
func TestYarnHAConf(t *testing.T) {
	c := &Cluster{spec: ClusterSpec{Name: "ha", Masters: 3, Components: []string{ComponentSolr, ComponentRanger}}}
	yarn, _ := c.yarnConf(nil)
	for k, want := range map[string]string{
		"yarn.resourcemanager.ha.enabled":                    "true",
		"yarn.resourcemanager.ha.automatic-failover.enabled": "true",
		"yarn.resourcemanager.ha.rm-ids":                     "rm0,rm1,rm2",
		"yarn.resourcemanager.cluster-id":                    "ha",
		"yarn.resourcemanager.hostname.rm0":                  "ha-m-0",
		"yarn.resourcemanager.hostname.rm2":                  "ha-m-2",
		"yarn.resourcemanager.webapp.address.rm1":            "ha-m-1:8088",
		"yarn.resourcemanager.recovery.enabled":              "true",
		"yarn.resourcemanager.store.class":                   "org.apache.hadoop.yarn.server.resourcemanager.recovery.ZKRMStateStore",
		"hadoop.zk.address":                                  "ha-m-0:2181,ha-m-1:2181,ha-m-2:2181",
		"yarn.authorization-provider":                        "org.apache.ranger.authorization.yarn.authorizer.RangerYarnAuthorizer",
	} {
		if yarn[k] != want {
			t.Errorf("%s = %q; want %q", k, yarn[k], want)
		}
	}
	for _, k := range []string{"yarn.resourcemanager.hostname", "yarn.resourcemanager.webapp.address"} {
		if v, ok := yarn[k]; ok {
			t.Errorf("an HA cluster's %s = %q", k, v)
		}
	}
	if got := c.rangerYarnConf()["ranger-yarn-audit.xml"]["xasecure.audit.destination.solr.urls"]; got != "http://ha-m-0:8983/solr/ranger_audits" {
		t.Errorf("rm1's and rm2's audit = %q", got)
	}
	if got := c.resourceManagers(); strings.Join(got, ",") != "ha-m-0,ha-m-1,ha-m-2" {
		t.Errorf("resourceManagers = %v", got)
	}
	std := &Cluster{spec: ClusterSpec{Name: "st"}}
	y, _ := std.yarnConf(nil)
	if y["yarn.resourcemanager.hostname"] != "st-m" || y["yarn.resourcemanager.ha.enabled"] != "" {
		t.Errorf("a standard cluster's yarn-site.xml = %v", y)
	}
}
