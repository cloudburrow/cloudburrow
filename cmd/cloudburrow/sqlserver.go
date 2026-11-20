package main

// Cloud SQL for SQL Server's servers (#1916, internal/service/sqladmin's
// sqlserver.go): Microsoft's linux/amd64-only image, served where the
// node runs linux/amd64 binaries (sqllegacy.go), and started with
// ACCEPT_EULA=Y only for an instance whose record says who accepted
// Microsoft's licence.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/service/sqladmin"
)

// sqlServerBackend is spec's server. An instance with no acceptance on
// record gets no ACCEPT_EULA, so its server does not start: the API never
// creates one (instances.insert refuses it), and a record edited by hand
// cannot start one either.
func sqlServerBackend(spec sqladmin.Spec) components.Backend {
	pid, _ := sqladmin.MSSQLPID(spec.DatabaseVersion)
	return components.CloudSQLServerBackend(spec.ServerName(), spec.DatabaseVersion, pid, spec.SAPassword,
		eulaAccepted(spec.MSSQLEULA))
}

// eulaAccepted is whether a an acceptance record names who accepted and
// when.
func eulaAccepted(a *sqladmin.EULAAcceptance) bool {
	return a != nil && a.AcceptedBy != "" && a.AcceptedAt != ""
}

// mssqlEULAOf is the acceptance of Microsoft's licence cfg was started
// with (`up --accept-mssql-eula`, CLOUDBURROW_ACCEPT_MSSQL_EULA=Y or the
// configuration file's acceptMssqlEula), recorded as made by the user
// running `up`, now; nil when it was not.
func mssqlEULAOf(cfg config.Config, now string) *sqladmin.EULAAcceptance {
	if !cfg.AcceptMSSQLEULA {
		return nil
	}
	via := sqladmin.EULAViaFile
	switch cfg.Source.MSSQLEULA {
	case "flag":
		via = sqladmin.EULAViaFlag
	case "env":
		via = sqladmin.EULAViaEnv
	}
	return &sqladmin.EULAAcceptance{AcceptedBy: sqladmin.LocalUser(), AcceptedAt: now, Via: via}
}

// mssqlEULAFile is where `up` records the acceptance it was started with,
// in the instance's directory, for `status`; it is removed when `up` is
// started without one.
const mssqlEULAFile = "mssql-eula.json"

func recordMSSQLEULA(cfg config.Config, a *sqladmin.EULAAcceptance) error {
	path := filepath.Join(cfg.InstanceDir(), mssqlEULAFile)
	if a == nil {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.InstanceDir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// mssqlEULAStatus is what `status` says of Microsoft's SQL Server licence:
// the acceptance `up` was started with, and each SQL Server instance's own.
type mssqlEULAStatus struct {
	Up        *sqladmin.EULAAcceptance            `json:"up,omitempty"`
	Instances map[string]*sqladmin.EULAAcceptance `json:"instances,omitempty"`
}

func readMSSQLEULAStatus(cfg config.Config) mssqlEULAStatus {
	var st mssqlEULAStatus
	if b, err := os.ReadFile(filepath.Join(cfg.InstanceDir(), mssqlEULAFile)); err == nil {
		var a sqladmin.EULAAcceptance
		if json.Unmarshal(b, &a) == nil {
			st.Up = &a
		}
	}
	if path := sqlInstancesStatePath(cfg); path != "" {
		if b, err := os.ReadFile(path); err == nil {
			var specs []sqladmin.Spec
			if json.Unmarshal(b, &specs) == nil {
				for _, sp := range specs {
					if sqladmin.IsSQLServer(sp.DatabaseVersion) {
						if st.Instances == nil {
							st.Instances = map[string]*sqladmin.EULAAcceptance{}
						}
						st.Instances[sp.Name] = sp.MSSQLEULA
					}
				}
			}
		}
	}
	return st
}

// mssqlEULALines are status's lines for st.
func mssqlEULALines(st mssqlEULAStatus) []string {
	var out []string
	if st.Up != nil {
		out = append(out, fmt.Sprintf("SQL Server licence: accepted by %s at %s via %s", st.Up.AcceptedBy, st.Up.AcceptedAt, st.Up.Via))
	} else {
		out = append(out, "SQL Server licence: not accepted (cloudburrow up --accept-mssql-eula accepts it for every instance)")
	}
	names := make([]string, 0, len(st.Instances))
	for n := range st.Instances {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if a := st.Instances[n]; a != nil {
			out = append(out, fmt.Sprintf("  %s: accepted by %s at %s via %s", n, a.AcceptedBy, a.AcceptedAt, a.Via))
		} else {
			out = append(out, fmt.Sprintf("  %s: no acceptance on record; its server is not started", n))
		}
	}
	return out
}

// mssqlEULAReport is status --format json's mssql_eula: nil when there is
// no acceptance to report.
func mssqlEULAReport(cfg config.Config) *mssqlEULAStatus {
	st := readMSSQLEULAStatus(cfg)
	if st.Up == nil && len(st.Instances) == 0 {
		return nil
	}
	return &st
}
