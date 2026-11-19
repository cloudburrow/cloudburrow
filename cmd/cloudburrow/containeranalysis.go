package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/grafeas"
	"github.com/cloudburrow/cloudburrow/internal/osvscan"
	"github.com/cloudburrow/cloudburrow/internal/prefetch"
	"github.com/cloudburrow/cloudburrow/internal/service/serviceusage"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// scanningServiceUsage is the instance's Service Usage, whose Container
// Scanning API state turns Artifact Registry's scanning on (#1912).
var scanningServiceUsage atomic.Pointer[serviceusage.Server]

// containerAnalysisJSONPrefix is where Container Analysis's JSON API is
// served on the Run port: gcloud's containeranalysis override names it
// and appends v1/.
const containerAnalysisJSONPrefix = "/containeranalysis"

// openContainerAnalysis opens the instance's Container Analysis store,
// under its state directory, and turns scanning on over it.
func openContainerAnalysis(cfg config.Config) (*grafeas.Server, error) {
	dir := filepath.Join(cfg.InstanceDir(), "containeranalysis")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("open the Container Analysis store: %w", err)
	}
	file := filepath.Join(dir, "grafeas.json")
	st := grafeas.NewStore(func(b []byte) error {
		tmp := file + ".tmp"
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			return err
		}
		return os.Rename(tmp, file)
	})
	if b, err := os.ReadFile(file); err == nil {
		if err := st.Load(b); err != nil {
			return nil, fmt.Errorf("read %s: %w", file, err)
		}
	}
	cache := osvscan.Dir(prefetch.CacheDir(cfg.StateDir))
	runadapter.UseContainerAnalysis(st, cache, func(project, api string) bool {
		su := scanningServiceUsage.Load()
		return su != nil && su.Enabled(project, api)
	}, filepath.Join(dir, "scanned-images.json"))
	return grafeas.NewServer(st), nil
}

// containerAnalysisHandlerOr serves Container Analysis's JSON API under
// its prefix and passes every other request to next.
func containerAnalysisHandlerOr(s *grafeas.Server, next http.Handler) http.Handler {
	t := &rest.Transcoder{Packages: []string{"grafeas.v1", "google.devtools.containeranalysis.v1"}}
	s.Register(t)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, containerAnalysisJSONPrefix+"/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = strings.TrimPrefix(r.URL.Path, containerAnalysisJSONPrefix)
		r2.URL.RawPath = ""
		t.ServeHTTP(w, r2)
	})
}
