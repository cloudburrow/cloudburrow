package iameval

import (
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/iamcheck"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
)

// #1998: a check names a service agent of a project, which the host
// numbers, and reads a service account's policy the host keeps.
func TestCheckRemoteAgentAndHostPolicies(t *testing.T) {
	agent := ServiceAgent("p", "gcp-sa-pubsub.iam.gserviceaccount.com")
	res := "projects/p/serviceAccounts/" + sa
	src := fakeSource{policies: map[string]*iampolicy.Stored{
		res: pol(iampolicy.Binding{Role: "roles/iam.serviceAccountTokenCreator", Members: []string{"serviceAccount:" + agent}}),
	}}
	e := &Enforcer{Eval: &Evaluator{Source: src}, OwnerEmail: "owner@x.iam.gserviceaccount.com"}
	perm := "iam.serviceAccounts.getAccessToken"
	in := iamcheck.Request{AgentDomain: "gcp-sa-pubsub.iam.gserviceaccount.com", AgentProject: "p", Service: "iam.googleapis.com",
		Permissions: []string{perm}, Resource: res, Type: "iam.googleapis.com/ServiceAccount", Project: "p"}
	if d := e.CheckRemote(in); d.Holds(perm) || d.Owner || d.Email != agent {
		t.Fatalf("without the account's policy: %+v; want not held, as %s", d, agent)
	}
	in.HostPolicies = []string{res}
	if d := e.CheckRemote(in); !d.Holds(perm) {
		t.Errorf("with the account's policy: %+v; want held", d)
	}
	in.AgentProject = "q"
	if d := e.CheckRemote(in); d.Holds(perm) {
		t.Errorf("another project's agent holds it: %+v", d)
	}
}
