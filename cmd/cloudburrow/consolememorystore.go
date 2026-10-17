package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	redispb "cloud.google.com/go/redis/apiv1/redispb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/service/redisadmin"
)

// memorystoreProvider is Memorystore's Instances page (#1302), read and
// changed through the admin API's own methods, so the screen shows what
// gcloud and the SDK see: Create instance (CreateInstance), Edit
// (UpdateInstance: display name, labels, capacity, configs) and Delete
// (DeleteInstance), and Flush, which empties the instance `up` runs as
// FLUSHALL does — the Memorystore console's own data-plane flush; the
// admin API has no method for it.
type memorystoreProvider struct {
	// api is the admin API `up` serves; a stand-in describing the server
	// at the RESP address alone before it has started.
	api     func() *redisadmin.Server
	backend redisadmin.Valkey
}

// newMemorystoreProvider builds the screen for the server at addr, over
// svc's API when it runs.
func newMemorystoreProvider(cfg config.Config, addr string, svc *memorystoreAdminService) memorystoreProvider {
	be := redisadmin.Valkey{Addr: func() string { return addr }}
	fallback := redisadmin.NewServer(memorystoreInstance(cfg, time.Time{}, be.Addr), be)
	return memorystoreProvider{backend: be, api: func() *redisadmin.Server {
		if svc != nil && svc.api != nil {
			return svc.api
		}
		return fallback
	}}
}

func (memorystoreProvider) ID() string    { return "memorystore" }
func (memorystoreProvider) Title() string { return "Memorystore" }

const memorystoreNote = "The instance memorystore is the Valkey server --services memorystore runs; it cannot be edited or deleted. " +
	"Each instance created here is a server of its own, BASIC tier, or a primary and replicas, STANDARD_HA (REDIS_6_X Redis 6.2, REDIS_7_0 Redis 7.0, REDIS_7_2 Valkey), with or without in-transit encryption (TLS, with a CA of its own)."

// List implements console.Provider: the project's instances.
func (p memorystoreProvider) List(ctx context.Context, project string) (console.Listing, error) {
	l := console.Listing{
		Columns:    []string{"Location", "Version", "Endpoint", "Tier", "Capacity", "AUTH"},
		NameColumn: "Instance ID",
		Noun:       "instances",
		Note:       memorystoreNote,
	}
	resp, err := p.api().ListInstances(ctx, &redispb.ListInstancesRequest{Parent: "projects/" + project + "/locations/-", PageSize: 1000})
	if err != nil {
		l.Unavailable = "cannot reach Memorystore: " + err.Error()
		return l, nil
	}
	for _, inst := range resp.GetInstances() {
		r := console.Resource{
			Name:   memorystoreID(inst.GetName()),
			Status: inst.GetState().String(),
			Fields: map[string]string{
				"Location": inst.GetLocationId(),
				"Version":  inst.GetRedisVersion(),
				"Endpoint": memorystoreEndpoint(inst),
				"Tier":     inst.GetTier().String(),
				"Capacity": fmt.Sprintf("%d GB", inst.GetMemorySizeGb()),
				"AUTH":     map[bool]string{true: "Enabled", false: "Disabled"}[inst.GetAuthEnabled()],
			},
		}
		r.Actions = p.Actions(r)
		l.Items = append(l.Items, r)
	}
	l.Total = len(l.Items)
	return l, nil
}

func memorystoreEndpoint(inst *redispb.Instance) string {
	if inst.GetHost() == "" {
		return "—"
	}
	return net.JoinHostPort(inst.GetHost(), strconv.Itoa(int(inst.GetPort())))
}

// memorystoreReadEndpoint is a Standard tier instance's read endpoint,
// host:port, "" for none.
func memorystoreReadEndpoint(inst *redispb.Instance) string {
	if inst.GetReadEndpoint() == "" {
		return ""
	}
	return net.JoinHostPort(inst.GetReadEndpoint(), strconv.Itoa(int(inst.GetReadEndpointPort())))
}

// memorystoreID is the console's name for an instance: its ID, with its
// region when it is not the default one, region/ID.
func memorystoreID(name string) string {
	parts := strings.Split(name, "/")
	if len(parts) != 6 {
		return name
	}
	if parts[3] == redisadmin.Region {
		return parts[5]
	}
	return parts[3] + "/" + parts[5]
}

// memorystoreName is the resource name of the console's name in project.
func memorystoreName(project, id string) (string, error) {
	region := redisadmin.Region
	if r, i, ok := strings.Cut(id, "/"); ok {
		region, id = r, i
	}
	if id == "" || region == "" || strings.Contains(id, "/") {
		return "", errors.New("no such instance: " + id)
	}
	return "projects/" + project + "/locations/" + region + "/instances/" + id, nil
}

// Actions implements console.Actor: Flush, on the instance `up` runs.
func (memorystoreProvider) Actions(r console.Resource) []console.Action {
	if r.Name != redisadmin.InstanceID {
		return nil
	}
	return []console.Action{{ID: "flush", Label: "Flush", Destructive: true,
		Confirm: "Every key in every database of the instance is deleted (FLUSHALL)."}}
}

// Act implements console.Actor.
func (p memorystoreProvider) Act(ctx context.Context, project, name, action string) error {
	if action != "flush" {
		return fmt.Errorf("unknown action %q", action)
	}
	if name != redisadmin.InstanceID {
		return errors.New("Flush empties the instance memorystore only: connect to " + name + " with a Redis client")
	}
	// The instance must be this project's, as the list shows it.
	if _, err := p.api().GetInstance(ctx, &redispb.GetInstanceRequest{
		Name: "projects/" + project + "/locations/" + redisadmin.Region + "/instances/" + redisadmin.InstanceID}); err != nil {
		return errors.New("project " + project + " has no Memorystore instance memorystore")
	}
	return p.backend.Flush(ctx)
}

// CreateForm implements console.Creator: CreateInstance's fields that work
// here: BASIC or STANDARD_HA (#1385), with read replicas. Versions the
// API refuses are not offered.
func (memorystoreProvider) CreateForm() (string, []console.Field) {
	return "Create instance", []console.Field{
		{Name: "id", Label: "Instance ID", Type: "text", Required: true, Pattern: `^[a-z]([a-z0-9\-]{0,38}[a-z0-9])?$`,
			Help: "1 to 40 lowercase letters, digits and hyphens, starting with a letter."},
		{Name: "displayName", Label: "Display name", Type: "text"},
		{Name: "region", Label: "Region", Type: "text", Required: true, Default: redisadmin.Region,
			Help: "CloudBurrow does not place instances geographically."},
		{Name: "memorySizeGb", Label: "Capacity (GB)", Type: "number", Required: true, Default: "1", Pattern: `^[0-9]{1,3}$`,
			Help: "1 to 300: the server's maxmemory."},
		{Name: "tier", Label: "Tier", Type: "select", Default: "BASIC", Options: []string{"BASIC", "STANDARD_HA"},
			Help: "STANDARD_HA is a primary and replicas, each a server of its own, with failover."},
		{Name: "readReplicas", Label: "Enable read replicas", Type: "checkbox",
			Help: "STANDARD_HA only, 5 GB or more: a read endpoint over the replicas. It cannot be turned off later."},
		{Name: "replicaCount", Label: "Replicas", Type: "number", Pattern: `^[0-5]?$`,
			Help: "STANDARD_HA: 1 without read replicas, 1 to 5 with them (default 2)."},
		{Name: "redisVersion", Label: "Version", Type: "select", Default: redisadmin.DefaultRedisVersion, Options: redisadmin.Versions,
			Help: "REDIS_6_X is Redis 6.2, REDIS_7_0 Redis 7.0, REDIS_7_2 Valkey 8.1."},
		{Name: "authEnabled", Label: "Enable AUTH", Type: "checkbox",
			Help: "Clients must AUTH with the instance's AUTH string, which GetInstanceAuthString returns."},
		{Name: "transitEncryption", Label: "Enable in-transit encryption", Type: "checkbox",
			Help: "SERVER_AUTHENTICATION: the server serves TLS only, with a certificate of the instance's own CA, which its page shows. It cannot be changed later."},
		{Name: "customerManagedKey", Label: "Customer-managed encryption key (CMEK)", Type: "text",
			Pattern: `^(projects/[^\/]+/locations/[^\/]+/keyRings/[^\/]+/cryptoKeys/[^\/]+)?$`,
			Help:    "Optional. projects/{project}/locations/{region}/keyRings/{ring}/cryptoKeys/{key}: a Cloud KMS key in the instance's region. It cannot be changed later; while the key is disabled or destroyed the instance is suspended."},
		{Name: "labels", Label: "Labels", Type: "map", Help: "Optional. One key=value per line."},
		{Name: "redisConfigs", Label: "Redis configs", Type: "map",
			Help: "Optional. One key=value per line: maxmemory-policy, notify-keyspace-events, activedefrag, " +
				"lfu-decay-time, lfu-log-factor, stream-node-max-bytes, stream-node-max-entries, maxmemory-gb."},
	}
}

// CreateOnPage implements console.PageCreator.
func (memorystoreProvider) CreateOnPage() bool { return true }

// Create implements console.Creator, waiting for the operation, as the
// Memorystore console shows an instance once it is ready.
func (p memorystoreProvider) Create(ctx context.Context, project string, values map[string]string) (string, error) {
	size, err := strconv.Atoi(strings.TrimSpace(values["memorySizeGb"]))
	if err != nil {
		return "", fmt.Errorf("capacity %q is not a number of GB", values["memorySizeGb"])
	}
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return "", err
	}
	configs, err := console.ParseMap(values["redisConfigs"])
	if err != nil {
		return "", err
	}
	region := strings.TrimSpace(values["region"])
	in := &redispb.Instance{Tier: redispb.Instance_BASIC, MemorySizeGb: int32(size), DisplayName: values["displayName"],
		AuthEnabled: values["authEnabled"] == "true", Labels: labels, RedisConfigs: configs, RedisVersion: values["redisVersion"]}
	if values["transitEncryption"] == "true" {
		in.TransitEncryptionMode = redispb.Instance_SERVER_AUTHENTICATION
	}
	in.CustomerManagedKey = strings.TrimSpace(values["customerManagedKey"])
	if values["tier"] == "STANDARD_HA" {
		in.Tier = redispb.Instance_STANDARD_HA
		if values["readReplicas"] == "true" {
			in.ReadReplicasMode = redispb.Instance_READ_REPLICAS_ENABLED
		}
	}
	if rc := strings.TrimSpace(values["replicaCount"]); rc != "" {
		n, err := strconv.Atoi(rc)
		if err != nil {
			return "", fmt.Errorf("replicas %q is not a number", rc)
		}
		in.ReplicaCount = int32(n)
	}
	op, err := p.api().CreateInstance(ctx, &redispb.CreateInstanceRequest{
		Parent: "projects/" + project + "/locations/" + region, InstanceId: strings.TrimSpace(values["id"]), Instance: in})
	if err != nil {
		return "", err
	}
	if err := p.wait(ctx, op.GetName()); err != nil {
		return "", err
	}
	return memorystoreID("projects/" + project + "/locations/" + region + "/instances/" + strings.TrimSpace(values["id"])), nil
}

// wait polls an operation until it is done, and returns its error.
func (p memorystoreProvider) wait(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	for {
		op, err := p.api().Operation(name)
		if err != nil {
			return err
		}
		if op.GetDone() {
			if e := op.GetError(); e != nil {
				return errors.New(e.GetMessage())
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("operation %s is still running", name)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Delete implements console.Deleter: DeleteInstance, waited for.
func (p memorystoreProvider) Delete(ctx context.Context, project, id string) error {
	name, err := memorystoreName(project, id)
	if err != nil {
		return err
	}
	op, err := p.api().DeleteInstance(ctx, &redispb.DeleteInstanceRequest{Name: name})
	if err != nil {
		return err
	}
	return p.wait(ctx, op.GetName())
}

// Detail implements console.Detailer: an instance's properties, and its
// Edit form for an instance CreateInstance made.
func (p memorystoreProvider) Detail(ctx context.Context, project string, path []string) (console.Detail, error) {
	if len(path) > 2 || (len(path) == 2 && path[0] == redisadmin.Region) {
		return console.DeeperThan(1, path), nil
	}
	if project == "" {
		return console.Detail{Prompt: "Choose a project in the toolbar."}, nil
	}
	name, err := memorystoreName(project, strings.Join(path, "/"))
	if err != nil {
		return console.Detail{Unavailable: err.Error()}, nil
	}
	inst, err := p.api().GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if err != nil {
		return console.Detail{Unavailable: "cannot read the instance: " + consoleErr(err)}, nil
	}
	props := []console.Property{
		{Label: "State", Value: inst.GetState().String()},
		{Label: "Endpoint", Value: memorystoreEndpoint(inst)},
		{Label: "Version", Value: inst.GetRedisVersion()},
		{Label: "Tier", Value: inst.GetTier().String()},
		{Label: "Capacity", Value: fmt.Sprintf("%d GB", inst.GetMemorySizeGb())},
		{Label: "AUTH", Value: map[bool]string{true: "Enabled", false: "Disabled"}[inst.GetAuthEnabled()]},
		{Label: "In-transit encryption", Value: memorystoreTransit(inst)},
		{Label: "Encryption", Value: memorystoreEncryption(inst)},
		{Label: "Display name", Value: orDash(inst.GetDisplayName())},
		{Label: "Labels", Value: orDash(memorystorePairs(inst.GetLabels()))},
		{Label: "Redis configs", Value: orDash(memorystorePairs(inst.GetRedisConfigs()))},
		{Label: "Persistence", Value: memorystorePersistence(inst.GetPersistenceConfig())},
	}
	if inst.GetTier() == redispb.Instance_STANDARD_HA {
		var nodes []string
		for _, n := range inst.GetNodes() {
			nodes = append(nodes, n.GetId()+" ("+n.GetZone()+")")
		}
		props = append(props,
			console.Property{Label: "Replicas", Value: strconv.Itoa(int(inst.GetReplicaCount()))},
			console.Property{Label: "Read replicas", Value: inst.GetReadReplicasMode().String()},
			console.Property{Label: "Read endpoint", Value: orDash(memorystoreReadEndpoint(inst))},
			console.Property{Label: "Primary zone", Value: inst.GetCurrentLocationId()},
			console.Property{Label: "Alternative zone", Value: inst.GetAlternativeLocationId()},
			console.Property{Label: "Nodes", Value: strings.Join(nodes, ", ")})
	}
	if inst.GetAuthEnabled() {
		if a, err := p.api().GetInstanceAuthString(ctx, &redispb.GetInstanceAuthStringRequest{Name: name}); err == nil {
			props = append(props, console.Property{Label: "AUTH string", Value: orDash(a.GetAuthString())})
		}
	}
	if rs := inst.GetSuspensionReasons(); len(rs) > 0 {
		var names []string
		for _, r := range rs {
			names = append(names, r.String())
		}
		props = append(props, console.Property{Label: "Suspension reasons", Value: strings.Join(names, ", ")})
	}
	if m := inst.GetStatusMessage(); m != "" {
		props = append(props, console.Property{Label: "Status", Value: m})
	}
	d := console.Detail{Sections: []console.Section{{ID: "config", Label: "Configuration", Kind: console.KindProperties,
		Groups: []console.PropertyGroup{{Properties: props}}}}}
	if cas := inst.GetServerCaCerts(); len(cas) > 0 {
		// The server CA its clients trust, as the Memorystore console's
		// Security tab offers it for download (#1413).
		var text strings.Builder
		for _, ca := range cas {
			fmt.Fprintf(&text, "# Serial %s, SHA-1 %s, expires %s\n%s\n", ca.GetSerialNumber(), ca.GetSha1Fingerprint(),
				ca.GetExpireTime().AsTime().Format("2006-01-02"), strings.TrimSpace(ca.GetCert()))
		}
		d.Sections = append(d.Sections, console.Section{ID: "server-ca", Label: "Server CA", Kind: console.KindText, Text: text.String()})
	}
	if memorystoreID(name) == redisadmin.InstanceID {
		return d, nil
	}
	d.Edit = &console.EditForm{
		Label: "Edit instance",
		Fields: []console.Field{
			{Name: "displayName", Label: "Display name", Type: "text", Default: inst.GetDisplayName()},
			{Name: "memorySizeGb", Label: "Capacity (GB)", Type: "number", Required: true, Pattern: `^[0-9]{1,3}$`,
				Default: strconv.Itoa(int(inst.GetMemorySizeGb())), Help: "1 to 300: set on the running server, which keeps its keys."},
			{Name: "labels", Label: "Labels", Type: "map", Default: console.FormatMap(inst.GetLabels())},
			{Name: "redisConfigs", Label: "Redis configs", Type: "map", Default: console.FormatMap(inst.GetRedisConfigs()),
				Help: "A config removed here goes back to its default."},
			{Name: "persistence", Label: "Persistence", Type: "select", Default: memorystorePersistenceChoice(inst.GetPersistenceConfig()),
				Options: memorystorePersistenceChoices,
				Help:    "RDB snapshots every period (the server's save point), or none; set on the running server."},
		},
		Note: "Saved through UpdateInstance. The ID, region, tier, version and AUTH cannot be changed here.",
	}
	switch {
	case inst.GetReadReplicasMode() == redispb.Instance_READ_REPLICAS_ENABLED:
		d.Edit.Fields = append(d.Edit.Fields, console.Field{Name: "replicaCount", Label: "Replicas", Type: "number", Required: true,
			Pattern: `^[1-5]$`, Default: strconv.Itoa(int(inst.GetReplicaCount())), Help: "1 to 5: replicas are started or removed, and the read endpoint follows."})
	case inst.GetTier() == redispb.Instance_STANDARD_HA:
		d.Edit.Fields = append(d.Edit.Fields, console.Field{Name: "readReplicas", Label: "Enable read replicas", Type: "checkbox",
			Help: "5 GB or more: a read endpoint over the replicas (2 by default). It cannot be turned off later."})
	}
	return d, nil
}

// Edit implements console.Editor: UpdateInstance of the form's fields.
func (p memorystoreProvider) Edit(ctx context.Context, project string, path []string, values map[string]string) error {
	name, err := memorystoreName(project, strings.Join(path, "/"))
	if err != nil {
		return err
	}
	size, err := strconv.Atoi(strings.TrimSpace(values["memorySizeGb"]))
	if err != nil {
		return fmt.Errorf("capacity %q is not a number of GB", values["memorySizeGb"])
	}
	labels, err := console.ParseMap(values["labels"])
	if err != nil {
		return err
	}
	configs, err := console.ParseMap(values["redisConfigs"])
	if err != nil {
		return err
	}
	paths := []string{"display_name", "memory_size_gb", "labels", "redis_configs"}
	in := &redispb.Instance{Name: name, DisplayName: values["displayName"], MemorySizeGb: int32(size),
		Labels: labels, RedisConfigs: configs}
	if v, ok := values["persistence"]; ok {
		cur, err := p.api().GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
		if err != nil {
			return err
		}
		if v != memorystorePersistenceChoice(cur.GetPersistenceConfig()) {
			pc := &redispb.PersistenceConfig{PersistenceMode: redispb.PersistenceConfig_DISABLED}
			if period, ok := redispb.PersistenceConfig_SnapshotPeriod_value[v]; ok {
				pc = &redispb.PersistenceConfig{PersistenceMode: redispb.PersistenceConfig_RDB,
					RdbSnapshotPeriod: redispb.PersistenceConfig_SnapshotPeriod(period)}
			} else if v != "DISABLED" {
				return fmt.Errorf("persistence %q is not DISABLED or a snapshot period", v)
			}
			paths, in.PersistenceConfig = append(paths, "persistence_config"), pc
		}
	}
	if values["readReplicas"] == "true" {
		paths, in.ReadReplicasMode = append(paths, "read_replicas_mode"), redispb.Instance_READ_REPLICAS_ENABLED
	}
	if v := strings.TrimSpace(values["replicaCount"]); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("replicas %q is not a number", v)
		}
		paths, in.ReplicaCount = append(paths, "replica_count"), int32(n)
	}
	op, err := p.api().UpdateInstance(ctx, &redispb.UpdateInstanceRequest{
		UpdateMask: &fieldmaskpb.FieldMask{Paths: paths}, Instance: in})
	if err != nil {
		return err
	}
	return p.wait(ctx, op.GetName())
}

// memorystorePairs is a map as key=value pairs, in key order.
func memorystorePairs(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		keys[i] = k + "=" + m[k]
	}
	return strings.Join(keys, ", ")
}

// DetailActions implements console.PathActor: Export and Import (#1384)
// on an instance CreateInstance made, as the Memorystore console's
// instance page offers them, to and from a gs:// RDB file in this
// instance's Cloud Storage.
func (p memorystoreProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	name, err := memorystoreName(project, strings.Join(path, "/"))
	if err != nil || memorystoreID(name) == redisadmin.InstanceID {
		return nil
	}
	var upgrade []console.Action
	inst, ierr := p.api().GetInstance(ctx, &redispb.GetInstanceRequest{Name: name})
	if ierr == nil {
		var later []string
		seen := false
		for _, v := range redisadmin.Versions {
			if seen {
				later = append(later, v)
			}
			seen = seen || v == inst.GetRedisVersion()
		}
		if len(later) > 0 {
			upgrade = []console.Action{{ID: "upgrade", Label: "Upgrade", Confirm: "The instance's server is started again on the new version, with its data; an instance cannot be downgraded.",
				Fields: []console.Field{{Name: "redisVersion", Label: "Version", Type: "select", Required: true, Options: later, Default: later[0]}}}}
		}
	}
	if ierr == nil && inst.GetTier() == redispb.Instance_STANDARD_HA {
		upgrade = append(upgrade, console.Action{ID: "failover", Label: "Failover", Destructive: true,
			Confirm: "A replica becomes the primary; the endpoint stays the same and its connections are dropped (FailoverInstance).",
			Fields: []console.Field{{Name: "dataProtectionMode", Label: "Data protection mode", Type: "select", Required: true,
				Options: []string{"LIMITED_DATA_LOSS", "FORCE_DATA_LOSS"}, Default: "LIMITED_DATA_LOSS",
				Help: "LIMITED_DATA_LOSS aborts when a replica is 30 MB or more behind the primary; FORCE_DATA_LOSS does not check."}}})
	}
	if ierr == nil && inst.GetTransitEncryptionMode() == redispb.Instance_SERVER_AUTHENTICATION {
		upgrade = append(upgrade, console.Action{ID: "rotate-ca", Label: "Rotate server CA",
			Confirm: "A new server CA is listed beside the current one, which stays valid until it expires; the server is started again with a certificate of the new CA. Install every listed CA on your clients first. (Memorystore rotates five years after creation, or on a support request.)"})
	}
	return append(upgrade, []console.Action{
		{ID: "export", Label: "Export", Fields: []console.Field{{Name: "uri", Label: "Destination", Type: "text", Required: true,
			Pattern: "^gs://[^\\/]+/.+$", Help: "gs://bucket/object: the RDB file ExportInstance writes, in this instance's Cloud Storage."}}},
		{ID: "import", Label: "Import", Destructive: true,
			Confirm: "Every key of the instance is replaced by the RDB file's (ImportInstance overwrites the data).",
			Fields: []console.Field{{Name: "uri", Label: "Source", Type: "text", Required: true,
				Pattern: "^gs://[^\\/]+/.+$", Help: "gs://bucket/object: an RDB file in this instance's Cloud Storage."}}},
	}...)
}

// ActAt implements console.PathActor: ExportInstance or ImportInstance,
// waited for.
func (p memorystoreProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	name, err := memorystoreName(project, strings.Join(path, "/"))
	if err != nil {
		return err
	}
	uri := strings.TrimSpace(values["uri"])
	var opName string
	switch action {
	case "export":
		op, err := p.api().ExportInstance(ctx, &redispb.ExportInstanceRequest{Name: name,
			OutputConfig: &redispb.OutputConfig{Destination: &redispb.OutputConfig_GcsDestination{GcsDestination: &redispb.GcsDestination{Uri: uri}}}})
		if err != nil {
			return err
		}
		opName = op.GetName()
	case "import":
		op, err := p.api().ImportInstance(ctx, &redispb.ImportInstanceRequest{Name: name,
			InputConfig: &redispb.InputConfig{Source: &redispb.InputConfig_GcsSource{GcsSource: &redispb.GcsSource{Uri: uri}}}})
		if err != nil {
			return err
		}
		opName = op.GetName()
	case "failover":
		mode := redispb.FailoverInstanceRequest_DataProtectionMode(redispb.FailoverInstanceRequest_DataProtectionMode_value[values["dataProtectionMode"]])
		op, err := p.api().FailoverInstance(ctx, &redispb.FailoverInstanceRequest{Name: name, DataProtectionMode: mode})
		if err != nil {
			return err
		}
		opName = op.GetName()
	case "rotate-ca":
		return p.api().RotateServerCA(ctx, name)
	case "upgrade":
		op, err := p.api().UpgradeInstance(ctx, &redispb.UpgradeInstanceRequest{Name: name, RedisVersion: values["redisVersion"]})
		if err != nil {
			return err
		}
		opName = op.GetName()
	default:
		return fmt.Errorf("unknown action %q", action)
	}
	return p.wait(ctx, opName)
}

// memorystorePersistenceChoices are the Edit form's persistence options:
// none, or RDB with one of Memorystore's snapshot periods.
var memorystorePersistenceChoices = []string{"DISABLED", "ONE_HOUR", "SIX_HOURS", "TWELVE_HOURS", "TWENTY_FOUR_HOURS"}

func memorystorePersistenceChoice(pc *redispb.PersistenceConfig) string {
	if pc.GetPersistenceMode() == redispb.PersistenceConfig_RDB {
		return pc.GetRdbSnapshotPeriod().String()
	}
	return "DISABLED"
}

// memorystorePersistence is persistence_config as the page shows it.
func memorystorePersistence(pc *redispb.PersistenceConfig) string {
	if pc.GetPersistenceMode() != redispb.PersistenceConfig_RDB {
		return "Disabled"
	}
	return fmt.Sprintf("RDB every %s, next %s", pc.GetRdbSnapshotPeriod(), pc.GetRdbNextSnapshotTime().AsTime().UTC().Format(time.RFC3339))
}

// memorystoreEncryption is an instance's encryption at rest, as its page
// shows it: Google-managed, or the customer-managed key (#1463).
func memorystoreEncryption(inst *redispb.Instance) string {
	if k := inst.GetCustomerManagedKey(); k != "" {
		return "Customer-managed key " + k
	}
	return "Google-managed key"
}

// memorystoreTransit is an instance's in-transit encryption, as its page
// shows it (#1413).
func memorystoreTransit(inst *redispb.Instance) string {
	if inst.GetTransitEncryptionMode() == redispb.Instance_SERVER_AUTHENTICATION {
		return "Server authentication (TLS, port " + strconv.Itoa(redisadmin.TLSPort) + " in the cluster)"
	}
	return "Disabled"
}
