package run

import (
	"context"
	"html/template"
	"net/http"
	"net/url"
	"strings"

	dcpb "cloud.google.com/go/developerconnect/apiv1/developerconnectpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// An account connector's browser pages (#2105): its oauth_start_uri, which
// starts the flow for whoever opens it and sends the browser to the
// authorization server's consent page, and the OAuth client's redirect URI,
// which exchanges the code and records the user, as Google's
// developerconnect.google.com redirect finishes the flow its console
// opened. A browser carries no credentials here, so the user is the
// instance's own account, whom the consent page signs in.

var dcOAuthPage = template.Must(template.New("dc").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} - Developer Connect</title>
<style>
body{font-family:system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;margin:0;background:#f1f3f4;color:#202124}
main{max-width:30rem;margin:3rem auto;background:#fff;border:1px solid #dadce0;border-radius:8px;padding:2rem}
h1{font-size:1.5rem;font-weight:400;margin:0 0 1rem}
p{line-height:1.5}
.err{color:#a50e0e}
a{color:#1a73e8}
a:focus-visible{outline:3px solid #174ea6;outline-offset:2px}
</style></head><body><main>
<h1 id="dc-oauth-title">{{.Title}}</h1>
<p{{if .Err}} class="err"{{end}} id="dc-oauth-message">{{.Message}}</p>
{{if .Back}}<p><a id="dc-oauth-back" href="{{.Back}}">Back to the account connector</a></p>{{end}}
</main></body></html>`))

type dcPageData struct {
	Title, Message, Back string
	Err                  bool
}

func dcPage(w http.ResponseWriter, code int, d dcPageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = dcOAuthPage.Execute(w, d)
}

// dcConsoleLink is the console's page of a connector, or "".
func dcConsoleLink(connector string) string {
	if BuildLogURLBase == "" || connector == "" {
		return ""
	}
	parts := strings.Split(connector, "/")
	if len(parts) != 6 {
		return ""
	}
	return BuildLogURLBase + "/developer-connect/account-connectors/" + url.PathEscape(parts[5]) +
		"?" + url.Values{"project": {parts[1]}}.Encode()
}

// OAuthHandlerOr serves an account connector's oauth_start_uri and the
// OAuth redirect URI, and passes every other request to next.
func (d *DeveloperConnectServer) OAuthHandlerOr(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case DeveloperConnectOAuthStartPath:
			d.serveStart(w, r)
		case DeveloperConnectOAuthRedirectPath:
			d.serveRedirect(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func (d *DeveloperConnectServer) serveStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	connector := r.URL.Query().Get("accountConnector")
	resp, err := d.StartOAuth(context.WithoutCancel(r.Context()), &dcpb.StartOAuthRequest{AccountConnector: connector})
	if err != nil {
		e := apierror.From(err)
		dcPage(w, e.HTTPStatus(), dcPageData{Title: "Cannot connect your account", Message: e.Message, Back: dcConsoleLink(connector), Err: true})
		return
	}
	http.Redirect(w, r, resp.GetAuthUri(), http.StatusFound)
}

func (d *DeveloperConnectServer) serveRedirect(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ticket := q.Get("state")
	dcTickets.mu.Lock()
	t := dcTickets.m[ticket]
	dcTickets.mu.Unlock()
	if t == nil {
		dcPage(w, http.StatusBadRequest, dcPageData{Title: "Cannot connect your account",
			Message: "This sign-in is unknown or expired. Start again from the account connector.", Err: true})
		return
	}
	back := dcConsoleLink(t.connector)
	if e := q.Get("error"); e != "" {
		msg := "Access was not allowed, so no account was connected."
		if e != "access_denied" {
			msg = "The authorization server answered " + e + ": " + q.Get("error_description")
		}
		dcPage(w, http.StatusOK, dcPageData{Title: "Account not connected", Message: msg, Back: back, Err: true})
		return
	}
	resp, err := finishTicket(t.connector, ticket, q.Get("code"), t.caller, false)
	switch {
	case err != nil:
		e := apierror.From(err)
		dcPage(w, e.HTTPStatus(), dcPageData{Title: "Cannot connect your account", Message: e.Message, Back: back, Err: true})
	case resp.GetExchangeError() != nil:
		dcPage(w, http.StatusBadRequest, dcPageData{Title: "Cannot connect your account",
			Message: resp.GetExchangeError().GetCode() + ": " + resp.GetExchangeError().GetDescription(), Back: back, Err: true})
	default:
		dcPage(w, http.StatusOK, dcPageData{Title: "Account connected",
			Message: "Your account is connected to account connector " + t.connector + ". You can close this window.", Back: back})
	}
}
