package dataproc

// The Spark UI (#2162): every workload's Spark event log is kept, and
// Spark's History Server shows it. A cluster's History Server runs on its
// master and is its Component Gateway's "Spark History Server"
// (/component-gateway/UUID/sparkhistory/ on this API's port); the
// instance's own History Server, started when first opened, shows the
// batches' and sessions' (/sparkhistory/), which the console's View
// Spark UI opens. A Persistent History Server is a cluster whose
// spark:spark.history.fs.logDirectory is a gs:// path: its History
// Server reads the event logs in Cloud Storage there, copied to it as
// they are written. A cluster's jobs' event logs are written to its
// spark:spark.eventLog.dir, or to its temp bucket's
// CLUSTER_UUID/spark-job-history/ ("Spark job history files are saved in
// the cluster temp bucket in the /spark-job-history directory", the
// Persistent History Server page, read 2026-11-22:
// https://docs.cloud.google.com/dataproc/docs/concepts/jobs/history-server).

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	dataprocpb "cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"

	"github.com/klauspost/compress/zstd"

	"github.com/cloudburrow/cloudburrow/internal/sparkproc"
)

// Lister lists a bucket's objects, for a Persistent History Server's
// event logs; Storage may implement it.
type Lister interface {
	List(ctx context.Context, bucket, prefix string) (map[string]int64, error)
}

// HistoryBase is the path the instance's History Server is served at.
const HistoryBase = "/sparkhistory"

// historyServer is the instance's History Server, started on demand.
type historyServer struct {
	mu    sync.Mutex
	nodes Nodes
	err   error
	used  time.Time
}

func (h *historyServer) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.nodes != nil {
		h.nodes.Delete()
		h.nodes = nil
	}
}

// appID is the workload ID a batch or session runs as, from its UUID: the
// path its Spark UI is opened at, resolved to the Spark application whose
// event log records it (resolveApp).
func appID(uuid string) string { return "app-" + strings.ReplaceAll(uuid, "-", "") }

// SparkUIPath is the path of a batch's or session's Spark UI on this API's
// port, through the instance's History Server.
func SparkUIPath(uuid string) string { return HistoryBase + "/history/" + appID(uuid) + "/jobs/" }

// instanceHistory is the instance's History Server's address, started
// when first asked for.
func (s *Server) instanceHistory(ctx context.Context) (string, error) {
	s.mu.Lock()
	if s.history == nil {
		s.history = &historyServer{}
	}
	h := s.history
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return "", errStopping
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.used = time.Now()
	if h.nodes == nil {
		ce, ok := s.opts.Engine.(ClusterEngine)
		if !ok || s.opts.HistoryDir == "" {
			return "", &sparkproc.Unavailable{Msg: "this instance runs no Apache Spark History Server"}
		}
		n, err := ce.StartCluster(ctx, sparkproc.ClusterSpec{ID: "history-" + s.opts.InstanceID, Name: "history", HistoryOnly: true, HistoryBase: HistoryBase})
		if err != nil {
			return "", err
		}
		h.nodes = n
	}
	addr := h.nodes.Web(ctx)["history"]
	if addr == "" {
		return "", &sparkproc.Unavailable{Msg: "the History Server's port is not published"}
	}
	return addr, nil
}

// workloadRE is a workload's Spark UI path on the instance's History
// Server.
var workloadRE = regexp.MustCompile(`^` + HistoryBase + `/history/(app-[0-9a-f]{32})(/.*)?$`)

// resolveApp is the Spark application ID whose event log in the
// instance's history directory records workload ID w, or "". Spark's
// local scheduler names a batch's or session's application local-MILLIS,
// so the workload's own ID is a property of its environment, read from
// the log's SparkListenerEnvironmentUpdate (a plain or zstd log, Spark's
// default event log codec).
func (s *Server) resolveApp(w string) string {
	s.mu.Lock()
	if id, ok := s.apps[w]; ok {
		s.mu.Unlock()
		return id
	}
	s.mu.Unlock()
	dir := s.opts.HistoryDir
	entries, err := os.ReadDir(dir)
	if dir == "" || err != nil {
		return ""
	}
	needle := []byte(`"` + sparkproc.WorkloadProperty + `":"` + w + `"`)
	for _, e := range entries {
		file, app := filepath.Join(dir, e.Name()), strings.TrimSuffix(e.Name(), ".inprogress")
		if e.IsDir() {
			app = strings.TrimPrefix(e.Name(), "eventlog_v2_")
			if app == e.Name() {
				continue
			}
			parts, _ := filepath.Glob(filepath.Join(file, "events_1_*"))
			if len(parts) == 0 {
				continue
			}
			file = parts[0]
		}
		for _, ext := range []string{".zstd", ".lz4", ".snappy", ".lzf"} {
			app = strings.TrimSuffix(app, ext)
		}
		if logRecords(file, needle) {
			s.mu.Lock()
			if s.apps == nil {
				s.apps = map[string]string{}
			}
			s.apps[w] = app
			s.mu.Unlock()
			return app
		}
	}
	return ""
}

// logRecords is whether an event log's lines up to its environment hold
// needle.
func logRecords(file string, needle []byte) bool {
	f, err := os.Open(file)
	if err != nil {
		return false
	}
	defer f.Close()
	var r io.Reader = f
	if strings.HasSuffix(file, ".zstd") || strings.Contains(file, ".zstd.") {
		d, err := zstd.NewReader(f)
		if err != nil {
			return false
		}
		defer d.Close()
		r = d
	}
	br := bufio.NewReaderSize(r, 1<<20)
	for n := 0; n < 64; n++ {
		line, err := br.ReadBytes('\n')
		if bytes.Contains(line, needle) {
			return true
		}
		if err != nil || bytes.Contains(line, []byte(`"SparkListenerEnvironmentUpdate"`)) {
			return false
		}
	}
	return false
}

var errStopping = &sparkproc.Unavailable{Msg: "the instance is stopping"}

var gatewayRE = regexp.MustCompile(`^/component-gateway/([0-9a-f-]{36})/([a-z]+)(/.*)?$`)

// WebUI serves the History Servers beside next: the instance's at
// /sparkhistory/, each cluster's at its Component Gateway path.
func (s *Server) WebUI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		var addr, base, strip, rewrite string
		var masters []string
		switch {
		case p == HistoryBase || strings.HasPrefix(p, HistoryBase+"/"):
			a, err := s.instanceHistory(r.Context())
			if err != nil {
				http.Error(w, "The Spark UI is not available: "+err.Error(), http.StatusServiceUnavailable)
				return
			}
			if m := workloadRE.FindStringSubmatch(p); m != nil {
				app := s.resolveApp(m[1])
				if app == "" {
					http.Error(w, "No Spark application of this workload has been recorded yet: its Spark UI shows once its driver has started", http.StatusNotFound)
					return
				}
				http.Redirect(w, r, HistoryBase+"/history/"+app+m[2]+query(r.URL), http.StatusFound)
				return
			}
			addr, base, strip = a, HistoryBase, HistoryBase
		case gatewayRE.MatchString(p):
			m := gatewayRE.FindStringSubmatch(p)
			s.mu.Lock()
			var nodes Nodes
			for name, c := range s.clusters {
				if c.ClusterUuid == m[1] {
					nodes = s.nodes[name]
					masters = masterNames(c)
				}
			}
			s.mu.Unlock()
			if nodes == nil {
				http.Error(w, "The cluster is not running, so its web interfaces are not available", http.StatusServiceUnavailable)
				return
			}
			key, ok := gatewayKeys[m[2]]
			if !ok {
				http.NotFound(w, r)
				return
			}
			addr, base = nodes.Web(r.Context())[key], gatewayBase(m[1])+m[2]
			if gatewayFull[m[2]] {
				strip = ""
			} else {
				strip = base
			}
			if _, ok := gatewayRewritten[m[2]]; ok {
				rewrite = gatewayBase(m[1])
			}
		case strings.HasPrefix(p, GatewayBase):
			s.serveGateway(w, r)
			return
		default:
			next.ServeHTTP(w, r)
			return
		}
		if addr == "" {
			http.Error(w, "The History Server is not reachable", http.StatusServiceUnavailable)
			return
		}
		target := &url.URL{Scheme: "http", Host: addr}
		proxy := &httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, strip)
			if pr.Out.URL.Path == "" {
				pr.Out.URL.Path = "/"
			}
			pr.Out.URL.RawPath = ""
			pr.Out.Host = addr
			if rewrite != "" {
				// The pages are rewritten, so they come uncompressed.
				pr.Out.Header.Del("Accept-Encoding")
			}
		}, ModifyResponse: func(resp *http.Response) error {
			if rewrite != "" {
				return rewriteHadoop(resp, rewrite, base, masters)
			}
			// The History Server redirects to its own paths, which the
			// proxy base prefixes.
			if loc := resp.Header.Get("Location"); loc != "" {
				if u, err := url.Parse(loc); err == nil && (u.Host == addr || u.Host == "") && strip != "" && !strings.HasPrefix(u.Path, base) {
					resp.Header.Set("Location", base+u.Path+query(u))
				}
			}
			return nil
		}}
		proxy.ServeHTTP(w, r)
	})
}

func query(u *url.URL) string {
	if u.RawQuery == "" {
		return ""
	}
	return "?" + u.RawQuery
}

// eventLogs are the files of the application app in dir (its event
// log, and its rolled or in-progress parts).
func eventLogs(dir, app string) []string {
	if app == "" || dir == "" {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), app) || strings.HasPrefix(e.Name(), "eventlog_v2_"+app) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// eventLogDir is where a cluster's jobs' event logs go in Cloud Storage.
func eventLogDir(c *dataprocpb.Cluster) string {
	if d := c.GetConfig().GetSoftwareConfig().GetProperties()["spark:spark.eventLog.dir"]; strings.HasPrefix(d, "gs://") {
		return strings.TrimSuffix(d, "/")
	}
	return "gs://" + c.GetConfig().GetTempBucket() + "/" + c.GetClusterUuid() + "/spark-job-history"
}

// uploadEventLog copies a job's event log to the cluster's event log
// directory in Cloud Storage.
func (s *Server) uploadEventLog(c *dataprocpb.Cluster, nodes Nodes, app string) {
	if s.opts.Storage == nil || nodes == nil {
		return
	}
	files := eventLogs(nodes.HistoryDir(), app)
	if len(files) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir := eventLogDir(c)
	if dir == "gs://"+c.GetConfig().GetTempBucket()+"/"+c.GetClusterUuid()+"/spark-job-history" && c.GetConfig().GetTempBucket() == s.TempBucket(c.GetProjectId(), regionOfCluster(c)) {
		if err := s.opts.Storage.EnsureBucket(ctx, c.GetProjectId(), c.GetConfig().GetTempBucket(), regionOfCluster(c)); err != nil {
			s.logf("dataproc: temp bucket %s: %v", c.GetConfig().GetTempBucket(), err)
			return
		}
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if err := s.opts.Storage.Write(ctx, dir+"/"+filepath.Base(f), b); err != nil {
			s.logf("dataproc: event log of %s: %v", app, err)
		}
	}
}

// historyDirOf is a Persistent History Server's gs:// log directory
// (spark:spark.history.fs.logDirectory), or "".
func historyDirOf(c *dataprocpb.Cluster) string {
	d := c.GetConfig().GetSoftwareConfig().GetProperties()["spark:spark.history.fs.logDirectory"]
	if strings.HasPrefix(d, "gs://") {
		return strings.TrimSuffix(d, "/")
	}
	return ""
}

// syncHistory copies the event logs under a Persistent History Server's
// log directory (its * matching one path segment, as the page's
// gs://bucket-name/*/spark-job-history) into its History Server's.
func (s *Server) syncHistory(c *dataprocpb.Cluster, nodes Nodes) {
	dir := historyDirOf(c)
	lister, ok := s.opts.Storage.(Lister)
	if dir == "" || !ok || nodes == nil {
		return
	}
	bucket, pattern, _ := strings.Cut(strings.TrimPrefix(dir, "gs://"), "/")
	prefix := pattern
	if i := strings.Index(prefix, "*"); i >= 0 {
		prefix = prefix[:i]
	}
	re, err := regexp.Compile("^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, `[^/]+`) + "/([^/]+)$")
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	objects, err := lister.List(ctx, bucket, prefix)
	if err != nil {
		return
	}
	local := nodes.HistoryDir()
	for name, size := range objects {
		m := re.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		dst := filepath.Join(local, m[1])
		if st, err := os.Stat(dst); err == nil && st.Size() == size {
			continue
		}
		b, err := s.opts.Storage.Read(ctx, "gs://"+bucket+"/"+name)
		if err != nil {
			continue
		}
		tmp := dst + ".cbtmp"
		if os.WriteFile(tmp, b, 0o644) == nil {
			_ = os.Rename(tmp, dst)
		}
	}
}

// copyToPHS copies a batch's or session's event log to the Persistent
// History Server its peripherals_config names.
func (s *Server) copyToPHS(phs, uuid string) {
	if phs == "" || s.opts.HistoryDir == "" {
		return
	}
	if i := strings.Index(phs, "projects/"); i >= 0 {
		phs = phs[i:]
	}
	s.mu.Lock()
	nodes := s.nodes[phs]
	c := s.clusters[phs]
	s.mu.Unlock()
	if nodes == nil || c == nil {
		return
	}
	for _, f := range eventLogs(s.opts.HistoryDir, s.resolveApp(appID(uuid))) {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		_ = os.WriteFile(filepath.Join(nodes.HistoryDir(), filepath.Base(f)), b, 0o644)
		if dir := historyDirOf(c); dir != "" && s.opts.Storage != nil && !strings.Contains(dir, "*") {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			_ = s.opts.Storage.Write(ctx, dir+"/"+filepath.Base(f), b)
			cancel()
		}
	}
}

// rewriteHadoop rewrites a Hadoop web interface's response for its
// Component Gateway path base (gateway is the cluster's gateway base):
// its redirects, its pages' root-relative links, and the absolute URLs of
// the masters' interfaces (http://CLUSTER-m:8088/proxy/..., the
// JobHistory Server's http://CLUSTER-m:19888/jobhistory/...; on a High
// Availability cluster the active ResourceManager's, on any of the
// masters), each to its own gateway path.
func rewriteHadoop(resp *http.Response, gateway, base string, masters []string) error {
	hosts := func(port string) []string {
		out := []string{"0.0.0.0:" + port, "localhost:" + port}
		for _, m := range masters {
			out = append(out, m+":"+port)
		}
		return out
	}
	abs := func(u string) string {
		for key, port := range gatewayRewritten {
			for _, host := range hosts(port) {
				if rest, ok := strings.CutPrefix(u, "http://"+host); ok {
					return gateway + key + rest
				}
			}
		}
		return u
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		if strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, base) {
			resp.Header.Set("Location", base+loc)
		} else {
			resp.Header.Set("Location", abs(loc))
		}
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "html") && !strings.Contains(ct, "javascript") {
		return nil
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	resp.Body.Close()
	if err != nil {
		return err
	}
	page := string(b)
	for key, port := range gatewayRewritten {
		for _, host := range hosts(port) {
			if strings.HasPrefix(host, "localhost:") {
				continue
			}
			page = strings.ReplaceAll(page, "http://"+host, strings.TrimSuffix(gateway, "/")+"/"+key)
		}
	}
	if strings.HasSuffix(base, "/hdfs") {
		// The NameNode's pages read its JMX, configuration, startup
		// progress and WebHDFS by root-relative URLs in their scripts.
		for _, p := range []string{"/jmx", "/webhdfs/", "/conf", "/startupProgress"} {
			for _, q := range []string{`'`, `"`} {
				page = strings.ReplaceAll(page, q+p, q+base+p)
			}
		}
	}
	for _, attr := range []string{`href="/`, `src="/`, `href='/`, `src='/`, `action="/`} {
		q := attr[len(attr)-2 : len(attr)-1]
		page = rootLinks(page, attr, attr[:len(attr)-2]+q+base+"/", gateway)
	}
	resp.Body = io.NopCloser(strings.NewReader(page))
	resp.ContentLength = int64(len(page))
	resp.Header.Set("Content-Length", fmt.Sprint(len(page)))
	return nil
}

// rootLinks replaces each attr (href="/) in page with repl, except where
// the link is already a gateway path.
func rootLinks(page, attr, repl, gateway string) string {
	var b strings.Builder
	for {
		i := strings.Index(page, attr)
		if i < 0 {
			b.WriteString(page)
			return b.String()
		}
		b.WriteString(page[:i])
		rest := page[i+len(attr)-1:]
		if strings.HasPrefix(rest, gateway) {
			b.WriteString(attr)
		} else {
			b.WriteString(repl)
		}
		page = page[i+len(attr):]
	}
}
