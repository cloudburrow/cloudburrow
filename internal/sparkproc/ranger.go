package sparkproc

// The RANGER optional component (#2162), as the component page documents
// it (https://docs.cloud.google.com/dataproc/docs/concepts/components/ranger,
// read 2026-11-23): the Ranger Admin server and web UI on the first
// master's port 6080, its database in "the MySql database instance running
// on the cluster's first master node", its audit logs in Solr (the SOLR
// component, which RANGER requires), and the admin user's password the one
// the cluster's dataproc:ranger.admin.password.uri holds, encrypted with
// dataproc:ranger.kms.key.uri (decrypted by the API before the cluster
// starts: ClusterSpec.RangerPassword). The Admin server's image is built
// on this machine from the pinned Apache Ranger 2.8.0 source release
// (ranger/Dockerfile); MySQL is the Docker official image Cloud SQL for
// MySQL 8.0 runs.

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ComponentRanger is the RANGER optional component.
const ComponentRanger = "RANGER"

//go:embed ranger/Dockerfile ranger/ranger.sh ranger/build.pom.xml ranger/distro.pom.xml ranger/m2.sha256 ranger/hive4.patch
var rangerFS embed.FS

var rangerFiles = []string{"Dockerfile", "ranger.sh", "build.pom.xml", "distro.pom.xml", "m2.sha256", "hive4.patch"}

// RangerTag is the Ranger Admin image's tag: the build context's hash.
func RangerTag() string {
	h := sha256.New()
	for _, name := range rangerFiles {
		b, _ := rangerFS.ReadFile("ranger/" + name)
		h.Write([]byte(name))
		h.Write(b)
	}
	return "cloudburrow/dataproc-ranger:" + hex.EncodeToString(h.Sum(nil))[:16]
}

var rangerMu sync.Mutex

// RangerBuildTimeout bounds the image's build: a Maven build of the
// Ranger modules the Admin server and the four plugins need.
const RangerBuildTimeout = 3 * time.Hour

// RangerImage builds the Ranger Admin image once, labelled for instance,
// and returns its tag.
func RangerImage(ctx context.Context, instance string) (string, error) {
	rangerMu.Lock()
	defer rangerMu.Unlock()
	tag := RangerTag()
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", tag).Run(); err == nil {
		return tag, nil
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range rangerFiles {
		b, _ := rangerFS.ReadFile("ranger/" + name)
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(b))}); err != nil {
			return "", err
		}
		if _, err := tw.Write(b); err != nil {
			return "", err
		}
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	args := []string{"build", "-t", tag}
	if instance != "" {
		args = append(args, "--label", InstanceLabel+"="+instance)
	}
	ctx, cancel := context.WithTimeout(ctx, RangerBuildTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", append(args, "-")...)
	cmd.Stdin = &buf
	out, err := cmd.CombinedOutput()
	if err != nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		// BuildKit's summary (the last lines) quotes the failed step, not
		// why it failed: Maven's errors and the files m2.sha256 does not
		// pin come first.
		var why []string
		unpinned := false
		for _, l := range lines {
			unpinned = unpinned || strings.Contains(l, "not in m2.sha256")
			if unpinned || strings.Contains(l, "[ERROR]") {
				why = append(why, l)
			}
		}
		if len(why) > 30 {
			why = why[:30]
		}
		if len(lines) > 15 {
			lines = lines[len(lines)-15:]
		}
		return "", fmt.Errorf("docker build: %v: %s", err, strings.Join(append(why, lines...), "\n"))
	}
	return tag, nil
}

// startRanger starts the cluster's MySQL and Ranger Admin server on the
// master, Solr's ranger_audits core first (Solr runs: RANGER requires it).
func (c *Cluster) startRanger(ctx context.Context, e *Ecosystem) error {
	if e.Ranger == nil || e.RangerMySQL == "" {
		return &Unavailable{Msg: "the Spark runner serves no Ranger Admin image"}
	}
	if c.spec.RangerPassword == "" {
		return &Unavailable{Msg: "the Ranger admin password was not decrypted"}
	}
	image, err := e.Ranger(ctx)
	if err != nil {
		return &Unavailable{Msg: "build the Ranger Admin image from the Apache Ranger 2.8.0 source release: " + err.Error()}
	}
	dir := filepath.Join(c.dir, "ranger")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	root := make([]byte, 16)
	if _, err := rand.Read(root); err != nil {
		return err
	}
	// The containers run as root (MySQL's entrypoint reads the file
	// before it drops to mysql), so the files need only be readable.
	for name, v := range map[string]string{"admin-password": c.spec.RangerPassword, "db-root-password": "Rr1" + hex.EncodeToString(root)} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v), 0o644); err != nil {
			return err
		}
	}
	if err := c.runComponent(ctx, "ranger-db", "", "-v", dir+":/cb/ranger:ro",
		"-e", "MYSQL_ROOT_PASSWORD_FILE=/cb/ranger/db-root-password", e.RangerMySQL,
		// Ranger's MySQL schema: its JDBC setup signs in with native
		// passwords, and its indexes fit utf8mb3, not 8.0's utf8mb4.
		"--default-authentication-plugin=mysql_native_password", "--character-set-server=utf8mb3", "--collation-server=utf8mb3_general_ci"); err != nil {
		return err
	}
	if err := c.waitLog(ctx, c.component("ranger-db"), regexp.MustCompile(`ready for connections.*port: 3306`), 5*time.Minute); err != nil {
		return err
	}
	if err := c.runComponent(ctx, "ranger", "", "-v", dir+":/cb/ranger:ro", "-e", "CB_MASTER="+c.nodeAlias(-1), image); err != nil {
		return err
	}
	ranger := c.component("ranger")
	// The Hive plugin, for HiveServer2's container (hivePluginDir).
	_ = os.RemoveAll(filepath.Join(dir, "hive-plugin"))
	if out, err := c.d.docker(ctx, "cp", ranger+":/opt/ranger/ranger-2.8.0-hive-plugin/lib/.", filepath.Join(dir, "hive-plugin")); err != nil {
		return &Unavailable{Msg: "copy Ranger's Hive plugin: " + out}
	}
	if err := c.rangerAuditCore(ctx, ranger, dir); err != nil {
		return err
	}
	return c.waitLog(ctx, ranger, regexp.MustCompile(`CLOUDBURROW RANGER ADMIN READY`), 15*time.Minute)
}

// rangerAuditCore creates Solr's ranger_audits core, where the Admin
// server reads and the plugins write the audit log, with Ranger's own
// audit schema (its solr_audit_conf): written for Solr 8, its Trie fields
// are the Point fields Solr 9 has in their place, in the config set of
// the Solr component's own release, soft-committing every 5 seconds as
// Ranger's own config does (solr.autoSoftCommit.maxTime).
func (c *Cluster) rangerAuditCore(ctx context.Context, ranger, dir string) error {
	solr := c.component("solr")
	schema := filepath.Join(dir, "managed-schema")
	if out, err := c.d.docker(ctx, "cp", ranger+":/opt/ranger/solr_audit_conf/managed-schema", schema); err != nil {
		return &Unavailable{Msg: "copy Ranger's Solr audit schema: " + out}
	}
	b, err := os.ReadFile(schema)
	if err != nil {
		return err
	}
	out := solrPointSchema(b)
	if err := os.WriteFile(filepath.Join(dir, "managed-schema.xml"), out, 0o644); err != nil {
		return err
	}
	const conf = "/var/solr/ranger_audits_conf"
	if o, err := c.d.docker(ctx, "exec", solr, "bash", "-c", "rm -rf "+conf+" && cp -r /opt/solr/server/solr/configsets/_default/conf "+conf+" && rm -f "+conf+"/managed-schema.xml"); err != nil {
		return &Unavailable{Msg: "make Solr's ranger_audits config set: " + o}
	}
	if o, err := c.d.docker(ctx, "cp", filepath.Join(dir, "managed-schema.xml"), solr+":"+conf+"/managed-schema.xml"); err != nil {
		return &Unavailable{Msg: "copy the audit schema into Solr: " + o}
	}
	if o, err := c.d.docker(ctx, "exec", solr, "solr", "create", "-c", "ranger_audits", "-d", conf); err != nil && !strings.Contains(o, "already exists") {
		return &Unavailable{Msg: "create Solr's ranger_audits core: " + o}
	}
	if o, err := c.d.docker(ctx, "exec", solr, "curl", "-sS", "-f", "-H", "Content-Type: application/json", "-d",
		`{"set-property":{"updateHandler.autoSoftCommit.maxTime":5000}}`, "http://localhost:8983/solr/ranger_audits/config"); err != nil {
		return &Unavailable{Msg: "set the ranger_audits soft commit: " + o}
	}
	return nil
}

// solrTrieRE is a Solr 8 Trie field type's class and precisionStep.
var (
	solrTrieRE      = regexp.MustCompile(`solr\.Trie(Date|Double|Float|Int|Long)Field`)
	solrPrecisionRE = regexp.MustCompile(`\s+precisionStep="[0-9]+"`)
)

// solrDefaultTypes are the field types Solr 9's _default config set
// (whose solrconfig.xml the ranger_audits core runs) names in its
// schemaless update chain and spell checker, which Ranger's schema does
// not define.
var solrDefaultTypes = map[string]string{
	"text_general": `<fieldType name="text_general" class="solr.TextField" positionIncrementGap="100" multiValued="true"><analyzer><tokenizer class="solr.StandardTokenizerFactory"/><filter class="solr.LowerCaseFilterFactory"/></analyzer></fieldType>`,
	"booleans":     `<fieldType name="booleans" class="solr.BoolField" sortMissingLast="true" multiValued="true"/>`,
	"pdates":       `<fieldType name="pdates" class="solr.DatePointField" docValues="true" multiValued="true"/>`,
	"plongs":       `<fieldType name="plongs" class="solr.LongPointField" docValues="true" multiValued="true"/>`,
	"pdoubles":     `<fieldType name="pdoubles" class="solr.DoublePointField" docValues="true" multiValued="true"/>`,
	"*_str":        `<dynamicField name="*_str" type="string" indexed="false" stored="false" docValues="true" multiValued="true" useDocValuesAsStored="false"/>`,
}

// solrPointSchema is a Solr 8 schema for Solr 9: its Trie fields Point
// fields, with the field types of solrDefaultTypes it lacks.
func solrPointSchema(b []byte) []byte {
	b = solrTrieRE.ReplaceAll(b, []byte("solr.${1}PointField"))
	b = solrPrecisionRE.ReplaceAll(b, nil)
	var add strings.Builder
	for _, name := range []string{"text_general", "booleans", "pdates", "plongs", "pdoubles", "*_str"} {
		if !bytes.Contains(b, []byte(`name="`+name+`"`)) {
			add.WriteString("  " + solrDefaultTypes[name] + "\n")
		}
	}
	if add.Len() == 0 {
		return b
	}
	i := bytes.LastIndex(b, []byte("</schema>"))
	if i < 0 {
		return b
	}
	return append(append(append([]byte{}, b[:i]...), add.String()...), b[i:]...)
}

// hivePluginDir is where startRanger copies Ranger's Hive plugin (its
// shim, class loader and ranger-hive-plugin-impl), for HiveServer2.
func (c *Cluster) hivePluginDir() string { return filepath.Join(c.dir, "ranger", "hive-plugin") }

// rangerHiveConf is HiveServer2's configuration for Ranger's Hive plugin,
// as Ranger's enable-hive-plugin.sh writes it: the authorizer in
// hiveserver2-site.xml (HiveServer2's alone: Spark and the drivers read
// hive-site.xml), the plugin's service (hive-dataproc), the Admin server
// on the master's 6080, and the audit log to Solr's ranger_audits.
func (c *Cluster) rangerHiveConf() map[string]map[string]string {
	return map[string]map[string]string{
		"hiveserver2-site.xml": {
			"hive.security.authorization.enabled":           "true",
			"hive.security.authorization.manager":           "org.apache.ranger.authorization.hive.authorizer.RangerHiveAuthorizerFactory",
			"hive.security.authenticator.manager":           "org.apache.hadoop.hive.ql.security.SessionStateUserAuthenticator",
			"hive.conf.restricted.list":                     "hive.security.authorization.enabled,hive.security.authorization.manager,hive.security.authenticator.manager",
			"hive.security.metastore.authorization.manager": "org.apache.hadoop.hive.ql.security.authorization.DefaultHiveMetastoreAuthorizationProvider",
		},
		"ranger-hive-security.xml": {
			"ranger.plugin.hive.service.name":                            "hive-dataproc",
			"ranger.plugin.hive.policy.source.impl":                      "org.apache.ranger.admin.client.RangerAdminRESTClient",
			"ranger.plugin.hive.policy.rest.url":                         "http://" + c.nodeAlias(-1) + ":6080",
			"ranger.plugin.hive.policy.pollIntervalMs":                   "5000",
			"ranger.plugin.hive.policy.cache.dir":                        "/tmp/ranger-hive-policycache",
			"ranger.plugin.hive.policy.rest.client.connection.timeoutMs": "120000",
			"ranger.plugin.hive.policy.rest.client.read.timeoutMs":       "30000",
			"xasecure.hive.update.xapolicies.on.grant.revoke":            "true",
			"ranger.plugin.hive.urlauth.filesystem.schemes":              "hdfs:,file:,gs:",
		},
		"ranger-hive-audit.xml": {
			"xasecure.audit.is.enabled":                           "true",
			"xasecure.audit.destination.solr":                     "true",
			"xasecure.audit.destination.solr.urls":                "http://localhost:8983/solr/ranger_audits",
			"xasecure.audit.destination.solr.batch.filespool.dir": "/tmp/ranger-hive-audit-spool",
		},
	}
}

// rangerYarnPlugin is where the Ranger Admin image has the YARN plugin's
// lib directory: its shim and class loader, with the plugin itself in
// ranger-yarn-plugin-impl beside them, where the shim's class loader
// looks for it.
const rangerYarnPlugin = "/opt/ranger/ranger-2.8.0-yarn-plugin/lib"

// rangerYarnSite are the yarn-site.xml properties Ranger's
// enable-yarn-plugin.sh sets (its yarn-site-changes.cfg): YARN's ACLs on,
// authorized by Ranger's YARN plugin. A queue access no Ranger policy
// decides falls back to the queue's own ACLs (ranger.add-yarn-authorization,
// true by default), so a deny policy denies and the rest is as before.
func rangerYarnSite() map[string]string {
	return map[string]string{
		"yarn.acl.enable":             "true",
		"yarn.authorization-provider": "org.apache.ranger.authorization.yarn.authorizer.RangerYarnAuthorizer",
	}
}

// rangerYarnConf is the ResourceManager's configuration for Ranger's YARN
// plugin, as enable-yarn-plugin.sh writes it: the plugin's service
// (yarn-dataproc), the Admin server on the master's 6080, and the audit
// log to Solr's ranger_audits.
func (c *Cluster) rangerYarnConf() map[string]map[string]string {
	return map[string]map[string]string{
		"ranger-yarn-security.xml": {
			"ranger.plugin.yarn.service.name":                            "yarn-dataproc",
			"ranger.plugin.yarn.policy.source.impl":                      "org.apache.ranger.admin.client.RangerAdminRESTClient",
			"ranger.plugin.yarn.policy.rest.url":                         "http://" + c.nodeAlias(-1) + ":6080",
			"ranger.plugin.yarn.policy.pollIntervalMs":                   "5000",
			"ranger.plugin.yarn.policy.cache.dir":                        "/tmp/ranger-yarn-policycache",
			"ranger.plugin.yarn.policy.rest.client.connection.timeoutMs": "120000",
			"ranger.plugin.yarn.policy.rest.client.read.timeoutMs":       "30000",
		},
		"ranger-yarn-audit.xml": {
			"xasecure.audit.is.enabled":                           "true",
			"xasecure.audit.destination.solr":                     "true",
			"xasecure.audit.destination.solr.urls":                "http://" + c.nodeAlias(-1) + ":8983/solr/ranger_audits",
			"xasecure.audit.destination.solr.batch.filespool.dir": "/tmp/ranger-yarn-audit-spool",
		},
	}
}
