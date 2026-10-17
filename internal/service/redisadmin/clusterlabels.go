package redisadmin

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// A cluster's labels (#1519). redis.googleapis.com's REST Cluster has
// labels, which the Terraform provider sends (goog-terraform-provisioned
// among them), but google.cloud.redis.cluster.v1's Go proto here has no
// such field, so the transcoder would refuse the body. ClusterLabels takes
// them out of a create or update's JSON body, keeps them with the cluster,
// and puts them back in every Cluster the JSON API returns, the ones
// operations carry among them. Over gRPC a cluster has no labels.

// ClusterLabels wraps the JSON API next.
func (s *Server) ClusterLabels(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := s.clusters
		if c == nil || !strings.HasPrefix(r.URL.Path, "/v1/projects/") ||
			!(strings.Contains(r.URL.Path, "/clusters") || strings.Contains(r.URL.Path, "/operations")) {
			next.ServeHTTP(w, r)
			return
		}
		var labels map[string]string
		var target string
		hasLabels := false
		if (r.Method == http.MethodPost || r.Method == http.MethodPatch) && strings.Contains(r.URL.Path, "/clusters") {
			body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
			if err == nil {
				var m map[string]json.RawMessage
				if json.Unmarshal(body, &m) == nil {
					if raw, ok := m["labels"]; ok {
						hasLabels = true
						_ = json.Unmarshal(raw, &labels)
						delete(m, "labels")
						body, _ = json.Marshal(m)
					}
				}
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.ContentLength = int64(len(body))
			name := strings.TrimPrefix(r.URL.Path, "/v1/")
			if r.Method == http.MethodPost {
				target = name + "/" + r.URL.Query().Get("clusterId")
			} else {
				target = name
				if m := r.URL.Query().Get("updateMask"); m != "" && !strings.Contains(m, "labels") {
					hasLabels = false
				}
			}
		}
		rec := &bufferedResponse{header: http.Header{}, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		if hasLabels && rec.code/100 == 2 {
			c.mu.Lock()
			if cr, ok := c.byName[target]; ok {
				cr.Labels = copyMap(labels)
				_ = c.save(cr)
			}
			c.mu.Unlock()
		}
		out := rec.body.Bytes()
		if rec.code/100 == 2 {
			var v any
			if json.Unmarshal(out, &v) == nil && s.injectLabels(v) {
				if b, err := json.Marshal(v); err == nil {
					out = b
				}
			}
		}
		for k, vs := range rec.header {
			w.Header()[k] = vs
		}
		w.Header().Del("Content-Length")
		w.WriteHeader(rec.code)
		_, _ = w.Write(out)
	})
}

// injectLabels sets labels on every Cluster in v, and reports whether it
// set any.
func (s *Server) injectLabels(v any) bool {
	changed := false
	switch t := v.(type) {
	case map[string]any:
		if name, ok := t["name"].(string); ok && clusterRE.MatchString(name) {
			if _, isCluster := t["shardCount"]; isCluster {
				s.clusters.mu.Lock()
				if cr, ok := s.clusters.byName[name]; ok && len(cr.Labels) > 0 {
					t["labels"] = cr.Labels
					changed = true
				}
				s.clusters.mu.Unlock()
			}
		}
		for _, e := range t {
			changed = s.injectLabels(e) || changed
		}
	case []any:
		for _, e := range t {
			changed = s.injectLabels(e) || changed
		}
	}
	return changed
}

type bufferedResponse struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header         { return b.header }
func (b *bufferedResponse) WriteHeader(code int)        { b.code = code }
func (b *bufferedResponse) Write(p []byte) (int, error) { return b.body.Write(p) }
