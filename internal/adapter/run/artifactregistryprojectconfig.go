package run

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// A project's Artifact Registry projectConfig (#1519): its platform logs
// configuration, projects/{project}/locations/{location}/projectConfig, as
// artifactregistry.googleapis.com's REST reference and discovery document
// describe it (getProjectConfig, updateProjectConfig). The artifactregistry
// v1 protos in cloud.google.com/go/artifactregistry do not carry it, so the
// transcoder cannot serve it: its JSON is served by hand, and its official
// client is the generated google.golang.org/api/artifactregistry/v1. It is
// stored, as rules and the VPC-SC config are: this instance writes no
// platform logs for it to switch. Over gRPC it is UNIMPLEMENTED, as no
// published proto names it.

// arProjectConfig is a ProjectConfig resource.
type arProjectConfig struct {
	Name               string                `json:"name,omitempty"`
	PlatformLogsConfig *arPlatformLogsConfig `json:"platformLogsConfig,omitempty"`
}

type arPlatformLogsConfig struct {
	LoggingState  string `json:"loggingState,omitempty"`
	SeverityLevel string `json:"severityLevel,omitempty"`
}

var (
	arLoggingStates  = map[string]bool{"LOGGING_STATE_UNSPECIFIED": true, "ENABLED": true, "DISABLED": true}
	arSeverityLevels = map[string]bool{"SEVERITY_LEVEL_UNSPECIFIED": true, "DEBUG": true, "INFO": true, "NOTICE": true,
		"WARNING": true, "ERROR": true, "CRITICAL": true, "ALERT": true, "EMERGENCY": true}
)

// projectConfigPath is the resource name of a projectConfig path.
func projectConfigPath(p string) (string, bool) {
	name, ok := strings.CutPrefix(p, "/v1/")
	if !ok || !strings.HasPrefix(name, "projects/") {
		return "", false
	}
	loc, ok := strings.CutSuffix(name, "/projectConfig")
	if !ok {
		return "", false
	}
	return name, strings.Count(loc, "/") == 3
}

// serveProjectConfig serves getProjectConfig and updateProjectConfig and
// reports true, or false for any other path.
func serveProjectConfig(w http.ResponseWriter, r *http.Request) bool {
	name, ok := projectConfigPath(r.URL.Path)
	if !ok {
		return false
	}
	if _, _, err := resource.ParseLocation(strings.TrimSuffix(name, "/projectConfig")); err != nil {
		apierror.WriteJSON(w, apierror.InvalidArgument("invalid project config name %q: want projects/*/locations/*/projectConfig", name))
		return true
	}
	var out *arProjectConfig
	var err error
	switch r.Method {
	case http.MethodGet:
		out = getProjectConfig(name)
	case http.MethodPatch:
		out, err = updateProjectConfig(name, r)
	default:
		err = apierror.Unimplemented("%s %s is not an Artifact Registry method", r.Method, r.URL.Path)
	}
	if err != nil {
		apierror.WriteJSON(w, err)
		return true
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(out)
	return true
}

func getProjectConfig(name string) *arProjectConfig {
	arConfig.mu.Lock()
	defer arConfig.mu.Unlock()
	if c, ok := arConfig.projectConfigs[name]; ok {
		cp := *c
		if c.PlatformLogsConfig != nil {
			pl := *c.PlatformLogsConfig
			cp.PlatformLogsConfig = &pl
		}
		return &cp
	}
	return &arProjectConfig{Name: name}
}

// updateProjectConfig sets platformLogsConfig, or the fields of it the
// update mask names; no mask replaces it whole.
func updateProjectConfig(name string, r *http.Request) (*arProjectConfig, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, apierror.InvalidArgument("read the request: %v", err)
	}
	in := &arProjectConfig{}
	if len(body) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(body)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(in); err != nil {
			return nil, apierror.InvalidArgument("invalid ProjectConfig: %v", err)
		}
	}
	if in.Name != "" && in.Name != name {
		return nil, apierror.InvalidArgument("name %q does not match the path's %q", in.Name, name)
	}
	pl := in.PlatformLogsConfig
	if pl == nil {
		pl = &arPlatformLogsConfig{}
	}
	if pl.LoggingState != "" && !arLoggingStates[pl.LoggingState] {
		return nil, apierror.InvalidArgument("invalid platformLogsConfig.loggingState %q", pl.LoggingState)
	}
	if pl.SeverityLevel != "" && !arSeverityLevels[pl.SeverityLevel] {
		return nil, apierror.InvalidArgument("invalid platformLogsConfig.severityLevel %q", pl.SeverityLevel)
	}
	var paths []string
	if m := r.URL.Query().Get("updateMask"); m != "" {
		paths = strings.Split(m, ",")
	}
	cur := getProjectConfig(name)
	next := &arPlatformLogsConfig{}
	if cur.PlatformLogsConfig != nil {
		*next = *cur.PlatformLogsConfig
	}
	if len(paths) == 0 {
		next = pl
	}
	for _, p := range paths {
		switch strings.TrimSpace(p) {
		case "platformLogsConfig", "platform_logs_config":
			next = pl
		case "platformLogsConfig.loggingState", "platform_logs_config.logging_state":
			next.LoggingState = pl.LoggingState
		case "platformLogsConfig.severityLevel", "platform_logs_config.severity_level":
			next.SeverityLevel = pl.SeverityLevel
		default:
			return nil, apierror.InvalidArgument("updateMask path %q is not an updatable field of ProjectConfig", p)
		}
	}
	c := &arProjectConfig{Name: name}
	if next.LoggingState != "" || next.SeverityLevel != "" {
		cp := *next
		c.PlatformLogsConfig = &cp
	}
	arConfig.mu.Lock()
	arConfig.projectConfigs[name] = c
	saveARConfigLocked()
	arConfig.mu.Unlock()
	return getProjectConfig(name), nil
}
