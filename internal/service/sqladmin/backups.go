package sqladmin

// Backups (#1330): backupRuns.insert, list, get and delete,
// instances.restoreBackup and instances.clone, each over a real dump. A
// backup is the server's databases dumped with its own tools (pg_dump, or
// mysqldump), the mechanism `cloudburrow snapshot` uses for Cloud SQL, kept
// in a directory of its own on the host; a restore drops the target's
// databases and loads the dump's, so the rows written after the backup are
// gone afterwards, as they are after Cloud SQL's restore.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DumpTarget names the server a dump is taken from or loaded into.
type DumpTarget struct {
	Instance string
	MySQL    bool
	// Created is whether the instance was created through the API, rather
	// than being a server `up` runs.
	Created bool
}

// Dumper takes and loads backups.
type Dumper interface {
	// Dump writes the target's databases into dir, which exists and is
	// empty.
	Dump(ctx context.Context, t DumpTarget, dir string) error
	// Load replaces the target's databases with those dumped in dir.
	Load(ctx context.Context, t DumpTarget, dir string) error
}

// BackupRun is one backup's record.
type BackupRun struct {
	ID              string `json:"id"`
	Project         string `json:"project"`
	Instance        string `json:"instance"`
	DatabaseVersion string `json:"databaseVersion"`
	Type            string `json:"type"` // ON_DEMAND or AUTOMATED
	Description     string `json:"description,omitempty"`
	Status          string `json:"status"`
	Error           string `json:"error,omitempty"`
	EnqueuedTime    string `json:"enqueuedTime"`
	StartTime       string `json:"startTime,omitempty"`
	EndTime         string `json:"endTime,omitempty"`
	WindowStartTime string `json:"windowStartTime"`
}

type backupStore struct {
	dumper Dumper
	// dir holds one directory per backup, named by its ID, and, when
	// persist is set, backups.json.
	dir     string
	persist bool

	mu   sync.Mutex
	runs map[string]*BackupRun
	last int64
}

// defaultRetained is how many automated backups Cloud SQL keeps by default.
const defaultRetained = 7

// EnableBackups serves backupRuns, restoreBackup and clone over d. dir is
// where the dumps are kept; with persist the records are kept there too,
// and survive `up`, otherwise dir is emptied first.
func (s *Server) EnableBackups(d Dumper, dir string, persist bool) error {
	b := &backupStore{dumper: d, dir: dir, persist: persist, runs: map[string]*BackupRun{}}
	if !persist {
		_ = os.RemoveAll(dir)
	} else if raw, err := os.ReadFile(b.recordsPath()); err == nil {
		var runs []*BackupRun
		if err := json.Unmarshal(raw, &runs); err != nil {
			return fmt.Errorf("sqladmin: %s: %w", b.recordsPath(), err)
		}
		for _, r := range runs {
			if r.Status == "RUNNING" || r.Status == "ENQUEUED" {
				r.Status, r.Error = "FAILED", "CloudBurrow stopped while the backup was being taken"
			}
			b.runs[r.ID] = r
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("sqladmin: backups: %w", err)
	}
	s.emu.Lock()
	s.backups = b
	s.emu.Unlock()
	return nil
}

func (b *backupStore) recordsPath() string { return filepath.Join(b.dir, "backups.json") }

func (b *backupStore) runDir(id string) string { return filepath.Join(b.dir, id) }

// saveLocked writes the records, with b.mu held.
func (b *backupStore) saveLocked() error {
	if !b.persist {
		return nil
	}
	runs := b.sortedLocked("", "")
	raw, err := json.MarshalIndent(runs, "", "  ")
	if err != nil {
		return err
	}
	tmp := b.recordsPath() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, b.recordsPath())
}

// sortedLocked is project's instance's runs, newest first; "" matches all.
func (b *backupStore) sortedLocked(project, instance string) []*BackupRun {
	var out []*BackupRun
	for _, r := range b.runs {
		if (project == "" || r.Project == project) && (instance == "" || r.Instance == instance) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := strconv.ParseInt(out[i].ID, 10, 64)
		c, _ := strconv.ParseInt(out[j].ID, 10, 64)
		return a > c
	})
	return out
}

// newID is a backup run's ID: Cloud SQL's are int64s, increasing.
func (b *backupStore) newIDLocked() string {
	id := time.Now().UnixMilli()
	if id <= b.last {
		id = b.last + 1
	}
	b.last = id
	return strconv.FormatInt(id, 10)
}

func (s *Server) noBackups(what string) error {
	return unimplemented(what, "this server was started without a backup store (docs/cloudsql.md)")
}

// target is the dump target for an instance's engine.
func (s *Server) target(e Engine) DumpTarget {
	s.emu.RLock()
	defer s.emu.RUnlock()
	created := s.writes != nil && s.writes.byName[e.Instance()] != nil
	return DumpTarget{Instance: e.Instance(), MySQL: strings.HasPrefix(e.DatabaseVersion(), "MYSQL_"), Created: created}
}

func backupRunJSON(base string, r *BackupRun) map[string]any {
	out := map[string]any{
		"kind": "sql#backupRun", "id": r.ID, "instance": r.Instance, "status": r.Status, "type": r.Type,
		"enqueuedTime": r.EnqueuedTime, "windowStartTime": r.WindowStartTime, "backupKind": "SNAPSHOT",
		"databaseVersion": r.DatabaseVersion, "location": Region,
		"selfLink": base + "projects/" + r.Project + "/instances/" + r.Instance + "/backupRuns/" + r.ID,
	}
	if r.Description != "" {
		out["description"] = r.Description
	}
	if r.StartTime != "" {
		out["startTime"] = r.StartTime
	}
	if r.EndTime != "" {
		out["endTime"] = r.EndTime
	}
	if r.Error != "" {
		out["error"] = map[string]any{"kind": "sql#operationError", "code": "INTERNAL_ERROR", "message": r.Error}
	}
	return out
}

// busy is an error when a created instance is not RUNNABLE.
func (s *Server) busy(name string) error {
	s.emu.RLock()
	defer s.emu.RUnlock()
	if s.writes == nil {
		return nil
	}
	if c := s.writes.byName[name]; c != nil && c.state != "RUNNABLE" {
		return Errorf(http.StatusConflict, "Operation failed because another operation was already in progress (the instance is %s).", c.state)
	}
	return nil
}

// setState sets a created instance's state, if it is one, and returns the
// state it had.
func (s *Server) setState(name, state string) string {
	s.emu.Lock()
	defer s.emu.Unlock()
	if s.writes == nil {
		return ""
	}
	c := s.writes.byName[name]
	if c == nil {
		return ""
	}
	prev := c.state
	c.state = state
	return prev
}

func (s *Server) backupRuns(r *http.Request, base, project string, e Engine, rest []string) (string, any, error) {
	b := s.backups
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		if b == nil {
			return "sql.backupRuns.list", nil, s.noBackups("backupRuns.list")
		}
		return "sql.backupRuns.list", s.listBackupRuns(r, base, project, e.Instance()), nil
	case len(rest) == 0 && r.Method == http.MethodPost:
		if b == nil {
			return "sql.backupRuns.insert", nil, s.noBackups("backupRuns.insert")
		}
		var body struct {
			Description string `json:"description"`
			Type        string `json:"type"`
		}
		if err := decode(r, &body); err != nil {
			return "sql.backupRuns.insert", nil, err
		}
		if body.Type == "AUTOMATED" {
			return "sql.backupRuns.insert", nil, Errorf(http.StatusBadRequest, "Invalid request: only an ON_DEMAND backup can be requested.")
		}
		if err := s.busy(e.Instance()); err != nil {
			return "sql.backupRuns.insert", nil, err
		}
		op, err := s.takeBackup(base, project, e, "ON_DEMAND", body.Description)
		return "sql.backupRuns.insert", op, err
	case len(rest) == 1:
		if b == nil {
			return "sql.backupRuns.get", nil, s.noBackups("backupRuns")
		}
		b.mu.Lock()
		run := b.runs[rest[0]]
		if run != nil && (run.Project != project || run.Instance != e.Instance()) {
			run = nil
		}
		var cp BackupRun
		if run != nil {
			cp = *run
		}
		b.mu.Unlock()
		if run == nil {
			return "sql.backupRuns.get", nil, Errorf(http.StatusNotFound, "The backup run %s does not exist for instance %s.", rest[0], e.Instance())
		}
		switch r.Method {
		case http.MethodGet:
			return "sql.backupRuns.get", backupRunJSON(base, &cp), nil
		case http.MethodDelete:
			if cp.Status == "RUNNING" || cp.Status == "ENQUEUED" {
				return "sql.backupRuns.delete", nil, Errorf(http.StatusConflict, "Operation failed because the backup is still being taken.")
			}
			b.mu.Lock()
			delete(b.runs, cp.ID)
			err := b.saveLocked()
			b.mu.Unlock()
			if err == nil {
				err = os.RemoveAll(b.runDir(cp.ID))
			}
			if err != nil {
				return "sql.backupRuns.delete", nil, err
			}
			return "sql.backupRuns.delete", s.done(base, project, e, "DELETE_BACKUP", e.Instance()), nil
		}
	}
	return "sql.backupRuns", nil, Errorf(http.StatusNotFound, "%s %s is not a Cloud SQL Admin API method", r.Method, r.URL.Path)
}

func (s *Server) listBackupRuns(r *http.Request, base, project, instance string) any {
	b := s.backups
	b.mu.Lock()
	runs := b.sortedLocked(project, instance)
	items := []any{}
	for _, run := range runs {
		items = append(items, backupRunJSON(base, run))
	}
	b.mu.Unlock()
	start := 0
	if t := r.URL.Query().Get("pageToken"); t != "" {
		start, _ = strconv.Atoi(t)
		if start < 0 || start > len(items) {
			start = len(items)
		}
	}
	out := map[string]any{"kind": "sql#backupRunsList"}
	end := len(items)
	if m, err := strconv.Atoi(r.URL.Query().Get("maxResults")); err == nil && m > 0 && start+m < end {
		end = start + m
		out["nextPageToken"] = strconv.Itoa(end)
	}
	out["items"] = items[start:end]
	return out
}

// takeBackup records a backup run and takes it in the background; the
// operation it returns finishes when the dump does.
func (s *Server) takeBackup(base, project string, e Engine, typ, description string) (map[string]any, error) {
	b := s.backups
	now := time.Now().UTC().Format(time.RFC3339Nano)
	b.mu.Lock()
	run := &BackupRun{ID: b.newIDLocked(), Project: project, Instance: e.Instance(), DatabaseVersion: e.DatabaseVersion(),
		Type: typ, Description: description, Status: "RUNNING", EnqueuedTime: now, StartTime: now, WindowStartTime: now}
	b.runs[run.ID] = run
	err := b.saveLocked()
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	op := s.pending(base, project, e.Instance(), "BACKUP_VOLUME")
	id := op["name"].(string)
	t := s.target(e)
	go func() {
		err := s.dump(t, run.ID)
		b.mu.Lock()
		run.EndTime = time.Now().UTC().Format(time.RFC3339Nano)
		run.Status = "SUCCESSFUL"
		if err != nil {
			run.Status, run.Error = "FAILED", err.Error()
		}
		_ = b.saveLocked()
		b.mu.Unlock()
		if err == nil {
			err = s.takePITRBase(e.Instance())
		}
		if err == nil && typ == "AUTOMATED" {
			s.pruneAutomated(project, e.Instance())
		}
		s.finish(id, err)
	}()
	return op, nil
}

func (s *Server) dump(t DumpTarget, id string) error {
	b := s.backups
	dir := b.runDir(id)
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	if err := b.dumper.Dump(ctx, t, dir); err != nil {
		_ = os.RemoveAll(dir)
		return err
	}
	return nil
}

// pruneAutomated keeps the newest retained automated backups of an
// instance, as Cloud SQL's backupRetentionSettings do.
func (s *Server) pruneAutomated(project, instance string) {
	keep := defaultRetained
	s.emu.RLock()
	if s.writes != nil {
		if c := s.writes.byName[instance]; c != nil {
			if n := retainedBackups(c.spec); n > 0 {
				keep = n
			}
		}
	}
	s.emu.RUnlock()
	b := s.backups
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, r := range b.sortedLocked(project, instance) {
		if r.Type != "AUTOMATED" || r.Status != "SUCCESSFUL" {
			continue
		}
		if n++; n > keep {
			delete(b.runs, r.ID)
			_ = os.RemoveAll(b.runDir(r.ID))
		}
	}
	_ = b.saveLocked()
}

// removeInstanceBackups deletes a deleted instance's backups, as deleting
// a Cloud SQL instance deletes its backups.
func (s *Server) removeInstanceBackups(project, instance string) {
	b := s.backups
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, r := range b.sortedLocked(project, instance) {
		delete(b.runs, r.ID)
		_ = os.RemoveAll(b.runDir(r.ID))
	}
	_ = b.saveLocked()
}

func retainedBackups(spec Spec) int {
	bc, _ := spec.Settings["backupConfiguration"].(map[string]any)
	rs, _ := bc["backupRetentionSettings"].(map[string]any)
	switch v := rs["retainedBackups"].(type) {
	case float64:
		return int(v)
	case string:
		n, _ := strconv.Atoi(v)
		return n
	}
	return 0
}

// restoreBackup is instances.restoreBackup: the backup run, of this
// instance or another of the same engine, is loaded into this instance.
func (s *Server) restoreBackup(r *http.Request, base, project string, e Engine) (any, error) {
	b := s.backups
	if b == nil {
		return nil, s.noBackups("instances.restoreBackup")
	}
	var body struct {
		Ctx *struct {
			BackupRunID json.Number `json:"backupRunId"`
			InstanceID  string      `json:"instanceId"`
			Project     string      `json:"project"`
		} `json:"restoreBackupContext"`
		Backup              string          `json:"backup"`
		BackupdrBackup      string          `json:"backupdrBackup"`
		RestoreInstanceSets json.RawMessage `json:"restoreInstanceSettings"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if body.Backup != "" || body.BackupdrBackup != "" {
		return nil, unimplemented("restoring a backup by its Backups resource name", "the project-level Backups API is not served; restore a backup run by restoreBackupContext.backupRunId")
	}
	if body.Ctx == nil || body.Ctx.BackupRunID == "" {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: restoreBackupContext.backupRunId is required.")
	}
	srcProject, srcInstance := project, e.Instance()
	if body.Ctx.Project != "" {
		srcProject = body.Ctx.Project
	}
	if body.Ctx.InstanceID != "" {
		srcInstance = body.Ctx.InstanceID
	}
	b.mu.Lock()
	run := b.runs[body.Ctx.BackupRunID.String()]
	var cp BackupRun
	if run != nil {
		cp = *run
	}
	b.mu.Unlock()
	if run == nil || cp.Project != srcProject || cp.Instance != srcInstance {
		return nil, Errorf(http.StatusNotFound, "The backup run %s does not exist for instance %s.", body.Ctx.BackupRunID, srcInstance)
	}
	if cp.Status != "SUCCESSFUL" {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: the backup run %s is %s, not SUCCESSFUL.", cp.ID, cp.Status)
	}
	if engineOf(cp.DatabaseVersion) != engineOf(e.DatabaseVersion()) {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: a %s backup cannot be restored into a %s instance.", cp.DatabaseVersion, e.DatabaseVersion())
	}
	if err := s.busy(e.Instance()); err != nil {
		return nil, err
	}
	t := s.target(e)
	prev := s.setState(e.Instance(), "MAINTENANCE")
	op := s.pending(base, project, e.Instance(), "RESTORE_VOLUME")
	id := op["name"].(string)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
		defer cancel()
		err := b.dumper.Load(ctx, t, b.runDir(cp.ID))
		if prev != "" {
			s.setState(e.Instance(), prev)
		}
		s.finish(id, err)
	}()
	return op, nil
}

func engineOf(version string) string { return strings.SplitN(version, "_", 2)[0] }

// cloneInstance is instances.clone: a new instance with the source's
// settings, into which the source's databases, dumped now, are loaded.
func (s *Server) cloneInstance(r *http.Request, base, project string, e Engine) (any, error) {
	if s.writes == nil {
		return nil, s.noWrites("instances.clone")
	}
	if s.backups == nil {
		return nil, s.noBackups("instances.clone")
	}
	var body struct {
		Ctx *struct {
			Destination      string          `json:"destinationInstanceName"`
			PointInTime      string          `json:"pointInTime"`
			BinLog           json.RawMessage `json:"binLogCoordinates"`
			DatabaseNames    []string        `json:"databaseNames"`
			AllocatedIPRange string          `json:"allocatedIpRange"`
		} `json:"cloneContext"`
	}
	if err := decode(r, &body); err != nil {
		return nil, err
	}
	if body.Ctx == nil || body.Ctx.Destination == "" {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: cloneContext.destinationInstanceName is required.")
	}
	if len(body.Ctx.BinLog) > 0 {
		return nil, unimplemented("cloning to binary log coordinates", "a clone is recovered to cloneContext.pointInTime (docs/cloudsql.md)")
	}
	var at time.Time
	if body.Ctx.PointInTime != "" {
		var err error
		if at, err = s.checkPointInTime(e, body.Ctx.PointInTime); err != nil {
			return nil, err
		}
	}
	if len(body.Ctx.DatabaseNames) > 0 {
		return nil, unimplemented("cloning some databases only", "a clone copies every database of the source")
	}
	dest := body.Ctx.Destination
	if !instanceNameRE.MatchString(dest) || len(dest) > maxInstanceName {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: instance name %q must be lowercase letters, digits and hyphens, "+
			"start with a letter, not end with a hyphen, and be at most %d characters in CloudBurrow.", dest, maxInstanceName)
	}
	if err := s.busy(e.Instance()); err != nil {
		return nil, err
	}
	spec := Spec{Tier: "db-custom-1-3840", AvailabilityType: "ZONAL"}
	s.emu.RLock()
	if c := s.writes.byName[e.Instance()]; c != nil {
		spec = c.spec
	} else if m, ok := e.(*MySQL); ok {
		spec.RootPassword = m.RootPassword
	}
	s.emu.RUnlock()
	spec.Name, spec.Project, spec.DatabaseVersion = dest, project, e.DatabaseVersion()
	spec.CreateTime, spec.SettingsVersion, spec.DeletionProtection = time.Now().UTC().Format(time.RFC3339Nano), 1, false
	if !SupportedVersions[spec.DatabaseVersion] {
		return nil, unimplemented("cloning a "+spec.DatabaseVersion+" instance", "a created instance runs POSTGRES_17 or MYSQL_8_4")
	}
	src := s.target(e)
	s.emu.Lock()
	if s.engineLocked(dest) != nil {
		s.emu.Unlock()
		return nil, Errorf(http.StatusConflict, "The Cloud SQL instance already exists.")
	}
	c := &created{spec: spec, engine: s.writes.prov.Engine(spec), state: "PENDING_CREATE"}
	s.writes.byName[dest] = c
	err := s.saveLocked()
	s.emu.Unlock()
	if err != nil {
		return nil, err
	}
	if s.conn != nil {
		if err := s.conn.addInstance(dest); err != nil {
			return nil, err
		}
	}
	op := s.pending(base, project, dest, "CLONE")
	id := op["name"].(string)
	go func() {
		s.bringUp(dest, "")
		s.emu.RLock()
		state, failure := c.state, c.failure
		s.emu.RUnlock()
		if state != "RUNNABLE" {
			s.finish(id, fmt.Errorf("the clone's server did not start: %s", failure))
			return
		}
		s.setState(dest, "PENDING_CREATE")
		dst := DumpTarget{Instance: dest, MySQL: spec.MySQL(), Created: true}
		var err error
		if at.IsZero() {
			err = s.copyData(src, dst)
		} else {
			err = s.copyAt(src, dst, at)
		}
		if err == nil {
			err = s.takePITRBase(dest)
		}
		s.emu.Lock()
		if s.writes.byName[dest] == c && c.state == "PENDING_CREATE" {
			c.state = "RUNNABLE"
			if err != nil {
				c.state, c.failure = "FAILED", err.Error()
			}
		}
		s.emu.Unlock()
		s.finish(id, err)
	}()
	return op, nil
}

// copyData dumps src into a scratch directory and loads it into dst.
func (s *Server) copyData(src, dst DumpTarget) error {
	b := s.backups
	dir, err := os.MkdirTemp(b.dir, "clone-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	if err := b.dumper.Dump(ctx, src, dir); err != nil {
		return fmt.Errorf("dump %s: %w", src.Instance, err)
	}
	if err := b.dumper.Load(ctx, dst, dir); err != nil {
		return fmt.Errorf("load into %s: %w", dst.Instance, err)
	}
	return nil
}

// backupConfig is a created instance's backupConfiguration as recorded.
func backupConfig(spec Spec) (enabled bool, startTime string) {
	bc, _ := spec.Settings["backupConfiguration"].(map[string]any)
	st, _ := bc["startTime"].(string)
	return spec.BackupEnabled, st
}

// automate takes each created instance's automated backup when its
// startTime comes, checking once a minute. A backup whose time passes
// while CloudBurrow is not running is not taken afterwards.
func (s *Server) automate(stop <-chan struct{}) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			s.automatedTick(now.UTC())
		}
	}
}

func (s *Server) automatedTick(now time.Time) {
	hm := now.Format("15:04")
	type due struct {
		project string
		engine  Engine
	}
	var dues []due
	s.emu.RLock()
	if s.writes != nil {
		for _, c := range s.writes.byName {
			if on, st := backupConfig(c.spec); on && st == hm && c.state == "RUNNABLE" {
				dues = append(dues, due{c.spec.Project, c.engine})
			}
		}
	}
	s.emu.RUnlock()
	b := s.backups
	for _, d := range dues {
		b.mu.Lock()
		recent := false
		for _, r := range b.sortedLocked(d.project, d.engine.Instance()) {
			if r.Type != "AUTOMATED" {
				continue
			}
			if t, err := time.Parse(time.RFC3339Nano, r.EnqueuedTime); err == nil && now.Sub(t) < 23*time.Hour {
				recent = true
			}
			break
		}
		b.mu.Unlock()
		if !recent {
			_, _ = s.takeBackup("http://"+s.Addr()+"/v1/", d.project, d.engine, "AUTOMATED", "")
		}
	}
}
