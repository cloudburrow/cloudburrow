package main

import (
	"io"
	"net"
	"strconv"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
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
	// listener is not moved by --port-base. A CA that cannot be read leaves
	// the rest of the API serving, and connectSettings says why.
	_ = srv.EnableConnector(net.JoinHostPort(cfg.BindAddress, strconv.Itoa(sqladmin.ConnectorPort)), cfg.InstanceDir())
	// Instance writes (#1330): each created instance is a server of its
	// own in the cluster, kept across `up` when the instance is persistent.
	srv.EnableInstanceWrites(newSQLProvisioner(cfg, logw), sqlInstancesStatePath(cfg))
	return srv
}

// sqlAdminEnabled is whether `up` binds the Admin API's port.
func sqlAdminEnabled(cfg config.Config) bool {
	return cfg.Endpoints.SQLAdmin >= 0 &&
		(serviceEnabled(cfg, config.ServiceCloudSQL) || serviceEnabled(cfg, config.ServiceCloudSQLMySQL))
}
