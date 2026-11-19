package sqladmin

// Stopping and starting an instance (#1875): settings.activationPolicy.
// "Start: gcloud sql instances patch INSTANCE_NAME --activation-policy=ALWAYS;
// Stop: --activation-policy=NEVER" (docs.cloud.google.com/sql/docs/mysql/
// start-stop-restart-instance). A stopped instance "remains stopped, and
// does not respond to application connections, until you start it again";
// its storage is kept. The API keeps reporting it RUNNABLE ("The instance
// is running, or has been stopped by owner"), with activationPolicy NEVER,
// which the console and gcloud show as Stopped. The change is an UPDATE
// operation, as every instances.patch is. ON_DEMAND is deprecated (it was
// First Generation's), and gcloud offers only always and never. "NEVER is
// not supported for read replica instances."
//
// Here the instance's server, its Deployment (and a REGIONAL instance's
// standby), is scaled to no pods, its volume kept, and its host tunnel
// stopped; starting it applies the server again, with its data.

import (
	"net/http"
	"strings"
)

// Stopped reports whether the instance's owner stopped it.
func (s Spec) Stopped() bool { return s.ActivationPolicy == "NEVER" }

// notRunningErr is Cloud SQL's answer to a request a stopped instance's
// server would have to serve.
func notRunningErr() error {
	return Errorf(http.StatusBadRequest, "Invalid request: Invalid request since instance is not running.")
}

// checkActivationPolicy is what Cloud SQL accepts in settings.activationPolicy.
func checkActivationPolicy(p string) error {
	switch p {
	case "", "ALWAYS", "NEVER":
		return nil
	case "ON_DEMAND":
		return Errorf(http.StatusBadRequest, "Invalid request: activationPolicy ON_DEMAND is deprecated and is not "+
			"supported for Second Generation instances; use ALWAYS or NEVER.")
	}
	return Errorf(http.StatusBadRequest, "Invalid request: Invalid value for activationPolicy: %s.", p)
}

// replicaNeverErr is the refusal of NEVER on a read replica.
func replicaNeverErr() error {
	return Errorf(http.StatusBadRequest, "Invalid request: NEVER is not supported for read replica instances: "+
		"you cannot stop a read replica instance.")
}

// stoppedServes is whether a request beneath a stopped instance is served:
// what reads or changes the instance's record, its certificates and
// backups' records; not what its server would have to answer.
func stoppedServes(method string, rest []string, verb string) bool {
	if verb != "" {
		return false
	}
	if len(rest) == 2 {
		return true // get, patch, update, delete
	}
	switch rest[2] {
	case "backupRuns":
		return method != http.MethodPost
	case "sslCerts":
		return method == http.MethodGet || method == http.MethodDelete
	case "listServerCertificates", "listServerCas", "connectSettings", "getLatestRecoveryTime",
		"getDiskShrinkConfig", "rescheduleMaintenance", "addServerCa", "addServerCertificate":
		return true
	}
	return false
}

// stoppedRefusal answers a request beneath a stopped created instance.
func (s *Server) stoppedRefusal(c *created, method string, rest []string, verb string) error {
	if c == nil {
		return nil
	}
	s.emu.RLock()
	stopped := c.spec.Stopped() && c.state == "RUNNABLE"
	s.emu.RUnlock()
	if !stopped || stoppedServes(method, rest, strings.TrimSpace(verb)) {
		return nil
	}
	return notRunningErr()
}
