package iameval

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/cloudburrow/cloudburrow/internal/iamcheck"
)

// ServeCheck answers the in-cluster servers' checks (internal/iamcheck,
// #1879) with this evaluator: the caller named from the bearer token this
// instance minted, the server's own policies on the resource, then the
// project's, its folders' and its organization's. It answers whatever the
// mode, as the servers only ask when `up --iam-enforcement=on` gave them
// this address.
func (e *Enforcer) ServeCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var in iamcheck.Request
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&in); err != nil {
		http.Error(w, "bad check: "+err.Error(), http.StatusBadRequest)
		return
	}
	out := e.CheckRemote(in)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// CheckRemote is ServeCheck's decision.
func (e *Enforcer) CheckRemote(in iamcheck.Request) iamcheck.Decision {
	out := iamcheck.Decision{Held: []string{}}
	var c Caller
	switch {
	case in.Bearer != "" && e.CallerOf != nil:
		c = e.CallerOf(in.Bearer)
	case in.Bearer != "":
		c = Caller{Invalid: true}
	case !in.Anonymous:
		c = Caller{Owner: true}
	}
	switch {
	case c.Invalid:
		out.Unauthenticated = true
		return out
	case c.Owner:
		out.Owner, out.Email = true, c.Email
	}
	email := e.email(c)
	if !c.Owner {
		out.Email = c.Email
		out.Members = e.Eval.Members(c.Email)
	}
	project := in.Project
	if project == "" {
		project = Project(in.Resource)
	}
	req := Request{Email: email, Resource: in.Resource, Type: in.Type, Service: in.Service, Project: project, Time: e.now()}
	for _, p := range in.Own {
		req.Own = append(req.Own, Attached{Resource: p.Resource, Policy: p.Policy})
	}
	for _, p := range in.Permissions {
		req.Permission = p
		if c.Owner && !e.Eval.Denies(req) || !c.Owner && e.Eval.Check(req).Granted {
			out.Held = append(out.Held, p)
		}
	}
	return out
}
