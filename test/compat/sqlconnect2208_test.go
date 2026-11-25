//go:build compat

package compat

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestGcloudSQLConnectInContainer (#2208): `gcloud sql connect` (gcloud
// 593.0.0, which connects through Google's Cloud SQL Auth Proxy v2 and
// then runs the engine's client) and the Auth Proxy binary on its own,
// on a port of the test's choosing, each in a container that has the
// client: psql from the official postgres image, mysql from the official
// mysql image (testdata/sqlconnect). A wrong password is refused with the
// server's own words, and the right one runs a statement.
//
// The containers are on an internal Docker network, with no route out, so
// nothing they run can reach Google whatever it is configured with; a
// relay, the only container that reaches the host, forwards the
// instance's ports that `cloudburrow env` names. The Auth Proxy always
// dials port 3307 at the address connectSettings reports (127.0.0.1 for
// the servers `up` runs); the container's own 3307 is forwarded to the
// instance's connector listener wherever it is (--cloudsql-connector), so
// the test never needs the host's 3307.
// covers: sql.connect.get, sql.connect.generateEphemeral, sql.instances.get
func TestGcloudSQLConnectInContainer(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvSQLAdmin)
	if os.Getenv(EnvCloudSQL) == "" && os.Getenv(EnvMySQL) == "" {
		t.Skip("needs --services cloudsql or cloudsql-mysql")
	}
	if os.Getenv(EnvCLI) == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	if exec.Command("docker", "info").Run() != nil {
		t.Skip("docker is not available")
	}
	vars := envVarsFromCLI(t)
	for _, k := range []string{"CLOUDSDK_API_ENDPOINT_OVERRIDES_SQL", "GOOGLE_APPLICATION_CREDENTIALS"} {
		if vars[k] == "" {
			t.Fatalf("cloudburrow env exported no %s", k)
		}
	}
	h.requireLocal(strings.TrimSuffix(strings.TrimPrefix(vars["CLOUDSDK_API_ENDPOINT_OVERRIDES_SQL"], "http://"), "/"))
	connector := vars["CLOUDBURROW_CLOUDSQL_CONNECTOR_ENDPOINT"]
	if connector == "" {
		connector = "127.0.0.1:3307"
	}
	fixture := vars["GOOGLE_APPLICATION_CREDENTIALS"]
	adc, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatalf("the exported fixture: %v", err)
	}

	// Every instance port the variables and the fixture name.
	portRE := regexp.MustCompile(`127\.0\.0\.1:([0-9]+)`)
	portSet := map[string]bool{}
	var envFile strings.Builder
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := vars[k]
		// What a client reads; no admin credential goes into the container.
		if strings.Contains(k, "TOKEN") && !strings.HasSuffix(k, "TOKEN_HOST") || strings.Contains(k, "PASSWORD") || k == "MYSQL_PWD" {
			continue
		}
		fmt.Fprintf(&envFile, "%s=%s\n", k, v)
		for _, m := range portRE.FindAllStringSubmatch(v, -1) {
			portSet[m[1]] = true
		}
	}
	for _, m := range portRE.FindAllStringSubmatch(string(adc), -1) {
		portSet[m[1]] = true
	}
	ports := make([]string, 0, len(portSet))
	for p := range portSet {
		ports = append(ports, p)
	}
	sort.Strings(ports)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	arch, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Arch}}").Output()
	if err != nil {
		t.Fatalf("docker version: %v", err)
	}
	dir := t.TempDir()
	build := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(dir, "sink"), "./testdata/egress/sink")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+strings.TrimSpace(string(arch)), "CGO_ENABLED=0")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the sink: %v\n%s", err, b)
	}
	script, err := os.ReadFile(filepath.Join("testdata", "sqlconnect", "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "run.sh"), script, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "env"), []byte(envFile.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	type engine struct {
		instance, target, client, user, password string
	}
	var engines []engine
	if os.Getenv(EnvCloudSQL) != "" {
		engines = append(engines, engine{"postgres", "postgres", "psql", "cloudburrow", os.Getenv(EnvCloudSQLPassword)})
	}
	if os.Getenv(EnvMySQL) != "" {
		engines = append(engines, engine{"mysql", "mysql", "mysql", "cloudburrow", os.Getenv(EnvMySQLPassword)})
	}
	for _, e := range engines {
		if e.password == "" {
			t.Fatalf("cloudburrow env exported no password for %s", e.instance)
		}
	}
	images := map[string]string{}
	for _, e := range engines {
		images[e.target] = sqlConnectImage(ctx, t, e.target)
	}

	suffix := fmt.Sprint(time.Now().UnixNano() % 1e9)
	network, relay := "cb-sqlconnect-"+suffix, "cb-sqlconnect-relay-"+suffix
	if b, err := exec.CommandContext(ctx, "docker", "network", "create", "--internal", network).CombinedOutput(); err != nil {
		t.Fatalf("create the internal network: %v\n%s", err, b)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", network).Run() })
	args := []string{"run", "-d", "--name", relay, "-v", dir + ":/egress:ro", "--entrypoint", "/egress/sink"}
	hostIP := "127.0.0.1"
	if runtime.GOOS == "linux" {
		hostIP = bridgeGateway(ctx, t)
		args = append(args, "--add-host", "host.docker.internal:"+hostIP)
	}
	args = append(args, images[engines[0].target])
	for _, p := range ports {
		args = append(args, "-forward", "0.0.0.0:"+p+"=host.docker.internal:"+hostForwarder(t, hostIP, "127.0.0.1:"+p))
	}
	// The connector's listener, at a relay port of its own: the clients'
	// 3307.
	const connectorRelayPort = "13307"
	args = append(args, "-forward", "0.0.0.0:"+connectorRelayPort+"=host.docker.internal:"+hostForwarder(t, hostIP, connector))
	if b, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("start the relay: %v\n%s", err, b)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "-v", relay).Run() })
	if b, err := exec.CommandContext(ctx, "docker", "network", "connect", "--alias", "relay", network, relay).CombinedOutput(); err != nil {
		t.Fatalf("connect the relay: %v\n%s", err, b)
	}

	// run runs run.sh in e's container with stdin, and returns its output.
	run := func(t *testing.T, e engine, stdin string, extra []string, script ...string) (string, error) {
		t.Helper()
		a := []string{"run", "--rm", "-i", "--network", network, "-v", dir + ":/egress:ro", "-v", fixture + ":" + fixture + ":ro",
			"--env-file", filepath.Join(dir, "env"), "-e", "SQLCONNECT_PORTS=" + strings.Join(ports, " "),
			"-e", "SQLCONNECT_CONNECTOR_PORT=" + connectorRelayPort}
		a = append(a, extra...)
		a = append(a, images[e.target], "/bin/sh", "/egress/run.sh")
		a = append(a, script...)
		c := exec.CommandContext(ctx, "docker", a...)
		// The password, then the statements once the client has read it: a
		// client that reads the password from a buffered stdin (mysql's
		// getpass) would otherwise take the statements with it.
		pr, pw := io.Pipe()
		c.Stdin = pr
		go func() {
			first, rest, _ := strings.Cut(stdin, "\n")
			_, _ = io.WriteString(pw, first+"\n")
			time.Sleep(5 * time.Second)
			_, _ = io.WriteString(pw, rest)
			_ = pw.Close()
		}()
		b, err := c.CombinedOutput()
		return string(b), err
	}

	svc := sqlAdminService(t, h)
	for _, e := range engines {
		in, err := svc.Instances.Get(h.Project(), e.instance).Do()
		if err != nil {
			t.Fatal(err)
		}
		marker := "right-" + suffix
		query, refused := fmt.Sprintf("SELECT '%s' AS marker;\n", marker), fmt.Sprintf(`password authentication failed for user "%s"`, e.user)
		connect := []string{"connect", e.instance, "--user", e.user, "--port", "9470"}
		if e.client == "psql" {
			query += "\\q\n"
			connect = append(connect, "--database", "cloudburrow")
		} else {
			query += "exit\n"
			refused = fmt.Sprintf("Access denied for user '%s'", e.user)
		}
		t.Run("gcloud-sql-connect-"+e.instance, func(t *testing.T) {
			out, err := run(t, e, "wrong-"+e.password+"\n"+query, nil, connect...)
			if err == nil || !strings.Contains(out, refused) || strings.Contains(out, marker+"\n") {
				t.Errorf("a wrong password through gcloud sql connect: %v\n%s", err, out)
			}
			if !strings.Contains(out, "Starting Cloud SQL Proxy") {
				t.Errorf("gcloud sql connect did not start the Cloud SQL Proxy:\n%s", out)
			}
			out, err = run(t, e, e.password+"\n"+query, nil, connect...)
			if err != nil || !regexp.MustCompile(`(?m)^\|?\s*`+marker+`\s*\|?\s*$`).MatchString(out) {
				t.Errorf("the right password through gcloud sql connect: %v\n%s", err, out)
			}
		})
		t.Run("auth-proxy-"+e.instance, func(t *testing.T) {
			// The proxy on a port of the test's own, as `--port` sets it.
			port := "6543"
			var client []string
			var env []string
			if e.client == "psql" {
				client = []string{"psql", "-h", "127.0.0.1", "-p", port, "-U", e.user, "-d", "cloudburrow", "-tAc", "SELECT '" + marker + "'"}
				env = []string{"-e", "PGCONNECT_TIMEOUT=20"}
			} else {
				client = []string{"mysql", "-h", "127.0.0.1", "-P", port, "-u", e.user, "-N", "-e", "SELECT '" + marker + "'"}
			}
			pw := func(p string) []string {
				if e.client == "psql" {
					return append(env, "-e", "PGPASSWORD="+p)
				}
				return append(env, "-e", "MYSQL_PWD="+p)
			}
			script := append([]string{"proxy", in.ConnectionName, port}, client...)
			out, err := run(t, e, "", pw("wrong-"+e.password), script...)
			if err == nil || !strings.Contains(out, refused) || !strings.Contains(out, "PROXY Listening on 127.0.0.1:"+port) {
				t.Errorf("a wrong password through the Auth Proxy: %v\n%s", err, out)
			}
			out, err = run(t, e, "", pw(e.password), script...)
			if err != nil || !strings.Contains(out, "\n"+marker+"\n") || !strings.Contains(out, "EXIT 0") {
				t.Errorf("the right password through the Auth Proxy: %v\n%s", err, out)
			}
		})
	}
}

// sqlConnectImage builds testdata/sqlconnect's target ("postgres" or
// "mysql"), tagged by the hash of the Dockerfile and the target.
func sqlConnectImage(ctx context.Context, t *testing.T, target string) string {
	t.Helper()
	src, err := filepath.Abs(filepath.Join("testdata", "sqlconnect"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(src, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(append(b, target...))
	image := fmt.Sprintf("cloudburrow-compat-sqlconnect-%s:%x", target, sum[:6])
	if exec.CommandContext(ctx, "docker", "image", "inspect", image).Run() != nil {
		if out, err := exec.CommandContext(ctx, "docker", "build", "--target", target, "-t", image, src).CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", image, err, out)
		}
	}
	return image
}
