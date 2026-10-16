package sqladmin

// High availability (#1408). A REGIONAL instance is two servers: its
// primary, and a standby the primary replicates to synchronously, so a
// commit returns only once the standby has it (PostgreSQL's
// synchronous_standby_names with synchronous_commit on, MySQL's lossless
// semi-synchronous replication). The standby is not an instance: it is
// not listed, and it serves no client. instances.failover makes the
// standby the primary behind the same endpoint (connection name, host
// tunnel, connector route), and rebuilds the old primary as the new
// standby, as Cloud SQL does. The servers and their replication are the
// provisioner's (cmd/cloudburrow/sqlha.go); here are the API's checks and
// the record.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// failover is instances.failover.
func (s *Server) failover(r *http.Request, base, project, name string) (any, error) {
	c, err := s.createdFor(project, name, "instances.failover")
	if err != nil {
		return nil, err
	}
	var req struct {
		FailoverContext *struct {
			SettingsVersion json.RawMessage `json:"settingsVersion"`
		} `json:"failoverContext"`
	}
	if body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20)); len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, Errorf(http.StatusBadRequest, "Invalid JSON payload received: %v", err)
		}
	}
	s.emu.Lock()
	if !c.spec.Regional() {
		s.emu.Unlock()
		return nil, Errorf(http.StatusBadRequest, "The instance is not a high availability instance: its availabilityType is ZONAL. "+
			"Patch settings.availabilityType to REGIONAL first.")
	}
	if fc := req.FailoverContext; fc != nil && len(fc.SettingsVersion) > 0 {
		v, err := strconv.Atoi(strings.Trim(string(fc.SettingsVersion), `"`))
		if err != nil {
			s.emu.Unlock()
			return nil, Errorf(http.StatusBadRequest, "Invalid request: failoverContext.settingsVersion %s is not a number.", fc.SettingsVersion)
		}
		if v != c.spec.SettingsVersion {
			s.emu.Unlock()
			// Cloud SQL's answer to a stale settingsVersion.
			return nil, Errorf(http.StatusPreconditionFailed, "Precondition check failed: the instance's settingsVersion is %d, not %d.",
				c.spec.SettingsVersion, v)
		}
	}
	if c.state != "RUNNABLE" {
		st := c.state
		s.emu.Unlock()
		return nil, Errorf(http.StatusConflict, "Operation failed because another operation was already in progress (the instance is %s).", st)
	}
	from := c.spec
	to := c.spec
	old := from.PrimaryNode()
	to.HAPrimary = "a"
	if old == 0 {
		to.HAPrimary = "b"
	}
	to.HAEpochs[old] = max(from.HAEpochs[0], from.HAEpochs[1]) + 1
	c.spec = to
	c.state = "MAINTENANCE"
	err = s.saveLocked()
	s.emu.Unlock()
	if err != nil {
		return nil, err
	}
	op := s.pending(base, project, name, "FAILOVER")
	id := op["name"].(string)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*startTimeout)
		defer cancel()
		err := s.writes.prov.Failover(ctx, from, to)
		s.emu.Lock()
		if s.writes.byName[name] == c && c.state == "MAINTENANCE" {
			c.state, c.failure = "RUNNABLE", ""
			if err != nil {
				c.state, c.failure = "FAILED", err.Error()
			}
		}
		s.emu.Unlock()
		s.finish(id, err)
	}()
	return op, nil
}
