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
	"context"
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

// appID is the Spark application ID a batch or session runs as: its UUID,
// so its Spark UI is found by it.
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

var errStopping = &sparkproc.Unavailable{Msg: "the instance is stopping"}

var gatewayRE = regexp.MustCompile(`^/component-gateway/([0-9a-f-]{36})/sparkhistory(/.*)?$`)

// WebUI serves the History Servers beside next: the instance's at
// /sparkhistory/, each cluster's at its Component Gateway path.
func (s *Server) WebUI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		var addr, base string
		switch {
		case p == HistoryBase || strings.HasPrefix(p, HistoryBase+"/"):
			a, err := s.instanceHistory(r.Context())
			if err != nil {
				http.Error(w, "The Spark UI is not available: "+err.Error(), http.StatusServiceUnavailable)
				return
			}
			addr, base = a, HistoryBase
		case gatewayRE.MatchString(p):
			m := gatewayRE.FindStringSubmatch(p)
			s.mu.Lock()
			var nodes Nodes
			for name, c := range s.clusters {
				if c.ClusterUuid == m[1] {
					nodes = s.nodes[name]
				}
			}
			s.mu.Unlock()
			if nodes == nil {
				http.Error(w, "The cluster is not running, so its web interfaces are not available", http.StatusServiceUnavailable)
				return
			}
			addr, base = nodes.Web(r.Context())["history"], gatewayBase(m[1])+"sparkhistory"
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
			pr.Out.URL.Path = strings.TrimPrefix(pr.In.URL.Path, base)
			if pr.Out.URL.Path == "" {
				pr.Out.URL.Path = "/"
			}
			pr.Out.URL.RawPath = ""
			pr.Out.Host = addr
		}, ModifyResponse: func(resp *http.Response) error {
			// The History Server redirects to its own paths, which the
			// proxy base prefixes.
			if loc := resp.Header.Get("Location"); loc != "" {
				if u, err := url.Parse(loc); err == nil && (u.Host == addr || u.Host == "") && !strings.HasPrefix(u.Path, base) {
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
	for _, f := range eventLogs(s.opts.HistoryDir, appID(uuid)) {
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
