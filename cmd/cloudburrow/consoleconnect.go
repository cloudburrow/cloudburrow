package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/admin"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
)

// The console's Connect page (#802), built from the functions `env`,
// `gcloud-setup`, `terraform` and `diagnose` use, so the page cannot export
// or describe anything the commands do not.

// consoleConnect is the page's source for one running instance.
type consoleConnect struct {
	cfg config.Config
	// admin is the admin API's routes, called in process with the page's
	// own token for the diagnose bundle's admin events. The console holds no
	// token of its own (#553).
	admin     http.Handler
	tokenFile string
}

func newConsoleConnect(cfg config.Config, api *admin.API) consoleConnect {
	mux := http.NewServeMux()
	api.Routes(mux)
	return consoleConnect{cfg: cfg, admin: mux, tokenFile: adminTokenPath(cfg)}
}

// envFormatLabels names each format as the page's control shows it.
var envFormatLabels = map[string]string{
	"shell": "Shell", "plain": "Plain (docker --env-file)", "json": "JSON", "terraform": "Terraform",
	"docker-compose": "docker-compose", "kubernetes": "Kubernetes (in-cluster)",
}

// Connect is `env` for the running instance, every format of it, less the
// variables whose value is a credential.
func (c consoleConnect) Connect(context.Context) (console.Connect, error) {
	info, live := running(c.cfg)
	if !live {
		return console.Connect{}, fmt.Errorf("instance %q has no runtime file naming this process, so `cloudburrow env` would print nothing for it", c.cfg.Name)
	}
	if len(info.Endpoints) == 0 {
		return console.Connect{}, fmt.Errorf("instance %q is still starting: its endpoints are recorded once every service has bound its port", c.cfg.Name)
	}
	served := servedConfig(c.cfg, info, live)
	exported, vars, err := exportedEnv(served, info, live)
	if err != nil {
		return console.Connect{}, err
	}
	shown, withheld := withholdSecrets(vars)

	out := console.Connect{
		Instance: served.Name, Project: served.DefaultProject(),
		Warnings:         unexportableWarnings(exported),
		DiagnoseExcluded: append([]string(nil), diagnoseExcluded...),
		AdminTokenFile:   c.tokenFile,
	}
	for _, v := range shown {
		out.Variables = append(out.Variables, console.EnvVar{Name: v.Name, Value: v.Value, Comment: v.Comment})
	}
	for _, v := range withheld {
		out.Withheld = append(out.Withheld, console.WithheldVar{Name: v.Name, Reason: envSecrets[v.Name]})
	}
	for _, f := range envFormats {
		var b strings.Builder
		if err := writeEnvFormat(&b, f, exported, shown); err != nil {
			return console.Connect{}, err
		}
		out.Formats = append(out.Formats, console.EnvFormat{ID: f, Label: envFormatLabels[f], Text: b.String()})
	}
	// kubernetes is written from the in-cluster addresses, as `env --format
	// kubernetes` writes it: from the served configuration, before the host
	// ports are applied.
	k8s, _ := withholdSecrets(kubernetesEnvVars(served, info, served.DefaultProject()))
	var b strings.Builder
	writeKubernetesEnv(&b, served.Name, k8s)
	out.Formats = append(out.Formats, console.EnvFormat{ID: "kubernetes", Label: envFormatLabels["kubernetes"], Text: b.String()})
	out.Snippets = console.SnippetsFor(out.Variables)

	flags := instanceFlags(served)
	gcloudPath, err := gcloudConfigPath(served)
	if err != nil {
		gcloudPath = "gcloud's configuration directory (" + err.Error() + ")"
	}
	out.Env = console.Command{Command: `eval "$(cloudburrow env` + flags + `)"`,
		Writes: "Nothing: it prints these variables and the shell exports them. It writes the credentials fixture if it is missing."}
	out.GcloudSetup = console.Command{Command: `eval "$(cloudburrow gcloud-setup` + flags + `)"`,
		Writes: "Writes the gcloud configuration " + gcloudConfigName(served) + " as " + gcloudPath +
			" and selects it in that shell only, with CLOUDSDK_ACTIVE_CONFIG_NAME; your default configuration and active_config are not touched."}
	out.GcloudTeardown = console.Command{Command: `eval "$(cloudburrow gcloud-teardown` + flags + `)"`,
		Writes: "Removes that file, only if cloudburrow wrote it, and unsets CLOUDSDK_ACTIVE_CONFIG_NAME."}
	out.Terraform = console.Command{Command: "cloudburrow terraform" + flags + " -- plan",
		Writes: "Writes cloudburrow_providers.tf (cloudburrow_providers_override.tf when the module declares provider \"google\") " +
			"in the module's directory, runs terraform with the arguments after --, and removes the file when terraform exits."}
	out.Diagnose = console.Command{Command: "cloudburrow diagnose" + flags,
		Writes: "Writes the same bundle as the download below, cloudburrow-diagnose-" + served.Name + "-<time>.tar.gz, in the current directory."}
	return out, nil
}

// withholdSecrets splits vars into those the page shows and those whose
// value is a credential (envSecrets).
func withholdSecrets(vars []envVar) (shown, withheld []envVar) {
	for _, v := range vars {
		if _, secret := envSecrets[v.Name]; secret {
			withheld = append(withheld, v)
			continue
		}
		shown = append(shown, v)
	}
	return shown, withheld
}

// instanceFlags are the flags that name this instance to another command:
// its name and state directory, when they are not the defaults.
func instanceFlags(cfg config.Config) string {
	var b strings.Builder
	if cfg.Name != config.DefaultName {
		b.WriteString(" --name " + shellQuote(cfg.Name))
	}
	if cfg.StateDir != config.Default().StateDir {
		b.WriteString(" --state-dir " + shellQuote(cfg.StateDir))
	}
	return b.String()
}

// shellQuote quotes s for a POSIX shell when it needs it.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./:@") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// adminGet sends one GET to the admin API in process, with only the page's
// Authorization header, and returns its status and body.
func (c consoleConnect) adminGet(ctx context.Context, authorization, path string) (int, []byte) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		b, _ := json.Marshal(map[string]string{"error": err.Error()})
		return http.StatusInternalServerError, b
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := &adminResponse{header: http.Header{}}
	c.admin.ServeHTTP(rec, req)
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	return rec.status, rec.body.Bytes()
}

// adminResponse is what the in-process admin handler wrote.
type adminResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (a *adminResponse) Header() http.Header { return a.header }
func (a *adminResponse) WriteHeader(status int) {
	if a.status == 0 {
		a.status = status
	}
}
func (a *adminResponse) Write(b []byte) (int, error) {
	if a.status == 0 {
		a.status = http.StatusOK
	}
	return a.body.Write(b)
}

// Diagnose is `cloudburrow diagnose` for the page. The admin API is asked
// first, with the page's token: a request it refuses gets its status and
// message and no bundle, since the bundle carries the admin API's events and
// the instance's configuration, logs and pods. With the token, the bundle's
// admin events are read with that same token, never one the console holds.
func (c consoleConnect) Diagnose(ctx context.Context, authorization string) (string, []byte, error) {
	status, body := c.adminGet(ctx, authorization, "/admin/events?limit=1")
	if status != http.StatusOK {
		var parsed struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &parsed) != nil || parsed.Error == "" {
			parsed.Error = strings.TrimSpace(string(body))
		}
		return "", nil, &console.AdminRefusal{Status: status, Message: parsed.Error, TokenFile: c.tokenFile}
	}
	pageToken := func(ctx context.Context, _ string, path string) ([]byte, error) {
		_, body := c.adminGet(ctx, authorization, path)
		return body, nil
	}
	b := collectDiagnostics(ctx, c.cfg, newKubectl(c.cfg.KubeconfigPath()), pageToken)
	var buf bytes.Buffer
	if err := b.writeTo(&buf); err != nil {
		return "", nil, err
	}
	return diagnoseFileName(c.cfg, time.Now()), buf.Bytes(), nil
}

var _ console.ConnectSource = consoleConnect{}
