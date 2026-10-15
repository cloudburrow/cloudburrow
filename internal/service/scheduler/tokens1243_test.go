package scheduler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"cloud.google.com/go/scheduler/apiv1/schedulerpb"
)

type fakeMinter struct{ asked chan string }

func (f fakeMinter) ServiceAccountIDToken(account, audience string) (string, error) {
	f.asked <- "id " + account + " " + audience
	return "id-token", nil
}

func (f fakeMinter) ServiceAccountAccessToken(account, scope string) (string, error) {
	f.asked <- "access " + account + " " + scope
	return "access-token", nil
}

// A job's oidcToken or oauthToken is kept, returned, and minted into
// Authorization: Bearer at each attempt; the audience defaults to the URI
// and the scope to cloud-platform (#1243).
func TestJobTokensAreKeptAndSent(t *testing.T) {
	c, runner, _, _ := start(t)
	m := fakeMinter{asked: make(chan string, 4)}
	runner.WithTokens(m)
	ctx := context.Background()
	got := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("Authorization")
	}))
	defer srv.Close()

	oidc := httpJob("oidc", srv.URL+"/run")
	oidc.GetHttpTarget().AuthorizationHeader = &schedulerpb.HttpTarget_OidcToken{OidcToken: &schedulerpb.OidcToken{ServiceAccountEmail: "sa@p"}}
	oauth := httpJob("oauth", srv.URL)
	oauth.GetHttpTarget().AuthorizationHeader = &schedulerpb.HttpTarget_OauthToken{OauthToken: &schedulerpb.OAuthToken{ServiceAccountEmail: "sa@p", Scope: "s"}}
	for _, c2 := range []struct {
		job          *schedulerpb.Job
		header, mint string
	}{
		{oidc, "Bearer id-token", "id sa@p " + srv.URL + "/run"},
		{oauth, "Bearer access-token", "access sa@p s"},
	} {
		j, err := c.CreateJob(ctx, &schedulerpb.CreateJobRequest{Parent: parent, Job: c2.job})
		if err != nil {
			t.Fatal(err)
		}
		back, err := c.GetJob(ctx, &schedulerpb.GetJobRequest{Name: j.GetName()})
		if err != nil {
			t.Fatal(err)
		}
		if back.GetHttpTarget().GetOidcToken().GetServiceAccountEmail()+back.GetHttpTarget().GetOauthToken().GetServiceAccountEmail() != "sa@p" {
			t.Errorf("%s read back without its token: %v", j.GetName(), back.GetHttpTarget())
		}
		if _, err := c.RunJob(ctx, &schedulerpb.RunJobRequest{Name: j.GetName()}); err != nil {
			t.Fatal(err)
		}
		if h := <-got; h != c2.header {
			t.Errorf("%s: Authorization = %q, want %q", j.GetName(), h, c2.header)
		}
		if a := <-m.asked; a != c2.mint {
			t.Errorf("%s: minted %q, want %q", j.GetName(), a, c2.mint)
		}
	}
}
