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

	"github.com/cloudburrow/cloudburrow/internal/egress"
	"github.com/cloudburrow/cloudburrow/internal/sparkproc"
)

// The Dataproc clusters' other engines (#2162), pinned in
// dependencies.json (components.dataproc.*): each project's official
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

	gcsConnectorURL  = "https://repo1.maven.org/maven2/com/google/cloud/bigdataoss/gcs-connector/hadoop3-2.2.30/gcs-connector-hadoop3-2.2.30-shaded.jar"
	gcsConnectorSHA  = "dd7959e4d59a74023781c9e0666e8c2207528a0e7871eaf5f9b42eea0430a079"
	gcsConnectorFile = "gcs-connector-hadoop3-2.2.30-shaded.jar"
	pigURL           = "https://downloads.apache.org/pig/pig-0.18.0/pig-0.18.0.tar.gz"
	pigSHA           = "6845adb936a3c3bcc71451765953cae4103e410ba2f6c5a47cc9becade0a434a"
	pigDir           = "pig-0.18.0"
)

// dataprocEcosystem is the runner's Ecosystem, its downloads cached in
// cache.
func dataprocEcosystem(cache, instance string, out io.Writer) *sparkproc.Ecosystem {
	var mu sync.Mutex
	return &sparkproc.Ecosystem{
		Hive: dataprocHiveImage, Trino: dataprocTrinoImage, Flink: dataprocFlinkImage,
		Zookeeper: dataprocZookeeperImage, Solr: dataprocSolrImage,
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
	}
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
