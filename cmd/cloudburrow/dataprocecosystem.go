package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/egress"
	"github.com/cloudburrow/cloudburrow/internal/sparkproc"
)

// The Dataproc clusters' other engines (#2162), pinned in
// dependencies.json (components.bigqueryml.dataproc*): each project's official
// image by its multi-arch index digest, Google's Cloud Storage connector
// for Hadoop from Maven Central, and the Apache Pig release from the
// Apache distribution site, each checked against its SHA-256.
const (
	// dataprocHiveImage is apache/hive:4.0.1: Apache Hadoop 3.3.6 (the
	// Hadoop of image versions 2.2 and 2.3), Hive 4.0.1 and Tez on Java
	// 8. The official 3.1.3 image (the Hive of 2.2 and 2.3) is amd64 only.
	dataprocHiveImage = "apache/hive@sha256:5194161ef50b80875f937dff04936df047cbe894e9727ab8f0606349345b1bd3"
	// dataprocTrinoImage is trinodb/trino:432, the Trino of image
	// versions 2.2 and 2.3.
	dataprocTrinoImage = "trinodb/trino@sha256:3a958446c5e0133683f6b625028602f0b5d9182e54db76d9bde4cbb98f42f193"
	// dataprocFlinkImage is flink:1.17.2-scala_2.12-java11, the Flink
	// 1.17 line of image versions 2.2 and 2.3.
	dataprocFlinkImage = "flink@sha256:657ca6218ee6f0e73065bfbfc6124fda8d72f75ee371776f5b0914fb7c65951f"
	// dataprocZookeeperImage is zookeeper:3.9.5, the ZooKeeper of image
	// version 2.3.
	dataprocZookeeperImage = "zookeeper@sha256:e6b279d0135070507e867a3220c941fe90b791937b6d469b6929b79a5dfb01ba"
	// dataprocSolrImage is solr:9.4.1.
	dataprocSolrImage = "solr@sha256:96e915755c6e0f40200e6a0306e959ed5c2b24b871fef2c34f436a1ca4796f4e"
	// dataprocZeppelinImage is apache/zeppelin:0.10.1, the Zeppelin of
	// image versions 2.1 to 2.3: linux/amd64 only (run under emulation on
	// arm64), 3.7 GB compressed.
	dataprocZeppelinImage = "apache/zeppelin@sha256:9c1b5ddd6225ad45cedc327a853f6f13e45797ff419260d20bb9c14f5cbe3a87"
	// dataprocMetastore42Image is apache/hive:standalone-metastore-4.2.0,
	// the Hive Metastore 4.2.0 of image version 3.0, whose Iceberg REST
	// catalog (port 9001, /iceberg) is the ICEBERG component's
	// default_iceberg catalog on 3.0.
	// dataprocDocker24Image and dataprocDocker28Image are
	// docker:24.0-dind-rootless and docker:28.1-dind-rootless, the DOCKER
	// component's daemons: Docker 24.0 is image 2.2's (and 2.1's), 28.1
	// 2.3's and 3.0's.
	dataprocDocker24Image = "docker@sha256:e162081c6623cdd809c09d549b742fa08d9d3e1da47e55b8d42fe87a676ff80e"
	dataprocDocker28Image = "docker@sha256:921cf550ca1fda0b9e6999ffd4a97cc0e248207479c73febbf5bf105108c2fe5"
	// dataprocZeppelin30Image is apache/zeppelin:0.12.1, image 3.0's
	// Zeppelin (the release notes list 0.12.0, whose image is linux/amd64
	// only on Java 11; 0.12.1 is published for linux/arm64 too), 2.7 GB.
	dataprocZeppelin30Image  = "apache/zeppelin@sha256:ebb76b9b98f1b5457cd10c118fc6624269918a3602711a08d9f237673c0c0abb"
	dataprocMetastore42Image = "apache/hive@sha256:6ec027489678f06404c8e201010c10e6c67e507b77e41b7e69233d7652453fdf"

	gcsConnectorURL  = "https://repo1.maven.org/maven2/com/google/cloud/bigdataoss/gcs-connector/hadoop3-2.2.30/gcs-connector-hadoop3-2.2.30-shaded.jar"
	gcsConnectorSHA  = "dd7959e4d59a74023781c9e0666e8c2207528a0e7871eaf5f9b42eea0430a079"
	gcsConnectorFile = "gcs-connector-hadoop3-2.2.30-shaded.jar"
	pigURL           = "https://downloads.apache.org/pig/pig-0.18.0/pig-0.18.0.tar.gz"
	pigSHA           = "6845adb936a3c3bcc71451765953cae4103e410ba2f6c5a47cc9becade0a434a"
	pigDir           = "pig-0.18.0"
)

// dataprocEcosystem is the runner's Ecosystem, its downloads cached in
// cache. The DOCKER component's daemons run unconfined, so they are
// served only with up's --allow-unconfined-containers (allowUnconfined).
func dataprocEcosystem(cache, instance string, allowUnconfined bool, out io.Writer) *sparkproc.Ecosystem {
	var mu sync.Mutex
	w := out
	e := &sparkproc.Ecosystem{
		Hive: dataprocHiveImage, Trino: dataprocTrinoImage, Flink: dataprocFlinkImage,
		Zookeeper: dataprocZookeeperImage, Solr: dataprocSolrImage, Zeppelin: dataprocZeppelinImage,
		Metastore42: dataprocMetastore42Image, Zeppelin30: dataprocZeppelin30Image,
		// RANGER (#2162): its database the MySQL 8.0 image Cloud SQL for
		// MySQL 8.0 runs; the Admin server and plugins the image built on
		// the first use from the pinned Apache Ranger 2.8.0 source release.
		RangerMySQL: components.CloudSQLMySQL80Image,
		Ranger: func(ctx context.Context) (string, error) {
			return sparkproc.RangerImage(ctx, instance)
		},
		Docker: func(v string) string {
			if v >= "2.3" {
				return dataprocDocker28Image
			}
			return dataprocDocker24Image
		},
		GCS: func(ctx context.Context) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			return fetchPinned(ctx, filepath.Join(cache, gcsConnectorFile), gcsConnectorURL, gcsConnectorSHA, "Google's Cloud Storage connector for Hadoop", out)
		},
		Pig: func(ctx context.Context) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			return fetchPig(ctx, cache, out)
		},
		Jupyter: func(ctx context.Context, spark string) (string, error) {
			return sparkproc.JupyterImage(ctx, spark, instance)
		},
		Lake: func(ctx context.Context, component, version, iceberg string) ([]string, error) {
			mu.Lock()
			defer mu.Unlock()
			jars, err := lakeJarsFor(component, version, iceberg)
			if err != nil {
				return nil, err
			}
			var out []string
			for _, j := range jars {
				p, err := fetchPinned(ctx, filepath.Join(cache, "lake", j.file()), mavenCentral+j.path, j.sha256, j.file(), w)
				if err != nil {
					return nil, err
				}
				out = append(out, p)
			}
			return out, nil
		},
	}
	if !allowUnconfined {
		e.Docker = nil
	}
	return e
}

// mavenCentral is Maven Central's repository root.
const mavenCentral = "https://repo1.maven.org/maven2/"

// lakeJar is one table format jar on Maven Central, by its path under
// the repository root and its SHA-256 (dependencies.json's
// components.bigqueryml.dataprocLake).
type lakeJar struct{ path, sha256 string }

func (j lakeJar) file() string { return filepath.Base(j.path) }

// lakeJars are the table formats' jars of each image version's Spark, the
// versions its release notes list (read 2026-11-23:
// https://docs.cloud.google.com/dataproc/docs/concepts/versioning/dataproc-release-2.1,
// -2.2, -2.3 and -3.0): Delta Lake 3.2.1, Iceberg 1.6.1 (1.10 by
// dataproc:dataproc.iceberg.version) and Hudi 0.15.0 on 2.2 and 2.3
// (Spark 3.5, Scala 2.12); Hudi 0.12.3 on 2.1 (Spark 3.3); Delta Lake
// 4.2.0, Iceberg 1.11.0 and Hudi 1.2.0 on 3.0 (Spark 4.1, Scala 2.13).
var lakeJars = map[string][]lakeJar{
	"DELTA/2.2": {{"io/delta/delta-spark_2.12/3.2.1/delta-spark_2.12-3.2.1.jar", "088e187da689a347a6a8556dcb22318e3dfcfb995d807f5e2c19b4d0a7ee9499"},
		{"io/delta/delta-storage/3.2.1/delta-storage-3.2.1.jar", "4dcc179fc4076bda5060a4038f979c53e1f5916cf04971e28f9441db390763c7"}},
	"DELTA/3.0": {{"io/delta/delta-spark_2.13/4.2.0/delta-spark_2.13-4.2.0.jar", "8480050a55c7e50427f3bec3af9243f98fc7cbfa453a2ef55e6ea8d0b7b121ff"},
		{"io/delta/delta-storage/4.2.0/delta-storage-4.2.0.jar", "2d84df76551f2b3cd27b120abec67ccbba324cea6ec45eb659edcc54234f99f3"}},
	"ICEBERG/2.2": {{"org/apache/iceberg/iceberg-spark-runtime-3.5_2.12/1.6.1/iceberg-spark-runtime-3.5_2.12-1.6.1.jar", "87e7184f31ef0caac415bbdfcf1bc4943346a58b98d747dc83434f7139e12acb"}},
	"ICEBERG/2.2/1.10": {{"org/apache/iceberg/iceberg-spark-runtime-3.5_2.12/1.10.0/iceberg-spark-runtime-3.5_2.12-1.10.0.jar",
		"70f9471870b2d456167d703c829f098a446b86bb1379eee338c0efa03a341026"}},
	// Iceberg 1.11.0 on 3.0 (Spark 4.1, Scala 2.13), as the 3.0 release
	// notes list it.
	"ICEBERG/3.0": {{"org/apache/iceberg/iceberg-spark-runtime-4.1_2.13/1.11.0/iceberg-spark-runtime-4.1_2.13-1.11.0.jar",
		"d6ea6c5d099288daeb7d5a92061bd3d7d8f296492632b42378e5f2f0e3066242"}},
	"HUDI/2.1": {{"org/apache/hudi/hudi-spark3.3-bundle_2.12/0.12.3/hudi-spark3.3-bundle_2.12-0.12.3.jar", "99142b2f0f67d8b48bf7114f7e8fee8859772b9e288064abb28b07655bb0277c"}},
	"HUDI/2.2": {{"org/apache/hudi/hudi-spark3.5-bundle_2.12/0.15.0/hudi-spark3.5-bundle_2.12-0.15.0.jar", "aab6ecdb5c65fcd20406b980b31ccec294fae95693de947d5778f0b3fb4e302a"}},
	"HUDI/3.0": {{"org/apache/hudi/hudi-spark4.1-bundle_2.13/1.2.0/hudi-spark4.1-bundle_2.13-1.2.0.jar", "69ba6fa7e29096c15737614a98e85536935365f044dc4d647dfdec6bcc24f6ce"}},
}

// lakeJarsFor are a component's jars on an image version (2.3's are
// 2.2's: the same Spark and the same versions).
func lakeJarsFor(component, version, iceberg string) ([]lakeJar, error) {
	if version == "2.3" {
		version = "2.2"
	}
	key := component + "/" + version
	if component == sparkproc.ComponentIceberg && iceberg != "" && iceberg != "1.6" {
		key += "/" + iceberg
	}
	jars, ok := lakeJars[key]
	if !ok {
		return nil, fmt.Errorf("no %s jars are pinned for image version %s", component, version)
	}
	return jars, nil
}

// fetchPinned is the file at p, fetched from url and checked against its
// pinned SHA-256 on the first use.
func fetchPinned(ctx context.Context, p, url, sum, what string, out io.Writer) (string, error) {
	if ok, _ := fileHas(p, sum); ok {
		return p, nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	fmt.Fprintf(out, "cloudburrow: fetching %s (%s)\n", what, url)
	tmp := p + ".tmp"
	if err := download(ctx, url, tmp); err != nil {
		return "", err
	}
	if ok, got := fileHas(tmp, sum); !ok {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("%s: SHA-256 %s, want the pinned %s", url, got, sum)
	}
	return p, os.Rename(tmp, p)
}

func fileHas(p, sum string) (bool, string) {
	f, err := os.Open(p)
	if err != nil {
		return false, ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false, ""
	}
	got := hex.EncodeToString(h.Sum(nil))
	return got == sum, got
}

func download(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := egress.Client(&http.Client{Timeout: 20 * time.Minute}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", url, resp.Status)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, io.LimitReader(resp.Body, 1<<30)); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// fetchPig is Apache Pig's home in cache: the release tarball, checked,
// of which bin/, conf/, lib/ and the core jar are kept, and the tarball
// removed.
func fetchPig(ctx context.Context, cache string, out io.Writer) (string, error) {
	home := filepath.Join(cache, pigDir)
	marker := filepath.Join(home, ".complete")
	if _, err := os.Stat(marker); err == nil {
		return home, nil
	}
	tgz := filepath.Join(cache, pigDir+".tar.gz")
	if _, err := fetchPinned(ctx, tgz, pigURL, pigSHA, "Apache Pig 0.18.0", out); err != nil {
		return "", err
	}
	defer os.Remove(tgz)
	_ = os.RemoveAll(home)
	f, err := os.Open(tgz)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	tr := tar.NewReader(gz)
	keep := func(name string) bool {
		rest, ok := strings.CutPrefix(name, pigDir+"/")
		if !ok {
			return false
		}
		for _, p := range []string{"bin/", "conf/", "lib/"} {
			if strings.HasPrefix(rest, p) {
				return true
			}
		}
		return rest == "pig-0.18.0-core-h3.jar" || rest == "LICENSE.txt" || rest == "NOTICE.txt"
	}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		if !keep(h.Name) || strings.Contains(h.Name, "..") {
			continue
		}
		dst := filepath.Join(cache, filepath.FromSlash(h.Name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return "", err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return "", err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			w, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return "", err
			}
			if _, err := io.Copy(w, tr); err != nil {
				w.Close()
				return "", err
			}
			if err := w.Close(); err != nil {
				return "", err
			}
		}
	}
	return home, os.WriteFile(marker, nil, 0o644)
}
