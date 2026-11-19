package main

import (
	"context"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/metadata"
)

// Cloud Run invoke (run.routes.invoke, #1879). With `up
// --iam-enforcement=on` Kourier's gateway asks this handler, through its
// external authorization (components.ConfigureInvokeAuthz), before routing
// any request to a service, and the host's one evaluator decides: Google's
// "Authenticating service-to-service" and troubleshooting pages
// (https://docs.cloud.google.com/run/docs/authenticating/service-to-service,
// https://docs.cloud.google.com/run/docs/troubleshooting, read 2026-11-19)
// require an ID token, whose principal holds run.routes.invoke on the
// service (roles/run.invoker), unless allUsers does. So:
//
//   - no Authorization header is allUsers alone: 403 "Your client does not
//     have permission to get URL <path> from this server." unless allUsers
//     holds the role (gcloud's --allow-unauthenticated);
//   - a token that is not an ID token this instance signed, has expired,
//     or whose audience is not the service's URL is 401 (an access token
//     too, as Google refuses one);
//   - an ID token of a principal without the permission is the 403 page;
//   - the instance's own account's ID token is the owner.
//
// A request for a host that names no Cloud Run service is let through.

// runInvokeAuthz answers Kourier's authorization requests.
type runInvokeAuthz struct {
	e     *iameval.Enforcer
	creds *metadata.Credentials
	run   *runService
}

func isRunInvokeAuthz(r *http.Request) bool {
	return r.URL.Path == components.InvokeAuthzPath || strings.HasPrefix(r.URL.Path, components.InvokeAuthzPath+"/")
}

const runForbiddenPage = `<html><head>
<meta http-equiv="content-type" content="text/html;charset=utf-8">
<title>403 Forbidden</title>
</head>
<body text=#000000 bgcolor=#ffffff>
<h1>Error: Forbidden</h1>
<h2>Your client does not have permission to get URL <code>%s</code> from this server.</h2>
<h2></h2>
</body></html>
`

const runUnauthorizedPage = `<html><head>
<meta http-equiv="content-type" content="text/html;charset=utf-8">
<title>401 Unauthorized</title>
</head>
<body text=#000000 bgcolor=#ffffff>
<h1>Error: Unauthorized</h1>
<h2>Your client does not have permission to the requested URL <code>%s</code>.</h2>
<h2></h2>
</body></html>
`

func (a runInvokeAuthz) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, components.InvokeAuthzPath)
	if path == "" {
		path = "/"
	}
	if a.run == nil || a.e == nil || a.e.On == nil || !a.e.On() {
		w.WriteHeader(http.StatusOK)
		return
	}
	srv := a.run.adapter.Load()
	if srv == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	name, policy, ok := srv.InvokeTarget(r.Context(), r.Host)
	if !ok {
		w.WriteHeader(http.StatusOK)
		return
	}
	email := ""
	if bearer := iameval.Bearer(r.Header.Get("Authorization")); bearer != "" {
		e, aud, ok := a.creds.IDTokenClaims(bearer, time.Now())
		if !ok || !audienceMatches(aud, r.Host) && !sameService(r.Context(), srv, aud, name) {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token" error_description="The access token could not be verified"`)
			writeRunPage(w, http.StatusUnauthorized, runUnauthorizedPage, path)
			return
		}
		if strings.EqualFold(e, a.creds.Email) {
			w.WriteHeader(http.StatusOK)
			return
		}
		email = strings.ToLower(e)
	}
	req := iameval.Request{Email: email, Permission: "run.routes.invoke", Resource: name, Type: "run.googleapis.com/Service",
		Service: "run.googleapis.com", Project: iameval.Project(name), Time: time.Now(),
		Own: []iameval.Attached{{Resource: "//run.googleapis.com/" + name, Policy: policy}}}
	if a.e.Eval.Check(req).Granted {
		w.WriteHeader(http.StatusOK)
		return
	}
	writeRunPage(w, http.StatusForbidden, runForbiddenPage, path)
}

func writeRunPage(w http.ResponseWriter, code int, page, path string) {
	w.Header().Set("Content-Type", "text/html; charset=UTF-8")
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, page, html.EscapeString(path))
}

// sameService is whether an ID token's audience is a URL of the service
// name (its own or a tag's): a request through a domain mapping reaches
// the gateway with the domain's Host and again with the service's, and
// Google takes the service's URL as the audience there.
func sameService(ctx context.Context, srv interface {
	ServiceAt(context.Context, string) string
}, aud, name string) bool {
	u, err := url.Parse(aud)
	if err != nil || u.Hostname() == "" {
		return false
	}
	return srv.ServiceAt(ctx, u.Hostname()) == name
}

// audienceMatches is whether an ID token's audience is the URL of the
// service a request is for: its host, whatever the scheme and port (the
// service's URL, or a tag's).
func audienceMatches(aud, host string) bool {
	u, err := url.Parse(aud)
	if err != nil || u.Hostname() == "" {
		return false
	}
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	return strings.EqualFold(u.Hostname(), h)
}
