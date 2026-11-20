package main

import (
	"context"

	"github.com/cloudburrow/cloudburrow/internal/cmek"
	"github.com/cloudburrow/cloudburrow/internal/iamcheck"
	"github.com/cloudburrow/cloudburrow/internal/iameval"
)

// cmekKeys is the customer-managed keys of the host's in-process services
// (#1950: Cloud Logging, Document AI, Speech-to-Text): the instance's Cloud
// KMS, and, with `up --iam-enforcement=on`, the host's evaluator checking
// each service agent's use of a key.
func cmekKeys(kmsSvc *kmsService, e *iameval.Enforcer) cmek.Keys {
	k := cmek.Keys{KMS: func() cmek.KMS {
		if api := kmsSvc.API(); api != nil {
			return cmek.InProcess(api)
		}
		return nil
	}}
	if e != nil && e.On != nil && e.On() {
		k.Check = func(_ context.Context, r iamcheck.Request) (iamcheck.Decision, error) { return e.CheckRemote(r), nil }
	}
	return k
}
