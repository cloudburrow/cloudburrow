package main

import (
	"errors"

	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
)

// wireMemberRewrites passes a deleted service account's binding rewrites
// (#1751) to the services that keep IAM policies on their own resources in
// this process: Cloud Tasks queues, Secret Manager secrets and Cloud KMS
// key rings, keys and EKM resources. Google shows the account as
// deleted:serviceAccount:EMAIL?uid=ID in every allow policy that names
// it, turns it back on undelete and drops it on purge. Each service is
// read when the hook runs, since a service starts after the registry
// opens.
func wireMemberRewrites(reg *resourcemanager.Registry, tasksSvc *tasksService, secretsSvc *secretsService, kmsSvc *kmsService) {
	reg.OnMemberRewrite(func(from, to string) error {
		var errs []error
		if st := tasksSvc.Store(); st != nil {
			errs = append(errs, st.RewriteIAMMember(from, to))
		}
		if secretsSvc != nil && secretsSvc.store != nil {
			errs = append(errs, secretsSvc.store.RewriteIAMMember(from, to))
		}
		if api := kmsSvc.API(); api != nil {
			errs = append(errs, api.RewriteIAMMember(from, to))
		}
		return errors.Join(errs...)
	})
}
