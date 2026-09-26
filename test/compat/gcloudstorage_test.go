//go:build compat

package compat

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

// gcloud storage and gsutil (#517) against the builtin server. Each test
// runs real tools in an isolated CLOUDSDK_CONFIG (and BOTO_CONFIG), with
// every proxy variable pointing at egressGuard: anything but loopback goes
// to it, is refused, and fails the test. The tools skip when absent.

// egressGuard is an HTTP proxy that refuses every request and records its
// host. The tools reach loopback directly (NO_PROXY), so any request it sees
// was on its way off the machine.
func egressGuard(t *testing.T) []string {
	t.Helper()
	var mu sync.Mutex
	var hosts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hosts = append(hosts, r.Host)
		mu.Unlock()
		http.Error(w, "CloudBurrow compat: egress refused", http.StatusForbidden)
	}))
	t.Cleanup(func() {
		srv.Close()
		mu.Lock()
		defer mu.Unlock()
		if len(hosts) > 0 {
			t.Errorf("a tool tried to leave the machine, to %v", hosts)
		}
	})
	return []string{
		"HTTP_PROXY=" + srv.URL, "HTTPS_PROXY=" + srv.URL, "http_proxy=" + srv.URL, "https_proxy=" + srv.URL,
		"NO_PROXY=127.0.0.1,localhost,::1", "no_proxy=127.0.0.1,localhost,::1",
	}
}

type gcloudSession struct {
	t       *testing.T
	bin     string
	env     []string
	project string
}

// newGcloudSession runs `cloudburrow gcloud-setup` into a fresh
// CLOUDSDK_CONFIG and returns gcloud configured by it alone.
func newGcloudSession(t *testing.T, h *Harness) *gcloudSession {
	t.Helper()
	bin, err := exec.LookPath("gcloud")
	if err != nil {
		t.Skip("gcloud is not on PATH")
	}
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	flags := strings.Fields(os.Getenv(EnvCLIArgs))
	out, _ := exec.Command(cli, append([]string{"status", "--format", "json"}, flags...)...).Output()
	var st struct{ Project string }
	if err := json.Unmarshal(out, &st); err != nil || st.Project == "" {
		t.Fatalf("status gave no project: %s", out)
	}
	gdir := t.TempDir()
	cmd := exec.Command(cli, append([]string{"gcloud-setup"}, flags...)...)
	cmd.Env = append(os.Environ(), "CLOUDSDK_CONFIG="+gdir)
	b, err := cmd.Output()
	if err != nil {
		t.Fatalf("cloudburrow gcloud-setup: %v", err)
	}
	name, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "export CLOUDSDK_ACTIVE_CONFIG_NAME=")
	if !ok {
		t.Fatalf("gcloud-setup printed %q", b)
	}
	env := append(os.Environ(), "CLOUDSDK_CONFIG="+gdir, "CLOUDSDK_ACTIVE_CONFIG_NAME="+name,
		"CLOUDSDK_CORE_DISABLE_PROMPTS=1")
	return &gcloudSession{t: t, bin: bin, env: append(env, egressGuard(t)...), project: st.Project}
}

func (g *gcloudSession) run(extraEnv []string, args ...string) (string, error) {
	cmd := exec.Command(g.bin, args...)
	cmd.Env = append(append([]string{}, g.env...), extraEnv...)
	b, err := cmd.CombinedOutput()
	return string(b), err
}

func (g *gcloudSession) must(args ...string) string {
	g.t.Helper()
	out, err := g.run(nil, args...)
	if err != nil {
		g.t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// storageCalls are the builtin server's calls on a bucket since a time,
// from /admin/events, after the CLI's scrape (every 2 s) has caught up.
func storageCalls(t *testing.T, h *Harness, bucket string, since time.Time) []adminEvent {
	t.Helper()
	time.Sleep(3 * time.Second)
	u := fmt.Sprintf("http://%s/admin/events?service=storage&kind=request&limit=1000&since=%s",
		h.Endpoint(EnvControl), url.QueryEscape(since.UTC().Format(time.RFC3339Nano)))
	resp, err := http.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	defer resp.Body.Close()
	var body struct{ Events []adminEvent }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	var out []adminEvent
	for _, e := range body.Events {
		if e.Detail["bucket"] == bucket {
			out = append(out, e)
		}
	}
	return out
}

func countCalls(es []adminEvent, method, object string) int {
	n := 0
	for _, e := range es {
		if e.Target == method && (object == "" || e.Detail["object"] == object) {
			n++
		}
	}
	return n
}

func sha256File(t *testing.T, path string) [32]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

// TestGcloudStorageParallelCompositeUpload: `gcloud storage cp` of a 200 MiB
// file takes the parallel composite path (transfer.py: over the 150 MiB
// threshold, in components, composed, then the components deleted), and a
// download over the sliced threshold reads it back in ranged slices.
// covers: storage.objects.compose, storage.buckets.getStorageLayout
func TestGcloudStorageParallelCompositeUpload(t *testing.T) {
	h := New(t)
	g := newGcloudSession(t, h)
	c := storageClient(t, h)
	name := h.Project() + "-pcu"
	g.must("storage", "buckets", "create", "gs://"+name)
	t.Cleanup(func() { _, _ = g.run(nil, "storage", "rm", "-r", "gs://"+name) })

	dir := t.TempDir()
	src := filepath.Join(dir, "big.bin")
	data := make([]byte, 200<<20)
	_, _ = rand.Read(data)
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	since := time.Now()
	pcu := []string{"CLOUDSDK_STORAGE_PARALLEL_COMPOSITE_UPLOAD_ENABLED=True"}
	if out, err := g.run(pcu, "storage", "cp", src, "gs://"+name+"/big.bin"); err != nil {
		t.Fatalf("gcloud storage cp: %v\n%s", err, out)
	}
	attrs, err := c.Bucket(name).Object("big.bin").Attrs(h.Context())
	if err != nil {
		t.Fatal(err)
	}
	if attrs.Size != int64(len(data)) || attrs.ComponentCount < 2 {
		t.Errorf("big.bin = %d bytes, %d components; want %d bytes, composed", attrs.Size, attrs.ComponentCount, len(data))
	}
	var names []string
	it := c.Bucket(name).Objects(h.Context(), &storage.Query{Versions: true})
	for {
		o, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, o.Name)
	}
	if len(names) != 1 || names[0] != "big.bin" {
		t.Errorf("objects after the upload = %v, want only big.bin: the components were not deleted", names)
	}
	calls := storageCalls(t, h, name, since)
	if countCalls(calls, "storage.objects.compose", "") == 0 {
		t.Errorf("no storage.objects.compose among the upload's calls")
	}
	t.Logf("upload: %d inserts, %d composes, %d deletes", countCalls(calls, "storage.objects.insert", ""),
		countCalls(calls, "storage.objects.compose", ""), countCalls(calls, "storage.objects.delete", ""))

	since = time.Now()
	dst := filepath.Join(dir, "back.bin")
	// Hashes are compared here, by SHA-256, not by gcloud: its CRC32C of a
	// sliced download runs the gcloud-crc32c helper, which on a macOS install
	// under Gatekeeper quarantine hangs on any file, CloudBurrow or not
	// (measured on #517).
	sliced := []string{"CLOUDSDK_STORAGE_SLICED_OBJECT_DOWNLOAD_THRESHOLD=1M", "CLOUDSDK_STORAGE_SLICED_OBJECT_DOWNLOAD_MAX_COMPONENTS=4",
		"CLOUDSDK_STORAGE_CHECK_HASHES=never"}
	if out, err := g.run(sliced, "storage", "cp", "gs://"+name+"/big.bin", dst); err != nil {
		t.Fatalf("gcloud storage cp (download): %v\n%s", err, out)
	}
	if sha256File(t, dst) != sha256.Sum256(data) {
		t.Error("the downloaded file differs from the uploaded one")
	}
	gets := countCalls(storageCalls(t, h, name, since), "storage.objects.get", "big.bin")
	// One metadata read, then a media read per slice.
	if gets < 3 {
		t.Errorf("the download made %d object reads, want a metadata read and several slices", gets)
	}
	t.Logf("download: %d object reads", gets)
}

// TestGcloudStorageClearLabels: `buckets update --update-labels` then
// `--clear-labels` (a patch whose labels are null), and `objects update`
// setting custom metadata.
// covers: storage.buckets.patch, storage.objects.patch, storage.managedFolders.list
func TestGcloudStorageClearLabels(t *testing.T) {
	h := New(t)
	g := newGcloudSession(t, h)
	c := storageClient(t, h)
	name := h.Project() + "-labels"
	g.must("storage", "buckets", "create", "gs://"+name)
	t.Cleanup(func() { _, _ = g.run(nil, "storage", "rm", "-r", "gs://"+name) })

	g.must("storage", "buckets", "update", "gs://"+name, "--update-labels=team=burrow,env=dev")
	attrs, err := c.Bucket(name).Attrs(h.Context())
	if err != nil {
		t.Fatal(err)
	}
	if attrs.Labels["team"] != "burrow" || attrs.Labels["env"] != "dev" {
		t.Errorf("labels after --update-labels = %v", attrs.Labels)
	}
	g.must("storage", "buckets", "update", "gs://"+name, "--clear-labels")
	if attrs, err = c.Bucket(name).Attrs(h.Context()); err != nil {
		t.Fatal(err)
	}
	if len(attrs.Labels) != 0 {
		t.Errorf("labels after --clear-labels = %v, want none", attrs.Labels)
	}
	if got := g.must("storage", "ls"); !strings.Contains(got, "gs://"+name+"/") {
		t.Errorf("gcloud storage ls does not show gs://%s/:\n%s", name, got)
	}

	if err := writeObject(h, c, name, "o.txt", "object"); err != nil {
		t.Fatal(err)
	}
	g.must("storage", "objects", "update", "gs://"+name+"/o.txt", "--custom-metadata=k=v", "--content-type=text/plain")
	o, err := c.Bucket(name).Object("o.txt").Attrs(h.Context())
	if err != nil {
		t.Fatal(err)
	}
	if o.Metadata["k"] != "v" || o.ContentType != "text/plain" {
		t.Errorf("after objects update: metadata %v, content type %q", o.Metadata, o.ContentType)
	}
}

func writeObject(h *Harness, c *storage.Client, bucket, name, body string) error {
	w := c.Bucket(bucket).Object(name).NewWriter(h.Context())
	if _, err := io.WriteString(w, body); err != nil {
		return err
	}
	return w.Close()
}

// TestGcloudStorageHMAC: `gcloud storage hmac create/list/update/delete`.
// covers: storage.projects.hmacKeys.create, storage.projects.hmacKeys.list, storage.projects.hmacKeys.update, storage.projects.hmacKeys.delete
func TestGcloudStorageHMAC(t *testing.T) {
	h := New(t)
	g := newGcloudSession(t, h)
	c := storageClient(t, h)
	email := fmt.Sprintf("gc-%s@%s.iam.gserviceaccount.com", h.Project()[len(h.Project())-6:], g.project)

	var created struct {
		Metadata struct{ AccessId, State string }
		Secret   string
	}
	out := g.must("storage", "hmac", "create", email, "--format=json")
	if err := json.Unmarshal([]byte(out), &created); err != nil || created.Metadata.AccessId == "" || created.Secret == "" {
		t.Fatalf("hmac create printed %s (%v)", out, err)
	}
	id := created.Metadata.AccessId
	t.Cleanup(func() { deleteHMACKeys(h.Context(), c, g.project, email) })

	if got := g.must("storage", "hmac", "list", "--service-account="+email, "--format=value(accessId)"); !strings.Contains(got, id) {
		t.Errorf("hmac list does not show %s:\n%s", id, got)
	}
	g.must("storage", "hmac", "update", id, "--deactivate")
	key, err := c.HMACKeyHandle(g.project, id).Get(h.Context())
	if err != nil {
		t.Fatal(err)
	}
	if key.State != storage.Inactive {
		t.Errorf("after --deactivate the key is %s", key.State)
	}
	g.must("storage", "hmac", "delete", id)
	if key, err = c.HMACKeyHandle(g.project, id).Get(h.Context()); err == nil && key.State != storage.Deleted {
		t.Errorf("after delete the key is %s", key.State)
	}
}

// tlsFront is an HTTPS reverse proxy in front of the storage endpoint,
// recording each request's method, path and whether it carried
// X-GUploader-No-308. gsutil refuses plain HTTP on both APIs
// (gcs_json_api.py: http_base = 'https://'; __main__.py refuses
// is_secure = False), and CloudBurrow serves none, so the test supplies it.
type tlsFront struct {
	srv   *httptest.Server
	ca    string
	mu    sync.Mutex
	no308 int
	lines []string
}

func newTLSFront(t *testing.T, target string) *tlsFront {
	t.Helper()
	u, _ := url.Parse(target)
	f := &tlsFront{}
	rp := httputil.NewSingleHostReverseProxy(u)
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		if r.Header.Get("X-GUploader-No-308") != "" {
			f.no308++
		}
		f.lines = append(f.lines, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		rp.ServeHTTP(w, r)
	}))
	// A certificate naming localhost: boto matches the host against DNS
	// names only, so httptest's (an IP SAN and example.com) is refused.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	f.ca = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(f.ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

// TestGsutilJSONAndHMACXML: gsutil cp, ls and rm over the JSON API with no
// credentials, then over the XML API with HMAC-only credentials (which
// cloud_api_delegator.py selects XML for), both through a test TLS front.
// A 10 MiB upload is over gsutil's 8 MiB resumable threshold. Records
// whether apitools sends X-GUploader-No-308.
func TestGsutilJSONAndHMACXML(t *testing.T) {
	h := New(t)
	gsutil, err := exec.LookPath("gsutil")
	if err != nil {
		t.Skip("gsutil is not on PATH")
	}
	c := storageClient(t, h)
	storageURL := h.Endpoint(EnvStorage)
	if !strings.Contains(storageURL, "://") {
		storageURL = "http://" + storageURL
	}
	front := newTLSFront(t, storageURL)
	fu, _ := url.Parse(front.srv.URL)
	host := "localhost"
	name := h.Project() + "-gsutil"
	if err := c.Bucket(name).Create(h.Context(), h.Project(), nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		it := c.Bucket(name).Objects(h.Context(), nil)
		for o, err := it.Next(); err == nil; o, err = it.Next() {
			_ = c.Bucket(name).Object(o.Name).Delete(h.Context())
		}
		_ = c.Bucket(name).Delete(h.Context())
	})

	dir := t.TempDir()
	src := filepath.Join(dir, "up.bin")
	data := make([]byte, 10<<20)
	_, _ = rand.Read(data)
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	guard := egressGuard(t)
	gs := func(boto string, args ...string) string {
		t.Helper()
		cfg := filepath.Join(dir, "boto")
		if err := os.WriteFile(cfg, []byte(boto), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(gsutil, append([]string{"-o", "GSUtil:parallel_composite_upload_threshold=0"}, args...)...)
		cmd.Env = append(append(os.Environ(), guard...),
			"BOTO_CONFIG="+cfg, "BOTO_PATH="+cfg, "CLOUDSDK_CONFIG="+t.TempDir(),
			"CLOUDSDK_CORE_PASS_CREDENTIALS_TO_GSUTIL=false", "CLOUDSDK_CORE_DISABLE_PROMPTS=1",
			// The Cloud SDK's component update check, which goes to dl.google.com.
			"CLOUDSDK_COMPONENT_MANAGER_DISABLE_UPDATE_CHECK=true")
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gsutil %s: %v\n%s", strings.Join(args, " "), err, b)
		}
		return string(b)
	}
	common := fmt.Sprintf("[Boto]\nca_certificates_file = %s\n[GSUtil]\ndefault_project_id = %s\nsoftware_update_check_period = 0\n", front.ca, h.Project())

	jsonCfg := common + fmt.Sprintf("prefer_api = json\n[Credentials]\ngs_json_host = %s\ngs_json_port = %s\n", host, fu.Port())
	gs(jsonCfg, "cp", src, "gs://"+name+"/json.bin")
	if got := gs(jsonCfg, "ls", "gs://"+name); !strings.Contains(got, "gs://"+name+"/json.bin") {
		t.Errorf("gsutil ls (JSON) does not show json.bin:\n%s", got)
	}
	back := filepath.Join(dir, "json-back.bin")
	gs(jsonCfg, "cp", "gs://"+name+"/json.bin", back)
	if sha256File(t, back) != sha256.Sum256(data) {
		t.Error("gsutil (JSON) read back different bytes")
	}
	gs(jsonCfg, "rm", "gs://"+name+"/json.bin")

	key, err := c.CreateHMACKey(h.Context(), h.Project(), "gsutil@"+h.Project()+".iam.gserviceaccount.com")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { deleteHMACKeys(h.Context(), c, h.Project(), key.ServiceAccountEmail) })
	xmlCfg := common + fmt.Sprintf("[Credentials]\ngs_host = %s\ngs_port = %s\ngs_access_key_id = %s\ngs_secret_access_key = %s\n",
		host, fu.Port(), key.AccessID, key.Secret)
	gs(xmlCfg, "cp", src, "gs://"+name+"/xml.bin")
	if got := gs(xmlCfg, "ls", "gs://"+name); !strings.Contains(got, "gs://"+name+"/xml.bin") {
		t.Errorf("gsutil ls (XML) does not show xml.bin:\n%s", got)
	}
	// Read back through the Go client, not gsutil: gsutil v5.37 cannot
	// download over XML from any port but 443 or 80, whatever the server
	// (boto_translation.py _AddCustomEndpointToKey sets the connection's
	// port to the config's string, and boto's server_name formats it with
	// %d: a TypeError, measured on #517).
	r, err := c.Bucket(name).Object("xml.bin").NewReader(h.Context())
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	_ = r.Close()
	if !bytes.Equal(got, data) {
		t.Error("gsutil (XML) stored different bytes")
	}
	gs(xmlCfg, "rm", "gs://"+name+"/xml.bin")

	front.mu.Lock()
	defer front.mu.Unlock()
	t.Logf("X-GUploader-No-308 on %d of %d requests", front.no308, len(front.lines))
	t.Logf("requests through the TLS front:\n%s", strings.Join(front.lines, "\n"))
}
