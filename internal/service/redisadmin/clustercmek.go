package redisadmin

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// CMEK of a Redis cluster and a Memorystore for Valkey instance (#1557),
// kms_key, as Memorystore documents it and as a Memorystore for Redis
// instance's is (cmek.go): the key must be a CryptoKey in the cluster's
// region that this instance's Cloud KMS can use (an Encrypt with it wraps
// the cluster's data key, 256-bit AES, which is kept only wrapped).
//
// What the key protects: the cluster's kept secrets (the token auth
// users' tokens, the nodes' default user's password, the in-transit CA's
// private key) are kept sealed with the data key (AES-GCM), never in the
// clear; each backup's RDB files are sealed with it too, the backup
// keeping the wrapped data key, so a backup of a CMEK cluster is readable
// (restored, exported) only while the key can be used. What it does not:
// a node's data in memory, which Valkey serves to its clients as it is;
// an export, which Memorystore writes to the bucket as plain RDB files
// that the bucket's own encryption protects; and the cluster's record
// beside those secrets (its shape, labels, configs).
//
// The key is checked every keyEvery. Once it cannot be used (disabled,
// destroyed, its version disabled), the cluster is suspended, as a
// Memorystore for Redis instance (#1463) and a Cloud SQL instance (#1538)
// are: no ACTIVE state (STATE_UNSPECIFIED), encryption_info's
// kms_key_primary_state says why, and its nodes are stopped, their data
// first kept as RDB files sealed with the data key, so no client reaches
// it. Once the key can be used again the cluster resumes on its own: new
// nodes are formed (UPDATING) and the data loaded into them, then ACTIVE.

// clusterSealed is what a CMEK cluster keeps sealed with its data key.
type clusterSealed struct {
	AdminPassword string                `json:"adminPassword,omitempty"`
	TransitCAKey  string                `json:"transitCaKey,omitempty"`
	TokenUsers    map[string]*tokenUser `json:"tokenUsers,omitempty"`
}

// keys is this instance's Cloud KMS, nil when it is not enabled.
func (c *clusters) keys() KeyService {
	if c.api.created == nil {
		return nil
	}
	return c.api.created.kms
}

// newClusterKey checks kms_key and wraps a data key of r's with it.
func (c *clusters) newClusterKey(ctx context.Context, r *clusterRecord, key, region string) error {
	if err := checkKeyField("kms_key", key, region); err != nil {
		return err
	}
	k := c.keys()
	if k == nil {
		return apierror.FailedPrecondition("kms_key needs Cloud KMS: run up with kms in --services")
	}
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return apierror.Internal(err, "generate the data key")
	}
	wrapped, err := k.Encrypt(ctx, key, dek)
	if err != nil {
		return keyError(key, err)
	}
	r.KmsKey, r.WrappedKey, r.dek = key, wrapped, dek
	return nil
}

// unwrapKey is a data key wrapped by key: an error when the key cannot be
// used.
func (c *clusters) unwrapKey(ctx context.Context, key string, wrapped []byte) ([]byte, error) {
	k := c.keys()
	if k == nil {
		return nil, fmt.Errorf("Cloud KMS is not enabled")
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return k.Decrypt(cctx, key, wrapped)
}

// kept is r as it is written: with CMEK, its secrets sealed with the data
// key and not in the clear.
func (r *clusterRecord) kept() (*clusterRecord, error) {
	if r.KmsKey == "" {
		return r, nil
	}
	out := *r
	if r.dek != nil {
		b, err := json.Marshal(clusterSealed{AdminPassword: r.AdminPassword, TransitCAKey: r.TransitCAKey, TokenUsers: r.TokenUsers})
		if err != nil {
			return nil, err
		}
		if out.Sealed, err = seal(r.dek, b); err != nil {
			return nil, err
		}
	}
	out.AdminPassword, out.TransitCAKey, out.TokenUsers = "", "", nil
	return &out, nil
}

// unsealWith restores r's secrets with its data key dek.
func (r *clusterRecord) unsealWith(dek []byte) error {
	r.dek = dek
	if len(r.Sealed) == 0 {
		return nil
	}
	b, err := open(dek, r.Sealed)
	if err != nil {
		return fmt.Errorf("open the cluster's sealed metadata: %w", err)
	}
	var s clusterSealed
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	r.AdminPassword, r.TransitCAKey, r.TokenUsers = s.AdminPassword, s.TransitCAKey, s.TokenUsers
	return nil
}

// openBackupFile is a backup file's RDB data: unsealed with the backup's
// data key when it was made of a CMEK cluster.
func (c *clusters) openBackupFile(b *backupRecord, d []byte) ([]byte, error) {
	if b.KmsKey == "" {
		return d, nil
	}
	dek, err := c.unwrapKey(c.bg, b.KmsKey, b.WrappedKey)
	if err != nil {
		return nil, keyError(b.KmsKey, err)
	}
	return open(dek, d)
}

// encryptionInfo is a CMEK cluster's encryption_info; nil without CMEK.
func (r *clusterRecord) encryptionInfo() *clusterpb.EncryptionInfo {
	if r.KmsKey == "" {
		return nil
	}
	st := clusterpb.EncryptionInfo_ENABLED
	if r.KeyState != "" {
		st = clusterpb.EncryptionInfo_KmsKeyState(clusterpb.EncryptionInfo_KmsKeyState_value[r.KeyState])
	}
	out := &clusterpb.EncryptionInfo{EncryptionType: clusterpb.EncryptionInfo_CUSTOMER_MANAGED_ENCRYPTION,
		KmsKeyPrimaryState: st}
	if !r.KeyChecked.IsZero() {
		out.LastUpdateTime = timestamppb.New(r.KeyChecked)
	}
	return out
}

// keyState is the kms_key_primary_state an unusable key's error says.
func keyState(err error) string {
	switch msg := strings.ToLower(err.Error()); {
	case strings.Contains(msg, "destroy"):
		return clusterpb.EncryptionInfo_DESTROYED.String()
	case strings.Contains(msg, "permission"):
		return clusterpb.EncryptionInfo_PERMISSION_DENIED.String()
	}
	return clusterpb.EncryptionInfo_DISABLED.String()
}

// watchKeys checks every CMEK cluster's key until ctx ends.
func (c *clusters) watchKeys(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c.checkKeys(ctx)
	}
}

// checkKeys suspends each CMEK cluster whose key cannot be used, and
// resumes each suspended one whose key can be again.
func (c *clusters) checkKeys(ctx context.Context) {
	type target struct {
		name, key string
		wrapped   []byte
	}
	var ts []target
	c.mu.Lock()
	for name, r := range c.byName {
		if _, pending := c.busy[name]; pending || r.KmsKey == "" || r.state == clusterpb.Cluster_DELETING ||
			r.state == clusterpb.Cluster_CREATING {
			continue
		}
		ts = append(ts, target{name, r.KmsKey, r.WrappedKey})
	}
	c.mu.Unlock()
	sort.Slice(ts, func(i, j int) bool { return ts[i].name < ts[j].name })
	for _, tg := range ts {
		dek, err := c.unwrapKey(ctx, tg.key, tg.wrapped)
		c.mu.Lock()
		r, ok := c.byName[tg.name]
		if _, pending := c.busy[tg.name]; !ok || pending {
			c.mu.Unlock()
			continue
		}
		r.KeyChecked = c.now()
		if err != nil {
			r.KeyState = keyState(err)
			if !r.suspended {
				c.suspendCluster(r, "suspended: its kms_key "+tg.key+" cannot be used: "+keyError(tg.key, err).(*apierror.Error).Message)
			}
			c.mu.Unlock()
			continue
		}
		r.KeyState = ""
		if !r.suspended {
			c.mu.Unlock()
			continue
		}
		if err := r.unsealWith(dek); err != nil {
			r.message = err.Error()
			c.mu.Unlock()
			continue
		}
		c.resumeCluster(r)
		c.mu.Unlock()
	}
}

// suspendCluster keeps r's data sealed and stops its nodes. Call with mu
// held.
func (c *clusters) suspendCluster(r *clusterRecord, why string) {
	r.suspended = true
	r.state, r.message = clusterpb.Cluster_STATE_UNSPECIFIED, why
	c.busy[r.Name] = "suspend"
	snap := *r
	nodes := append([]*clusterNode(nil), r.Nodes...)
	go func() {
		ctx, cancel := context.WithTimeout(c.bg, clusterWait)
		defer cancel()
		var files [][]byte
		var err error
		if _, ok := c.rt.(Shell); ok && snap.dek != nil {
			for _, n := range primariesOf(nodes) {
				var d []byte
				if d, err = c.snapshotNode(ctx, &snap, n); err != nil {
					break
				}
				if d, err = seal(snap.dek, d); err != nil {
					break
				}
				files = append(files, d)
			}
		}
		if err != nil {
			log.Printf("memorystore: suspend %s: keep its data: %v", snap.Name, err)
			files = nil
		}
		c.removeNodes(nodes)
		c.mu.Lock()
		if live := c.byName[snap.Name]; live != nil {
			live.SuspendedData = files
			for _, n := range live.Nodes {
				n.addr, n.podIP, n.port, n.id = "", "", 0, ""
			}
			_ = c.save(live)
		}
		delete(c.busy, snap.Name)
		// The data key is forgotten with the nodes: it is unwrapped again
		// to resume.
		if live := c.byName[snap.Name]; live != nil {
			live.dek = nil
		}
		c.mu.Unlock()
	}()
}

// resumeCluster forms r's nodes again and loads its kept data. Call with
// mu held, r's data key unwrapped.
func (c *clusters) resumeCluster(r *clusterRecord) {
	r.state, r.message = clusterpb.Cluster_UPDATING, "the kms_key can be used again: resuming"
	c.busy[r.Name] = "resume"
	snap := *r
	var files [][]byte
	for _, d := range r.SuspendedData {
		if p, err := open(r.dek, d); err == nil {
			files = append(files, p)
		}
	}
	name := r.Name
	go func() {
		ctx, cancel := context.WithTimeout(c.bg, clusterWait)
		defer cancel()
		err := c.rebuild(ctx, &snap, files)
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.busy, name)
		live := c.byName[name]
		if live == nil {
			return
		}
		if err != nil {
			live.state, live.message = clusterpb.Cluster_STATE_UNSPECIFIED, "the cluster did not resume: "+err.Error()
			return
		}
		live.Nodes, live.NextNode, live.SuspendedData, live.suspended = snap.Nodes, snap.NextNode, nil, false
		live.state, live.message = clusterpb.Cluster_ACTIVE, ""
		// A CLUSTER_DISABLED instance's endpoints, at its new nodes, and
		// its new pods labelled with their roles at the next check.
		if e := c.eps[name]; e != nil {
			e.labelled = ""
		}
		c.openEndpoints(live)
		_ = c.save(live)
	}()
}

// primariesOf are nodes' primaries, in shard order.
func primariesOf(nodes []*clusterNode) []*clusterNode {
	var out []*clusterNode
	for _, n := range nodes {
		if n.Primary {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Shard < out[j].Shard })
	return out
}

// rebuild gives r (a copy, mu not held) new nodes of its shape, forms
// them, and loads files into them: an engine upgrade and a resume.
func (c *clusters) rebuild(ctx context.Context, r *clusterRecord, files [][]byte) error {
	c.mu.Lock()
	r.Nodes = nil
	for sh := 0; sh < int(r.ShardCount); sh++ {
		for k := 0; k <= int(r.ReplicaCount); k++ {
			c.newNode(r, sh, k == 0)
		}
	}
	nodes := append([]*clusterNode(nil), r.Nodes...)
	c.mu.Unlock()
	err := c.startNodes(ctx, r, nodes)
	if err == nil {
		err = c.form(ctx, r, nodes)
	}
	if err == nil && len(files) > 0 {
		err = c.load(ctx, r, files)
	}
	if err != nil {
		c.removeNodes(nodes)
	}
	return err
}
