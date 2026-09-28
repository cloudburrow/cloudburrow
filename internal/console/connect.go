package console

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/version"
)

// Connect and About (#802): the console's page for what `cloudburrow env`,
// `gcloud-setup`, `terraform`, `version` and `diagnose` give at a terminal.
//
// The page shows no credential. The ADC fixture's path is shown, as `env`
// exports it, but never its key; a variable whose value is a credential
// (the generated MySQL password) is named and withheld; the admin token is
// never shown and never held. The diagnose download needs the admin token
// from the page, as /admin does, because the bundle carries the admin API's
// events and a workload in the cluster can reach this console on Docker
// Desktop (#553): the console passes the page's token to the admin API in
// process, and the admin API's own token check decides.

// ConnectSource is what the Connect page reads. cmd/cloudburrow implements
// it with the functions its commands use, so the page cannot drift from them.
type ConnectSource interface {
	// Connect is what `env` exports for this running instance, rendered in
	// each format, with the setup commands.
	Connect(ctx context.Context) (Connect, error)
	// Diagnose builds the bundle `cloudburrow diagnose` writes. authorization
	// is the request's Authorization header, the admin token the page holds;
	// without the right one it returns an *AdminRefusal and builds nothing.
	Diagnose(ctx context.Context, authorization string) (name string, bundle []byte, err error)
}

// Connect is the page's content.
type Connect struct {
	// Instance and Project name what the variables point at.
	Instance string `json:"instance"`
	Project  string `json:"project"`
	// Variables are what `env --format json` prints, in its order, less the
	// Withheld ones.
	Variables []EnvVar `json:"variables"`
	// Withheld are variables `env` exports whose value is a credential; the
	// page names them and never carries the value.
	Withheld []WithheldVar `json:"withheld,omitempty"`
	// Formats are `env --format <id>`'s output, less the withheld values.
	Formats []EnvFormat `json:"formats"`
	// Warnings are what `env` says on stderr: a variable it cannot export.
	Warnings []string `json:"warnings,omitempty"`
	// Snippets are client-library examples for the variables exported.
	Snippets []Snippet `json:"snippets"`
	// Commands are the terminal commands for this instance.
	GcloudSetup    Command `json:"gcloudSetup"`
	GcloudTeardown Command `json:"gcloudTeardown"`
	Terraform      Command `json:"terraform"`
	Env            Command `json:"env"`
	Diagnose       Command `json:"diagnose"`
	// DiagnoseExcluded names what a bundle never collects.
	DiagnoseExcluded []string `json:"diagnoseExcluded"`
	// AdminTokenFile is where the admin token is, for the page to say. The
	// path, never the token.
	AdminTokenFile string `json:"adminTokenFile,omitempty"`
}

// EnvVar is one exported variable.
type EnvVar struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	Comment string `json:"comment"`
}

// WithheldVar is an exported variable the page does not show.
type WithheldVar struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// EnvFormat is one `env --format` output.
type EnvFormat struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Text  string `json:"text"`
}

// Command is a terminal command to copy, and what running it does on the
// host. The console never runs it.
type Command struct {
	Command string `json:"command"`
	Writes  string `json:"writes"`
}

// About is `cloudburrow version`, field by field.
type About struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"buildDate"`
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"`
	// Line is the whole line `cloudburrow version` prints.
	Line string `json:"line"`
}

// AdminRefusal is the admin API's answer to a request it refused, relayed
// with its own status and message.
type AdminRefusal struct {
	Status  int
	Message string
	// TokenFile is where the admin token is, named on a 401.
	TokenFile string
}

func (e *AdminRefusal) Error() string { return e.Message }

// SetConnect attaches the Connect page's source; without one, the page says
// the console is not attached to an instance.
func (s *Server) SetConnect(src ConnectSource) { s.connect = src }

func aboutOf(i version.Info) About {
	return About{Version: i.Version, Commit: i.Commit, BuildDate: i.BuildDate,
		GoVersion: i.GoVersion, Platform: i.Platform, Line: i.String()}
}

// handleAbout is GET /api/about: the build this console is part of, as
// `cloudburrow version` prints it. The console is served by the CLI's own
// process, so it is the same binary.
func (s *Server) handleAbout(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, aboutOf(version.Get()))
}

func (s *Server) connectWired(w http.ResponseWriter) bool {
	if s.connect == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "Connect is not available on this console: it is not attached to a running instance"})
		return false
	}
	return true
}

// handleConnect is GET /api/connect.
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	if !s.connectWired(w) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readBudget)
	defer cancel()
	c, err := s.connect.Connect(ctx)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// handleDiagnose is GET /api/diagnose: the bundle, as a download, for the
// admin token in the request's Authorization header. A refusal is the admin
// API's own status and message, and a 401 names the token file.
func (s *Server) handleDiagnose(w http.ResponseWriter, r *http.Request) {
	if !s.connectWired(w) {
		return
	}
	// doctor, kubectl and the log tail each bound themselves; this bounds
	// the whole, as `diagnose` has the terminal's patience and a page does not.
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	name, bundle, err := s.connect.Diagnose(ctx, r.Header.Get("Authorization"))
	var refused *AdminRefusal
	if errors.As(err, &refused) {
		out := map[string]any{"error": refused.Message}
		if refused.Status == http.StatusUnauthorized {
			out["token_required"] = true
			if refused.TokenFile != "" {
				out["token_file"] = refused.TokenFile
			}
		}
		writeJSON(w, refused.Status, out)
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.Header().Set("Content-Length", fmt.Sprint(len(bundle)))
	_, _ = w.Write(bundle)
}
