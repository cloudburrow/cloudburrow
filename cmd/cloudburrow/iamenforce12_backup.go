package main

import (
	"net/http"

	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/service/backupdr"
)

// useIAMEnforcementBackup checks the Backup and DR Service's and the Cloud
// Storage companion APIs' JSON requests (#1879, round 12), inside any
// wrap already set (the API enablement gate's), so a disabled API is
// refused first.
func useIAMEnforcementBackup(e *iameval.Enforcer, bdr *backupdr.Server, jobs *storageJobs) {
	if e == nil || e.On == nil || !e.On() {
		return
	}
	chain := func(prev func(http.Handler) http.Handler, w func(http.Handler) http.Handler) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			h := w(next)
			if prev != nil {
				h = prev(h)
			}
			return h
		}
	}
	rules := func(svc *iameval.Service, rs []iameval.PathRule) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler { return e.HTTPRules(svc, rs, nil, next) }
	}
	if bdr != nil {
		bdr.Wrap = chain(bdr.Wrap, rules(iameval.BackupDR, iameval.BackupDRRules))
	}
	if jobs == nil {
		return
	}
	if jobs.batch != nil {
		jobs.batch.Wrap = chain(jobs.batch.Wrap, rules(iameval.StorageBatchOperations, iameval.StorageBatchOperationsRules))
	}
	if jobs.insights != nil {
		jobs.insights.Wrap = chain(jobs.insights.Wrap, rules(iameval.StorageInsights, iameval.StorageInsightsRules))
	}
	if jobs.transfer != nil {
		jobs.transfer.Wrap = chain(jobs.transfer.Wrap, func(next http.Handler) http.Handler {
			// An operation is checked on the project it runs in, as
			// the handler answers it.
			lookup := func(r *http.Request, name string) string { return iameval.AnswerProject(r, "/v1/"+name, next) }
			return e.HTTPRules(iameval.StorageTransfer, iameval.StorageTransferRules, lookup, next)
		})
	}
}
