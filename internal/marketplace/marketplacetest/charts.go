package marketplacetest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/marketplace"
)

//go:embed artifacthub.json
var artifactHubJSON []byte

// HelloChart is a small chart, hello 0.1.0: a Deployment of nginx:alpine
// and its Service, a values.schema.json (replicaCount, service.type,
// service.port, image.tag) and a README.
var helloFiles = map[string]string{
	"Chart.yaml": `apiVersion: v2
name: hello
version: 0.1.0
appVersion: "1.29"
description: A small web server, for the Marketplace's tests.
keywords: [web, nginx]
home: https://example.com/hello
`,
	"values.yaml": `replicaCount: 1
image:
  repository: nginx
  tag: alpine
service:
  type: ClusterIP
  port: 80
`,
	"values.schema.json": `{"$schema":"https://json-schema.org/draft-07/schema#","type":"object","properties":{
 "replicaCount":{"type":"integer","title":"Replicas","description":"How many pods serve the site."},
 "image":{"type":"object","properties":{"repository":{"type":"string"},"tag":{"type":"string","title":"Image tag"}}},
 "service":{"type":"object","properties":{"type":{"type":"string","enum":["ClusterIP","NodePort","LoadBalancer"],"title":"Service type"},"port":{"type":"integer","title":"Service port"}}}}}`,
	"README.md": "# hello\n\nA small web server.\n\n## Parameters\n\n| Name | Default |\n|---|---|\n| replicaCount | 1 |\n",
	"templates/deployment.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}
  labels: {app.kubernetes.io/instance: {{ .Release.Name }}, app.kubernetes.io/name: hello}
spec:
  replicas: {{ .Values.replicaCount }}
  selector:
    matchLabels: {app.kubernetes.io/instance: {{ .Release.Name }}}
  template:
    metadata:
      labels: {app.kubernetes.io/instance: {{ .Release.Name }}, app.kubernetes.io/name: hello}
    spec:
      containers:
      - name: web
        image: "{{ .Values.image.repository }}:{{ .Values.image.tag }}"
        ports: [{containerPort: 80}]
`,
	"templates/service.yaml": `apiVersion: v1
kind: Service
metadata:
  name: {{ .Release.Name }}
  labels: {app.kubernetes.io/instance: {{ .Release.Name }}}
spec:
  type: {{ .Values.service.type }}
  selector: {app.kubernetes.io/instance: {{ .Release.Name }}}
  ports: [{port: {{ .Values.service.port }}, targetPort: 80}]
`,
}

// HelloChart is hello-0.1.0.tgz.
func HelloChart(t testing.TB) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"Chart.yaml", "values.yaml", "values.schema.json", "README.md", "templates/deployment.yaml", "templates/service.yaml"} {
		body := helloFiles[name]
		if err := tw.WriteHeader(&tar.Header{Name: "hello/" + name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Charts serves Artifact Hub's recorded answers (artifacthub.json,
// bitnami/nginx's content URL replaced by hello-0.1.0.tgz) and a Helm
// repository, /repo/index.yaml, listing hello 0.1.0. It answers no other
// path.
type Charts struct {
	URL      string
	Recorded *Recorded
}

// ChartServer starts the server.
func ChartServer(t testing.TB) *Charts {
	t.Helper()
	var fx struct {
		Total  int                        `json:"total"`
		Search map[string]json.RawMessage `json:"search_nginx"`
		Pkg    map[string]any             `json:"package_bitnami_nginx"`
	}
	if err := json.Unmarshal(artifactHubJSON, &fx); err != nil {
		t.Fatal(err)
	}
	tgz := HelloChart(t)
	rec := &Recorded{}
	c := &Charts{Recorded: rec}
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/api/v1/packages/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		w.Header().Set("Pagination-Total-Count", "336")
		if q.Get("ts_query_web") != "nginx" || q.Get("offset") != "0" {
			w.Header().Set("Pagination-Total-Count", "0")
			writeJSON(w, map[string]any{"packages": []any{}})
			return
		}
		writeJSON(w, fx.Search)
	})
	pkg := func() map[string]any {
		p := map[string]any{}
		for k, v := range fx.Pkg {
			p[k] = v
		}
		p["content_url"] = c.URL + "/charts/hello-0.1.0.tgz"
		p["has_values_schema"] = true
		return p
	}
	mux.HandleFunc("/api/v1/packages/helm/bitnami/nginx", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, pkg()) })
	mux.HandleFunc("/api/v1/packages/helm/bitnami/nginx/{version}", func(w http.ResponseWriter, r *http.Request) {
		p := pkg()
		p["version"] = r.PathValue("version")
		writeJSON(w, p)
	})
	id, _ := fx.Pkg["package_id"].(string)
	mux.HandleFunc("/api/v1/packages/"+id+"/{version}/values", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(helloFiles["values.yaml"]))
	})
	mux.HandleFunc("/api/v1/packages/"+id+"/{version}/values-schema", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(helloFiles["values.schema.json"]))
	})
	mux.HandleFunc("/charts/hello-0.1.0.tgz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(tgz) })
	mux.HandleFunc("/repo/index.yaml", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`apiVersion: v1
entries:
  hello:
  - apiVersion: v2
    name: hello
    version: 0.1.0
    appVersion: "1.29"
    description: A small web server, for the Marketplace's tests.
    keywords: [web, nginx]
    created: "2026-11-22T10:00:00Z"
    digest: fixture
    urls: [../charts/hello-0.1.0.tgz]
    annotations:
      catalog.cattle.io/display-name: Hello
      catalog.cattle.io/certified: partner
  old:
  - apiVersion: v2
    name: old
    version: 1.0.0
    deprecated: true
    urls: [old-1.0.0.tgz]
`))
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.URLs = append(rec.URLs, r.URL.String())
		rec.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	c.URL = srv.URL
	return c
}

// ArtifactHub is an Artifact Hub source over the server.
func (c *Charts) ArtifactHub() *marketplace.ArtifactHub {
	return &marketplace.ArtifactHub{BaseURL: c.URL}
}

// Repo is a Helm index source over the server's /repo.
func (c *Charts) Repo(id, label string) *marketplace.HelmIndex {
	return &marketplace.HelmIndex{SourceID: id, SourceLabel: label,
		Repos: func() []marketplace.HelmRepo { return []marketplace.HelmRepo{{Name: "fixture", URL: c.URL + "/repo"}} }}
}
