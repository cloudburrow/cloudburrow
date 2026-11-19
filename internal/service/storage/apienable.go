package storage

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/apienable"
)

// Disabled APIs (#1914). The Cloud Storage API disabled in a project
// refuses every call made in it, as Google's does: a call's project is
// its quota project (x-goog-user-project), buckets.list's and
// buckets.insert's project parameter, or the project of the bucket it
// names. The servers that run in the cluster ask the host
// (internal/apienable).

// APIEnableOptions are the gate's options for this server: the project
// of the bucket a request names, and an XML API request answered in XML.
func (s *Server) APIEnableOptions() apienable.Options {
	return apienable.Options{
		ProjectOf: s.requestProject,
		WriteJSON: func(w http.ResponseWriter, r *http.Request, d apienable.Denial) {
			if s.isXMLRequest(r) {
				b, _ := s.xmlTarget(r)
				writeXMLFromError(w, r, errorf(http.StatusForbidden, "accessNotConfigured", "%s", d.Message()), b)
				return
			}
			d.WriteJSON(w)
		},
		Skip: func(r *http.Request) bool {
			p := r.URL.EscapedPath()
			return p == batchPath || strings.HasPrefix(p, batchPath+"/") || p == lifecyclePath || p == resetPath ||
				p == statePath || p == ChannelsPath || p == ChannelOutboxPath || p == eventsPath || p == CloudBuildLogsPath
		},
	}
}

func (s *Server) isXMLRequest(r *http.Request) bool {
	p := r.URL.EscapedPath()
	return !strings.HasPrefix(p, jsonPrefix) && !strings.HasPrefix(p, uploadPrefix) && !strings.HasPrefix(p, resumablePrefix) &&
		!strings.HasPrefix(p, downloadPrefix) && !strings.HasPrefix(p, controlPrefix)
}

// requestProject is the project of the bucket r names, "" for none.
func (s *Server) requestProject(r *http.Request) string {
	if p := apienable.ProjectOfHeaders(r.Header.Values); p != "" {
		return p
	}
	bucket := ""
	if apienable.IsGRPC(r) {
		for _, v := range r.Header.Values("X-Goog-Request-Params") {
			for _, kv := range strings.Split(v, "&") {
				_, val, _ := strings.Cut(kv, "=")
				if u, err := url.QueryUnescape(val); err == nil {
					val = u
				}
				if b, ok := strings.CutPrefix(val, "projects/_/buckets/"); ok {
					bucket, _, _ = strings.Cut(b, "/")
				}
			}
		}
	} else {
		if p := strings.TrimSpace(r.URL.Query().Get("project")); p != "" && strings.HasPrefix(r.URL.EscapedPath(), jsonPrefix) {
			return p
		}
		bucket, _ = s.ipFilterTarget(r)
	}
	if bucket == "" {
		return ""
	}
	project := ""
	_ = s.meta.View(func(tx Tx) error {
		raw, ok := tx.Get(bucketPrefix + bucket)
		if !ok {
			return nil
		}
		var b struct {
			Project string `json:"project"`
		}
		if json.Unmarshal(raw, &b) == nil {
			project = b.Project
		}
		return nil
	})
	return project
}
