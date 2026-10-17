package sqladmin

// Instance writes (#1330): instances.insert, patch, update, delete and
// restart, each over a real server. A created instance is a server of its
// own, from the same image and the same component code as the servers `up`
// runs, started by a Provisioner (cmd/cloudburrow's runs it in the
// cluster, with its own tunnel); the instances `up` runs stay as they are.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Flag is one database flag.
type Flag struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Spec is what an instance was created or patched with: the record a
// created instance is kept as.
type Spec struct {
	Name            string `json:"name"`
	Project         string `json:"project"`
	DatabaseVersion string `json:"databaseVersion"`
	// RootPassword is MySQL's root password; never answered.
	RootPassword       string            `json:"rootPassword,omitempty"`
	Tier               string            `json:"tier"`
	Labels             map[string]string `json:"labels,omitempty"`
	Flags              []Flag            `json:"flags,omitempty"`
	DeletionProtection bool              `json:"deletionProtection,omitempty"`
	BackupEnabled      bool              `json:"backupEnabled,omitempty"`
	AvailabilityType   string            `json:"availabilityType,omitempty"`
	ActivationPolicy   string            `json:"activationPolicy,omitempty"`
	CreateTime         string            `json:"createTime"`
	// Settings are the other settings the instance was given (disk size
	// and type, maintenance window, insights, ...), recorded and answered
	// as given: they describe the machine Cloud SQL would run, which the
	// server here does not change.
	Settings        map[string]any `json:"settings,omitempty"`
	SettingsVersion int            `json:"settingsVersion"`
	// MasterInstanceName is a read replica's primary (#1330); MasterUp is
	// whether it is a server `up` runs. ReplicationPassword is the
	// primary's replication user's, never answered.
	MasterInstanceName  string `json:"masterInstanceName,omitempty"`
	MasterUp            bool   `json:"masterUp,omitempty"`
	ReplicationPassword string `json:"replicationPassword,omitempty"`
	// ReplicationStopped is set by stopReplica.
	ReplicationStopped bool `json:"replicationStopped,omitempty"`
	// Region is the region the instance was created in, "" for Region
	// (#1381): a cross-region replica's differs from its primary's. Every
	// server runs in the one cluster; the region is what the instance
	// reports, in its connection name too.
	Region string `json:"region,omitempty"`
	// HAPrimary is which of a high-availability instance's two servers is
	// its primary (#1408): "" or "a", the server the instance was created
	// with, or "b", its second. A failover swaps them; a ZONAL instance
	// keeps the one it was left with.
	HAPrimary string `json:"haPrimary,omitempty"`
	// HAEpochs are, per server, the failover a server was last rebuilt as
	// a standby at: 0 for a server that never was one. A server rebuilds
	// its data from the primary when its epoch changes, and keeps it
	// otherwise, so a promoted standby keeps its data across restarts.
	HAEpochs [2]int `json:"haEpochs,omitempty"`
	// HAPassword is the standby's replication user's; never answered.
	HAPassword string `json:"haPassword,omitempty"`
	// External is the outside server a replica of a source representation
	// instance replicates from (#1501); ExternalSyncStarted is set by
	// startExternalSync, before which the replica's server runs idle.
	External            *ExternalSource `json:"external,omitempty"`
	ExternalSyncStarted bool            `json:"externalSyncStarted,omitempty"`
}

// Regional reports whether the instance has a standby (#1408).
func (s Spec) Regional() bool { return s.AvailabilityType == "REGIONAL" }

// PrimaryNode is the index of the primary's server, 0 ("a") or 1 ("b").
func (s Spec) PrimaryNode() int {
	if s.HAPrimary == "b" {
		return 1
	}
	return 0
}

// zone is the zone of a server, by index: the instance's gceZone, and its
// secondaryGceZone for the standby, as Cloud SQL reports them.
func (s Spec) zone(node int) string {
	return s.region() + "-" + string(rune('a'+node))
}

// region is the instance's region.
func (s Spec) region() string {
	if s.Region == "" {
		return Region
	}
	return s.Region
}

var regionRE = regexp.MustCompile(`^[a-z]+(-[a-z]+)+[0-9]+$`)

// MySQL reports whether the spec is a MySQL instance.
func (s Spec) MySQL() bool { return strings.HasPrefix(s.DatabaseVersion, "MYSQL_") }

// Provisioner runs created instances' servers.
type Provisioner interface {
	// Engine is spec's server, whose Addr is "" until Start has brought
	// its tunnel up.
	Engine(spec Spec) Engine
	// Start starts spec's server, or applies a changed spec to it, and
	// returns once the server serves through its tunnel.
	Start(ctx context.Context, spec Spec) error
	// Restart restarts the server's pod and returns once it serves again.
	Restart(ctx context.Context, spec Spec) error
	// Remove deletes the server and its volume.
	Remove(ctx context.Context, name string) error
	// Prune removes every created server not named in keep: what an
	// earlier run left that this one does not keep.
	Prune(ctx context.Context, keep []string) error
	// Failover makes a REGIONAL instance's standby its primary (#1408):
	// from is the instance before, to after, with HAPrimary swapped and
	// the old primary's epoch advanced, so it is rebuilt as the standby.
	// The instance's endpoint, its host tunnel and connector route, reach
	// the new primary once it returns.
	Failover(ctx context.Context, from, to Spec) error
}

type created struct {
	spec   Spec
	engine Engine
	// state is the instance's Cloud SQL state: PENDING_CREATE while its
	// server starts, MAINTENANCE while it restarts or applies a patch,
	// RUNNABLE, FAILED or, once deletion has begun, PENDING_DELETE.
	state string
	// failure is why the server did not start, for a FAILED instance.
	failure string
}

// instances are the created instances, guarded by Server.emu.
type instances struct {
	prov Provisioner
	// path is where the records are kept, "" for an ephemeral instance.
	path   string
	byName map[string]*created
	// sources are the source representation instances (#1501).
	sources map[string]*SourceRep
}

// EnableInstanceWrites serves instances.insert, patch, update, delete and
// restart over p. statePath is the file the records are kept in, for a
// persistent instance; "" keeps none, and a created server then goes with
// the next `up`.
func (s *Server) EnableInstanceWrites(p Provisioner, statePath string) {
	s.emu.Lock()
	defer s.emu.Unlock()
	s.writes = &instances{prov: p, path: statePath, byName: map[string]*created{}, sources: map[string]*SourceRep{}}
}

// restore brings back the recorded instances and prunes the rest, in the
// background once the server is listening.
func (s *Server) restore() {
	w := s.writes
	var specs []Spec
	if w.path != "" {
		if b, err := os.ReadFile(w.path); err == nil {
			_ = json.Unmarshal(b, &specs)
		}
	}
	var keep []string
	s.emu.Lock()
	s.restoreSourcesLocked()
	for _, sp := range specs {
		if s.engineLocked(sp.Name) != nil || !SupportedVersions[sp.DatabaseVersion] {
			continue
		}
		s.writes.byName[sp.Name] = &created{spec: sp, engine: w.prov.Engine(sp), state: "PENDING_CREATE"}
		keep = append(keep, sp.Name)
	}
	s.emu.Unlock()
	go func() {
		ctx := context.Background()
		_ = w.prov.Prune(ctx, keep)
		for _, name := range keep {
			go s.bringUp(name, "")
		}
	}()
}

// save writes the records, for a persistent instance. Called with emu held.
func (s *Server) saveLocked() error {
	w := s.writes
	if w.path == "" {
		return nil
	}
	specs := []Spec{}
	for _, c := range w.byName {
		specs = append(specs, c.spec)
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	b, err := json.MarshalIndent(specs, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(w.path), 0o700); err != nil {
		return err
	}
	tmp := w.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, w.path)
}

// bringUp starts name's server and marks the instance RUNNABLE, or FAILED
// with the server's words. opID, when set, is the operation it finishes.
func (s *Server) bringUp(name, opID string) {
	s.emu.RLock()
	c := s.writes.byName[name]
	var spec Spec
	if c != nil {
		spec = c.spec
	}
	s.emu.RUnlock()
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()
	err := s.writes.prov.Start(ctx, spec)
	if err == nil && spec.PITREnabled() && spec.MasterInstanceName == "" {
		// The first base, so the instance is recoverable from its start.
		if err = s.takePITRBase(name); err != nil {
			err = fmt.Errorf("take the point-in-time recovery base: %w", err)
		}
	}
	s.emu.Lock()
	if cur := s.writes.byName[name]; cur == c && c.state != "PENDING_DELETE" {
		if err != nil {
			c.state, c.failure = "FAILED", err.Error()
		} else {
			c.state, c.failure = "RUNNABLE", ""
		}
	}
	s.emu.Unlock()
	if opID != "" {
		s.finish(opID, err)
	}
}

// startTimeout bounds a server's start: an image already in the cluster
// starts in seconds, a first MySQL initialises its data directory in
// about half a minute.
const startTimeout = 5 * time.Minute

var instanceNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]*[a-z0-9]$`)

// maxInstanceName keeps the server's Kubernetes names, "sqlinst-" and the
// instance's name, within the 63 characters a Service's name may have.
const maxInstanceName = 55

const upInstanceWhy = "it is the server `cloudburrow up` runs for --services %s; it stays while that service is " +
	"enabled and goes when it is not (docs/cloudsql.md). Create an instance with instances.insert to change or delete one"

func (s *Server) upInstanceError(e Engine) error {
	svc := "cloudsql"
	if strings.HasPrefix(e.DatabaseVersion(), "MYSQL_") {
		svc = "cloudsql-mysql"
	}
	return Errorf(http.StatusBadRequest, "The instance %s cannot be changed or deleted through the API: "+upInstanceWhy, e.Instance(), svc)
}

// legacyFailoverErr is Cloud SQL's answer to a failoverReplica or a
// replicaConfiguration.failoverTarget (#1462): the legacy MySQL
// high-availability configuration, deprecated for every instance on
// 2025-01-13, with which "you can't create instances" nor enable it on an
// existing one (docs.cloud.google.com/sql/docs/mysql/configure-legacy-ha).
// High availability is availabilityType REGIONAL (#1408).
func legacyFailoverErr() error {
	return Errorf(http.StatusBadRequest, "Invalid request: the legacy high availability configuration (a failover replica) is deprecated, "+
		"and instances can't be created with it or have it enabled; set settings.availabilityType to REGIONAL for high availability.")
}

// instanceBody is the part of DatabaseInstance a write reads.
type instanceBody struct {
	raw map[string]any // settings, as sent

	Name                 string           `json:"name"`
	DatabaseVersion      string           `json:"databaseVersion"`
	RootPassword         string           `json:"rootPassword"`
	MasterInstanceName   string           `json:"masterInstanceName"`
	InstanceType         string           `json:"instanceType"`
	Region               string           `json:"region"`
	ReplicaConfiguration *json.RawMessage `json:"replicaConfiguration"`
	FailoverReplica      *json.RawMessage `json:"failoverReplica"`
	// IncludeReplicas is includeReplicasForMajorVersionUpgrade (#1421).
	IncludeReplicas bool `json:"includeReplicasForMajorVersionUpgrade"`
	// OnPremisesConfiguration and instanceType ON_PREMISES_INSTANCE make a
	// source representation instance: a record of a server outside Cloud
	// SQL (#1304), Terraform's google_sql_source_representation_instance.
	OnPremisesConfiguration *json.RawMessage `json:"onPremisesConfiguration"`
	Settings                *struct {
		Tier                      string             `json:"tier"`
		UserLabels                *map[string]string `json:"userLabels"`
		DatabaseFlags             *[]Flag            `json:"databaseFlags"`
		DeletionProtectionEnabled *bool              `json:"deletionProtectionEnabled"`
		AvailabilityType          string             `json:"availabilityType"`
		ActivationPolicy          string             `json:"activationPolicy"`
		BackupConfiguration       *struct {
			Enabled                    *bool  `json:"enabled"`
			StartTime                  string `json:"startTime"`
			PointInTimeRecoveryEnabled bool   `json:"pointInTimeRecoveryEnabled"`
			BinaryLogEnabled           bool   `json:"binaryLogEnabled"`
		} `json:"backupConfiguration"`
	} `json:"settings"`
}

func (b instanceBody) unsupported() error {
	if b.FailoverReplica != nil {
		return legacyFailoverErr()
	}
	if b.InstanceType == "ON_PREMISES_INSTANCE" || b.OnPremisesConfiguration != nil {
		return Errorf(http.StatusBadRequest, "Invalid request: an instance's type cannot be changed to ON_PREMISES_INSTANCE; "+
			"insert a source representation instance of its own.")
	}
	if b.Settings != nil {
		if a := b.Settings.AvailabilityType; a != "" && a != "ZONAL" && a != "REGIONAL" {
			return Errorf(http.StatusBadRequest, "Invalid request: Invalid value for availabilityType: %s.", a)
		}
		if bc, ok := b.raw["backupConfiguration"].(map[string]any); ok {
			ed, _ := b.raw["edition"].(string)
			if err := retentionInvalid(bc, ed); err != nil {
				return err
			}
		}
		if bc := b.Settings.BackupConfiguration; bc != nil {
			if bc.StartTime != "" {
				if _, err := time.Parse("15:04", bc.StartTime); err != nil {
					return Errorf(http.StatusBadRequest, "Invalid request: backupConfiguration.startTime %q is not HH:MM.", bc.StartTime)
				}
			}
		}
		if b.Settings.ActivationPolicy == "NEVER" || b.Settings.ActivationPolicy == "ON_DEMAND" {
			return unimplemented("activationPolicy "+b.Settings.ActivationPolicy, "a created instance's server always runs")
		}
		if b.Settings.DatabaseFlags != nil {
			for _, f := range *b.Settings.DatabaseFlags {
				if f.Name == "" {
					return Errorf(http.StatusBadRequest, "Invalid request: a database flag has no name.")
				}
				if LogicalDecodingFlags[f.Name] {
					// An external primary's prerequisites (#1524).
				} else if strings.HasPrefix(f.Name, "cloudsql.") || strings.HasPrefix(f.Name, "cloudsql_") {
					return unimplemented("the database flag "+f.Name,
						"it is a Cloud SQL flag, not the server's (IAM authentication and Cloud SQL's extensions are not served); only flags the server itself takes are passed to it")
				}
				if strings.ContainsAny(f.Name, " =\n") || strings.ContainsAny(f.Value, "\n") {
					return Errorf(http.StatusBadRequest, "Invalid request: database flag %q is not a flag name.", f.Name)
				}
			}
		}
	}
	return nil
}

// apply copies a patch's settings onto spec; replace is update's
// semantics, where an absent setting is reset.
func (b instanceBody) apply(spec *Spec, replace bool) {
	st := b.Settings
	if replace {
		spec.Labels, spec.Flags, spec.DeletionProtection, spec.Settings = nil, nil, false, nil
	}
	if len(b.raw) > 0 {
		next := map[string]any{}
		for k, v := range spec.Settings {
			next[k] = v
		}
		for k, v := range b.raw {
			next[k] = v
		}
		spec.Settings = next
	}
	if st == nil {
		return
	}
	if st.Tier != "" {
		spec.Tier = st.Tier
	}
	if st.UserLabels != nil {
		spec.Labels = *st.UserLabels
	}
	if st.DatabaseFlags != nil {
		spec.Flags = *st.DatabaseFlags
	}
	if st.DeletionProtectionEnabled != nil {
		spec.DeletionProtection = *st.DeletionProtectionEnabled
	}
	if st.AvailabilityType != "" {
		spec.AvailabilityType = st.AvailabilityType
	}
	if st.BackupConfiguration != nil && st.BackupConfiguration.Enabled != nil {
		spec.BackupEnabled = *st.BackupConfiguration.Enabled
	}
}

func decodeInstance(r *http.Request) (instanceBody, error) {
	var b instanceBody
	var raw struct {
		Settings map[string]any `json:"settings"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return b, Errorf(http.StatusBadRequest, "Invalid request: %v", err)
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return b, nil
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return b, Errorf(http.StatusBadRequest, "Invalid JSON payload received: %v", err)
	}
	_ = json.Unmarshal(body, &raw)
	b.raw = raw.Settings
	return b, nil
}

func flagsEqual(a, b []Flag) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (s *Server) noWrites(what string) error {
	return unimplemented(what, instanceWhy)
}

// insertInstance is instances.insert.
func (s *Server) insertInstance(r *http.Request, base, project string) (any, error) {
	if s.writes == nil {
		return nil, s.noWrites("instances.insert")
	}
	b, err := decodeInstance(r)
	if err != nil {
		return nil, err
	}
	if b.Name == "" {
		return nil, Errorf(http.StatusBadRequest, "Missing parameter: name.")
	}
	if !instanceNameRE.MatchString(b.Name) || len(b.Name) > maxInstanceName {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: instance name %q must be lowercase letters, digits and hyphens, "+
			"start with a letter, not end with a hyphen, and be at most %d characters in CloudBurrow.", b.Name, maxInstanceName)
	}
	if b.InstanceType == "ON_PREMISES_INSTANCE" || b.OnPremisesConfiguration != nil {
		return s.insertSource(base, project, b)
	}
	replica := b.MasterInstanceName != ""
	if !replica && b.ReplicaConfiguration != nil {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: replicaConfiguration needs masterInstanceName.")
	}
	if b.DatabaseVersion == "" && !replica {
		return nil, Errorf(http.StatusBadRequest, "Missing parameter: databaseVersion.")
	}
	if b.DatabaseVersion != "" && !SupportedVersions[b.DatabaseVersion] {
		if strings.HasPrefix(b.DatabaseVersion, "POSTGRES_") || strings.HasPrefix(b.DatabaseVersion, "MYSQL_") {
			return nil, unimplemented("databaseVersion "+b.DatabaseVersion, unservedVersionWhy(b.DatabaseVersion))
		}
		if strings.HasPrefix(b.DatabaseVersion, "SQLSERVER_") {
			return nil, unimplemented("databaseVersion "+b.DatabaseVersion, sqlServerVersionWhy)
		}
		return nil, Errorf(http.StatusBadRequest, "Invalid request: Invalid value for databaseVersion: %s.", b.DatabaseVersion)
	}
	if err := b.unsupported(); err != nil {
		return nil, err
	}
	spec := Spec{Name: b.Name, Project: project, DatabaseVersion: b.DatabaseVersion, RootPassword: b.RootPassword,
		CreateTime: time.Now().UTC().Format(time.RFC3339Nano), SettingsVersion: 1, AvailabilityType: "ZONAL"}
	b.apply(&spec, false)
	if b.Region != "" {
		if !regionRE.MatchString(b.Region) {
			return nil, Errorf(http.StatusBadRequest, "Invalid request: Invalid value for region: %s.", b.Region)
		}
		if b.Region != Region {
			spec.Region = b.Region
		}
	}
	if replica && spec.Regional() {
		return nil, unimplemented("availabilityType REGIONAL on a read replica",
			"a read replica here is one server; make the primary REGIONAL, and its replicas follow it across a failover (docs/cloudsql.md)")
	}
	if spec.Regional() {
		// The second server, b, is built as the standby.
		spec.HAPassword, spec.HAEpochs[1] = newReplicationPassword(), 1
	}
	if replica {
		if s.engine(b.Name) != nil {
			return nil, Errorf(http.StatusConflict, "The Cloud SQL instance already exists.")
		}
		if err := s.prepareReplica(r.Context(), project, b, &spec); err != nil {
			return nil, err
		}
		b.DatabaseVersion = spec.DatabaseVersion
		if err := pgBefore12Why(spec.DatabaseVersion, "a read replica"); err != nil {
			return nil, err
		}
	} else if b.Settings == nil || b.Settings.Tier == "" {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: Invalid request since instance settings are missing tier.")
	}
	if err := spec.pgBefore12Unsupported(nil); err != nil {
		return nil, err
	}
	if !s.engineServed(b.DatabaseVersion) {
		svc := "cloudsql"
		if strings.HasPrefix(b.DatabaseVersion, "MYSQL_") {
			svc = "cloudsql-mysql"
		}
		return nil, Errorf(http.StatusPreconditionFailed, "Invalid request: a %s instance runs the image of --services %s, "+
			"which this instance was not started with; run `cloudburrow up --services ...,%s`.", b.DatabaseVersion, svc, svc)
	}
	if !spec.MySQL() {
		// PostgreSQL's postgres user trusts every connection, as the server
		// `up` runs does (docs/cloudsql.md): no password is kept.
		spec.RootPassword = ""
	}
	s.emu.Lock()
	if s.engineLocked(b.Name) != nil || s.writes.sources[b.Name] != nil {
		s.emu.Unlock()
		return nil, Errorf(http.StatusConflict, "The Cloud SQL instance already exists.")
	}
	c := &created{spec: spec, engine: s.writes.prov.Engine(spec), state: "PENDING_CREATE"}
	s.writes.byName[b.Name] = c
	err = s.saveLocked()
	s.emu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("sqladmin: keep instance %s: %w", b.Name, err)
	}
	if s.conn != nil {
		if err := s.conn.addInstance(b.Name); err != nil {
			return nil, err
		}
	}
	op := s.pending(base, project, b.Name, "CREATE")
	go s.bringUp(b.Name, op["name"].(string))
	return op, nil
}

// engineServed is whether a server of version's engine runs, so its image
// is in the cluster.
func (s *Server) engineServed(version string) bool {
	for _, e := range s.engines {
		if strings.SplitN(e.DatabaseVersion(), "_", 2)[0] == strings.SplitN(version, "_", 2)[0] {
			return true
		}
	}
	return false
}

// createdFor is the created instance name in project, or an error that
// answers for it: a server `up` runs, or none.
func (s *Server) createdFor(project, name, what string) (*created, error) {
	if s.writes == nil {
		return nil, s.noWrites(what)
	}
	s.emu.RLock()
	c := s.writes.byName[name]
	s.emu.RUnlock()
	if c == nil {
		for _, e := range s.engines {
			if e.Instance() == name {
				return nil, s.upInstanceError(e)
			}
		}
		return nil, Errorf(http.StatusNotFound, "The Cloud SQL instance does not exist.")
	}
	if c.spec.Project != project {
		return nil, Errorf(http.StatusNotFound, "The Cloud SQL instance does not exist.")
	}
	return c, nil
}

// patchInstance is instances.patch and, with replace, instances.update.
func (s *Server) patchInstance(r *http.Request, base, project, name string, replace bool) (any, error) {
	c, err := s.createdFor(project, name, "instances.patch and update")
	if err != nil {
		return nil, err
	}
	b, err := decodeInstance(r)
	if err != nil {
		return nil, err
	}
	if err := b.unsupported(); err != nil {
		return nil, err
	}
	upgrade := b.DatabaseVersion != "" && b.DatabaseVersion != c.spec.DatabaseVersion
	if b.Name != "" && b.Name != name {
		return nil, Errorf(http.StatusBadRequest, "Invalid request: an instance cannot be renamed.")
	}
	s.emu.Lock()
	if c.state != "RUNNABLE" && c.state != "FAILED" {
		st := c.state
		s.emu.Unlock()
		return nil, Errorf(http.StatusConflict, "Operation failed because another operation was already in progress (the instance is %s).", st)
	}
	if upgrade {
		if err := s.checkUpgrade(c, b.DatabaseVersion, b.IncludeReplicas); err != nil {
			s.emu.Unlock()
			return nil, err
		}
		if c.state != "RUNNABLE" {
			s.emu.Unlock()
			return nil, Errorf(http.StatusConflict, "Operation failed because the instance is %s.", c.state)
		}
		if c.spec.MasterInstanceName != "" {
			// A MySQL replica upgraded on its own (#1462) is rebuilt on the
			// new version from its primary, which goes on being the older
			// version's, as a replica created fresh would be.
			c.state = "MAINTENANCE"
			err = s.saveLocked()
			s.emu.Unlock()
			if err != nil {
				return nil, err
			}
			op := s.pending(base, project, name, "MAJOR_VERSION_UPGRADE")
			opID, version, include := op["name"].(string), b.DatabaseVersion, b.IncludeReplicas
			go func() {
				err := s.upgradeReplica(name, version)
				if err == nil && include {
					err = s.upgradeReplicas(project, name, version)
				}
				s.finish(opID, err)
			}()
			return op, nil
		}
		from := c.spec
		to := c.spec
		// Settings sent with the new version apply to the new server.
		b.apply(&to, replace)
		to.DatabaseVersion = b.DatabaseVersion
		if err := to.pgBefore12Unsupported(&from); err != nil {
			s.emu.Unlock()
			return nil, err
		}
		to.SettingsVersion++
		c.spec = to
		c.engine = s.writes.prov.Engine(to)
		c.state = "MAINTENANCE"
		err = s.saveLocked()
		s.emu.Unlock()
		if err != nil {
			return nil, err
		}
		op := s.pending(base, project, name, "MAJOR_VERSION_UPGRADE")
		go s.upgrade(name, from, to, op["name"].(string), b.IncludeReplicas)
		return op, nil
	}
	next := c.spec
	b.apply(&next, replace)
	if replace && (b.Settings == nil || b.Settings.Tier == "") {
		s.emu.Unlock()
		return nil, Errorf(http.StatusBadRequest, "Invalid request: Invalid request since instance settings are missing tier.")
	}
	if next.Regional() && next.MasterInstanceName != "" {
		s.emu.Unlock()
		return nil, unimplemented("availabilityType REGIONAL on a read replica",
			"a read replica here is one server; make the primary REGIONAL, and its replicas follow it across a failover (docs/cloudsql.md)")
	}
	if err := next.pgBefore12Unsupported(&c.spec); err != nil {
		s.emu.Unlock()
		return nil, err
	}
	// Turning high availability on adds the standby, and off removes it
	// (#1408); either is a change the provisioner applies.
	haChange := next.Regional() != c.spec.Regional()
	if haChange && next.Regional() {
		if next.HAPassword == "" {
			next.HAPassword = newReplicationPassword()
		}
		// The standby is built afresh.
		sb := 1 - next.PrimaryNode()
		next.HAEpochs[sb] = max(next.HAEpochs[0], next.HAEpochs[1]) + 1
	}
	restart := !flagsEqual(next.Flags, c.spec.Flags) || c.state == "FAILED" || haChange
	next.SettingsVersion++
	c.spec = next
	if restart {
		c.state = "MAINTENANCE"
	}
	err = s.saveLocked()
	s.emu.Unlock()
	if err != nil {
		return nil, err
	}
	opType := "UPDATE"
	if !restart {
		return s.done(base, project, c.engine, opType, name), nil
	}
	// The flags are the server's arguments: the server is started again
	// with them, as Cloud SQL restarts an instance for most flags.
	op := s.pending(base, project, name, opType)
	go s.bringUp(name, op["name"].(string))
	return op, nil
}

// deleteInstance is instances.delete.
func (s *Server) deleteInstance(r *http.Request, base, project, name string) (any, error) {
	c, err := s.createdFor(project, name, "instances.delete")
	if err != nil {
		return nil, err
	}
	fb, err := s.finalBackupOf(r)
	if err != nil {
		return nil, err
	}
	s.emu.Lock()
	if c.spec.DeletionProtection {
		s.emu.Unlock()
		return nil, Errorf(http.StatusBadRequest, "The instance is protected. Please disable the deletion protection and try again. "+
			"To disable deletion protection, update the instance settings with deletionProtectionEnabled set to false.")
	}
	if c.state == "PENDING_DELETE" {
		s.emu.Unlock()
		return nil, Errorf(http.StatusConflict, "Operation failed because another operation was already in progress.")
	}
	if rs := s.replicaNamesLocked(project, name); len(rs) > 0 {
		s.emu.Unlock()
		return nil, Errorf(http.StatusBadRequest, "The instance has read replicas (%s). Delete or promote the replicas first.", strings.Join(rs, ", "))
	}
	c.state = "PENDING_DELETE"
	s.emu.Unlock()
	op := s.pending(base, project, name, "DELETE")
	id := op["name"].(string)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
		defer cancel()
		var err error
		if fb.enabled {
			// The final backup is taken before the server is removed; when
			// it fails, the instance is kept, as on Cloud SQL.
			if err = s.takeFinalBackup(project, c.engine, fb); err != nil {
				s.emu.Lock()
				c.state = "RUNNABLE"
				s.emu.Unlock()
				s.finish(id, fmt.Errorf("the final backup: %w", err))
				return
			}
		}
		err = s.writes.prov.Remove(ctx, name)
		s.emu.Lock()
		if err == nil {
			if s.writes.byName[name] == c {
				delete(s.writes.byName, name)
			}
			err = s.saveLocked()
		} else {
			c.state = "RUNNABLE"
		}
		s.emu.Unlock()
		if err == nil && s.conn != nil {
			s.conn.removeInstance(name)
		}
		if err == nil {
			s.removeInstanceBackups(project, name)
		}
		s.finish(id, err)
	}()
	return op, nil
}

// restartInstance is instances.restart.
func (s *Server) restartInstance(base, project, name string) (any, error) {
	c, err := s.createdFor(project, name, "instances.restart")
	if err != nil {
		return nil, err
	}
	s.emu.Lock()
	if c.state != "RUNNABLE" {
		st := c.state
		s.emu.Unlock()
		return nil, Errorf(http.StatusConflict, "Operation failed because another operation was already in progress (the instance is %s).", st)
	}
	c.state = "MAINTENANCE"
	spec := c.spec
	s.emu.Unlock()
	op := s.pending(base, project, name, "RESTART")
	id := op["name"].(string)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
		defer cancel()
		err := s.writes.prov.Restart(ctx, spec)
		s.emu.Lock()
		if s.writes.byName[name] == c && c.state == "MAINTENANCE" {
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

// createdJSON overlays a created instance's record on what instance
// answers for its server.
func (s *Server) createdJSON(ctx context.Context, base string, c *created) (map[string]any, error) {
	s.emu.RLock()
	spec, state := c.spec, c.state
	s.emu.RUnlock()
	in, err := s.instance(ctx, base, spec.Project, c.engine)
	if err != nil {
		return nil, err
	}
	if state != "RUNNABLE" || in["state"] == "UNKNOWN_STATE" {
		if state == "RUNNABLE" {
			state = "UNKNOWN_STATE"
		}
		in["state"] = state
	}
	in["createTime"] = spec.CreateTime
	if r := spec.region(); r != Region {
		in["region"] = r
		in["connectionName"] = spec.Project + ":" + r + ":" + spec.Name
	}
	in["gceZone"] = spec.zone(spec.PrimaryNode())
	if spec.Regional() {
		in["secondaryGceZone"] = spec.zone(1 - spec.PrimaryNode())
	}
	// A MySQL replica is upgraded on its own (#1462); a PostgreSQL one
	// only with its primary.
	if up := upgradable(spec.DatabaseVersion); len(up) > 0 && (spec.MasterInstanceName == "" || spec.MySQL()) {
		in["upgradableDatabaseVersions"] = up
	}
	if spec.MySQL() {
		in["instanceType"] = "CLOUD_SQL_INSTANCE"
	}
	st := in["settings"].(map[string]any)
	// Cloud SQL's defaults for what a create leaves out.
	st["storageAutoResize"] = true
	st["storageAutoResizeLimit"] = "0"
	st["dataDiskSizeGb"] = "10"
	st["dataDiskType"] = "PD_SSD"
	st["replicationType"] = "SYNCHRONOUS"
	if spec.MasterInstanceName != "" {
		in["instanceType"] = "READ_REPLICA_INSTANCE"
		in["masterInstanceName"] = spec.Project + ":" + spec.MasterInstanceName
		rc := map[string]any{"kind": "sql#replicaConfiguration", "failoverTarget": false}
		if ext := spec.External; ext != nil && spec.MySQL() {
			rc["mysqlReplicaConfiguration"] = map[string]any{"kind": "sql#mysqlReplicaConfiguration", "username": ext.User}
		}
		in["replicaConfiguration"] = rc
		st["replicationType"] = "ASYNCHRONOUS"
		st["databaseReplicationEnabled"] = !spec.ReplicationStopped
	}
	for k, v := range spec.Settings {
		switch k {
		case "kind", "settingsVersion", "tier", "userLabels", "databaseFlags", "deletionProtectionEnabled",
			"availabilityType", "activationPolicy", "backupConfiguration":
		default:
			st[k] = v
		}
	}
	st["tier"] = spec.Tier
	st["settingsVersion"] = fmt.Sprint(spec.SettingsVersion)
	st["deletionProtectionEnabled"] = spec.DeletionProtection
	if spec.AvailabilityType != "" {
		st["availabilityType"] = spec.AvailabilityType
	}
	if len(spec.Labels) > 0 {
		st["userLabels"] = spec.Labels
	}
	if len(spec.Flags) > 0 {
		flags := []any{}
		for _, f := range spec.Flags {
			flags = append(flags, map[string]any{"name": f.Name, "value": f.Value})
		}
		st["databaseFlags"] = flags
	}
	bc := map[string]any{}
	if raw, ok := spec.Settings["backupConfiguration"].(map[string]any); ok {
		for k, v := range raw {
			bc[k] = v
		}
	}
	bc["kind"], bc["enabled"] = "sql#backupConfiguration", spec.BackupEnabled
	if _, ok := bc["transactionLogRetentionDays"]; !ok {
		bc["transactionLogRetentionDays"] = transactionLogDays(spec)
	}
	if _, ok := bc["backupRetentionSettings"]; !ok {
		bc["backupRetentionSettings"] = map[string]any{"retentionUnit": "COUNT", "retainedBackups": defaultRetained}
	}
	st["backupConfiguration"] = bc
	return in, nil
}

// pending records an operation that is still running; finish ends it.
func (s *Server) pending(base, project, instance, opType string) map[string]any {
	op := s.done(base, project, namedEngine(instance), opType, instance)
	s.mu.Lock()
	op["status"] = "RUNNING"
	delete(op, "endTime")
	cp := copyOp(op)
	s.mu.Unlock()
	return cp
}

func (s *Server) finish(id string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op := s.ops[id]
	if op == nil {
		return
	}
	op["status"] = "DONE"
	op["endTime"] = time.Now().UTC().Format(time.RFC3339Nano)
	if err != nil {
		code := "INTERNAL_ERROR"
		var ae *apiError
		if errors.As(err, &ae) {
			code = ae.status
		}
		op["error"] = map[string]any{"kind": "sql#operationErrors",
			"errors": []any{map[string]any{"kind": "sql#operationError", "code": code, "message": err.Error()}}}
	}
}

func copyOp(op map[string]any) map[string]any {
	cp := make(map[string]any, len(op))
	for k, v := range op {
		cp[k] = v
	}
	return cp
}

// namedEngine is an Engine that answers only its name, for an operation's
// target.
type namedEngine string

func (n namedEngine) Instance() string                                { return string(n) }
func (namedEngine) DatabaseVersion() string                           { return "" }
func (namedEngine) InstalledVersion(context.Context) (string, error)  { return "", nil }
func (namedEngine) Addr() string                                      { return "" }
func (namedEngine) ListDatabases(context.Context) ([]Database, error) { return nil, nil }
func (namedEngine) CreateDatabase(context.Context, Database) error    { return nil }
func (namedEngine) UpdateDatabase(context.Context, Database) error    { return nil }
func (namedEngine) DeleteDatabase(context.Context, string) error      { return nil }
func (namedEngine) ListUsers(context.Context) ([]User, error)         { return nil, nil }
func (namedEngine) CreateUser(context.Context, User, string) error    { return nil }
func (namedEngine) UpdateUser(context.Context, User, string) error    { return nil }
func (namedEngine) DeleteUser(context.Context, User) error            { return nil }
