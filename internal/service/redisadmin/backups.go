package redisadmin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

// Backups (#1549) of a Redis cluster and a Memorystore for Valkey
// instance, as Google documents them: each cluster has a backup collection
// (projects/P/locations/L/backupCollections/<cluster UID>), which outlives
// it; a backup (BackupCluster, BackupInstance, or the automated backup
// config's daily one) is an RDB snapshot of each shard's primary (BGSAVE,
// then the file, read from the node's container), kept in the instance's
// state beside the cluster's record: in its durable store in persistent
// mode, in memory otherwise. ExportBackup writes the files to a Cloud
// Storage bucket, under the backup's ID; a cluster created with
// managed_backup_source or gcs_source loads them once its nodes serve: each
// file is served by a second server on a loopback port in the first
// primary's container, which MIGRATEs every key to the node that owns its
// slot (to the primary, for a CLUSTER_DISABLED instance), so a backup
// restores into any shard count.

// backupFile is one shard's file of a backup.
type backupFile struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// backupRecord is a backup, as it is kept; its files' data are kept apart.
type backupRecord struct {
	Name          string       `json:"name"`
	Owner         string       `json:"owner"`
	OwnerUID      string       `json:"ownerUid"`
	UID           string       `json:"uid"`
	Created       time.Time    `json:"created"`
	Expire        time.Time    `json:"expire"`
	Automated     bool         `json:"automated,omitempty"`
	EngineVersion string       `json:"engineVersion"`
	NodeType      string       `json:"nodeType"`
	Shards        int32        `json:"shards"`
	Replicas      int32        `json:"replicas"`
	Files         []backupFile `json:"files"`
	creating      bool
}

// collectionRecord is a backup collection, as it is kept.
type collectionRecord struct {
	Name     string    `json:"name"`
	Owner    string    `json:"owner"`
	OwnerUID string    `json:"ownerUid"`
	Created  time.Time `json:"created"`
}

var backupRE = regexp.MustCompile(`^(projects/([^/]+)/locations/([^/]+)/backupCollections/([^/]+))/backups/([^/]+)$`)
var collectionRE = regexp.MustCompile(`^projects/([^/]+)/locations/([^/]+)/backupCollections/([^/]+)$`)
var backupIDRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// backupPrefix is where c's backups, collections and files are kept.
func (c *clusters) backupPrefix() string {
	if c.valkey {
		return "valkeybackups/"
	}
	return "clusterbackups/"
}

// collectionName is r's backup collection.
func collectionName(r *clusterRecord) string {
	m := nodesOwnerRE.FindStringSubmatch(r.Name)
	if m == nil || r.UID == "" {
		return ""
	}
	return "projects/" + m[1] + "/locations/" + m[2] + "/backupCollections/" + r.UID
}

// loadBackups reads the kept backups and collections, once. Call with mu held.
func (c *clusters) loadBackups() {
	if c.backups != nil {
		return
	}
	c.backups, c.collections, c.files = map[string]*backupRecord{}, map[string]*collectionRecord{}, map[string][]byte{}
	if c.db == nil {
		return
	}
	keys, _ := c.db.List(c.backupPrefix())
	for _, k := range keys {
		b, err := c.db.Get(k)
		if err != nil {
			continue
		}
		switch {
		case strings.HasPrefix(k, c.backupPrefix()+"b/"):
			r := &backupRecord{}
			if json.Unmarshal(b, r) == nil {
				c.backups[r.Name] = r
			}
		case strings.HasPrefix(k, c.backupPrefix()+"c/"):
			r := &collectionRecord{}
			if json.Unmarshal(b, r) == nil {
				c.collections[r.Name] = r
			}
		}
	}
}

func (c *clusters) put(key string, v any) error {
	if c.db == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.db.Put(key, b)
}

// addCollection records r's backup collection. Call with mu held.
func (c *clusters) addCollection(r *clusterRecord) {
	c.loadBackups()
	name := collectionName(r)
	if name == "" || c.collections[name] != nil {
		return
	}
	col := &collectionRecord{Name: name, Owner: r.Name, OwnerUID: r.UID, Created: c.now()}
	c.collections[name] = col
	_ = c.put(c.backupPrefix()+"c/"+clusterKey(name), col)
}

func fileKey(prefix, backup string, i int) string {
	return fmt.Sprintf("%sf/%s/%d", prefix, clusterKey(backup), i)
}

// putFile and getFile keep a backup file's data.
func (c *clusters) putFile(backup string, i int, data []byte) error {
	k := fileKey(c.backupPrefix(), backup, i)
	if c.db == nil {
		c.files[k] = data
		return nil
	}
	return c.db.Put(k, data)
}

func (c *clusters) getFile(backup string, i int) ([]byte, error) {
	k := fileKey(c.backupPrefix(), backup, i)
	if c.db == nil {
		d, ok := c.files[k]
		if !ok {
			return nil, store.ErrNotFound
		}
		return d, nil
	}
	return c.db.Get(k)
}

// dropBackup forgets b and its files. Call with mu held.
func (c *clusters) dropBackup(b *backupRecord) {
	delete(c.backups, b.Name)
	for i := range b.Files {
		k := fileKey(c.backupPrefix(), b.Name, i)
		delete(c.files, k)
		if c.db != nil {
			_ = c.db.Delete(k)
		}
	}
	if c.db != nil {
		_ = c.db.Delete(c.backupPrefix() + "b/" + clusterKey(b.Name))
	}
}

// expire drops the backups past their expire time. Call with mu held.
func (c *clusters) expire() {
	c.loadBackups()
	now := c.now()
	for _, b := range c.backups {
		if !b.creating && !b.Expire.IsZero() && now.After(b.Expire) {
			c.dropBackup(b)
		}
	}
}

// renderBackup is b as the Redis Cluster API shapes it; the Valkey API's
// Backup has the same fields by number (memorystore.go converts it).
func renderBackup(b *backupRecord) *clusterpb.Backup {
	out := &clusterpb.Backup{Name: b.Name, CreateTime: timestamppb.New(b.Created), Cluster: b.Owner, ClusterUid: b.OwnerUID,
		ExpireTime: timestamppb.New(b.Expire), EngineVersion: b.EngineVersion, ShardCount: b.Shards, ReplicaCount: b.Replicas,
		BackupType: clusterpb.Backup_ON_DEMAND, State: clusterpb.Backup_ACTIVE, Uid: b.UID}
	if b.Automated {
		out.BackupType = clusterpb.Backup_AUTOMATED
	}
	if b.creating {
		out.State = clusterpb.Backup_CREATING
	}
	for _, f := range b.Files {
		out.BackupFiles = append(out.BackupFiles, &clusterpb.BackupFile{FileName: f.Name, SizeBytes: f.Size, CreateTime: timestamppb.New(b.Created)})
		out.TotalSizeBytes += f.Size
	}
	return out
}

// renderCollection is col as the Redis Cluster API shapes it.
func renderCollection(col *collectionRecord) *clusterpb.BackupCollection {
	return &clusterpb.BackupCollection{Name: col.Name, Cluster: col.Owner, ClusterUid: col.OwnerUID, Uid: col.OwnerUID}
}

// listCollections and listBackups are a location's collections, and a
// collection's backups ("-" for every one), in name order. Call with mu held.
func (c *clusters) listCollections(project, location string) []*collectionRecord {
	c.expire()
	var out []*collectionRecord
	for _, col := range c.collections {
		m := collectionRE.FindStringSubmatch(col.Name)
		if m[1] == project && (location == "-" || m[2] == location) {
			out = append(out, col)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (c *clusters) listBackups(parent string) ([]*backupRecord, error) {
	m := collectionRE.FindStringSubmatch(parent)
	if m == nil {
		return nil, apierror.InvalidArgument("parent must be projects/{project}/locations/{location}/backupCollections/{backup_collection}, not %q", parent)
	}
	c.expire()
	if m[3] != "-" && c.collections[parent] == nil {
		return nil, apierror.NotFound("Resource '%s' was not found", parent)
	}
	var out []*backupRecord
	for _, b := range c.backups {
		k := backupRE.FindStringSubmatch(b.Name)
		if k[2] == m[1] && (m[2] == "-" || k[3] == m[2]) && (m[3] == "-" || k[1] == parent) {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// findBackup is a backup by name. Call with mu held.
func (c *clusters) findBackup(name string) (*backupRecord, error) {
	if backupRE.FindStringSubmatch(name) == nil {
		return nil, apierror.InvalidArgument("name must be projects/{project}/locations/{location}/backupCollections/{backup_collection}/backups/{backup}, not %q", name)
	}
	c.expire()
	b := c.backups[name]
	if b == nil {
		return nil, apierror.NotFound("Resource '%s' was not found", name)
	}
	return b, nil
}

// defaultBackupTTL is a backup's ttl when none is asked: Google's 100 years.
const defaultBackupTTL = 100 * 365 * 24 * time.Hour

// startBackup begins a backup of r (mu held) and returns the operation,
// completed with resp once every shard's file is kept.
func (c *clusters) startBackup(r *clusterRecord, id string, ttl time.Duration, automated bool, resp func() proto.Message) (string, error) {
	kind := "cluster"
	if c.valkey {
		kind = "instance"
	}
	if _, ok := c.rt.(Shell); !ok {
		return "", apierror.Unimplemented("backups are not supported by this instance's servers")
	}
	if _, pending := c.busy[r.Name]; pending || r.state.String() != "ACTIVE" {
		return "", apierror.FailedPrecondition("%s %s is %s or has an operation in progress: it is backed up only when ACTIVE", kind, r.Name, r.state)
	}
	if id == "" {
		id = c.now().UTC().Format("20060102150405")
	}
	if !backupIDRE.MatchString(id) {
		return "", apierror.InvalidArgument("backup_id %q must be 1 to 63 lowercase letters, digits or hyphens, starting with a letter or digit", id)
	}
	if ttl == 0 {
		ttl = defaultBackupTTL
	}
	if ttl < 24*time.Hour || ttl > defaultBackupTTL {
		return "", apierror.InvalidArgument("ttl must be between 1 day and 100 years")
	}
	c.addCollection(r)
	name := collectionName(r) + "/backups/" + id
	if c.backups[name] != nil {
		return "", apierror.AlreadyExists("Resource '%s' already exists", name)
	}
	now := c.now()
	b := &backupRecord{Name: name, Owner: r.Name, OwnerUID: r.UID, UID: newUID(), Created: now, Expire: now.Add(ttl), Automated: automated,
		NodeType: r.NodeType, Shards: r.ShardCount, Replicas: r.ReplicaCount, creating: true}
	if r.Valkey != nil {
		b.EngineVersion, b.NodeType = r.Valkey.EngineVersion, r.Valkey.NodeType
	} else {
		b.EngineVersion = "VALKEY_8_0"
	}
	c.backups[name] = b
	op := c.newOperation(r.Name, "backup")
	c.busy[r.Name] = op
	snap := *r
	var primaries []*clusterNode
	for _, n := range r.Nodes {
		if n.Primary {
			primaries = append(primaries, n)
		}
	}
	sort.Slice(primaries, func(i, j int) bool { return primaries[i].Shard < primaries[j].Shard })
	go func() {
		ctx, cancel := context.WithTimeout(c.bg, importOpTimeout)
		defer cancel()
		var files []backupFile
		var err error
		for _, n := range primaries {
			var data []byte
			if data, err = c.snapshotNode(ctx, &snap, n); err != nil {
				err = fmt.Errorf("shard %d: %w", n.Shard, err)
				break
			}
			if err = c.putFile(name, len(files), data); err != nil {
				break
			}
			files = append(files, backupFile{Name: fmt.Sprintf("%s-shard-%d.rdb", id, n.Shard), Size: int64(len(data))})
		}
		c.mu.Lock()
		delete(c.busy, snap.Name)
		b.creating, b.Files = false, files
		if err == nil {
			err = c.put(c.backupPrefix()+"b/"+clusterKey(name), b)
		}
		if err != nil {
			c.dropBackup(b)
		} else if automated {
			if live := c.byName[snap.Name]; live != nil {
				live.LastAutoBackup = now
				_ = c.save(live)
			}
		}
		var out proto.Message = &emptypb.Empty{}
		if resp != nil {
			out = resp()
		}
		c.mu.Unlock()
		if err != nil {
			err = fmt.Errorf("the backup failed: %w", err)
		}
		c.finish(op, out, err)
	}()
	return op, nil
}

// snapshotNode is a node's data as an RDB file: BGSAVE, then the file,
// read from its container.
func (c *clusters) snapshotNode(ctx context.Context, r *clusterRecord, n *clusterNode) ([]byte, error) {
	sh := c.rt.(Shell)
	cl := c.dial(n.addr, r.password(), r.TransitCA)
	defer cl.Close()
	for {
		_, err := cl.Do(ctx, "BGSAVE")
		if err == nil {
			break
		}
		if !strings.Contains(err.Error(), "in progress") {
			return nil, fmt.Errorf("BGSAVE: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(savePoll):
		}
	}
	for {
		info, err := cl.Do(ctx, "INFO", "persistence")
		if err != nil {
			return nil, err
		}
		if clusterInfoField(fmt.Sprint(info), "rdb_bgsave_in_progress") == "0" {
			if st := clusterInfoField(fmt.Sprint(info), "rdb_last_bgsave_status"); st != "ok" {
				return nil, fmt.Errorf("BGSAVE failed: rdb_last_bgsave_status is %s", st)
			}
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(savePoll):
		}
	}
	dir, err := cl.Do(ctx, "CONFIG", "GET", "dir")
	if err != nil {
		return nil, err
	}
	file, err := cl.Do(ctx, "CONFIG", "GET", "dbfilename")
	if err != nil {
		return nil, err
	}
	path := fmt.Sprint(pairs(dir)["dir"]) + "/" + fmt.Sprint(pairs(file)["dbfilename"])
	return sh.Exec(ctx, n.Server, nil, "cat", path)
}

// readBackup is the files of a backup, for a create from it.
func (c *clusters) readBackup(name, field string) ([][]byte, error) {
	// gcloud may send the backup's full resource name.
	name = strings.TrimPrefix(strings.TrimPrefix(name, "//memorystore.googleapis.com/"), "//redis.googleapis.com/")
	c.mu.Lock()
	defer c.mu.Unlock()
	if backupRE.FindStringSubmatch(name) == nil {
		return nil, apierror.InvalidArgument("%s must be projects/{project}/locations/{location}/backupCollections/{backup_collection}/backups/{backup}, not %q", field, name)
	}
	b, err := c.findBackup(name)
	if err != nil {
		return nil, err
	}
	if b.creating {
		return nil, apierror.FailedPrecondition("backup %s is still being made", name)
	}
	var out [][]byte
	for i := range b.Files {
		d, err := c.getFile(name, i)
		if err != nil {
			return nil, apierror.Internal(err, "read backup %s", name)
		}
		out = append(out, d)
	}
	return out, nil
}

// readGCS reads RDB files from this instance's Cloud Storage.
func (c *clusters) readGCS(ctx context.Context, uris []string, field string) ([][]byte, error) {
	if len(uris) == 0 {
		return nil, apierror.InvalidArgument("%s is required", field)
	}
	objs := c.api.created.objs
	if objs == nil {
		return nil, apierror.FailedPrecondition("importing reads this instance's Cloud Storage, which is not enabled: start it with --services memorystore,storage")
	}
	var out [][]byte
	for _, u := range uris {
		bucket, object, err := parseGCS(field, u)
		if err != nil {
			return nil, err
		}
		d, err := objs.Read(ctx, bucket, object)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

// exportBackup writes b's files to bucket, under the backup's ID.
func (c *clusters) exportBackup(ctx context.Context, name, bucket string) (string, error) {
	if bucket == "" {
		return "", apierror.InvalidArgument("gcs_bucket is required")
	}
	bucket = strings.TrimPrefix(bucket, "gs://")
	objs := c.api.created.objs
	if objs == nil {
		return "", apierror.FailedPrecondition("exporting writes this instance's Cloud Storage, which is not enabled: start it with --services memorystore,storage")
	}
	c.mu.Lock()
	b, err := c.findBackup(name)
	if err == nil && b.creating {
		err = apierror.FailedPrecondition("backup %s is still being made", name)
	}
	if err != nil {
		c.mu.Unlock()
		return "", err
	}
	snap := *b
	m := backupRE.FindStringSubmatch(name)
	ops := c.api.created.ops
	op := ops.Create("projects/"+m[2]+"/locations/"+m[3], name)
	c.setMetadata(op.Name, name, "export")
	c.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(c.bg, importOpTimeout)
		defer cancel()
		var err error
		for i, f := range snap.Files {
			var d []byte
			c.mu.Lock()
			d, err = c.getFile(name, i)
			c.mu.Unlock()
			if err == nil {
				err = objs.Write(ctx, bucket, m[5]+"/"+f.Name, d)
			}
			if err != nil {
				break
			}
		}
		c.finish(op.Name, c.backupOut(&snap), err)
	}()
	return op.Name, nil
}

// deleteBackup removes a backup and its files.
func (c *clusters) deleteBackup(name string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := c.findBackup(name)
	if err != nil {
		return "", err
	}
	if b.creating {
		return "", apierror.FailedPrecondition("backup %s is still being made", name)
	}
	c.dropBackup(b)
	m := backupRE.FindStringSubmatch(name)
	ops := c.api.created.ops
	op := ops.Create("projects/"+m[2]+"/locations/"+m[3], name)
	c.setMetadata(op.Name, name, "delete")
	return op.Name, nil
}

// load puts the keys of RDB files into r's nodes: each file is served by a
// second server in the first primary's container, on a loopback port,
// which MIGRATEs every key to the node that owns its slot.
func (c *clusters) load(ctx context.Context, r *clusterRecord, files [][]byte) error {
	sh, ok := c.rt.(Shell)
	if !ok {
		return apierror.Unimplemented("restoring is not supported by this instance's servers")
	}
	first := r.firstPrimary()
	server, bin := first.Server, "valkey"
	file := importDir + "/cloudburrow-restore.rdb"
	port := strconv.Itoa(importPort)
	cleanup := context.WithoutCancel(ctx)
	defer sh.Exec(cleanup, server, nil, "rm", "-f", file)
	for _, data := range files {
		if err := copyWhole(ctx, sh, server, file, data); err != nil {
			return err
		}
		if out, err := sh.Exec(ctx, server, nil, bin+"-check-rdb", file); err != nil {
			return apierror.InvalidArgument("the file is not an RDB file this cluster's servers can load: %s", rdbError(out, err))
		}
		args := []string{bin + "-server", "--port", port, "--bind", "127.0.0.1", "--protected-mode", "no", "--dir", importDir,
			"--dbfilename", "cloudburrow-restore.rdb", "--save", "", "--appendonly", "no", "--daemonize", "yes"}
		if r.TransitCA != "" {
			// MIGRATE to the nodes, which serve TLS only, over TLS.
			d := "/tmp/tls"
			args = append(args, "--tls-cluster", "yes", "--tls-replication", "yes", "--tls-cert-file", d+"/server.crt",
				"--tls-key-file", d+"/server.key", "--tls-ca-cert-file", d+"/ca.crt")
		}
		if out, err := sh.Exec(ctx, server, nil, args...); err != nil {
			return fmt.Errorf("start the server that serves the file: %s", lastLine(out, err))
		}
		err := c.migrateAll(ctx, sh, r, server, bin, port)
		_, _ = sh.Exec(cleanup, server, nil, bin+"-cli", "-p", port, "shutdown", "nosave")
		if err != nil {
			return err
		}
	}
	return nil
}

// migrateAll moves every key of the loopback server to its node.
func (c *clusters) migrateAll(ctx context.Context, sh Shell, r *clusterRecord, server, bin, port string) error {
	for {
		out, _ := sh.Exec(ctx, server, nil, bin+"-cli", "-p", port, "ping")
		if strings.TrimSpace(string(out)) == "PONG" {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the server did not load the file: %w", ctx.Err())
		case <-time.After(savePoll):
		}
	}
	out, err := sh.Exec(ctx, server, nil, bin+"-cli", "-p", port, "--scan", "--count", "1000")
	if err != nil {
		return fmt.Errorf("list the file's keys: %s", lastLine(out, err))
	}
	var keys []string
	for _, k := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	byNode := map[*clusterNode][]string{}
	first := r.firstPrimary()
	if r.standalone() {
		byNode[first] = keys
	} else {
		cmds := make([][]any, len(keys))
		for i, k := range keys {
			cmds[i] = []any{"CLUSTER", "KEYSLOT", k}
		}
		cl := c.dial(first.addr, r.password(), r.TransitCA)
		slots, err := cl.Pipe(ctx, cmds)
		cl.Close()
		if err != nil {
			return fmt.Errorf("CLUSTER KEYSLOT: %w", err)
		}
		owned, err := c.ownedSlots(ctx, r, first)
		if err != nil {
			return err
		}
		owner := map[int]*clusterNode{}
		for _, n := range r.Nodes {
			for _, s := range owned[n.id] {
				owner[s] = n
			}
		}
		for i, k := range keys {
			s, _ := toInt(slots[i])
			n := owner[s]
			if n == nil {
				return fmt.Errorf("slot %d of key %q has no primary", s, k)
			}
			byNode[n] = append(byNode[n], k)
		}
	}
	for n, ks := range byNode {
		for len(ks) > 0 {
			batch := ks
			if len(batch) > 200 {
				batch = ks[:200]
			}
			ks = ks[len(batch):]
			argv := []string{bin + "-cli", "-p", port, "MIGRATE", n.podIP, strconv.Itoa(n.port), "", "0", "60000", "REPLACE"}
			if pw := r.password(); pw != "" {
				argv = append(argv, "AUTH", pw)
			}
			argv = append(append(argv, "KEYS"), batch...)
			out, err := sh.Exec(ctx, server, nil, argv...)
			if err != nil || strings.Contains(string(out), "ERR") || strings.Contains(string(out), "IOERR") {
				return fmt.Errorf("MIGRATE to node %s: %s", n.Server, lastLine(out, errors.Join(err, errors.New("MIGRATE failed"))))
			}
		}
	}
	return nil
}

// checkAutoBackup checks an automated_backup_config.
func checkAutoBackup(ab *clusterpb.AutomatedBackupConfig) error {
	if ab == nil {
		return nil
	}
	if r := ab.GetRetention(); r != nil && (r.AsDuration() < 24*time.Hour || r.AsDuration() > 365*24*time.Hour) {
		return apierror.InvalidArgument("automated_backup_config.retention must be between 1 day and 365 days")
	}
	if st := ab.GetFixedFrequencySchedule().GetStartTime(); st != nil && (st.GetHours() < 0 || st.GetHours() > 23) {
		return apierror.InvalidArgument("automated_backup_config.fixed_frequency_schedule.start_time.hours must be 0 to 23")
	}
	return nil
}

// autoBackupConfig reads r's automated_backup_config: whether it is
// ENABLED, the hour of day (UTC) and the retention.
func autoBackupConfig(r *clusterRecord) (on bool, hour int, retention time.Duration) {
	if r.AutoBackup == "" {
		return false, 0, 0
	}
	ab := &clusterpb.AutomatedBackupConfig{}
	if protojson.Unmarshal([]byte(r.AutoBackup), ab) != nil {
		// A Valkey instance's config has the same fields.
		return false, 0, 0
	}
	retention = 35 * 24 * time.Hour
	if ab.GetRetention() != nil {
		retention = ab.GetRetention().AsDuration()
	}
	return ab.GetAutomatedBackupMode() == clusterpb.AutomatedBackupConfig_ENABLED,
		int(ab.GetFixedFrequencySchedule().GetStartTime().GetHours()), retention
}

// dueBackups are the clusters whose daily automated backup is due at now:
// the latest start time at or before now is after the cluster's creation
// and its last automated backup. Call with mu held.
func (c *clusters) dueBackups(now time.Time) []*clusterRecord {
	var out []*clusterRecord
	for _, r := range c.byName {
		on, hour, _ := autoBackupConfig(r)
		if !on || r.state.String() != "ACTIVE" {
			continue
		}
		u := now.UTC()
		due := time.Date(u.Year(), u.Month(), u.Day(), hour, 0, 0, 0, time.UTC)
		if due.After(u) {
			due = due.Add(-24 * time.Hour)
		}
		if due.After(r.Created) && due.After(r.LastAutoBackup) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// autoBackups takes the automated backups as they fall due, until ctx ends.
func (c *clusters) autoBackups(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c.mu.Lock()
		c.loadBackups()
		for _, r := range c.dueBackups(c.now()) {
			if _, pending := c.busy[r.Name]; pending {
				continue
			}
			_, _, retention := autoBackupConfig(r)
			id := "auto-" + c.now().UTC().Format("20060102150405")
			_, _ = c.startBackup(r, id, retention, true, nil)
		}
		c.mu.Unlock()
	}
}

// setMetadata sets an operation's metadata, of c's API.
func (c *clusters) setMetadata(op, target, verb string) {
	ops := c.api.created.ops
	if c.valkey {
		_ = ops.SetMetadata(op, valkeyMetadata(c.now(), target, verb))
		return
	}
	_ = ops.SetMetadata(op, &clusterpb.OperationMetadata{CreateTime: timestamppb.New(c.now()), Target: target, Verb: verb, ApiVersion: "v1"})
}
