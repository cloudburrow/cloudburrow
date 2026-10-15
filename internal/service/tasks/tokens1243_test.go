package tasks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/sched"
)

// fakeMinter records what it was asked for and returns a token naming it.
type fakeMinter struct{ asked []string }

func (f *fakeMinter) ServiceAccountIDToken(account, audience string) (string, error) {
	f.asked = append(f.asked, "id "+account+" "+audience)
	return "id-token", nil
}

func (f *fakeMinter) ServiceAccountAccessToken(account, scope string) (string, error) {
	f.asked = append(f.asked, "access "+account+" "+scope)
	return "access-token", nil
}

// A task with oidcToken or oauthToken is dispatched with Authorization:
// Bearer and the minted token, replacing any it set itself; the audience
// defaults to the URL and the scope to cloud-platform (#1243).
func TestDispatchMintsTheTaskToken(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	mustQueue(t, s, queueA)
	got := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("Authorization")
	}))
	defer srv.Close()
	m := &fakeMinter{}
	d := NewDispatcher(s, srv.Client(), sched.NewFakeClock(time.Now())).WithTokens(m)

	for _, c := range []struct {
		id    string
		hr    HTTPRequest
		want  string
		asked string
	}{
		{"oidc", HTTPRequest{URL: srv.URL + "/a", OIDCToken: &OIDCToken{ServiceAccountEmail: "sa@p"}}, "Bearer id-token", "id sa@p " + srv.URL + "/a"},
		{"aud", HTTPRequest{URL: srv.URL, Headers: map[string]string{"Authorization": "mine"},
			OIDCToken: &OIDCToken{ServiceAccountEmail: "sa@p", Audience: "https://svc"}}, "Bearer id-token", "id sa@p https://svc"},
		{"oauth", HTTPRequest{URL: srv.URL, OAuthToken: &OAuthToken{ServiceAccountEmail: "sa@p"}}, "Bearer access-token", "access sa@p " + DefaultOAuthScope},
		{"none", HTTPRequest{URL: srv.URL}, "", ""},
	} {
		m.asked = nil
		hr := c.hr
		task := Task{Name: TaskName(queueA, c.id), HTTPRequest: &hr}
		if _, err := s.CreateTask(task); err != nil {
			t.Fatal(err)
		}
		if err := d.Dispatch(context.Background(), task); err != nil {
			t.Fatalf("%s: %v", c.id, err)
		}
		if h := <-got; h != c.want {
			t.Errorf("%s: Authorization = %q, want %q", c.id, h, c.want)
		}
		if c.asked != "" && (len(m.asked) != 1 || m.asked[0] != c.asked) {
			t.Errorf("%s: minted %v, want %q", c.id, m.asked, c.asked)
		}
	}
}

// Without a minter the attempt fails, saying why, rather than going out
// without the header the target expects.
func TestDispatchWithoutMinterFails(t *testing.T) {
	t.Parallel()
	s := newStore(t)
	mustQueue(t, s, queueA)
	d := NewDispatcher(s, nil, sched.NewFakeClock(time.Now()))
	task := Task{Name: TaskName(queueA, "t"), HTTPRequest: &HTTPRequest{URL: "http://127.0.0.1:1/", OIDCToken: &OIDCToken{ServiceAccountEmail: "sa@p"}}}
	if _, err := s.CreateTask(task); err != nil {
		t.Fatal(err)
	}
	if err := d.Dispatch(context.Background(), task); err == nil {
		t.Error("dispatched a token task with no minter")
	}
}
