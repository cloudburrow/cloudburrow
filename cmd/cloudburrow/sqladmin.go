package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	runadapter "github.com/cloudburrow/cloudburrow/internal/adapter/run"
	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/iameval"
	"github.com/cloudburrow/cloudburrow/internal/metadata"
	"github.com/cloudburrow/cloudburrow/internal/netfwd"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// The Cloud SQL Admin API's instance names (#1301): one per server.
const (
	sqlAdminPostgresInstance = "postgres"
	sqlAdminMySQLInstance    = "mysql"
)

// newSQLAdminServer serves the Cloud SQL Admin API over the PostgreSQL and
// MySQL servers that are enabled, or returns nil when neither is.
func newSQLAdminServer(cfg config.Config, fwds []*netfwd.Forwarder, creds components.MySQLCredentials, logw io.Writer) *sqladmin.Server {
	if cfg.Endpoints.SQLAdmin < 0 {
		return nil
	}
	var engines []sqladmin.Engine
	if f := forwarderFor(fwds, string(config.ServiceCloudSQL)); f != nil {
		engines = append(engines, &sqladmin.Postgres{
			Name: sqlAdminPostgresInstance, Version: "POSTGRES_17",
			User: components.CloudSQLUser, Database: components.CloudSQLDatabase, HostAddr: f.HostAddr,
			SignInLog: pgSignInLog(cfg, string(config.ServiceCloudSQL), false),
		})
	}
	if f := forwarderFor(fwds, string(config.ServiceCloudSQLMySQL)); f != nil {
		engines = append(engines, &sqladmin.MySQL{
			Name: sqlAdminMySQLInstance, Version: "MYSQL_8_4",
			AppUser: components.CloudSQLUser, Database: components.CloudSQLDatabase,
			RootPassword: creds.RootPassword, HostAddr: f.HostAddr,
		})
	}
	if len(engines) == 0 {
		return nil
	}
	srv := sqladmin.NewServer(net.JoinHostPort(cfg.BindAddress, strconv.Itoa(cfg.Endpoints.SQLAdmin)), engines...)
	// The connector path (#1330): the connectors always dial 3307, so the
	// listener is not moved by --port-base; --cloudsql-connector moves its
	// address (#1364). A CA that cannot be read leaves the rest of the API
	// serving, and connectSettings says why.
	_ = srv.EnableConnector(cfg.CloudSQLConnectorAddr(), cfg.InstanceDir())
	// Recorded for `env`, a separate process (#1571).
	if err := recordCloudSQLConnector(cfg); err != nil && logw != nil {
		fmt.Fprintf(logw, "cloudsql: recording the connector address: %v\n", err)
	}
	srv.SetConnectorOwner(cfg.Name)
	// Instance writes (#1330): each created instance is a server of its
	// own in the cluster, kept across `up` when the instance is persistent.
	srv.EnableInstanceWrites(newSQLProvisioner(cfg, logw), sqlInstancesStatePath(cfg))
	// Microsoft's SQL Server licence (#1916), when the user accepted it
	// for every instance; recorded for `status`.
	eula := mssqlEULAOf(cfg, time.Now().UTC().Format(time.RFC3339))
	srv.AcceptMSSQLEULA(eula)
	if err := recordMSSQLEULA(cfg, eula); err != nil && logw != nil {
		fmt.Fprintf(logw, "cloudsql: recording the SQL Server licence acceptance: %v\n", err)
	}
	// A blue-green switchover moves an instance's name to another server (#1614).
	sqlServerOf = srv.ServerOf
	// Backups (#1330): dumps kept on the host, with their records when the
	// instance is persistent. A store that cannot be made leaves backupRuns
	// answering 501 and the rest serving.
	dumper := newSQLDumper(cfg)
	if err := srv.EnableBackups(dumper, sqlBackupsDir(cfg), cfg.Mode == config.ModePersistent); err != nil && logw != nil {
		fmt.Fprintf(logw, "cloudburrow: Cloud SQL backups are off: %v\n", err)
	}
	// Imports and exports (#1380) go to and from the instance's own Cloud
	// Storage; without it they answer 501.
	if serviceEnabled(cfg, config.ServiceStorage) {
		// The storage tunnel's bound address, read at each transfer: with an
		// OS-assigned port (--port-storage 0) the configured one is 0.
		sf := storageSourceFetcher{cfg: cfg, client: &http.Client{Timeout: 10 * time.Minute},
			addr: func() string {
				if f := forwarderFor(fwds, "storage"); f != nil {
					return f.HostAddr()
				}
				return ""
			}}
		srv.EnableTransfers(sqlTransferer{d: dumper}, sqlObjects{f: sf})
	}
	return srv
}

// cloudSQLForPods is what the Cloud Run adapter needs for Cloud SQL volumes
// (#1346): the Admin API's in-cluster URL, once it and the connector are
// published to pods, and the instance IDs it serves. Before that, nil.
func cloudSQLForPods(srv *sqladmin.Server, host *clusterHost) runadapter.CloudSQL {
	return func() *runadapter.CloudSQLConfig {
		api := host.InCluster("sqladmin")
		ip := host.ClusterIP()
		if api == "" || host.InCluster("cloudsql-connector") == "" || ip == "" {
			return nil
		}
		return &runadapter.CloudSQLConfig{Endpoint: "http://" + api, Region: sqladmin.Region, Instances: srv.Instances()}
	}
}

// sqlAdminEnabled is whether `up` binds the Admin API's port.
func sqlAdminEnabled(cfg config.Config) bool {
	return cfg.Endpoints.SQLAdmin >= 0 &&
		(serviceEnabled(cfg, config.ServiceCloudSQL) || serviceEnabled(cfg, config.ServiceCloudSQLMySQL))
}

// sqlAdminIAM is Cloud SQL IAM database authentication's token source
// (#1944): the live access tokens creds minted, by SHA-256, and the
// instance's IAM evaluator for the principal's permission on the
// instance, whether or not --iam-enforcement is on: an IAM sign-in is
// always checked, as Memorystore's IAM auth is.
func sqlAdminIAM(creds *metadata.Credentials, e *iameval.Enforcer) sqladmin.IAMSource {
	return sqladmin.IAMSource{
		Token: func(hash string, now time.Time) (string, time.Time, bool) {
			for _, t := range creds.LiveTokens(now) {
				if t.Hash == hash {
					return t.Email, t.Expires, true
				}
			}
			return "", time.Time{}, false
		},
		Allowed: func(project, instance, email, permission string) bool {
			if e == nil || e.Eval == nil {
				return false
			}
			return e.Eval.Check(iameval.Request{Email: strings.ToLower(email), Permission: permission,
				Resource: "projects/" + project + "/instances/" + instance, Type: "sqladmin.googleapis.com/Instance",
				Service: "sqladmin.googleapis.com", Project: project, Time: time.Now()}).Granted
		},
	}
}

// cloudSQLEnforcer checks the Cloud SQL Admin API's requests (#1879) by
// their paths (iameval.CloudSQLJSON), and refuses with Cloud SQL's own 403
// envelope, reason notAuthorized; a token this instance did not mint is
// 401, as elsewhere.
func cloudSQLEnforcer(e *iameval.Enforcer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if e == nil || e.On == nil || !e.On() {
				next.ServeHTTP(w, r)
				return
			}
			permission, resource, ok := iameval.MatchJSON(iameval.CloudSQLJSON, r.Method, r.URL.Path)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			err := e.Authorize(r.Context(), iameval.CloudSQL, iameval.Bearer(r.Header.Get("Authorization")), permission, resource)
			switch status.Code(err) {
			case codes.OK:
				next.ServeHTTP(w, r)
			case codes.PermissionDenied:
				w.Header().Set("Content-Type", "application/json; charset=UTF-8")
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
					"code": http.StatusForbidden, "message": iameval.CloudSQLDenied,
					"errors": []map[string]any{{"domain": "global", "reason": "notAuthorized", "message": iameval.CloudSQLDenied}},
				}})
			default:
				iameval.WriteJSONStatus(w, err)
			}
		})
	}
}
