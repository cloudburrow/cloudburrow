package pubsubfront

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
)

// restUpstream stands in for the emulator's REST API, which the in-memory
// Pub/Sub does not serve: it records every request and answers 200 with the
// body it was sent, or with the answer set for its method and path.
type restUpstream struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []restCall
	// answers is the body for "METHOD path", when set.
	answers map[string]string
}

type restCall struct {
	method, path string
	body         []byte
}

func newRESTUpstream(t *testing.T) *restUpstream {
	t.Helper()
	u := &restUpstream{answers: map[string]string{}}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.seen = append(u.seen, restCall{r.Method, r.URL.Path, b})
		if a, ok := u.answers[r.Method+" "+r.URL.Path]; ok {
			b = []byte(a)
		}
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if len(b) == 0 {
			b = []byte(`{"from":"upstream"}`)
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// answer sets the body the upstream answers method and path with.
func (u *restUpstream) answer(method, path, body string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.answers[method+" "+path] = body
}

func (u *restUpstream) addr() string { return strings.TrimPrefix(u.srv.URL, "http://") }

func (u *restUpstream) calls() []restCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]restCall(nil), u.seen...)
}

func (fx *fixture) rest(t *testing.T, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, "http://"+fx.addr+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 1 {
		t.Errorf("%s %s answered over HTTP/%d", method, path, resp.ProtoMajor)
	}
	out := map[string]any{}
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

// One port serves both: an official gRPC client and HTTP/1.1 REST calls
// reach the front on the same address, and a connection that says nothing,
// as a tcpSocket readiness probe does, disturbs neither.
func TestServesGRPCAndRESTOnOnePort(t *testing.T) {
	up := newRESTUpstream(t)
	fx := newFixtureREST(t, up.addr())
	c, err := net.Dial("tcp", fx.addr)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()

	fx.topic(t, "grpc-topic") // over gRPC, to the in-memory Pub/Sub
	code, body := fx.rest(t, http.MethodGet, "/v1/projects/"+project+"/topics", "")
	if code != http.StatusOK || body["from"] != "upstream" {
		t.Errorf("GET topics over REST = %d %v, want the upstream's answer", code, body)
	}
	if got := up.calls(); len(got) != 1 || got[0].path != "/v1/projects/"+project+"/topics" {
		t.Errorf("the REST upstream saw %v", got)
	}
	// And gRPC still works after REST on the same port.
	fx.topic(t, "after-rest")
}

// A REST create is checked as a gRPC one is: a policy Google refuses never
// reaches the emulator, and a subscription with none is sent on with the
// 31-day default and every other field as it was.
func TestRESTCreateChecksThePolicy(t *testing.T) {
	up := newRESTUpstream(t)
	fx := newFixtureREST(t, up.addr())
	path := "/v1/projects/" + project + "/subscriptions/"
	for _, c := range []struct{ id, body string }{
		{"hour", `{"topic":"projects/p/topics/t","expirationPolicy":{"ttl":"3600s"}}`},
		{"below-retention", `{"topic":"projects/p/topics/t","expirationPolicy":{"ttl":"172800s"},"messageRetentionDuration":"259200s"}`},
		{"below-default-retention", `{"topic":"projects/p/topics/t","expiration_policy":{"ttl":"86400s"}}`},
	} {
		code, body := fx.rest(t, http.MethodPut, path+c.id, c.body)
		e, _ := body["error"].(map[string]any)
		if code != http.StatusBadRequest || e["status"] != "INVALID_ARGUMENT" || e["code"] != float64(400) {
			t.Errorf("%s: %d %v, want 400 INVALID_ARGUMENT", c.id, code, body)
		}
	}
	if got := up.calls(); len(got) != 0 {
		t.Fatalf("refused creates reached the emulator: %v", got)
	}

	if code, _ := fx.rest(t, http.MethodPut, path+"default", `{"topic":"projects/p/topics/t","ackDeadlineSeconds":20}`); code != http.StatusOK {
		t.Fatalf("create with no policy = %d", code)
	}
	if code, _ := fx.rest(t, http.MethodPut, path+"never", `{"topic":"projects/p/topics/t","expirationPolicy":{}}`); code != http.StatusOK {
		t.Fatalf("create with a policy without a ttl = %d", code)
	}
	got := up.calls()
	if len(got) != 2 {
		t.Fatalf("the emulator saw %v", got)
	}
	var sent map[string]any
	if err := json.Unmarshal(got[0].body, &sent); err != nil {
		t.Fatal(err)
	}
	if p, _ := sent["expirationPolicy"].(map[string]any); p["ttl"] != "2678400s" ||
		sent["topic"] != "projects/p/topics/t" || sent["ackDeadlineSeconds"] != float64(20) {
		t.Errorf("a create with no policy was sent on as %s", got[0].body)
	}
	if !bytes.Equal(got[1].body, []byte(`{"topic":"projects/p/topics/t","expirationPolicy":{}}`)) {
		t.Errorf("a create with a policy was changed to %s", got[1].body)
	}
	for _, id := range []string{"default", "never"} {
		if !fx.tracked(subName(id)) {
			t.Errorf("%s: created over REST and not tracked", id)
		}
	}
}

// A REST update is checked as a gRPC one is: a retention above the ttl is
// refused, reading the ttl from the emulator; anything else, a change of
// the policy itself included, is the emulator's to answer.
func TestRESTUpdateChecksThePolicy(t *testing.T) {
	up := newRESTUpstream(t)
	fx := newFixtureREST(t, up.addr())
	topic := fx.topic(t, "t")
	if _, err := fx.create(t, &pubsubpb.Subscription{Name: subName("s"), Topic: topic,
		ExpirationPolicy: &pubsubpb.ExpirationPolicy{Ttl: day(2)}, MessageRetentionDuration: day(1)}); err != nil {
		t.Fatal(err)
	}
	path := "/v1/projects/" + project + "/subscriptions/s"
	for _, c := range []struct {
		body string
		ok   bool
	}{
		{`{"subscription":{"messageRetentionDuration":"259200s"},"updateMask":"messageRetentionDuration"}`, false},
		{`{"subscription":{"messageRetentionDuration":"172800s"},"updateMask":"messageRetentionDuration"}`, true},
		{`{"subscription":{"expirationPolicy":{"ttl":"3600s"}},"updateMask":"expirationPolicy"}`, true},
		{`{"subscription":{"ackDeadlineSeconds":30},"updateMask":"ackDeadlineSeconds"}`, true},
	} {
		code, body := fx.rest(t, http.MethodPatch, path, c.body)
		if c.ok != (code == http.StatusOK) {
			t.Errorf("PATCH %s = %d %v, want ok %v", c.body, code, body, c.ok)
		}
	}
	if n := len(up.calls()); n != 3 {
		t.Errorf("the emulator saw %d updates, want the 3 accepted", n)
	}
}

// Every REST call naming a subscription is activity on it, a REST delete
// forgets it, and one nothing touches is still expired.
func TestRESTCallsAreActivity(t *testing.T) {
	up := newRESTUpstream(t)
	fx := newFixtureREST(t, up.addr())
	topic := fx.topic(t, "t")
	oneDay := &pubsubpb.ExpirationPolicy{Ttl: day(1)}
	ids := []string{"idle", "pull", "ack", "modack", "get", "seek", "snapshot"}
	for _, id := range ids {
		if _, err := fx.create(t, &pubsubpb.Subscription{Name: subName(id), Topic: topic,
			ExpirationPolicy: oneDay, MessageRetentionDuration: day(1)}); err != nil {
			t.Fatal(err)
		}
	}
	fx.advance(t, 20*time.Hour)
	base := "/v1/projects/" + project + "/subscriptions/"
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, base + "pull:pull", `{"maxMessages":1}`},
		{http.MethodPost, base + "ack:acknowledge", `{"ackIds":["x"]}`},
		{http.MethodPost, base + "modack:modifyAckDeadline", `{"ackIds":["x"],"ackDeadlineSeconds":10}`},
		{http.MethodGet, base + "get", ""},
		{http.MethodPost, base + "seek:seek", `{"time":"2026-01-01T00:00:00Z"}`},
		{http.MethodPut, "/v1/projects/" + project + "/snapshots/snap", `{"subscription":"` + subName("snapshot") + `"}`},
	} {
		if code, body := fx.rest(t, c.method, c.path, c.body); code != http.StatusOK {
			t.Errorf("%s %s = %d %v", c.method, c.path, code, body)
		}
	}
	fx.advance(t, 5*time.Hour) // idle for 25h; the rest for 5h
	got := fx.listed(t)
	for _, id := range ids {
		if want := id != "idle"; got[subName(id)] != want {
			t.Errorf("after 25h, %s listed = %v, want %v", id, got[subName(id)], want)
		}
	}

	if code, _ := fx.rest(t, http.MethodDelete, base+"get", ""); code != http.StatusOK {
		t.Fatalf("DELETE = %d", code)
	}
	if fx.tracked(subName("get")) {
		t.Error("a subscription deleted over REST is still tracked")
	}
}

func TestRESTPath(t *testing.T) {
	for path, want := range map[string][3]string{
		"/v1/projects/p/subscriptions/s":                   {"subscriptions", "projects/p/subscriptions/s", ""},
		"/v1/projects/p/subscriptions/s:pull":              {"subscriptions", "projects/p/subscriptions/s", "pull"},
		"/v1/projects/p/subscriptions/s:modifyAckDeadline": {"subscriptions", "projects/p/subscriptions/s", "modifyAckDeadline"},
		"/v1/projects/p/snapshots/n":                       {"snapshots", "projects/p/snapshots/n", ""},
	} {
		c, n, v, ok := restPath(path)
		if !ok || [3]string{c, n, v} != want {
			t.Errorf("restPath(%q) = %q %q %q %v, want %q", path, c, n, v, ok, want)
		}
	}
	for _, path := range []string{"/v1/projects/p/subscriptions", "/v1/projects/p/topics/t", "/v1/projects/p/topics/t/subscriptions",
		"/v1/projects//subscriptions/s", "/v1/projects/p/subscriptions/:pull", "/v2/projects/p/subscriptions/s", "/"} {
		if _, _, _, ok := restPath(path); ok {
			t.Errorf("restPath(%q) names a subscription", path)
		}
	}
}

// tracked reports whether the front has a subscription's activity.
func (fx *fixture) tracked(name string) bool {
	fx.front.mu.Lock()
	defer fx.front.mu.Unlock()
	return fx.front.subs[name] != nil
}

// Split sends a connection that opens with the HTTP/2 preface one way and
// anything else the other, with its first bytes intact.
func TestSplitRoutesByPreface(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	g, h := Split(l)
	defer g.Close()
	read := func(side net.Listener, n int) chan string {
		out := make(chan string, 1)
		go func() {
			c, err := side.Accept()
			if err != nil {
				out <- "accept: " + err.Error()
				return
			}
			defer c.Close()
			b := make([]byte, n)
			_, err = io.ReadFull(c, b)
			if err != nil {
				out <- "read: " + err.Error()
				return
			}
			out <- string(b)
		}()
		return out
	}
	send := func(s string) {
		c, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		if _, err := c.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	preface := "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
	gotG := read(g, len(preface))
	send(preface)
	if got := <-gotG; got != preface {
		t.Errorf("the gRPC side read %q", got)
	}
	get := "GET /v1/projects/p/topics HTTP/1.1\r\n"
	gotH := read(h, len(get))
	send(get)
	if got := <-gotH; got != get {
		t.Errorf("the HTTP side read %q", got)
	}

	_ = h.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := g.Accept(); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Accept after Close succeeded")
		}
	case <-ctx.Done():
		t.Error("Accept after Close blocked")
	}
}
