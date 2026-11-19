package main

import (
	"context"
	"net/http"

	"github.com/cloudburrow/cloudburrow/internal/apienable"
	"github.com/cloudburrow/cloudburrow/internal/service/apikeys"
	"github.com/cloudburrow/cloudburrow/internal/service/backupdr"
	"github.com/cloudburrow/cloudburrow/internal/service/resourcemanager"
	"github.com/cloudburrow/cloudburrow/internal/service/serviceusage"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// apiGated are the in-process services the Service Usage server's
// decision gates (#1914), each by its googleapis.com name in its own
// Start.
type apiGated struct {
	tasks       *tasksService
	run         *runService
	scheduler   *schedulerService
	memorystore *memorystoreAdminService
	logging     *loggingService
	kms         *kmsService
	secrets     *secretsService
	sqlAdmin    *sqladmin.Server
	backupDR    *backupdr.Server
}

// useAPIEnablement enforces disabled APIs as Google does (#1914): a call
// to an API disabled in its project fails PERMISSION_DENIED with
// SERVICE_DISABLED. The in-process services ask su directly; the servers
// in the cluster ask it at apienable.Path on rm's port, through
// cloudburrow-host. A nil su (no Service Usage port) gates nothing.
func useAPIEnablement(su *serviceusage.Server, rm *resourcemanager.Server, g apiGated) {
	if su == nil {
		return
	}
	rm.HandleJSON(func(r *http.Request) bool { return r.URL.Path == apienable.Path }, apienable.Handler(su))
	if g.tasks != nil {
		g.tasks.apiGate = su
	}
	if g.run != nil {
		g.run.apiGate = su
	}
	if g.scheduler != nil {
		g.scheduler.apiGate = su
	}
	if g.memorystore != nil {
		g.memorystore.apiGate = su
	}
	if g.logging != nil {
		g.logging.apiGate = su
	}
	if g.kms != nil {
		g.kms.apiGate = su
	}
	if g.secrets != nil {
		g.secrets.apiGate = su
	}
	if g.sqlAdmin != nil {
		g.sqlAdmin.Wrap = func(h http.Handler) http.Handler { return apienable.Gate(su, "sqladmin.googleapis.com", h) }
	}
	if g.backupDR != nil {
		g.backupDR.Wrap = func(h http.Handler) http.Handler { return apienable.Gate(su, "backupdr.googleapis.com", h) }
	}
}

// apiKeysChecker is the API Keys server's decision on a call that carries
// a key (#1915), as the gates ask it.
type apiKeysChecker struct{ srv *apikeys.Server }

func (c apiKeysChecker) CheckKey(_ context.Context, call apienable.KeyCall) *apienable.Refusal {
	_, d := c.srv.Authorize(call.Key, apikeys.Request{Service: call.Service, Method: call.Method, Referrer: call.Referrer,
		IP: call.IP, AndroidPackage: call.AndroidPackage, AndroidCert: call.AndroidCert, IOSBundle: call.IOSBundle})
	if d == nil {
		return nil
	}
	return &apienable.Refusal{HTTP: d.HTTP, Status: d.Status, Message: d.Message, Reason: d.Reason, Metadata: d.Metadata}
}

// useAPIKeys enforces API keys where Google does (#1915): a call that
// carries one (x-goog-api-key, or key=) to any gated API is refused when
// the key is not valid or its restrictions do not allow the call. The
// in-process gates ask srv directly; the servers in the cluster ask it at
// apienable.KeyPath on rm's port. A nil srv checks no key.
func useAPIKeys(srv *apikeys.Server, rm *resourcemanager.Server) func() {
	if srv == nil {
		return func() {}
	}
	c := apiKeysChecker{srv: srv}
	apienable.UseKeys(c)
	rm.HandleJSON(func(r *http.Request) bool { return r.URL.Path == apienable.KeyPath }, apienable.KeyHandler(c))
	return func() { apienable.UseKeys(nil) }
}
