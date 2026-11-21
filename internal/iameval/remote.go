package iameval

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/cloudburrow/cloudburrow/internal/iamcheck"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// ServiceAgent is the email of the service agent of project whose domain
// is domain: service-PROJECT_NUMBER@DOMAIN, the project's number the
// registry's (resource.ProjectNumber).
func ServiceAgent(project, domain string) string {
	return "service-" + strconv.FormatInt(resource.ProjectNumber(project), 10) + "@" + domain
}

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
	case in.AgentDomain != "" && in.AgentProject != "":
		// A service agent of the project (#1998): never the owner.
		c = Caller{Email: ServiceAgent(in.AgentProject, in.AgentDomain)}
	case in.Signer != "":
		// The server verified the signature; the signer is checked.
		c = Caller{Email: strings.ToLower(in.Signer)}
		if e.OwnerEmail != "" && strings.EqualFold(in.Signer, e.OwnerEmail) {
			c.Owner = true
		}
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
	for _, r := range in.HostPolicies {
		var pol *iampolicy.Stored
		if e.Eval.Source != nil {
			pol = e.Eval.Source.Policy(r)
		}
		req.Own = append(req.Own, Attached{Resource: "//iam.googleapis.com/" + r, Policy: pol})
	}
	if in.ActAs != "" {
		// A service account the server's request attaches (a Pub/Sub
		// push subscription's OIDC token): its own policy is IAM's,
		// which this host keeps.
		resource := ServiceAccountResource(project, in.ActAs)
		req = e.Request(context.Background(), IAM, email, ActAs, resource)
		in.Permissions = []string{ActAs}
		project = req.Project
	}
	if project != "" {
		out.Teams = e.Eval.ProjectTeams(project, email, req.Time)
	}
	for _, p := range in.Permissions {
		req.Permission = p
		if c.Owner && !e.Eval.Denies(req) || !c.Owner && e.Eval.Check(req).Granted {
			out.Held = append(out.Held, p)
		}
	}
	return out
}
