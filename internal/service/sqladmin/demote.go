package sqladmin

// demoteMaster and demote (#1524): a standalone MySQL instance becomes a
// read replica of an external server, the reverse of promoteReplica, as
// Cloud SQL documents them. The instance's server is restarted as a replica
// (a server ID of its own, GTIDs on, read-only) of the server a source
// representation instance (ON_PREMISES_INSTANCE) records.
//
//   - demoteMaster keeps the instance's data and replicates from the
//     source by GTID auto-positioning, after checking (verifyGtidConsistency,
//     true unless sent false) that the instance has executed no
//     transaction the source has not.
//   - demote, Database Migration Service's step for a migration to an
//     existing instance, leaves it a replica waiting for startExternalSync,
//     which dumps the source's databases into it and starts replicating.
//     A PostgreSQL instance is demoted too (#1614): its databases named as
//     the migrated ones are dropped first, as Database Migration Service
//     documents.
//
// demoteMaster with skipReplicationSetup (#1614) installs Cloud SQL's
// mysql.* replication stored procedures instead of setting up the
// replication (demoteprocs.go).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

type demoteBody struct {
	DemoteMasterContext *struct {
		MasterInstanceName   string `json:"masterInstanceName"`
		ReplicaConfiguration *struct {
			Mysql *struct {
				Username string `json:"username"`
				Password string `json:"password"`
			} `json:"mysqlReplicaConfiguration"`
		} `json:"replicaConfiguration"`
		SkipReplicationSetup  bool  `json:"skipReplicationSetup"`
		VerifyGtidConsistency *bool `json:"verifyGtidConsistency"`
	} `json:"demoteMasterContext"`
	DemoteContext *struct {
		SourceRepresentativeInstanceName string `json:"sourceRepresentativeInstanceName"`
	} `json:"demoteContext"`
}

// demoteInstance is instances.demoteMaster and instances.demote.
func (s *Server) demoteInstance(r *http.Request, base, project, name, verb string) (any, error) {
	var b demoteBody
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, Errorf(http.StatusBadRequest, "Invalid JSON payload received: %v", err)
		}
	}
	var master, user, pass string
	verify, isMaster, manual := true, verb == "demoteMaster", false
	switch {
	case isMaster && b.DemoteMasterContext != nil:
		d := b.DemoteMasterContext
		master = d.MasterInstanceName
		if rc := d.ReplicaConfiguration; rc != nil && rc.Mysql != nil {
			user, pass = rc.Mysql.Username, rc.Mysql.Password
		}
		if d.VerifyGtidConsistency != nil {
			verify = *d.VerifyGtidConsistency
		}
		// The custom import guide demotes an instance whose imported
		// data has GTIDs of its own, then resets them (mysql.resetMaster):
		// with skipReplicationSetup there is no auto-positioning to check.
		manual = d.SkipReplicationSetup
		verify = verify && !manual
	case !isMaster && b.DemoteContext != nil:
		master = b.DemoteContext.SourceRepresentativeInstanceName
	}
	if master == "" {
		if isMaster {
			return nil, Errorf(http.StatusBadRequest, "Missing parameter: demoteMasterContext.masterInstanceName.")
		}
		return nil, Errorf(http.StatusBadRequest, "Missing parameter: demoteContext.sourceRepresentativeInstanceName.")
	}
	c, err := s.createdFor(project, name, "instances."+verb)
	if err != nil {
		return nil, err
	}
	if legacyMySQL(c.spec.DatabaseVersion) {
		return nil, legacyUnimplemented("instances." + verb)
	}
	master, err = masterName(project, master)
	if err != nil {
		return nil, err
	}
	rep := s.sourceNamed(project, master)
	if rep == nil {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: %s is not a source representation instance (ON_PREMISES_INSTANCE) of project %s; "+
			"an instance is demoted to a replica of one.", master, project)
	}
	s.emu.RLock()
	spec, state, replicas := c.spec, c.state, s.replicaNamesLocked(project, name)
	s.emu.RUnlock()
	switch {
	case !spec.MySQL() && isMaster:
		return nil, Errorf(http.StatusBadRequest, "Invalid request: demoteMaster is for a MySQL instance; %s runs %s.", name, spec.DatabaseVersion)
	case spec.MasterInstanceName != "":
		return nil, Errorf(http.StatusBadRequest, "Invalid request: %s is already a read replica (of %s).", name, spec.MasterInstanceName)
	case len(replicas) > 0:
		return nil, Errorf(http.StatusBadRequest, "Invalid request: %s has read replicas (%s); delete or promote them first.", name, strings.Join(replicas, ", "))
	case spec.Regional():
		return nil, Errorf(http.StatusBadRequest, "Invalid request: %s is a high-availability (REGIONAL) instance; a replica is ZONAL.", name)
	case state != "RUNNABLE":
		return nil, Errorf(http.StatusConflict, "Operation failed because the instance is %s.", state)
	}
	// The replica's spec is filled as for a replica created with
	// masterInstanceName; the instance keeps its own region, tier and
	// version, which must be the source's engine.
	body := instanceBody{Region: spec.Region, MasterInstanceName: master}
	if user != "" {
		rc, _ := json.Marshal(map[string]any{"mysqlReplicaConfiguration": map[string]string{"username": user, "password": pass}})
		m := json.RawMessage(rc)
		body.ReplicaConfiguration = &m
	}
	next := spec
	if err := s.prepareExternalReplica(body, rep, &next); err != nil {
		return nil, err
	}
	next.Region, next.Tier = spec.Region, spec.Tier
	ext := *next.External
	ext.DumpFile, ext.SyncMode = "", "" // the instance has its data, or startExternalSync gives it the source's
	if isMaster {
		addr := s.sourceAddr(&ext)
		probe := next
		probe.External = &ext
		if errs := verifyExternalSync(r.Context(), probe, addr, false, false); len(errs) > 0 {
			var d []string
			for _, e := range errs {
				m := e.(map[string]any)
				d = append(d, fmt.Sprintf("%s: %s", m["type"], m["detail"]))
			}
			return nil, Errorf(http.StatusBadRequest, "Invalid request: the source fails verification: %s.", strings.Join(d, "; "))
		}
		if verify {
			if err := s.gtidConsistent(r.Context(), c.engine, &ext, addr); err != nil {
				return nil, err
			}
		}
		ext.Demoted, ext.ManualSetup = true, manual
	} else if !spec.MySQL() {
		// demote of a PostgreSQL instance (#1614): as Database Migration
		// Service documents for a migration to an existing instance, a
		// database of the instance with the name of one being migrated is
		// dropped before the sync copies it (pgExternalSetup).
		ext.Demoted = true
	}
	if manual {
		m, ok := c.engine.(*MySQL)
		if !ok {
			return nil, unimplemented("demoteMaster with skipReplicationSetup", "this server is not MySQL")
		}
		if err := installDemoteProcedures(r.Context(), m); err != nil {
			return nil, err
		}
	}
	s.emu.Lock()
	if c.state != "RUNNABLE" || c.spec.MasterInstanceName != "" {
		s.emu.Unlock()
		return nil, Errorf(http.StatusConflict, "Operation failed because another operation was already in progress.")
	}
	c.spec.MasterInstanceName, c.spec.External = next.MasterInstanceName, &ext
	c.spec.ExternalSyncStarted = isMaster
	if !spec.MySQL() {
		// A PostgreSQL replica of an external server is a logical one,
		// which promoteReplica ends by dropping its subscriptions (#1614).
		c.engine = s.writes.prov.Engine(c.spec)
	}
	c.spec.ReplicationStopped = false
	c.state = "MAINTENANCE"
	err = s.saveLocked()
	s.emu.Unlock()
	if err != nil {
		return nil, err
	}
	opType := "DEMOTE"
	if isMaster {
		opType = "DEMOTE_MASTER"
	}
	op := s.pending(base, project, name, opType)
	go s.bringUp(name, op["name"].(string))
	return op, nil
}

// gtidConsistent is demoteMaster's verifyGtidConsistency: every
// transaction the instance has executed must be one the source has, or
// replicating from the source by auto-positioning would diverge.
func (s *Server) gtidConsistent(ctx context.Context, e Engine, ext *ExternalSource, addr string) error {
	m, ok := e.(*MySQL)
	if !ok {
		return unimplemented("verifyGtidConsistency", "this server is not MySQL")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	db, err := m.db()
	if err != nil {
		return err
	}
	defer db.Close()
	var mine string
	if err := db.QueryRowContext(ctx, "SELECT @@GLOBAL.gtid_executed").Scan(&mine); err != nil {
		return myError(err)
	}
	cfg := mysql.NewConfig()
	cfg.User, cfg.Passwd, cfg.Net, cfg.Addr = ext.User, ext.Password, "tcp", addr
	cfg.Timeout, cfg.AllowNativePasswords, cfg.TLSConfig = 10*time.Second, true, "preferred"
	src, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return err
	}
	defer src.Close()
	var theirs string
	var subset int
	if err := src.QueryRowContext(ctx, "SELECT @@GLOBAL.gtid_executed, GTID_SUBSET(?, @@GLOBAL.gtid_executed)", mine).Scan(&theirs, &subset); err != nil {
		return Errorf(http.StatusBadRequest, "Invalid request: the source's GTIDs cannot be read as %s: %v.", ext.User, err)
	}
	if subset != 1 {
		return Errorf(http.StatusBadRequest, "Invalid request: the instance's GTIDs are not consistent with the source's: it has executed %s, "+
			"the source %s. Send verifyGtidConsistency false to demote it anyway.", gtidOr(mine), gtidOr(theirs))
	}
	return nil
}

func gtidOr(v string) string {
	if v = strings.ReplaceAll(strings.TrimSpace(v), "\n", ""); v == "" {
		return "none"
	}
	return v
}
