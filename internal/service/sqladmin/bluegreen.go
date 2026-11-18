package sqladmin

// Blue-green deployments (#1614), as Cloud SQL documents them for MySQL
// (docs.cloud.google.com/sql/docs/mysql/about-blue-green-deployments,
// create-blue-green-deployment and switchover-blue-green-deployment):
//
//   - create makes the green instance, BLUE_INSTANCE_NAME-green-UNIQUE_ID
//     ("the blue name is truncated to 48 characters and the ID is an
//     8-character hexadecimal string"), a clone of the blue instance kept
//     in step with it by replication, with requestedConfig.databaseVersion
//     applied ("Major version upgrade targets ... are supported from MySQL
//     8.0 to 8.4"). The blue instance must be a RUNNING MySQL primary with
//     automated backups and binary logging, no read replicas, and not
//     MySQL 8.0.18;
//   - the deployment is SWITCHOVER_READY while replication is healthy, and
//     SWITCHOVER_NOT_READY, with errorDetail, when it is broken or stopped;
//   - switchover stops writes on the blue instance, waits for the green one
//     to apply every transaction ("zero data loss"; it fails, and the blue
//     instance takes writes again, when it does not in time), promotes the
//     green instance, and moves the blue instance's name to it: "Cloud SQL
//     automatically transfers your existing database IP address, instance
//     name, and backups to the new database". Here the instance named blue
//     is then the green server, reached through the same connection name,
//     and the old blue server, "a standalone read and write instance",
//     takes the green name. The servers are not restarted;
//   - the green instance is "a clone of the blue environment": a
//     high-availability (REGIONAL) blue instance's green instance is
//     REGIONAL too, its primary server replicating from the blue
//     instance's primary and its standby, in its secondary zone, from it,
//     semi-synchronously, as a REGIONAL instance's standby does; after the
//     switchover both instances keep their standbys;
//   - delete before a switchover deletes the green instance; after one it
//     deletes the record only, and the old blue instance with
//     deleteOldSource.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// blueGreen is a blue-green deployment's record.
type blueGreen struct {
	Project     string `json:"project"`
	Location    string `json:"location"`
	ID          string `json:"id"`
	Description string `json:"description,omitempty"`
	// Source is the blue instance's name, Target the green one's: the
	// names stay, and after the switchover Source names the green server.
	Source           string `json:"source"`
	Target           string `json:"target"`
	RequestedVersion string `json:"requestedVersion,omitempty"`
	CreateTime       string `json:"createTime"`
	// Phase is PROVISIONING, PROVISIONED, UPGRADE_FAILED,
	// SWITCHOVER_IN_PROGRESS, SWITCHOVER_FAILED, SWITCHOVER_SUCCEEDED or
	// DELETING: the paired node's state.
	Phase  string `json:"phase"`
	Detail string `json:"detail,omitempty"`
}

func (d *blueGreen) name() string {
	return "projects/" + d.Project + "/locations/" + d.Location + "/blueGreenDeployments/" + d.ID
}

func (d *blueGreen) switched() bool { return d.Phase == "SWITCHOVER_SUCCEEDED" }

// bgIDRE is a deployment ID: an RFC 1035 label, as Cloud SQL's resource IDs are.
var bgIDRE = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)

// bgMaxDescription is the description's limit: "The description can be up
// to 255 characters long."
const bgMaxDescription = 255

// bgSwitchoverTimeout bounds how long a switchover waits for the green
// instance to apply the blue one's transactions before it fails ("if
// replication lag is too high").
const bgSwitchoverTimeout = time.Minute

func (s *Server) bgPath() string {
	if s.writes == nil || s.writes.path == "" {
		return ""
	}
	return strings.TrimSuffix(s.writes.path, filepath.Ext(s.writes.path)) + "-bluegreen.json"
}

// loadBlueGreensLocked reads the records once. Called with emu held.
func (s *Server) loadBlueGreensLocked() {
	if s.bgs != nil {
		return
	}
	s.bgs = map[string]*blueGreen{}
	if p := s.bgPath(); p != "" {
		var list []*blueGreen
		if b, err := os.ReadFile(p); err == nil && json.Unmarshal(b, &list) == nil {
			for _, d := range list {
				s.bgs[d.name()] = d
			}
		}
	}
}

func (s *Server) saveBlueGreensLocked() error {
	p := s.bgPath()
	if p == "" {
		return nil
	}
	list := []*blueGreen{}
	for _, d := range s.bgs {
		list = append(list, d)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].name() < list[j].name() })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(p+".tmp", b, 0o600); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

// blueGreenDeployments serves projects/P/locations/L/blueGreenDeployments.
func (s *Server) blueGreenDeployments(r *http.Request, base string, p []string) (string, any, error) {
	project, location := p[1], p[3]
	if s.writes == nil {
		return "sql.blueGreenDeployments", nil, s.noWrites("blueGreenDeployments")
	}
	if len(p) == 5 {
		switch r.Method {
		case http.MethodPost:
			out, err := s.createBlueGreen(r, base, project, location)
			return "sql.blueGreenDeployments.create", out, err
		case http.MethodGet:
			out, err := s.listBlueGreens(project, location)
			return "sql.blueGreenDeployments.list", out, err
		}
	}
	if len(p) == 6 {
		id, verb, _ := strings.Cut(p[5], ":")
		name := "projects/" + project + "/locations/" + location + "/blueGreenDeployments/" + id
		switch {
		case verb == "switchover" && r.Method == http.MethodPost:
			out, err := s.switchoverBlueGreen(base, name)
			return "sql.blueGreenDeployments.switchover", out, err
		case verb == "" && r.Method == http.MethodGet:
			d, ok := s.blueGreenCopy(name)
			if !ok {
				return "sql.blueGreenDeployments.get", nil, bgNotFound(name)
			}
			return "sql.blueGreenDeployments.get", s.blueGreenJSON(r.Context(), &d, r.URL.Query().Get("view") == "DETAILED"), nil
		case verb == "" && r.Method == http.MethodDelete:
			out, err := s.deleteBlueGreen(r, base, name)
			return "sql.blueGreenDeployments.delete", out, err
		}
	}
	return "sql.blueGreenDeployments", nil, Errorf(http.StatusNotFound, "method %s is not served on %s", r.Method, strings.Join(p, "/"))
}

// blueGreenCopy is a copy of deployment name's record, taken under emu.
func (s *Server) blueGreenCopy(name string) (blueGreen, bool) {
	s.emu.Lock()
	defer s.emu.Unlock()
	s.loadBlueGreensLocked()
	if d := s.bgs[name]; d != nil {
		return *d, true
	}
	return blueGreen{}, false
}

func bgNotFound(name string) error {
	return &apiError{code: http.StatusNotFound, status: "NOT_FOUND", msg: "The blue-green deployment " + name + " does not exist."}
}

// createBlueGreen is blueGreenDeployments.create.
func (s *Server) createBlueGreen(r *http.Request, base, project, location string) (any, error) {
	var body struct {
		SourceInstance  string `json:"sourceInstance"`
		Description     string `json:"description"`
		RequestedConfig *struct {
			DatabaseVersion string `json:"databaseVersion"`
		} `json:"requestedConfig"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	id := r.URL.Query().Get("blueGreenDeploymentId")
	if id == "" {
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		id = "bgd-" + hex.EncodeToString(b)
	}
	if !bgIDRE.MatchString(id) {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: blueGreenDeploymentId %q must be 1 to 63 lowercase letters, digits and "+
			"hyphens, start with a letter and not end with a hyphen.", id)
	}
	if len(body.Description) > bgMaxDescription {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: description is %d characters; it can be up to %d characters long.",
			len(body.Description), bgMaxDescription)
	}
	blue := body.SourceInstance
	if blue == "" {
		return nil, Errorf(http.StatusBadRequest, "Missing parameter: sourceInstance.")
	}
	if rest, ok := strings.CutPrefix(blue, "projects/"); ok {
		pr, inst, ok := strings.Cut(rest, "/instances/")
		if !ok || pr != project {
			return nil, Errorf(http.StatusBadRequest, "Invalid request: sourceInstance %s must be projects/%s/instances/INSTANCE.", blue, project)
		}
		blue = inst
	}
	bc, err := s.createdFor(project, blue, "blueGreenDeployments.create")
	if err != nil {
		return nil, err
	}
	target := ""
	if body.RequestedConfig != nil {
		target = body.RequestedConfig.DatabaseVersion
	}
	s.emu.Lock()
	s.loadBlueGreensLocked()
	spec := bc.spec
	name := "projects/" + project + "/locations/" + location + "/blueGreenDeployments/" + id
	fail := func(err error) (any, error) {
		s.emu.Unlock()
		return nil, err
	}
	bcfg, _ := spec.Settings["backupConfiguration"].(map[string]any)
	binlog, _ := bcfg["binaryLogEnabled"].(bool)
	switch {
	case s.bgs[name] != nil:
		return fail(&apiError{code: http.StatusConflict, status: "ALREADY_EXISTS", msg: "The blue-green deployment " + name + " already exists."})
	case !spec.MySQL():
		return fail(Errorf(http.StatusBadRequest, "Invalid request: blue-green deployments support Cloud SQL for MySQL only; %s is %s.",
			blue, spec.DatabaseVersion))
	case location != spec.region():
		return fail(Errorf(http.StatusBadRequest, "Invalid request: the deployment's location must be the source instance's region, %s.", spec.region()))
	case spec.DatabaseVersion == "MYSQL_8_0_18":
		return fail(Errorf(http.StatusBadRequest, "Invalid request: blue-green deployments do not support MySQL 8.0.18."))
	case bc.state != "RUNNABLE":
		return fail(Errorf(http.StatusConflict, "Operation failed because the source instance %s is %s; it must be RUNNING with no ongoing operations.",
			blue, bc.state))
	case spec.MasterInstanceName != "":
		return fail(Errorf(http.StatusBadRequest, "Invalid request: %s is a read replica; the source of a blue-green deployment is a primary instance.", blue))
	case len(s.replicaNamesLocked(project, blue)) > 0:
		return fail(Errorf(http.StatusBadRequest, "Invalid request: %s has read replicas (%s); instances with read replicas aren't supported "+
			"by blue-green deployments.", blue, strings.Join(s.replicaNamesLocked(project, blue), ", ")))
	case !spec.BackupEnabled || !binlog:
		return fail(Errorf(http.StatusBadRequest, "Invalid request: %s must have automated backups and binary logging enabled "+
			"(settings.backupConfiguration.enabled and binaryLogEnabled).", blue))
	case target != "" && target != spec.DatabaseVersion && (target != "MYSQL_8_4" || !strings.HasPrefix(spec.DatabaseVersion, "MYSQL_8_0")):
		return fail(Errorf(http.StatusBadRequest, "Invalid request: requestedConfig.databaseVersion %s: major version upgrade targets are "+
			"supported from MySQL 8.0 to 8.4 (MYSQL_8_4); %s is %s.", target, blue, spec.DatabaseVersion))
	}
	for _, d := range s.bgs {
		if d.Project == project && (d.Source == blue || d.Target == blue) {
			return fail(Errorf(http.StatusBadRequest, "Invalid request: %s is already part of the blue-green deployment %s.", blue, d.name()))
		}
	}
	short := blue
	if len(short) > 48 {
		short = short[:48]
	}
	rb := make([]byte, 4)
	_, _ = rand.Read(rb)
	green := short + "-green-" + hex.EncodeToString(rb)
	if len(green) > maxInstanceName {
		return fail(unimplemented("a blue-green deployment of "+blue,
			fmt.Sprintf("its green instance's name, %s, is %d characters, and CloudBurrow's instance names are at most %d "+
				"(its server's Kubernetes names, \"sqlinst-\" and the name, are at most 63); use a source instance name of at most %d characters",
				green, len(green), maxInstanceName, maxInstanceName-16)))
	}
	if target == "" {
		target = spec.DatabaseVersion
	}
	d := &blueGreen{Project: project, Location: location, ID: id, Description: body.Description, Source: blue, Target: green,
		CreateTime: time.Now().UTC().Format(time.RFC3339Nano), Phase: "PROVISIONING"}
	if body.RequestedConfig != nil {
		d.RequestedVersion = body.RequestedConfig.DatabaseVersion
	}
	s.bgs[name] = d
	err = s.saveBlueGreensLocked()
	s.emu.Unlock()
	if err != nil {
		return nil, err
	}
	gspec := Spec{Name: green, Project: project, DatabaseVersion: spec.DatabaseVersion, CreateTime: time.Now().UTC().Format(time.RFC3339Nano),
		SettingsVersion: 1, AvailabilityType: spec.AvailabilityType, Tier: spec.Tier, Labels: maps.Clone(spec.Labels), Flags: slices.Clone(spec.Flags),
		Settings:      maps.Clone(spec.Settings),
		BackupEnabled: spec.BackupEnabled, Region: spec.Region, ActivationPolicy: spec.ActivationPolicy}
	if gspec.AvailabilityType == "" {
		gspec.AvailabilityType = "ZONAL"
	}
	if gspec.Regional() {
		// The green instance mirrors a high-availability blue one: a
		// primary server, kept in step with the blue instance, and a
		// standby in another zone replicating from it semi-synchronously,
		// so after the switchover the instance is as available as before.
		gspec.HAPassword, gspec.HAEpochs[1] = newReplicationPassword(), 1
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	err = s.prepareReplica(ctx, project, instanceBody{MasterInstanceName: blue, Region: spec.Region}, &gspec)
	if err == nil {
		gspec.DatabaseVersion = target
		err = s.replicaKey(ctx, &gspec, "")
	}
	if err == nil {
		s.emu.Lock()
		if s.engineLocked(green) != nil {
			err = Errorf(http.StatusConflict, "The Cloud SQL instance already exists.")
		} else {
			s.writes.byName[green] = &created{spec: gspec, engine: s.writes.prov.Engine(gspec), state: "PENDING_CREATE"}
			err = s.saveLocked()
		}
		s.emu.Unlock()
	}
	if err == nil && s.conn != nil {
		err = s.conn.addInstance(green)
	}
	if err != nil {
		s.emu.Lock()
		delete(s.bgs, name)
		_ = s.saveBlueGreensLocked()
		s.emu.Unlock()
		return nil, err
	}
	op := s.pending(base, project, blue, "CREATE_BLUE_GREEN_DEPLOYMENT")
	go func() {
		s.bringUp(green, "")
		s.emu.Lock()
		var err error
		if c := s.writes.byName[green]; c == nil || c.state != "RUNNABLE" {
			why := "it was deleted"
			if c != nil {
				why = c.failure
			}
			d.Phase, d.Detail = "UPGRADE_FAILED", "The green instance "+green+" did not start: "+why
			err = Errorf(http.StatusBadRequest, "%s", d.Detail)
		} else {
			d.Phase = "PROVISIONED"
			if d.RequestedVersion != "" && d.RequestedVersion != spec.DatabaseVersion {
				d.Phase = "UPGRADED"
			}
		}
		_ = s.saveBlueGreensLocked()
		s.emu.Unlock()
		s.finish(op["name"].(string), err)
	}()
	return op, nil
}

// bgState is the deployment's state and errorDetail, from its paired
// node's phase and, before a switchover, the green instance's replication.
func (s *Server) bgState(ctx context.Context, d *blueGreen) (string, string) {
	switch d.Phase {
	case "PROVISIONING":
		return "PROVISIONING", ""
	case "DELETING":
		return "DELETING", ""
	case "SWITCHOVER_IN_PROGRESS":
		return "SWITCHOVER_IN_PROGRESS", ""
	case "SWITCHOVER_SUCCEEDED":
		return "SWITCHOVER_COMPLETED", ""
	case "UPGRADE_FAILED":
		return "SWITCHOVER_NOT_READY", d.Detail
	}
	s.emu.RLock()
	gc, bc := s.writes.byName[d.Target], s.writes.byName[d.Source]
	var gspec Spec
	var gstate string
	var ge Engine
	if gc != nil {
		gspec, gstate, ge = gc.spec, gc.state, gc.engine
	}
	s.emu.RUnlock()
	detail := ""
	switch {
	case bc == nil:
		detail = "The source instance " + d.Source + " no longer exists."
	case gc == nil:
		detail = "The target instance " + d.Target + " no longer exists."
	case gstate != "RUNNABLE":
		detail = "The target instance " + d.Target + " is " + gstate + "."
	case gspec.MasterInstanceName != d.Source:
		detail = "The target instance " + d.Target + " no longer replicates from " + d.Source + "."
	case gspec.ReplicationStopped:
		detail = "Replication from " + d.Source + " to " + d.Target + " is stopped; start it with startReplica."
	}
	if detail == "" {
		if m, ok := ge.(*MySQL); ok {
			qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			st, err := queryString(qctx, m, "SELECT CONCAT((SELECT SERVICE_STATE FROM performance_schema.replication_connection_status LIMIT 1), "+
				"'/', (SELECT SERVICE_STATE FROM performance_schema.replication_applier_status LIMIT 1))")
			cancel()
			if err != nil || st != "ON/ON" {
				detail = fmt.Sprintf("Replication from %s to %s is broken (receiver/applier %s %v).", d.Source, d.Target, st, err)
			}
		}
	}
	if d.Phase == "SWITCHOVER_FAILED" && detail == "" {
		return "SWITCHOVER_READY", ""
	}
	if detail != "" {
		return "SWITCHOVER_NOT_READY", detail
	}
	return "SWITCHOVER_READY", ""
}

func (s *Server) blueGreenJSON(ctx context.Context, d *blueGreen, detailed bool) map[string]any {
	state, detail := s.bgState(ctx, d)
	if detail == "" && d.Phase == "SWITCHOVER_FAILED" {
		detail = d.Detail
	}
	src := "projects/" + d.Project + "/instances/" + d.Source
	tgt := "projects/" + d.Project + "/instances/" + d.Target
	pairState := d.Phase
	if pairState == "SWITCHOVER_FAILED" && state == "SWITCHOVER_READY" {
		pairState = "PROVISIONED"
	}
	pair := map[string]any{"source": map[string]any{"instance": src}, "target": map[string]any{"instance": tgt}, "state": pairState}
	out := map[string]any{"name": d.name(), "sourceInstance": src, "switchoverTargetInstance": tgt, "state": state,
		"createTime": d.CreateTime, "deploymentMappings": []any{pair}}
	if d.Description != "" {
		out["description"] = d.Description
	}
	if detail != "" {
		out["errorDetail"] = detail
	}
	if d.RequestedVersion != "" {
		out["requestedConfig"] = map[string]any{"databaseVersion": d.RequestedVersion}
	}
	if detailed {
		s.emu.RLock()
		var bs, gs Spec
		if c := s.writes.byName[d.Source]; c != nil {
			bs = c.spec
		}
		if c := s.writes.byName[d.Target]; c != nil {
			gs = c.spec
		}
		s.emu.RUnlock()
		diffs := []any{}
		add := func(field, a, b string) {
			if a != b {
				diffs = append(diffs, map[string]any{"field": field, "sourceValue": a, "targetValue": b})
			}
		}
		add("database_version", bs.DatabaseVersion, gs.DatabaseVersion)
		add("tier", bs.Tier, gs.Tier)
		add("edition", editionOf(bs), editionOf(gs))
		add("availability_type", bs.AvailabilityType, gs.AvailabilityType)
		if len(diffs) > 0 {
			pair["diffs"] = diffs
		}
	}
	return out
}

func (s *Server) listBlueGreens(project, location string) (any, error) {
	s.emu.Lock()
	s.loadBlueGreensLocked()
	var list []blueGreen
	for _, d := range s.bgs {
		if d.Project == project && (location == "-" || d.Location == location) {
			list = append(list, *d)
		}
	}
	s.emu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].name() < list[j].name() })
	out := []any{}
	for i := range list {
		out = append(out, s.blueGreenJSON(context.Background(), &list[i], false))
	}
	if len(out) == 0 {
		return map[string]any{}, nil
	}
	return map[string]any{"blueGreenDeployments": out}, nil
}

// switchoverBlueGreen is blueGreenDeployments.switchover.
func (s *Server) switchoverBlueGreen(base, name string) (any, error) {
	dc, ok := s.blueGreenCopy(name)
	if !ok {
		return nil, bgNotFound(name)
	}
	if state, detail := s.bgState(context.Background(), &dc); state != "SWITCHOVER_READY" {
		msg := "Invalid request: the blue-green deployment " + name + " is " + state + ", not SWITCHOVER_READY"
		if detail != "" {
			msg += ": " + detail
		}
		return nil, Errorf(http.StatusBadRequest, "%s", msg)
	}
	s.emu.Lock()
	d := s.bgs[name]
	if d == nil || d.Phase == "SWITCHOVER_IN_PROGRESS" || d.Phase == "DELETING" || d.switched() {
		s.emu.Unlock()
		return nil, Errorf(http.StatusConflict, "Operation failed because the blue-green deployment %s changed meanwhile.", name)
	}
	bc, gc := s.writes.byName[d.Source], s.writes.byName[d.Target]
	if bc == nil || gc == nil || bc.state != "RUNNABLE" || gc.state != "RUNNABLE" {
		s.emu.Unlock()
		return nil, Errorf(http.StatusConflict, "Operation failed because the source or target instance is not RUNNING.")
	}
	bc.state, gc.state = "MAINTENANCE", "MAINTENANCE"
	d.Phase, d.Detail = "SWITCHOVER_IN_PROGRESS", ""
	_ = s.saveBlueGreensLocked()
	s.emu.Unlock()
	op := s.pending(base, d.Project, d.Source, "SWITCHOVER_BLUE_GREEN_DEPLOYMENT")
	go func() {
		err := s.runBlueGreenSwitchover(d, bc, gc)
		s.emu.Lock()
		if err != nil {
			d.Phase, d.Detail = "SWITCHOVER_FAILED", err.Error()
		} else {
			d.Phase = "SWITCHOVER_SUCCEEDED"
		}
		_ = s.saveBlueGreensLocked()
		s.emu.Unlock()
		s.finish(op["name"].(string), err)
	}()
	return op, nil
}

// runBlueGreenSwitchover makes the green instance the blue one's
// successor: no transaction lost, the blue name moved to its server.
func (s *Server) runBlueGreenSwitchover(d *blueGreen, bc, gc *created) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), bgSwitchoverTimeout+time.Minute)
	defer cancel()
	s.emu.RLock()
	blue, ok1 := bc.engine.(*MySQL)
	green, ok2 := gc.engine.(*MySQL)
	s.emu.RUnlock()
	promoted := false
	defer func() {
		if err == nil {
			return
		}
		if !promoted {
			// The blue instance takes writes again; nothing else changed.
			_ = blue.exec(context.Background(), "SET PERSIST super_read_only = OFF", "SET PERSIST read_only = OFF")
		}
		s.emu.Lock()
		bc.state, gc.state = "RUNNABLE", "RUNNABLE"
		s.emu.Unlock()
	}()
	if !ok1 || !ok2 {
		return unimplemented("blueGreenDeployments.switchover", "these servers are not MySQL servers")
	}
	// The blue instance stops taking writes.
	if err := blue.exec(ctx, "SET PERSIST read_only = ON", "SET PERSIST super_read_only = ON"); err != nil {
		return fmt.Errorf("stop writes on %s: %w", d.Source, err)
	}
	gtids, err := queryString(ctx, blue, "SELECT @@GLOBAL.gtid_executed")
	if err != nil {
		return fmt.Errorf("read %s's GTIDs: %w", d.Source, err)
	}
	if err := waitForGTIDs(ctx, green, gtids, bgSwitchoverTimeout); err != nil {
		return Errorf(http.StatusBadRequest, "The switchover failed because replication lag is too high: %s did not apply %s's "+
			"transactions within %s (%v); %s is serving writes again.", d.Target, d.Source, bgSwitchoverTimeout, err, d.Source)
	}
	if err := green.Promote(ctx); err != nil {
		return fmt.Errorf("promote %s: %w", d.Target, err)
	}
	promoted = true
	// The old blue server is a standalone read and write instance.
	if err := blue.exec(ctx, "SET PERSIST super_read_only = OFF", "SET PERSIST read_only = OFF"); err != nil {
		return fmt.Errorf("make %s writable: %w", d.Source, err)
	}
	s.emu.Lock()
	bs, gs := bc.spec, gc.spec
	nb := gs // the blue name, on the green server
	nb.Name, nb.Server = bs.Name, gs.ServerName()
	nb.MasterInstanceName, nb.MasterUp, nb.ReplicationPassword, nb.ReplicationStopped, nb.Seeded = "", false, "", false, false
	nb.CreateTime, nb.DeletionProtection, nb.Labels = bs.CreateTime, bs.DeletionProtection, bs.Labels
	nb.SettingsVersion = bs.SettingsVersion + 1
	ng := bs // the green name, on the old blue server
	ng.Name, ng.Server = gs.Name, bs.ServerName()
	ng.CreateTime, ng.DeletionProtection = gs.CreateTime, false
	ng.SettingsVersion = gs.SettingsVersion + 1
	if nb.Server == nb.Name {
		nb.Server = ""
	}
	if ng.Server == ng.Name {
		ng.Server = ""
	}
	bc.spec, gc.spec = nb, ng
	bc.engine, gc.engine = s.writes.prov.Engine(nb), s.writes.prov.Engine(ng)
	bc.state, gc.state = "RUNNABLE", "RUNNABLE"
	s.noteServer(nb)
	s.noteServer(ng)
	err = s.saveLocked()
	s.emu.Unlock()
	if err != nil {
		return err
	}
	if nb.PITREnabled() {
		// The instance's point-in-time recovery starts again from its new server.
		_ = s.takePITRBase(nb.Name)
	}
	return nil
}

// deleteBlueGreen is blueGreenDeployments.delete: before a switchover it
// deletes the green instance; after one the record, and with
// deleteOldSource the old blue instance (now the green name).
func (s *Server) deleteBlueGreen(r *http.Request, base, name string) (any, error) {
	deleteOld := r.URL.Query().Get("deleteOldSource") == "true"
	s.emu.Lock()
	s.loadBlueGreensLocked()
	d := s.bgs[name]
	if d == nil {
		s.emu.Unlock()
		return nil, bgNotFound(name)
	}
	if d.Phase == "SWITCHOVER_IN_PROGRESS" || d.Phase == "PROVISIONING" || d.Phase == "DELETING" {
		ph := d.Phase
		s.emu.Unlock()
		return nil, Errorf(http.StatusConflict, "Operation failed because the blue-green deployment is %s.", ph)
	}
	if deleteOld && !d.switched() {
		s.emu.Unlock()
		return nil, Errorf(http.StatusBadRequest, "Invalid request: deleteOldSource applies after a switchover; %s has not been switched over.", name)
	}
	doomed := ""
	if !d.switched() || deleteOld {
		doomed = d.Target
	}
	if doomed != "" {
		if c := s.writes.byName[doomed]; c != nil && c.spec.DeletionProtection {
			s.emu.Unlock()
			return nil, Errorf(http.StatusBadRequest, "The instance %s is protected from deletion; disable deletion protection first.", doomed)
		}
	}
	d.Phase = "DELETING"
	_ = s.saveBlueGreensLocked()
	exists := doomed != "" && s.writes.byName[doomed] != nil
	s.emu.Unlock()
	op := s.pending(base, d.Project, d.Source, "DELETE_BLUE_GREEN_DEPLOYMENT")
	go func() {
		var err error
		if exists {
			req, _ := http.NewRequest(http.MethodDelete, "/", nil)
			var out any
			if out, err = s.deleteInstance(req, base, d.Project, doomed); err == nil {
				err = s.waitOp(d.Project, out)
			}
		}
		s.emu.Lock()
		if err == nil {
			delete(s.bgs, name)
		} else if d.switched() || deleteOld {
			d.Phase = "SWITCHOVER_SUCCEEDED"
		} else {
			d.Phase = "PROVISIONED"
		}
		_ = s.saveBlueGreensLocked()
		s.emu.Unlock()
		s.finish(op["name"].(string), err)
	}()
	return op, nil
}

// waitOp waits for the operation out (an instance operation) to end.
func (s *Server) waitOp(project string, out any) error {
	op, _ := out.(map[string]any)
	id, _ := op["name"].(string)
	for deadline := time.Now().Add(startTimeout); time.Now().Before(deadline); time.Sleep(time.Second) {
		cur, err := s.operation(project, id)
		if err != nil {
			return err
		}
		if cur["status"] == "DONE" {
			if e, ok := cur["error"].(map[string]any); ok {
				return fmt.Errorf("%v", e["errors"])
			}
			return nil
		}
	}
	return fmt.Errorf("operation %s did not finish", id)
}

// blueGreenInfoJSON sets an instance's deploymentInfo while it is part of a
// blue-green deployment.
func (s *Server) blueGreenInfoJSON(in map[string]any, spec Spec) {
	s.emu.Lock()
	s.loadBlueGreensLocked()
	var d *blueGreen
	for _, x := range s.bgs {
		if x.Project == spec.Project && (x.Source == spec.Name || x.Target == spec.Name) {
			d = x
		}
	}
	var dc blueGreen
	if d != nil {
		dc = *d
	}
	s.emu.Unlock()
	if d == nil {
		return
	}
	state := "PRE_SWITCHOVER"
	if dc.switched() {
		state = "POST_SWITCHOVER"
	}
	ref := func(n string) map[string]any {
		return map[string]any{"name": n, "project": dc.Project, "region": dc.Location}
	}
	info := map[string]any{"deploymentId": dc.ID, "state": state}
	if dc.Source == spec.Name {
		info["source"] = map[string]any{"targetId": ref(dc.Target)}
	} else {
		info["target"] = map[string]any{"sourceId": ref(dc.Source)}
	}
	in["deploymentInfo"] = info
}
