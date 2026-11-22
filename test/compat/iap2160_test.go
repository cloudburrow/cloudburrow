//go:build compat

package compat

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"cloud.google.com/go/iam/apiv1/iampb"
	iap "cloud.google.com/go/iap/apiv1"
	"google.golang.org/protobuf/proto"
)

// iapLocalOnly is a CheckRedirect that refuses to follow a redirect off this
// machine, so a sign-in flow can never reach Google's.
func iapLocalOnly(req *http.Request, via []*http.Request) error {
	if h := req.URL.Hostname(); h != "127.0.0.1" && h != "localhost" {
		return fmt.Errorf("refusing to follow a redirect to %s", req.URL)
	}
	if len(via) > 10 {
		return fmt.Errorf("too many redirects")
	}
	return nil
}

// covers: compute.backendServices.patch, google.cloud.iap.v1.IdentityAwareProxyAdminService/SetIamPolicy
//
// TestIAPBrowserSignInThroughAGoClient (#2160): with IAP on a backend
// service and no OAuth client of its own (IAP's Google-managed one), a
// browser-like client (cookies, following redirects, but only on this
// machine) asking for ?gcp-iap-mode=IDENTITY is sent to the sign-in
// (this instance's consent page, with IAP's redirect URI
// https://iap.googleapis.com/v1/oauth/clientIds/C:handleRedirect, as Google
// documents it), Allow brings it back with IAP's session cookie and the
// identity; an AJAX request without the cookie is 401, marked
// X-Goog-IAP-Generated-Response; with IAP-secured Web App User granted to
// the signed-in account the app itself answers, with IAP's headers; and
// CLEAR_LOGIN_COOKIE signs out.
func TestIAPBrowserSignInThroughAGoClient(t *testing.T) {
	h := New(t)
	h.Endpoint(EnvMetadata) // skips without it
	var seen sync.Map
	l := newLB2160(t, h, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store("email", r.Header.Get("X-Goog-Authenticated-User-Email"))
		_, _ = io.WriteString(w, "ok")
	}))
	ctx, project, n := l.ctx, l.project, l.n
	bs, _ := l.bss.Get(ctx, &computepb.GetBackendServiceRequest{Project: project, BackendService: n("app")})
	op, err := l.bss.Patch(ctx, &computepb.PatchBackendServiceRequest{Project: project, BackendService: n("app"), BackendServiceResource: &computepb.BackendService{
		Fingerprint: bs.Fingerprint, Iap: &computepb.BackendServiceIAP{Enabled: proto.Bool(true)}}})
	l.do("enabling IAP", op, err)
	t.Cleanup(func() {
		op, err := l.bss.Patch(context.Background(), &computepb.PatchBackendServiceRequest{Project: project, BackendService: n("app"),
			BackendServiceResource: &computepb.BackendService{Iap: &computepb.BackendServiceIAP{Enabled: proto.Bool(false)}}})
		if err == nil {
			_ = op.Wait(context.Background())
		}
	})
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: iapLocalOnly, Timeout: 30 * time.Second}

	// AJAX: 401.
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, l.base+"/", nil)
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 || resp.Header.Get("X-Goog-IAP-Generated-Response") == "" {
		t.Fatalf("AJAX without a session: %d %v", resp.StatusCode, resp.Header)
	}
	// The browser's first request: the redirect to the sign-in.
	noFollow := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err = noFollow.Get(l.base + "/?gcp-iap-mode=IDENTITY")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != 302 || loc.Path != "/o/oauth2/v2/auth" || loc.Query().Get("redirect_uri") != "https://iap.googleapis.com/v1/oauth/clientIds/"+loc.Query().Get("client_id")+":handleRedirect" ||
		loc.Query().Get("response_type") != "code" || !strings.Contains(loc.Query().Get("scope"), "email") {
		t.Fatalf("the sign-in redirect: %d %s", resp.StatusCode, loc)
	}
	if err := iapLocalOnly(&http.Request{URL: loc}, nil); err != nil {
		t.Fatal(err)
	}
	// The consent page, then Allow.
	resp, err = c.Get(loc.String())
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body := string(page)
	i := strings.Index(body, `name="nonce" value="`)
	if resp.StatusCode != 200 || i < 0 {
		t.Fatalf("the sign-in page: %d %s", resp.StatusCode, body)
	}
	i += len(`name="nonce" value="`)
	nonce := body[i : i+strings.Index(body[i:], `"`)]
	action := resp.Request.URL
	if j := strings.Index(body, `<form method="post" action="`); j >= 0 {
		j += len(`<form method="post" action="`)
		if a, err := action.Parse(html.UnescapeString(body[j : j+strings.Index(body[j:], `"`)])); err == nil {
			action = a
		}
	}
	resp, err = c.PostForm(action.String(), url.Values{"nonce": {nonce}, "decision": {"allow"}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var id map[string]string
	if err := json.Unmarshal(b, &id); err != nil || resp.Request.URL.Host != strings.TrimPrefix(l.base, "http://") || !strings.HasPrefix(id["email"], "accounts.google.com:") {
		t.Fatalf("after Allow: %d %s at %s", resp.StatusCode, b, resp.Request.URL)
	}
	email := strings.TrimPrefix(id["email"], "accounts.google.com:")
	// Signed in, but without the role: 403.
	resp, err = c.Get(l.base + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("signed in without the role: %d", resp.StatusCode)
	}
	admin, err := iap.NewIdentityAwareProxyAdminRESTClient(ctx, computeOpts(t, h)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	member := "user:" + email
	if strings.HasSuffix(email, ".gserviceaccount.com") {
		member = "serviceAccount:" + email
	}
	res := "projects/" + project + "/iap_web/compute/services/" + n("app")
	if _, err := admin.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/iap.httpsResourceAccessor", Members: []string{member}}}}}); err != nil {
		t.Fatalf("SetIamPolicy: %v", err)
	}
	resp, err = c.Get(l.base + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if got, _ := seen.Load("email"); resp.StatusCode != 200 || string(b) != "ok" || got != "accounts.google.com:"+email {
		t.Fatalf("signed in with the role: %d %q, the origin saw %v", resp.StatusCode, b, got)
	}
	// CLEAR_LOGIN_COOKIE: signed out.
	resp, err = noFollow.Get(l.base + "/?gcp-iap-mode=CLEAR_LOGIN_COOKIE")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	resp, err = noFollow.Get(l.base + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 302 || !strings.Contains(resp.Header.Get("Location"), "/o/oauth2/v2/auth") {
		t.Fatalf("after CLEAR_LOGIN_COOKIE: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// syncBuf is a process's output, read while it runs.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) add(v string) {
	s.mu.Lock()
	s.b.WriteString(v)
	s.mu.Unlock()
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// freePort is a port nothing listens on now.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// covers: google.cloud.iap.v1.IdentityAwareProxyAdminService/SetIamPolicy, compute.firewalls.insert
//
// TestGcloudStartIAPTunnel (#2160), as iap/docs/using-tcp-forwarding
// tunnels: the pinned gcloud through gcloud-setup, egress to Google
// blocked, its tunnel endpoint given with --iap-tunnel-url-override (the
// Compute Engine port): an instance running a TCP server on port 8080, a
// firewall rule allowing 35.235.240.0/20 on 22 and 8080; without
// IAP-Secured Tunnel User, `gcloud compute start-iap-tunnel` is refused
// with 4033; with it (granted on the project's iap_tunnel through the IAP
// Go client), the tunnel's local port reaches the instance's server, and
// `gcloud compute ssh --tunnel-through-iap` runs a command on it.
func TestGcloudStartIAPTunnel(t *testing.T) {
	h := New(t)
	opts := computeOpts(t, h) // skips without CLOUDBURROW_TEST_COMPUTE
	addr := h.Endpoint(EnvCompute)
	gcloud := gcloudBinary(t)
	cli := os.Getenv(EnvCLI)
	if cli == "" {
		t.Skipf("%s is not set", EnvCLI)
	}
	gdir, home := t.TempDir(), t.TempDir()
	name := gcloudSetup(t, cli, gdir)
	env := append(gcloudTargetEnv(noGoogleEgress()), "CLOUDSDK_CONFIG="+gdir, "CLOUDSDK_ACTIVE_CONFIG_NAME="+name, "HOME="+home)
	override := "--iap-tunnel-url-override=ws://" + addr
	run := func(args ...string) (string, error) {
		if !hasStr(args, "--project") {
			args = append(args, "--project", h.Project())
		}
		cmd := exec.Command(gcloud, append(args, "--quiet")...)
		cmd.Env = env
		b, err := cmd.CombinedOutput()
		return string(b), err
	}
	gc := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("gcloud %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	const zone = "us-central1-a"
	sfx := fmt.Sprintf("%d", time.Now().UnixNano()%1e6)
	vm, fw := "iap-vm-"+sfx, "allow-ingress-from-iap-"+sfx
	t.Cleanup(func() {
		_, _ = run("compute", "instances", "delete", vm, "--zone", zone)
		_, _ = run("compute", "firewall-rules", "delete", fw)
	})
	server := `nohup perl -MIO::Socket::INET -e '$s=IO::Socket::INET->new(LocalPort=>8080,Listen=>5,ReuseAddr=>1) or die $!; while($c=$s->accept){print $c "IAP-TUNNEL-OK\n"; close $c}' >/dev/null 2>&1 &`
	gc("compute", "instances", "create", vm, "--zone", zone, "--machine-type", "e2-small",
		"--image-family", "debian-12", "--image-project", "debian-cloud",
		"--metadata", "startup-script=#!/bin/bash\n"+server+"\necho IAP-SERVER-STARTED")
	gc("compute", "firewall-rules", "create", fw, "--network", "default", "--direction", "INGRESS",
		"--source-ranges", "35.235.240.0/20", "--allow", "tcp:22,tcp:8080")
	deadline := time.Now().Add(3 * time.Minute)
	for {
		out, _ := run("compute", "instances", "get-serial-port-output", vm, "--zone", zone)
		if strings.Contains(out, "IAP-SERVER-STARTED") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the startup script never ran:\n%s", lastLines(out, 30))
		}
		time.Sleep(3 * time.Second)
	}

	tunnel := func(port int) (*syncBuf, chan struct{}) {
		cmd := exec.Command(gcloud, "compute", "start-iap-tunnel", vm, "8080", fmt.Sprintf("--local-host-port=localhost:%d", port),
			"--zone", zone, "--project", h.Project(), override)
		cmd.Env = env
		out := &syncBuf{}
		pr, pw := io.Pipe()
		cmd.Stdout, cmd.Stderr = pw, pw
		listening := make(chan struct{})
		go func() {
			sc := bufio.NewScanner(pr)
			once := sync.Once{}
			for sc.Scan() {
				out.add(sc.Text() + "\n")
				if strings.Contains(sc.Text(), "Listening on port") {
					once.Do(func() { close(listening) })
				}
			}
		}()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go func() { _ = cmd.Wait(); pw.Close() }()
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return out, listening
	}

	// Without the role: refused with 4033.
	out, listening := tunnel(freePort(t))
	refused := time.Now().Add(90 * time.Second)
	for !strings.Contains(out.String(), "4033") {
		select {
		case <-listening:
			t.Fatalf("the tunnel listened without IAP-Secured Tunnel User:\n%s", out.String())
		default:
		}
		if time.Now().After(refused) {
			t.Fatalf("start-iap-tunnel was not refused with 4033:\n%s", out.String())
		}
		time.Sleep(300 * time.Millisecond)
	}

	account := strings.TrimSpace(gc("config", "get-value", "account"))
	member := "user:" + account
	if strings.HasSuffix(account, ".gserviceaccount.com") {
		member = "serviceAccount:" + account
	}
	ctx := context.Background()
	admin, err := iap.NewIdentityAwareProxyAdminRESTClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	res := "projects/" + h.Project() + "/iap_tunnel"
	if _, err := admin.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: &iampb.Policy{
		Bindings: []*iampb.Binding{{Role: "roles/iap.tunnelResourceAccessor", Members: []string{member}}}}}); err != nil {
		t.Fatalf("SetIamPolicy: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: res, Policy: &iampb.Policy{}})
	})

	port := freePort(t)
	out, listening = tunnel(port)
	select {
	case <-listening:
	case <-time.After(2 * time.Minute):
		t.Fatalf("start-iap-tunnel never listened:\n%s", out.String())
	}
	var line string
	deadline = time.Now().Add(2 * time.Minute)
	for {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
		if err == nil {
			_ = c.SetDeadline(time.Now().Add(20 * time.Second))
			line, _ = bufio.NewReader(c).ReadString('\n')
			c.Close()
			if strings.TrimSpace(line) == "IAP-TUNNEL-OK" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("through the tunnel: %q %v\n%s", line, err, out.String())
		}
		time.Sleep(2 * time.Second)
	}

	// gcloud compute ssh through IAP, port 22.
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Log("no ssh client on PATH: gcloud compute ssh --tunnel-through-iap not run")
		return
	}
	key := filepath.Join(home, "gce-key")
	deadline = time.Now().Add(3 * time.Minute)
	for {
		o, err := run("compute", "ssh", vm, "--zone", zone, "--tunnel-through-iap", override, "--ssh-key-file", key,
			"--strict-host-key-checking=no", "--ssh-flag=-o ConnectTimeout=20", "--command", "echo IAP-SSH-OK")
		if err == nil && strings.Contains(o, "IAP-SSH-OK") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("gcloud compute ssh --tunnel-through-iap: %v\n%s", err, lastLines(o, 30))
		}
		time.Sleep(5 * time.Second)
	}
}
