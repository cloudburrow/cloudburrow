package sparkproc

// The ZEPPELIN optional component (#2162): Apache Zeppelin 0.10.1, the
// Zeppelin of image versions 2.1 to 2.3, in its official apache/zeppelin
// image (Apache-2.0, pinned by digest in dependencies.json). The image is
// published for linux/amd64 only, so on an arm64 machine Docker runs it
// under emulation, and it is 3.7 GB compressed (12 GB unpacked); it is
// pulled on the first cluster with the component. Image 3.0's Zeppelin
// is 0.12.0; its official image is linux/amd64 only and its Java 11
// cannot run Spark 4.1's driver, so 3.0 runs 0.12.1, the patch release
// published for both architectures (startZeppelin).
//
// As on a Dataproc cluster, Zeppelin runs on the master (its container
// shares the master's network) at its Component Gateway path
// (ZEPPELIN_SERVER_CONTEXT_PATH), and its Spark interpreter runs the
// cluster's Spark: SPARK_HOME is the cluster image's Spark distribution,
// copied from the master node, spark.master the cluster's master, the
// driver at the master's host name. The interpreter's
// zeppelin.spark.enableSupportedVersionCheck is turned off (Zeppelin
// 0.10.1 knows Spark up to 3.2; Google ships it with the image's Spark
// 3.5), set through Zeppelin's interpreter REST API once it has started.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// zeppelinSparkPython runs the Python of the Spark image mounted at
// /cb-sparkimg in Zeppelin's container: with that image's own dynamic
// loader and libraries (its glibc is newer than the Zeppelin image's) and
// its standard library (PYTHONHOME).
const zeppelinSparkPython = `#!/bin/bash
r=/cb-sparkimg
lib=$(ls -d "$r"/usr/lib/*-linux-gnu | head -1)
ld=$(ls "$lib"/ld-linux-*.so.* | head -1)
export PYTHONHOME="$r/usr"
exec "$ld" --library-path "$lib" "$r/usr/bin/python3" "$@"
`

// startZeppelin starts Zeppelin and configures its Spark interpreter.
// On image 3.0 (Spark 4.1, which needs Java 17 or later) image is
// Zeppelin 0.12.1, whose official image is published for linux/arm64 as
// well as linux/amd64, so it runs natively; its Spark is the cluster's
// Spark image mounted read-only (a Docker image mount), whose Java runs
// the interpreter (its JAVA_HOME interpreter property: Zeppelin passes
// upper-case properties to the interpreter's process as its environment).
func (c *Cluster) startZeppelin(ctx context.Context, image string, spark4 bool) error {
	home := filepath.Join(c.dir, "zeppelin-spark")
	sparkHome := "/cb-spark"
	var mounts []string
	if spark4 {
		sparkHome = "/cb-sparkimg/opt/spark"
		py := filepath.Join(c.dir, "zeppelin-python3")
		if err := os.WriteFile(py, []byte(zeppelinSparkPython), 0o755); err != nil {
			return err
		}
		mounts = []string{"--mount", "type=image,source=" + c.rt.Image + ",target=/cb-sparkimg", "-v", py + ":/cb/python3:ro"}
	} else {
		if out, err := c.d.docker(ctx, "cp", c.master+":/opt/spark", home); err != nil {
			return &Unavailable{Msg: "copy the cluster's Spark for Zeppelin: " + out}
		}
		mounts = []string{"--platform", "linux/amd64", "-v", home + ":/cb-spark:ro"}
	}
	master := c.nodeAlias(-1)
	base := strings.TrimSuffix(c.spec.GatewayBase, "/") + "/zeppelin"
	submit := "--conf spark.driver.host=" + master + " --conf spark.driver.bindAddress=0.0.0.0" +
		" --conf spark.eventLog.enabled=true --conf spark.eventLog.dir=file:/cb-history" +
		" --conf spark.executor.extraJavaOptions=-Djavax.net.ssl.trustStore=/tmp/cb-trust.jks"
	// The interpreter's own jars are Zeppelin's --driver-class-path,
	// which a spark.driver.extraClassPath --conf would replace: the
	// connectors' and table formats' jars go on --jars instead.
	jars := []string{"/cb-connector/spark-bigquery.jar"}
	for k, v := range c.lakeProps {
		if k == "spark.driver.extraClassPath" {
			jars = append(jars, strings.Split(v, ":")...)
			continue
		}
		submit += " --conf " + k + "=" + v
	}
	submit += " --jars " + strings.Join(jars, ",")
	args := append(mounts, "-v", c.jar+":/cb-connector/spark-bigquery.jar:ro", "-v", c.HistoryDir()+":/cb-history")
	args = append(args, c.lakeMounts...)
	args = append(args, "-e", "ZEPPELIN_ADDR=0.0.0.0", "-e", "ZEPPELIN_PORT=8080",
		// Its web apps unpack under the context path, which the image's
		// user cannot write in /opt/zeppelin/webapps.
		"-e", "ZEPPELIN_WAR_TEMPDIR=/tmp/zeppelin-webapps", "-e", "ZEPPELIN_SERVER_CONTEXT_PATH="+base,
		"-e", "ZEPPELIN_MEM=-Xmx1024m", "-e", "SPARK_HOME="+sparkHome, "-e", "SPARK_MASTER=spark://"+master+":7077",
		"-e", "SPARK_SUBMIT_OPTIONS="+submit, "-e", "PYSPARK_PYTHON=python3", image)
	start := time.Now()
	if err := c.runComponent(ctx, "zeppelin", "", args...); err != nil {
		return err
	}
	if err := c.waitLog(ctx, c.component("zeppelin"), regexp.MustCompile(`Done, zeppelin server started`), 10*time.Minute); err != nil {
		return err
	}
	c.d.logf("spark: cluster %s's Zeppelin started in %s", c.spec.Name, time.Since(start).Round(time.Second))
	addr := c.Web(ctx)["zeppelin"]
	if addr == "" {
		return &Unavailable{Msg: "the cluster's Zeppelin port is not published"}
	}
	props := map[string]string{"zeppelin.spark.enableSupportedVersionCheck": "false", "zeppelin.spark.useHiveContext": "false",
		"zeppelin.spark.scala.color": "false"}
	if spark4 {
		props["JAVA_HOME"] = "/cb-sparkimg/opt/java/openjdk"
		// Spark 4.1's PySpark needs Python 3.10 or later, which the
		// Zeppelin image has not (3.7, and 3.9 in conda's base): the
		// driver runs the Spark image's python3 (zeppelinSparkPython),
		// the executors their own.
		props["spark.pyspark.driver.python"] = "/cb/python3"
		props["spark.pyspark.python"] = "python3"
	}
	return configureZeppelinSpark(ctx, "http://"+addr+base, props)
}

// configureZeppelinSpark sets properties of Zeppelin's spark interpreter
// setting through its REST API (GET and PUT /api/interpreter/setting/spark).
func configureZeppelinSpark(ctx context.Context, base string, set map[string]string) error {
	client := &http.Client{Timeout: 2 * time.Minute}
	var setting struct {
		Body struct {
			Properties   map[string]map[string]any `json:"properties"`
			Dependencies []any                     `json:"dependencies"`
			Option       map[string]any            `json:"option"`
		} `json:"body"`
	}
	var err error
	for i := 0; i < 60; i++ {
		var req *http.Request
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/interpreter/setting/spark", nil)
		if err != nil {
			return err
		}
		var resp *http.Response
		if resp, err = client.Do(req); err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				err = json.Unmarshal(b, &setting)
				break
			}
			err = fmt.Errorf("%s: %.200s", resp.Status, b)
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		return &Unavailable{Msg: "read Zeppelin's spark interpreter setting: " + err.Error()}
	}
	props := setting.Body.Properties
	if props == nil {
		props = map[string]map[string]any{}
	}
	for k, v := range set {
		typ := "string"
		if v == "true" || v == "false" {
			typ = "checkbox"
		}
		var val any = v
		if typ == "checkbox" {
			val = v == "true"
		}
		props[k] = map[string]any{"name": k, "value": val, "type": typ}
	}
	body, _ := json.Marshal(map[string]any{"properties": props, "dependencies": setting.Body.Dependencies, "option": setting.Body.Option})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, base+"/api/interpreter/setting/spark", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return &Unavailable{Msg: "configure Zeppelin's spark interpreter: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return &Unavailable{Msg: fmt.Sprintf("configure Zeppelin's spark interpreter: %s %.300s", resp.Status, b)}
	}
	return nil
}
