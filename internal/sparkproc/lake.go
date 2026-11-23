package sparkproc

// The open table formats' optional components (#2162): DELTA, ICEBERG
// and HUDI. As each component page documents
// (https://docs.cloud.google.com/dataproc/docs/concepts/components/delta,
// .../iceberg and .../hudi, read 2026-11-23), the component puts its
// jars on the cluster's Spark and sets spark-defaults.conf's properties:
//
//   - DELTA: spark.sql.extensions io.delta.sql.DeltaSparkSessionExtension
//     and spark.sql.catalog.spark_catalog
//     org.apache.spark.sql.delta.catalog.DeltaCatalog;
//   - ICEBERG (image 2.x): spark.sql.extensions
//     org.apache.iceberg.spark.extensions.IcebergSparkSessionExtensions
//     and the runtime jar on spark.driver.extraClassPath and
//     spark.executor.extraClassPath (/usr/lib/iceberg/lib/); on image 3.0
//     also the default_iceberg catalog, an Iceberg REST catalog
//     (spark.sql.catalog.default_iceberg org.apache.iceberg.spark.SparkCatalog,
//     .type rest, .uri env:ICEBERG_REST_CATALOG_URI), which is the Iceberg
//     REST catalog of a Hive Metastore 4.2 (image 3.0's Hive) on the
//     master, ICEBERG_REST_CATALOG_URI set on every node and driver;
//   - HUDI: spark.serializer KryoSerializer, spark.sql.catalog.spark_catalog
//     org.apache.spark.sql.hudi.catalog.HoodieCatalog, spark.sql.extensions
//     org.apache.spark.sql.hudi.HoodieSparkSessionExtension and the Spark
//     bundle on the class paths (/usr/lib/hudi/lib/).
//
// The jars are the versions the image versions' release notes list,
// from Maven Central, pinned by SHA-256 in dependencies.json
// (Ecosystem.Lake fetches them), mounted read-only at those paths on
// every node and driver. A cluster's own properties win over these.

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The table formats' components.
const (
	ComponentDelta   = "DELTA"
	ComponentIceberg = "ICEBERG"
	ComponentHudi    = "HUDI"
)

// lakeDirs are the directories each component's jars are at on a
// Dataproc node.
var lakeDirs = map[string]string{ComponentDelta: "/usr/lib/delta/jars", ComponentIceberg: "/usr/lib/iceberg/lib", ComponentHudi: "/usr/lib/hudi/lib"}

// lakeProps are each component's spark-defaults.conf properties, the
// class path's aside.
var lakeProps = map[string]map[string]string{
	ComponentDelta: {"spark.sql.extensions": "io.delta.sql.DeltaSparkSessionExtension",
		"spark.sql.catalog.spark_catalog": "org.apache.spark.sql.delta.catalog.DeltaCatalog"},
	ComponentIceberg: {"spark.sql.extensions": "org.apache.iceberg.spark.extensions.IcebergSparkSessionExtensions"},
	ComponentHudi: {"spark.serializer": "org.apache.spark.serializer.KryoSerializer",
		"spark.sql.catalog.spark_catalog": "org.apache.spark.sql.hudi.catalog.HoodieCatalog",
		"spark.sql.extensions":            "org.apache.spark.sql.hudi.HoodieSparkSessionExtension"},
}

// setupLake fetches the cluster's table formats' jars and sets the
// mounts and Spark properties every node and driver gets.
func (c *Cluster) setupLake(ctx context.Context) error {
	if c.spec.HistoryOnly {
		return nil
	}
	var comps []string
	for _, comp := range []string{ComponentDelta, ComponentIceberg, ComponentHudi} {
		if c.has(comp) {
			comps = append(comps, comp)
		}
	}
	e := c.d.Ecosystem
	props := map[string]string{}
	var extensions, classPath []string
	// Google's Cloud Storage connector, as a Dataproc image has it: a
	// job's gs:// paths are the instance's Cloud Storage, through the
	// cluster relay.
	if e != nil && e.GCS != nil && c.d.GCS != nil && c.d.GCS() != "" {
		jar, err := e.GCS(ctx)
		if err != nil {
			return &Unavailable{Msg: "Google's Cloud Storage connector for Hadoop is not available: " + err.Error()}
		}
		in := "/usr/local/share/google/dataproc/lib/gcs-connector.jar"
		c.lakeMounts = append(c.lakeMounts, "-v", jar+":"+in+":ro")
		classPath = append(classPath, in)
		for k, v := range map[string]string{"fs.gs.impl": "com.google.cloud.hadoop.fs.gcs.GoogleHadoopFileSystem",
			"fs.AbstractFileSystem.gs.impl": "com.google.cloud.hadoop.fs.gcs.GoogleHadoopFS", "fs.gs.project.id": c.spec.Project,
			"fs.gs.storage.root.url": "http://" + restHost + ":" + gcsPort + "/", "fs.gs.auth.service.account.enable": "false",
			"fs.gs.auth.null.enable": "true"} {
			props["spark.hadoop."+k] = v
		}
	}
	if len(comps) > 0 && (e == nil || e.Lake == nil) {
		return &Unavailable{Msg: "the Spark runner serves no table format jars"}
	}
	for _, comp := range comps {
		jars, err := e.Lake(ctx, comp, c.spec.ImageVersion, c.spec.IcebergVersion)
		if err != nil {
			return &Unavailable{Msg: "the " + comp + " component's jars are not available: " + err.Error()}
		}
		for _, j := range jars {
			in := lakeDirs[comp] + "/" + filepath.Base(j)
			c.lakeMounts = append(c.lakeMounts, "-v", j+":"+in+":ro")
			classPath = append(classPath, in)
		}
		if comp == ComponentIceberg && c.icebergREST() {
			for k, v := range map[string]string{"spark.sql.catalog.default_iceberg": "org.apache.iceberg.spark.SparkCatalog",
				"spark.sql.catalog.default_iceberg.type": "rest", "spark.sql.catalog.default_iceberg.uri": "env:ICEBERG_REST_CATALOG_URI"} {
				props[k] = v
			}
			c.lakeMounts = append(c.lakeMounts, "-e", "ICEBERG_REST_CATALOG_URI=http://"+c.nodeAlias(-1)+":"+icebergRESTPort+"/iceberg",
				"-v", filepath.Join(c.dir, "warehouse")+":/cb-warehouse")
		}
		for k, v := range lakeProps[comp] {
			if k == "spark.sql.extensions" {
				extensions = append(extensions, v)
				continue
			}
			props[k] = v
		}
	}
	if len(classPath) == 0 {
		return nil
	}
	sort.Strings(classPath)
	if len(extensions) > 0 {
		props["spark.sql.extensions"] = strings.Join(extensions, ",")
	}
	cp := strings.Join(classPath, ":")
	props["spark.driver.extraClassPath"] = cp
	props["spark.executor.extraClassPath"] = cp
	c.lakeProps = props
	return nil
}

// icebergRESTPort is the Hive Metastore's Iceberg REST catalog port
// (metastore.catalog.servlet.port).
const icebergRESTPort = "9001"

// icebergREST says whether the cluster's ICEBERG component has image
// 3.0's Iceberg REST catalog.
func (c *Cluster) icebergREST() bool {
	return c.has(ComponentIceberg) && c.spec.ImageVersion >= "3.0"
}

// startIcebergREST starts the Hive Metastore 4.2 whose Iceberg REST
// catalog (http://CLUSTER-m:9001/iceberg, metastore.catalog.servlet.auth
// none, as the image's own configuration has it) is image 3.0's
// default_iceberg catalog: a container sharing the master's network,
// its thrift port 9084 (the Hive services' metastore has 9083), its
// warehouse the cluster's (file:///cb-warehouse/iceberg), which every
// node and driver mounts.
func (c *Cluster) startIcebergREST(ctx context.Context, image string) error {
	if err := os.MkdirAll(filepath.Join(c.dir, "warehouse", "iceberg"), 0o777); err != nil {
		return err
	}
	_ = os.Chmod(filepath.Join(c.dir, "warehouse", "iceberg"), 0o777)
	if err := c.runComponent(ctx, "iceberg-rest", "", "-v", filepath.Join(c.dir, "warehouse")+":/cb-warehouse",
		"-e", "DEFAULT_FS=file://", "-e", "HIVE_WAREHOUSE_PATH=/cb-warehouse/iceberg", "-e", "METASTORE_PORT=9084",
		"-e", "SERVICE_OPTS=-Xmx512m", image); err != nil {
		return err
	}
	return c.waitPort(ctx, c.component("iceberg-rest"), icebergRESTPort, 5*time.Minute)
}
