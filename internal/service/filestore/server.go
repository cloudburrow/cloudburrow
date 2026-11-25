// Package filestore serves the Filestore API (file.googleapis.com v1,
// #2192): instances of the documented tiers with their capacity limits,
// one file share each with its NFS export options, snapshots, backups,
// restore and revert, the operations they return and the locations.
//
// Each instance is a real NFS server, run by a Backend: CloudBurrow's is
// internal/filestoreserver, NFS-Ganesha in a container on the Docker
// network the instance's clusters share, so a VM or a Kubernetes Engine
// pod mounts IP:/SHARE as on Google. Without a backend every mutation is
// UNIMPLEMENTED.
//
// As Google documents it (docs.cloud.google.com/filestore/docs:
// service-tiers, creating-instances, snapshots, backups,
// backups-limitations, and the v1 REST reference, read 2026-11-23):
//
//   - BASIC_HDD (legacy STANDARD) is 1 TiB to 63.9 TiB, BASIC_SSD (legacy
//     PREMIUM) 2.5 TiB to 63.9 TiB, both in 1 GiB steps, scaled up only,
//     NFSv3 only, zonal, with backups and no snapshots;
//   - ZONAL is 1 TiB to 9.75 TiB in 256 GiB steps or 10 TiB to 100 TiB in
//     2.5 TiB steps, scaled up or down within its range, zonal; REGIONAL is
//     100 GiB to 10,239 GiB in 1 GiB steps or 10 TiB to 100 TiB in 2.5 TiB
//     steps, regional; ENTERPRISE is 1 TiB to 10 TiB in 256 GiB steps,
//     regional; HIGH_SCALE_SSD (legacy) is ZONAL's higher range. These take
//     NFSv3 or NFSv4.1, snapshots (240 an instance) and revert;
//   - an instance has one file share; its name is 1 to 16 characters on
//     the basic tiers and 1 to 32 on the others, letters, digits and
//     underscores, starting with a letter and ending with a letter or
//     digit;
//   - a share has at most 10 export options and 64 IP ranges among them;
//     access mode defaults to READ_WRITE, squash mode to NO_ROOT_SQUASH,
//     and anon_uid and anon_gid (65534 by default) are set only with
//     ROOT_SQUASH;
//   - reserved_ip_range is a /29 on the basic tiers and a /26 on the
//     others;
//   - a backup is regional; a new instance restored from one has at least
//     its capacity and the same protocol; a basic tier backup restores to a
//     basic tier instance, new or existing; the other tiers' backups
//     restore only to a new instance.
//
// Error texts Google's pages do not quote are marked UNVERIFIED (docs re-read 2026-11-23: <https://docs.cloud.google.com/filestore/docs/reference/rest/v1/projects.locations.instances>) where they
// are made.
package filestore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/filestore/apiv1/filestorepb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/cloudburrow/cloudburrow/internal/cmek"
)

// Share is one file share as the backend serves it.
type Share struct {
	// Name is the share's name, exported as /NAME.
	Name string
	// Exports are the share's NFS export options, defaults applied; none
	// is every client, read-write, without root squash.
	Exports []Export
	// CapacityBytes is the share's size (#2194): its filesystem's, so a
	// full share refuses writes; 0 keeps the share a plain directory.
	CapacityBytes int64
}

// Limits are an instance's performance limits (#2194), as
// performance_limits reports them; 0 is no limit.
type Limits struct {
	ReadIOPS, WriteIOPS, ReadBps, WriteBps int64
}

// Export is one NFS export option of a share.
type Export struct {
	IPRanges   []string
	ReadOnly   bool
	RootSquash bool
	AnonUID    int64
	AnonGID    int64
}

// Spec is what an instance's NFS server serves.
type Spec struct {
	// Address is the instance's address on its VPC network, in its
	// reserved range, which the backend makes reach the server.
	Address string
	// ReadOnly serves every share read-only: a replica's.
	ReadOnly bool
	// NFSv41 serves NFSv4.1 (and NFSv3); otherwise NFSv3 alone.
	NFSv41 bool
	Shares []Share
	// Encrypted is an instance with a customer-managed key (#2194): its
	// shares are encrypted at rest with DataKey, which Cloud KMS unwraps;
	// with DataKey nil (the key cannot be used) no share is opened.
	Encrypted bool
	DataKey   []byte
	// Limits are the instance's IOPS and throughput.
	Limits Limits
}

// Backend runs the instances' NFS servers. key names an instance's server
// and its data; backup a backup's data, which outlives its instance.
type Backend interface {
	// Ensure starts (or reconfigures) key's server with spec, at ip when
	// one was given before, and answers its address.
	Ensure(ctx context.Context, key, ip string, spec Spec) (string, error)
	// Remove stops key's server and deletes its data and snapshots.
	Remove(ctx context.Context, key string) error
	// Usage is the bytes the share's files take.
	Usage(ctx context.Context, key, share string) (int64, error)
	// Snapshot copies the share's files as snapshot snap.
	Snapshot(ctx context.Context, key, share, snap string) error
	// DeleteSnapshot deletes snapshot snap.
	DeleteSnapshot(ctx context.Context, key, share, snap string) error
	// Revert replaces the share's files with snapshot snap's.
	Revert(ctx context.Context, key, share, snap string) error
	// Backup copies the share's files out as backup and answers its size:
	// encrypted with dek (AES-256-GCM) when it is not nil.
	Backup(ctx context.Context, key, share, backup string, dek []byte) (int64, error)
	// Restore replaces the share's files with backup's, decrypted with
	// dek when it is not nil.
	Restore(ctx context.Context, key, share, backup string, dek []byte) error
	// Replicate replaces to's share's files with a copy of from's.
	Replicate(ctx context.Context, from, to, share string) error
	// DeleteBackup deletes backup's data.
	DeleteBackup(ctx context.Context, backup string) error
	// RemoveShare deletes a share's files and snapshots, once the server
	// no longer serves it (multishare instances, #2194).
	RemoveShare(ctx context.Context, key, share string) error
}

// Options are the server's collaborators.
type Options struct {
	// Backend runs the NFS servers; nil answers every mutation
	// UNIMPLEMENTED.
	Backend Backend
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Networks checks an instance's network; nil checks nothing.
	Networks func() Networks
	// Keys are the customer-managed keys (Cloud KMS); nil refuses them.
	Keys *cmek.Keys
	// Tags binds tags through Resource Manager; nil refuses them.
	Tags func() TagBinder
	// KeyCheckEvery is how often the instances' keys are checked (30s
	// when zero); ReplicationEvery how often replicas are synced (30s).
	KeyCheckEvery, ReplicationEvery time.Duration
}

// Networks are the Compute Engine VPC networks (#1936) an instance's
// network is checked against when the instance serves Compute Engine.
type Networks interface {
	// HasNetwork reports whether the network ref exists, in project when
	// ref is a bare name.
	HasNetwork(ref, project string) bool
	// Conflict names what on network (projects/P/global/networks/N) n
	// overlaps: a subnetwork or an allocated range; "" when nothing.
	Conflict(network string, n *net.IPNet) string
	// AllocatedRange is the range of the VPC_PEERING global address name
	// allocated for private services access on network.
	AllocatedRange(network, name string) (*net.IPNet, bool)
	// AllocatedRanges are every such range on network, by name.
	AllocatedRanges(network string) map[string]*net.IPNet
}

// Server serves CloudFilestoreManager.
type Server struct {
	filestorepb.UnimplementedCloudFilestoreManagerServer

	opts  Options
	state string
	bg    context.Context
	stop  context.CancelFunc
	work  sync.WaitGroup

	mu        sync.Mutex
	instances map[string]*instRecord
	snapshots map[string]*snapRecord
	backups   map[string]*backupRecord
	ops       map[string]*opRecord
	// busy names the resources with an operation running.
	busy map[string]string
}

type instRecord struct {
	I *filestorepb.Instance
	// Key names its NFS server; IP is the server's address on the Docker
	// network, which only the backend uses.
	Key string
	IP  string
	// Block is the instance's range on its VPC network and Addr its
	// address there, the one it serves at.
	Block, Addr string
	// Sealed is the data key its shares are encrypted with at rest, as
	// Cloud KMS wrapped it with kms_key_name (#2194).
	Sealed *cmek.Sealed
	// Multi is a multishare instance's own fields and its shares
	// (v1beta1, #2194); nil for an instance of one file share.
	Multi *multiState
}

type snapRecord struct {
	S *filestorepb.Snapshot
	// Key is the snapshot's name in the backend.
	Key string
}

type backupRecord struct {
	B   *filestorepb.Backup
	Key string
	// Sealed is the data key of a backup with kms_key, as Cloud KMS
	// wrapped it.
	Sealed *cmek.Sealed
}

type opRecord struct {
	Op *longrunningpb.Operation
	// done is closed when the operation finishes.
	done chan struct{}
}

// NewServer loads the records in state ("" keeps them in memory).
func NewServer(state string, opts Options) (*Server, error) {
	bg, stop := context.WithCancel(context.Background())
	s := &Server{opts: opts, state: state, bg: bg, stop: stop,
		instances: map[string]*instRecord{}, snapshots: map[string]*snapRecord{},
		backups: map[string]*backupRecord{}, ops: map[string]*opRecord{}, busy: map[string]string{}}
	if state != "" {
		if err := s.load(); err != nil {
			return nil, err
		}
	}
	s.work.Add(1)
	go s.maintain()
	return s, nil
}

// maintain checks the instances' keys and syncs their replicas until
// Close.
func (s *Server) maintain() {
	defer s.work.Done()
	every := func(d time.Duration) time.Duration {
		if d <= 0 {
			return 30 * time.Second
		}
		return d
	}
	keys := time.NewTicker(every(s.opts.KeyCheckEvery))
	defer keys.Stop()
	repl := time.NewTicker(every(s.opts.ReplicationEvery))
	defer repl.Stop()
	for {
		select {
		case <-s.bg.Done():
			return
		case <-keys.C:
			s.checkKeys(s.bg)
		case <-repl.C:
			s.replicate(s.bg)
		}
	}
}

func (s *Server) now() time.Time {
	if s.opts.Now != nil {
		return s.opts.Now()
	}
	return time.Now()
}

// Resume starts the NFS server of every instance the records hold, as
// `up` does after `stop`: their data is where the backend left it. An
// instance whose server does not start is ERROR with the reason.
func (s *Server) Resume(ctx context.Context) {
	if s.opts.Backend == nil {
		return
	}
	s.mu.Lock()
	var recs []*instRecord
	for _, r := range s.instances {
		if r.I.GetState() != filestorepb.Instance_DELETING {
			recs = append(recs, r)
		}
	}
	s.mu.Unlock()
	for _, r := range recs {
		s.mu.Lock()
		spec, key, ip, suspended := r.spec(), r.Key, r.IP, r.I.GetState() == filestorepb.Instance_SUSPENDED
		if suspended {
			// Its key is checked again by checkKeys.
			spec.Shares = nil
		}
		s.mu.Unlock()
		got, err := s.ensure(ctx, s.opts.Backend, r, key, ip, spec)
		s.mu.Lock()
		if err != nil {
			r.I.State, r.I.StatusMessage = filestorepb.Instance_ERROR, "The NFS server did not start: "+err.Error()
		} else {
			r.IP = got
			r.setIP()
			if !suspended {
				r.I.State, r.I.StatusMessage = filestorepb.Instance_READY, ""
			}
		}
		_ = s.saveLocked()
		s.mu.Unlock()
	}
}

// Close waits for the running operations' work, after cancelling it.
func (s *Server) Close() {
	s.stop()
	s.work.Wait()
}

// state file

type stateFile struct {
	Instances []stateInst   `json:"instances"`
	Snapshots []stateSnap   `json:"snapshots"`
	Backups   []stateBackup `json:"backups"`
}

type stateInst struct {
	Instance json.RawMessage `json:"instance"`
	Key      string          `json:"key"`
	IP       string          `json:"ip"`
	Block    string          `json:"block,omitempty"`
	Address  string          `json:"address,omitempty"`
	Sealed   *cmek.Sealed    `json:"sealed,omitempty"`
	Multi    *multiState     `json:"multi,omitempty"`
}

type stateSnap struct {
	Snapshot json.RawMessage `json:"snapshot"`
	Key      string          `json:"key"`
}

type stateBackup struct {
	Backup json.RawMessage `json:"backup"`
	Key    string          `json:"key"`
	Sealed *cmek.Sealed    `json:"sealed,omitempty"`
}

func (s *Server) load() error {
	b, err := os.ReadFile(s.state)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("filestore: read state: %w", err)
	}
	var f stateFile
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("filestore: parse state %s: %w", s.state, err)
	}
	for _, x := range f.Instances {
		in := &filestorepb.Instance{}
		if err := protojson.Unmarshal(x.Instance, in); err != nil {
			return fmt.Errorf("filestore: parse state %s: %w", s.state, err)
		}
		s.instances[in.GetName()] = &instRecord{I: in, Key: x.Key, IP: x.IP, Block: x.Block, Addr: x.Address, Sealed: x.Sealed, Multi: x.Multi}
	}
	for _, x := range f.Snapshots {
		sn := &filestorepb.Snapshot{}
		if err := protojson.Unmarshal(x.Snapshot, sn); err != nil {
			return fmt.Errorf("filestore: parse state %s: %w", s.state, err)
		}
		s.snapshots[sn.GetName()] = &snapRecord{S: sn, Key: x.Key}
	}
	for _, x := range f.Backups {
		bk := &filestorepb.Backup{}
		if err := protojson.Unmarshal(x.Backup, bk); err != nil {
			return fmt.Errorf("filestore: parse state %s: %w", s.state, err)
		}
		s.backups[bk.GetName()] = &backupRecord{B: bk, Key: x.Key, Sealed: x.Sealed}
	}
	return nil
}

func (s *Server) saveLocked() error {
	if s.state == "" {
		return nil
	}
	var f stateFile
	for _, name := range sortedKeys(s.instances) {
		r := s.instances[name]
		b, err := protojson.Marshal(r.I)
		if err != nil {
			return err
		}
		f.Instances = append(f.Instances, stateInst{Instance: b, Key: r.Key, IP: r.IP, Block: r.Block, Address: r.Addr, Sealed: r.Sealed, Multi: r.Multi})
	}
	for _, name := range sortedKeys(s.snapshots) {
		r := s.snapshots[name]
		b, err := protojson.Marshal(r.S)
		if err != nil {
			return err
		}
		f.Snapshots = append(f.Snapshots, stateSnap{Snapshot: b, Key: r.Key})
	}
	for _, name := range sortedKeys(s.backups) {
		r := s.backups[name]
		b, err := protojson.Marshal(r.B)
		if err != nil {
			return err
		}
		f.Backups = append(f.Backups, stateBackup{Backup: b, Key: r.Key, Sealed: r.Sealed})
	}
	b, err := json.MarshalIndent(f, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.state), 0o700); err != nil {
		return err
	}
	tmp := s.state + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.state)
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// keyOf is a backend name: a prefix and a hash of the resource's name and
// creation time, so a resource made again under the same name has new
// data.
func keyOf(prefix, name string, t time.Time) string {
	h := sha256.Sum256([]byte(name + "@" + t.UTC().Format(time.RFC3339Nano)))
	return prefix + hex.EncodeToString(h[:6])
}

// etagOf is a resource's etag: a hash of its fields.
func etagOf(m proto.Message) string {
	c := proto.Clone(m)
	if in, ok := c.(*filestorepb.Instance); ok {
		in.Etag = ""
	}
	b, _ := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

func cloneInst(in *filestorepb.Instance) *filestorepb.Instance {
	c := proto.Clone(in).(*filestorepb.Instance)
	c.Etag = etagOf(in)
	return c
}

// specOf is what an instance's NFS server serves.
func specOf(in *filestorepb.Instance) Spec {
	spec := Spec{NFSv41: in.GetProtocol() == filestorepb.Instance_NFS_V4_1}
	for _, fs := range in.GetFileShares() {
		sh := Share{Name: fs.GetName()}
		for _, o := range fs.GetNfsExportOptions() {
			e := Export{IPRanges: append([]string(nil), o.GetIpRanges()...),
				ReadOnly:   o.GetAccessMode() == filestorepb.NfsExportOptions_READ_ONLY,
				RootSquash: o.GetSquashMode() == filestorepb.NfsExportOptions_ROOT_SQUASH,
				AnonUID:    65534, AnonGID: 65534}
			if o.GetAnonUid() != 0 {
				e.AnonUID = o.GetAnonUid()
			}
			if o.GetAnonGid() != 0 {
				e.AnonGID = o.GetAnonGid()
			}
			sh.Exports = append(sh.Exports, e)
		}
		sh.CapacityBytes = fs.GetCapacityGb() << 30
		spec.Shares = append(spec.Shares, sh)
	}
	spec.Encrypted = in.GetKmsKeyName() != ""
	return spec
}

// spec is what the instance's NFS server serves, at its address.
func (r *instRecord) spec() Spec { return r.specFor(r.I) }

func (r *instRecord) specFor(in *filestorepb.Instance) Spec {
	spec := specOf(in)
	if r.Multi != nil {
		spec.Shares = r.Multi.spec()
	}
	spec.Address = r.Addr
	spec.ReadOnly = isStandby(in)
	spec.Limits = limitsOf(PerformanceLimits(in.GetTier(), r.capacityOf(in), in.GetPerformanceConfig()))
	return spec
}

// setIP answers the instance's address on its network (the server's own,
// for an instance a release before #2192's addresses made).
func (r *instRecord) setIP() {
	ip := r.Addr
	if ip == "" {
		ip = r.IP
	}
	if len(r.I.GetNetworks()) > 0 && ip != "" {
		r.I.Networks[0].IpAddresses = []string{ip}
	}
}

// ResetProject deletes a project's instances (their servers and data),
// snapshots and backups; "" is every project.
func (s *Server) ResetProject(project string) error {
	s.mu.Lock()
	var keys, backupKeys []string
	match := func(name string) bool { return project == "" || strings.HasPrefix(name, "projects/"+project+"/") }
	for name, r := range s.instances {
		if match(name) {
			keys = append(keys, r.Key)
			delete(s.instances, name)
		}
	}
	for name := range s.snapshots {
		if match(name) {
			delete(s.snapshots, name)
		}
	}
	for name, r := range s.backups {
		if match(name) {
			backupKeys = append(backupKeys, r.Key)
			delete(s.backups, name)
		}
	}
	for name := range s.ops {
		if match(name) {
			delete(s.ops, name)
		}
	}
	err := s.saveLocked()
	s.mu.Unlock()
	if b := s.opts.Backend; b != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		for _, k := range keys {
			err = errors.Join(err, b.Remove(ctx, k))
		}
		for _, k := range backupKeys {
			err = errors.Join(err, b.DeleteBackup(ctx, k))
		}
	}
	return err
}

// Instances are the instances, for the console.
func (s *Server) Instances() []*filestorepb.Instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*filestorepb.Instance
	for _, name := range sortedKeys(s.instances) {
		out = append(out, cloneInst(s.instances[name].I))
	}
	return out
}

// ServerKey is the backend key of the instance named name, or "".
func (s *Server) ServerKey(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.instances[name]; ok {
		return r.Key
	}
	return ""
}
