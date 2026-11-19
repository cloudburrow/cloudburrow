package main

import (
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/service/serviceusage"
)

// Every API CloudBurrow serves has its generateServiceIdentity answer
// looked up in Google's service agent list (#1834): a newly served API
// must be added to serviceusage's serviceAgents or noPrimaryAgent.
func TestEveryServedAPIHasAServiceIdentityAnswer(t *testing.T) {
	for _, a := range serviceUsageAPIs {
		if !serviceusage.IdentityLookedUp(a.api.Name) {
			t.Errorf("%s: no generateServiceIdentity answer; look its primary service agent up in https://cloud.google.com/iam/docs/service-agents", a.api.Name)
		}
	}
}
