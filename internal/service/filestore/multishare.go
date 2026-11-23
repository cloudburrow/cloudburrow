package filestore

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"cloud.google.com/go/filestore/apiv1/filestorepb"
	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/aipfilter"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	betapb "github.com/cloudburrow/cloudburrow/internal/service/filestore/filestorev1beta1pb"
)

// Multishare instances (#2194): file.googleapis.com v1beta1, which
// Google's GKE Filestore CSI driver provisions `enterprise-multishare-rwx`
// volumes through, as https://docs.cloud.google.com/filestore/docs/multishares
// and the v1beta1 reference (projects.locations.instances.shares) document
// them (read 2026-11-23):
//
//   - an instance with multi_share_enabled has no file_shares: its shares
//     are resources of their own (instances/I/shares/S), each exported as
//     /MOUNT_NAME with its own capacity and NFS export options;
//   - multishare is "limited to the Filestore enterprise service tier";
//     the instance is 1 TiB to 10 TiB, in steps of 256 GiB
//     (capacity_step_size_gb 256, max_capacity_gb 10240);
//   - a share is 10 GiB to 1 TiB; max_share_count shares an instance at
//     most, 10 by default and up to 80, each at most 10 TiB / max_share_count
//     (the max-volume-size table: 128 GiB for 80 shares, 256 GiB for 40,
//     512 GiB for 20, 1 TiB for 10); the shares' capacities sum to at most
//     the instance's;
//   - snapshots are "not yet supported for Filestore multishares", nor is
//     replication; an instance's limits are its tier's for its capacity.
//
// Each share is a filesystem of its own on the instance's NFS server
// (filestoreserver), so a full share refuses writes while the instance's
// other shares do not. The rest of v1beta1 is v1's: requests and answers
// are converted through their JSON forms, which the two versions share.

const (
	multiMinGB, multiMaxGB, multiStepGB = 1024, 10240, 256
	shareMinGB, shareMaxGB              = 10, 1024
	defaultShareCount, maxShareCount    = 10, 80
)

var mountRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,62}$`)

// multiState is a multishare instance's own fields and its shares.
type multiState struct {
	CapacityGB    int64
	MaxShareCount int64
	Shares        []*betapb.Share
}

type multiJSON struct {
	CapacityGB    int64             `json:"capacityGb"`
	MaxShareCount int64             `json:"maxShareCount"`
	Shares        []json.RawMessage `json:"shares,omitempty"`
}

func (m *multiState) MarshalJSON() ([]byte, error) {
	out := multiJSON{CapacityGB: m.CapacityGB, MaxShareCount: m.MaxShareCount}
	for _, sh := range m.Shares {
		b, err := protojson.Marshal(sh)
		if err != nil {
			return nil, err
		}
		out.Shares = append(out.Shares, b)
	}
	return json.Marshal(out)
}

func (m *multiState) UnmarshalJSON(b []byte) error {
	var in multiJSON
	if err := json.Unmarshal(b, &in); err != nil {
		return err
	}
	m.CapacityGB, m.MaxShareCount, m.Shares = in.CapacityGB, in.MaxShareCount, nil
	for _, raw := range in.Shares {
		sh := &betapb.Share{}
		if err := protojson.Unmarshal(raw, sh); err != nil {
			return err
		}
		m.Shares = append(m.Shares, sh)
	}
	return nil
}

// maxShareGB is the largest share the instance takes.
func (m *multiState) maxShareGB() int64 {
	return min(shareMaxGB, multiMaxGB/max(m.MaxShareCount, 1))
}

// usedGB is the capacity the shares take, without skip's.
func (m *multiState) usedGB(skip string) int64 {
	var n int64
	for _, sh := range m.Shares {
		if sh.GetName() != skip {
			n += sh.GetCapacityGb()
		}
	}
	return n
}

func (m *multiState) find(name string) *betapb.Share {
	for _, sh := range m.Shares {
		if sh.GetName() == name {
			return sh
		}
	}
	return nil
}

// mounts are the shares' mount names.
func (m *multiState) mounts() []string {
	var out []string
	for _, sh := range m.Shares {
		out = append(out, sh.GetMountName())
	}
	return out
}

// spec is what the shares serve, in mount-name order.
func (m *multiState) spec() []Share {
	shares := append([]*betapb.Share(nil), m.Shares...)
	sort.Slice(shares, func(i, j int) bool { return shares[i].GetMountName() < shares[j].GetMountName() })
	var out []Share
	for _, sh := range shares {
		if sh.GetState() == betapb.Share_DELETING {
			continue
		}
		v1 := &filestorepb.FileShareConfig{Name: sh.GetMountName(), CapacityGb: sh.GetCapacityGb()}
		for _, o := range sh.GetNfsExportOptions() {
			v1.NfsExportOptions = append(v1.NfsExportOptions, exportToV1(o))
		}
		out = append(out, specOf(&filestorepb.Instance{FileShares: []*filestorepb.FileShareConfig{v1}}).Shares...)
	}
	return out
}

// checkCapacity checks a multishare instance's capacity.
func checkMultiCapacity(gb int64) error {
	if gb < multiMinGB || gb > multiMaxGB || gb%multiStepGB != 0 {
		// UNVERIFIED wording (https://docs.cloud.google.com/filestore/docs/multishares).
		return apierror.InvalidArgument("capacity_gb %d is invalid: a multishare instance is %d GiB to %d GiB, in steps of %d GiB", gb, multiMinGB, multiMaxGB, multiStepGB)
	}
	return nil
}

// checkNew checks a new multishare instance.
func (m *multiState) checkNew(in *filestorepb.Instance) error {
	if in.GetTier() != filestorepb.Instance_ENTERPRISE {
		return apierror.InvalidArgument("multi_share_enabled is supported only for tier ENTERPRISE, not %s", tierName(in.GetTier()))
	}
	if len(in.GetFileShares()) > 0 {
		return apierror.InvalidArgument("A multishare instance has no file_shares: create its shares with CreateShare")
	}
	if m.MaxShareCount == 0 {
		m.MaxShareCount = defaultShareCount
	}
	if m.MaxShareCount < 1 || m.MaxShareCount > maxShareCount {
		return apierror.InvalidArgument("max_share_count %d is invalid: it must be 1 to %d", m.MaxShareCount, maxShareCount)
	}
	if in.GetPerformanceConfig() != nil {
		return apierror.InvalidArgument("performance_config is supported only by the ZONAL and REGIONAL tiers")
	}
	return checkMultiCapacity(m.CapacityGB)
}

// multiRefused refuses what a multishare instance does not support.
func multiRefused(r *instRecord) error {
	if r.Multi != nil {
		// UNVERIFIED wording: "Snapshots are not yet supported for Filestore
		// multishares" (https://docs.cloud.google.com/filestore/docs/multishares).
		return apierror.FailedPrecondition("Instance %s is a multishare instance: snapshots, backups and restores of its file shares are not supported for multishare instances", r.I.GetName())
	}
	return nil
}

// conversion between the versions, through JSON

var discard = protojson.UnmarshalOptions{DiscardUnknown: true}

func convert[T proto.Message](from proto.Message, to T) T {
	b, err := protojson.Marshal(from)
	if err == nil {
		_ = discard.Unmarshal(b, to)
	}
	return to
}

func exportToV1(o *betapb.NfsExportOptions) *filestorepb.NfsExportOptions {
	return convert(o, &filestorepb.NfsExportOptions{})
}

func exportsToV1(opts []*betapb.NfsExportOptions) []*filestorepb.NfsExportOptions {
	var out []*filestorepb.NfsExportOptions
	for _, o := range opts {
		out = append(out, exportToV1(o))
	}
	return out
}

// BetaServer serves v1beta1's CloudFilestoreManager over the same records.
type BetaServer struct {
	betapb.UnimplementedCloudFilestoreManagerServer
	s *Server
}

// Beta is the v1beta1 API over s.
func (s *Server) Beta() *BetaServer { return &BetaServer{s: s} }

// toBeta is in as v1beta1 answers it.
func (b *BetaServer) toBeta(in *filestorepb.Instance) *betapb.Instance {
	out := convert(in, &betapb.Instance{})
	b.s.mu.Lock()
	defer b.s.mu.Unlock()
	if r := b.s.instances[in.GetName()]; r != nil && r.Multi != nil {
		out.MultiShareEnabled = true
		out.CapacityGb = r.Multi.CapacityGB
		out.MaxShareCount = r.Multi.MaxShareCount
		out.MaxCapacityGb = multiMaxGB
		out.CapacityStepSizeGb = multiStepGB
	}
	return out
}

func (b *BetaServer) answer(in *filestorepb.Instance) proto.Message {
	out := convert(in, &betapb.Instance{})
	if r := b.s.instances[in.GetName()]; r != nil && r.Multi != nil {
		out.MultiShareEnabled, out.CapacityGb, out.MaxShareCount = true, r.Multi.CapacityGB, r.Multi.MaxShareCount
		out.MaxCapacityGb, out.CapacityStepSizeGb = multiMaxGB, multiStepGB
	}
	return out
}

// ListInstances lists instances.
func (b *BetaServer) ListInstances(ctx context.Context, req *betapb.ListInstancesRequest) (*betapb.ListInstancesResponse, error) {
	got, err := b.s.ListInstances(ctx, convert(req, &filestorepb.ListInstancesRequest{}))
	if err != nil {
		return nil, err
	}
	out := &betapb.ListInstancesResponse{NextPageToken: got.GetNextPageToken(), Unreachable: got.GetUnreachable()}
	for _, in := range got.GetInstances() {
		out.Instances = append(out.Instances, b.toBeta(in))
	}
	return out, nil
}

// GetInstance answers an instance.
func (b *BetaServer) GetInstance(ctx context.Context, req *betapb.GetInstanceRequest) (*betapb.Instance, error) {
	in, err := b.s.GetInstance(ctx, &filestorepb.GetInstanceRequest{Name: req.GetName()})
	if err != nil {
		return nil, err
	}
	return b.toBeta(in), nil
}

// CreateInstance creates an instance: a multishare one with
// multi_share_enabled.
func (b *BetaServer) CreateInstance(ctx context.Context, req *betapb.CreateInstanceRequest) (*longrunningpb.Operation, error) {
	v1 := convert(req, &filestorepb.CreateInstanceRequest{})
	var multi *multiState
	if in := req.GetInstance(); in.GetMultiShareEnabled() {
		multi = &multiState{CapacityGB: in.GetCapacityGb(), MaxShareCount: in.GetMaxShareCount()}
	} else if req.GetInstance() != nil && req.GetInstance().GetCapacityGb() != 0 {
		return nil, apierror.InvalidArgument("capacity_gb is set only on a multishare instance: set the file share's capacity_gb")
	}
	return b.s.createInstance(ctx, v1, multi, b.answer)
}

// UpdateInstance updates an instance; a multishare instance's
// capacity_gb, description, labels and deletion protection.
func (b *BetaServer) UpdateInstance(ctx context.Context, req *betapb.UpdateInstanceRequest) (*longrunningpb.Operation, error) {
	s := b.s
	s.mu.Lock()
	r, err := s.instanceLocked(req.GetInstance().GetName())
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	multi := r.Multi != nil
	s.mu.Unlock()
	if !multi {
		for _, p := range req.GetUpdateMask().GetPaths() {
			if p == "capacity_gb" || p == "capacityGb" || p == "max_share_count" || p == "maxShareCount" {
				return nil, apierror.InvalidArgument("Field %q is updatable only on a multishare instance", p)
			}
		}
		return s.UpdateInstance(ctx, convert(req, &filestorepb.UpdateInstanceRequest{}))
	}
	return s.updateMulti(req, b.answer)
}

func (s *Server) updateMulti(req *betapb.UpdateInstanceRequest, answer func(*filestorepb.Instance) proto.Message) (*longrunningpb.Operation, error) {
	in := req.GetInstance()
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, apierror.InvalidArgument("update_mask is required")
	}
	if err := s.noBackend(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.instanceLocked(in.GetName())
	if err != nil {
		return nil, err
	}
	if err := s.busyLocked(r.I.GetName()); err != nil {
		return nil, err
	}
	if r.I.GetState() != filestorepb.Instance_READY {
		return nil, apierror.FailedPrecondition("Instance %s is %s: it can be updated only when READY", r.I.GetName(), r.I.GetState())
	}
	next := proto.Clone(r.I).(*filestorepb.Instance)
	nm := *r.Multi
	for _, p := range paths {
		switch p {
		case "description":
			next.Description = in.GetDescription()
		case "labels":
			if err := checkLabels(in.GetLabels()); err != nil {
				return nil, err
			}
			next.Labels = in.GetLabels()
		case "deletion_protection_enabled", "deletionProtectionEnabled":
			next.DeletionProtectionEnabled = in.GetDeletionProtectionEnabled()
			if !next.DeletionProtectionEnabled {
				next.DeletionProtectionReason = ""
			}
		case "deletion_protection_reason", "deletionProtectionReason":
			next.DeletionProtectionReason = in.GetDeletionProtectionReason()
		case "capacity_gb", "capacityGb":
			if err := checkMultiCapacity(in.GetCapacityGb()); err != nil {
				return nil, err
			}
			if used := nm.usedGB(""); in.GetCapacityGb() < used {
				return nil, apierror.InvalidArgument("capacity_gb %d is less than the %d GiB the instance's shares take", in.GetCapacityGb(), used)
			}
			nm.CapacityGB = in.GetCapacityGb()
		case "max_share_count", "maxShareCount":
			c := in.GetMaxShareCount()
			if c < int64(len(nm.Shares)) || c < 1 || c > maxShareCount {
				return nil, apierror.InvalidArgument("max_share_count %d is invalid: it must be 1 to %d and at least the instance's %d shares", c, maxShareCount, len(nm.Shares))
			}
			nm.MaxShareCount = c
			for _, sh := range nm.Shares {
				if sh.GetCapacityGb() > nm.maxShareGB() {
					return nil, apierror.InvalidArgument("max_share_count %d allows shares of at most %d GiB, and share %s has %d GiB", c, nm.maxShareGB(), sh.GetName(), sh.GetCapacityGb())
				}
			}
		default:
			return nil, apierror.InvalidArgument("Field %q is not an updatable field of a multishare instance", p)
		}
	}
	if !next.GetDeletionProtectionEnabled() && next.GetDeletionProtectionReason() != "" {
		return nil, apierror.InvalidArgument("deletion_protection_reason can be set only with deletion_protection_enabled")
	}
	old := r.Multi
	r.Multi = &nm
	spec := r.specFor(next)
	r.Multi = old
	reconfigure := nm.CapacityGB != old.CapacityGB
	key, ip, b := r.Key, r.IP, s.opts.Backend
	return s.startOpLocked(locationOf(r.I.GetName()), r.I.GetName(), "update", func(ctx context.Context) error {
		if !reconfigure {
			return nil
		}
		_, err := s.ensure(ctx, b, r, key, ip, spec)
		return err
	}, func(err error) proto.Message {
		if err != nil {
			return nil
		}
		next.State = r.I.GetState()
		r.I, r.Multi = next, &nm
		r.setLimits(r.I)
		return answer(cloneInst(r.I))
	}), nil
}

// The rest of the instance and its snapshots and backups are v1's.

func (b *BetaServer) RestoreInstance(ctx context.Context, req *betapb.RestoreInstanceRequest) (*longrunningpb.Operation, error) {
	return b.s.RestoreInstance(ctx, convert(req, &filestorepb.RestoreInstanceRequest{}))
}

func (b *BetaServer) RevertInstance(ctx context.Context, req *betapb.RevertInstanceRequest) (*longrunningpb.Operation, error) {
	return b.s.RevertInstance(ctx, convert(req, &filestorepb.RevertInstanceRequest{}))
}

func (b *BetaServer) PromoteReplica(ctx context.Context, req *betapb.PromoteReplicaRequest) (*longrunningpb.Operation, error) {
	return b.s.PromoteReplica(ctx, convert(req, &filestorepb.PromoteReplicaRequest{}))
}

func (b *BetaServer) DeleteInstance(ctx context.Context, req *betapb.DeleteInstanceRequest) (*longrunningpb.Operation, error) {
	return b.s.DeleteInstance(ctx, convert(req, &filestorepb.DeleteInstanceRequest{}))
}

func (b *BetaServer) ListSnapshots(ctx context.Context, req *betapb.ListSnapshotsRequest) (*betapb.ListSnapshotsResponse, error) {
	got, err := b.s.ListSnapshots(ctx, convert(req, &filestorepb.ListSnapshotsRequest{}))
	if err != nil {
		return nil, err
	}
	return convert(got, &betapb.ListSnapshotsResponse{}), nil
}

func (b *BetaServer) GetSnapshot(ctx context.Context, req *betapb.GetSnapshotRequest) (*betapb.Snapshot, error) {
	got, err := b.s.GetSnapshot(ctx, convert(req, &filestorepb.GetSnapshotRequest{}))
	if err != nil {
		return nil, err
	}
	return convert(got, &betapb.Snapshot{}), nil
}

func (b *BetaServer) CreateSnapshot(ctx context.Context, req *betapb.CreateSnapshotRequest) (*longrunningpb.Operation, error) {
	return b.s.CreateSnapshot(ctx, convert(req, &filestorepb.CreateSnapshotRequest{}))
}

func (b *BetaServer) DeleteSnapshot(ctx context.Context, req *betapb.DeleteSnapshotRequest) (*longrunningpb.Operation, error) {
	return b.s.DeleteSnapshot(ctx, convert(req, &filestorepb.DeleteSnapshotRequest{}))
}

func (b *BetaServer) UpdateSnapshot(ctx context.Context, req *betapb.UpdateSnapshotRequest) (*longrunningpb.Operation, error) {
	return b.s.UpdateSnapshot(ctx, convert(req, &filestorepb.UpdateSnapshotRequest{}))
}

func (b *BetaServer) ListBackups(ctx context.Context, req *betapb.ListBackupsRequest) (*betapb.ListBackupsResponse, error) {
	got, err := b.s.ListBackups(ctx, convert(req, &filestorepb.ListBackupsRequest{}))
	if err != nil {
		return nil, err
	}
	return convert(got, &betapb.ListBackupsResponse{}), nil
}

func (b *BetaServer) GetBackup(ctx context.Context, req *betapb.GetBackupRequest) (*betapb.Backup, error) {
	got, err := b.s.GetBackup(ctx, convert(req, &filestorepb.GetBackupRequest{}))
	if err != nil {
		return nil, err
	}
	return convert(got, &betapb.Backup{}), nil
}

func (b *BetaServer) CreateBackup(ctx context.Context, req *betapb.CreateBackupRequest) (*longrunningpb.Operation, error) {
	return b.s.CreateBackup(ctx, convert(req, &filestorepb.CreateBackupRequest{}))
}

func (b *BetaServer) DeleteBackup(ctx context.Context, req *betapb.DeleteBackupRequest) (*longrunningpb.Operation, error) {
	return b.s.DeleteBackup(ctx, convert(req, &filestorepb.DeleteBackupRequest{}))
}

func (b *BetaServer) UpdateBackup(ctx context.Context, req *betapb.UpdateBackupRequest) (*longrunningpb.Operation, error) {
	return b.s.UpdateBackup(ctx, convert(req, &filestorepb.UpdateBackupRequest{}))
}

// shares

var shareNameRE = regexp.MustCompile(`^(projects/[^/]+/locations/[^/]+/instances/[^/]+)/shares/([^/]+)$`)

// multiLocked is the multishare instance named name.
func (s *Server) multiLocked(name string) (*instRecord, error) {
	r, err := s.instanceLocked(name)
	if err != nil {
		return nil, err
	}
	if r.Multi == nil {
		return nil, apierror.FailedPrecondition("Instance %s is not a multishare instance: it has no shares", name)
	}
	return r, nil
}

func (s *Server) shareLocked(name string) (*instRecord, *betapb.Share, error) {
	m := shareNameRE.FindStringSubmatch(name)
	if m == nil {
		return nil, nil, apierror.InvalidArgument("name %q must be projects/{project}/locations/{location}/instances/{instance}/shares/{share}", name)
	}
	r, err := s.instanceLocked(m[1])
	if err != nil {
		return nil, nil, err
	}
	var sh *betapb.Share
	if r.Multi != nil {
		sh = r.Multi.find(name)
	}
	if sh == nil {
		return nil, nil, apierror.NotFound("Resource '%s' was not found", name)
	}
	return r, sh, nil
}

// ListShares lists an instance's shares.
func (b *BetaServer) ListShares(_ context.Context, req *betapb.ListSharesRequest) (*betapb.ListSharesResponse, error) {
	s := b.s
	s.mu.Lock()
	r, err := s.instanceLocked(req.GetParent())
	var items []*betapb.Share
	if err == nil && r.Multi != nil {
		for _, sh := range r.Multi.Shares {
			items = append(items, proto.Clone(sh).(*betapb.Share))
		}
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool { return items[i].GetName() < items[j].GetName() })
	page, next, err := aipfilter.ListPage("shares:"+req.GetParent(), req.GetFilter(), req.GetOrderBy(), items, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, err
	}
	return &betapb.ListSharesResponse{Shares: page, NextPageToken: next}, nil
}

// GetShare answers a share.
func (b *BetaServer) GetShare(_ context.Context, req *betapb.GetShareRequest) (*betapb.Share, error) {
	b.s.mu.Lock()
	defer b.s.mu.Unlock()
	_, sh, err := b.s.shareLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	return proto.Clone(sh).(*betapb.Share), nil
}

// checkShare checks a share's capacity, export options and labels.
func checkShare(m *multiState, sh *betapb.Share) error {
	if c := sh.GetCapacityGb(); c < shareMinGB || c > m.maxShareGB() {
		// UNVERIFIED wording (https://docs.cloud.google.com/filestore/docs/multishares).
		return apierror.InvalidArgument("capacity_gb %d is invalid: a share of this instance is %d GiB to %d GiB", c, shareMinGB, m.maxShareGB())
	}
	if err := checkExports(exportsToV1(sh.GetNfsExportOptions())); err != nil {
		return err
	}
	return checkLabels(sh.GetLabels())
}

// readyLocked checks that r takes a share change now.
func (s *Server) readyLocked(r *instRecord) error {
	if err := s.busyLocked(r.I.GetName()); err != nil {
		return err
	}
	if r.I.GetState() != filestorepb.Instance_READY {
		return apierror.FailedPrecondition("Instance %s is %s: its shares can be changed only when it is READY", r.I.GetName(), r.I.GetState())
	}
	return nil
}

// CreateShare creates a share of a multishare instance.
func (b *BetaServer) CreateShare(_ context.Context, req *betapb.CreateShareRequest) (*longrunningpb.Operation, error) {
	s := b.s
	if !idRE.MatchString(req.GetShareId()) {
		return nil, apierror.InvalidArgument("share_id %q is invalid: it must be 1 to 63 characters, start with a lowercase letter, contain only lowercase letters, digits and dashes, and end with a letter or digit", req.GetShareId())
	}
	in := req.GetShare()
	if in == nil {
		return nil, apierror.InvalidArgument("share is required")
	}
	sh := proto.Clone(in).(*betapb.Share)
	if !mountRE.MatchString(sh.GetMountName()) {
		return nil, apierror.InvalidArgument("mount_name %q is invalid: it must be 1 to 63 letters, digits and underscores, starting with a letter", sh.GetMountName())
	}
	if sh.GetBackup() != "" {
		return nil, apierror.Unimplemented("Restoring a share from a backup is not supported: backups of multishare instances' shares are not supported")
	}
	if err := s.noBackend(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.multiLocked(req.GetParent())
	if err != nil {
		return nil, err
	}
	name := req.GetParent() + "/shares/" + req.GetShareId()
	if r.Multi.find(name) != nil {
		return nil, apierror.AlreadyExists("Resource '%s' already exists", name)
	}
	for _, o := range r.Multi.Shares {
		if strings.EqualFold(o.GetMountName(), sh.GetMountName()) {
			return nil, apierror.AlreadyExists("Share %s already has mount_name %q", o.GetName(), sh.GetMountName())
		}
	}
	if err := checkShare(r.Multi, sh); err != nil {
		return nil, err
	}
	if err := s.readyLocked(r); err != nil {
		return nil, err
	}
	if int64(len(r.Multi.Shares)) >= r.Multi.MaxShareCount {
		return nil, apierror.FailedPrecondition("Instance %s has %d shares: max_share_count is %d", r.I.GetName(), len(r.Multi.Shares), r.Multi.MaxShareCount)
	}
	if used := r.Multi.usedGB(""); used+sh.GetCapacityGb() > r.Multi.CapacityGB {
		return nil, apierror.FailedPrecondition("Instance %s has %d GiB of its %d GiB free: a share of %d GiB does not fit; increase the instance's capacity_gb first", r.I.GetName(), r.Multi.CapacityGB-used, r.Multi.CapacityGB, sh.GetCapacityGb())
	}
	sh.Name, sh.State, sh.CreateTime = name, betapb.Share_CREATING, timestamppb.New(s.now())
	r.Multi.Shares = append(r.Multi.Shares, sh)
	_ = s.saveLocked()
	key, ip, spec, be := r.Key, r.IP, r.spec(), s.opts.Backend
	return s.startOpLocked(locationOf(r.I.GetName()), r.I.GetName(), "create", func(ctx context.Context) error {
		_, err := s.ensure(ctx, be, r, key, ip, spec)
		return err
	}, func(err error) proto.Message {
		if err != nil {
			r.Multi.Shares = without(r.Multi.Shares, name)
			return nil
		}
		sh.State = betapb.Share_READY
		return proto.Clone(sh)
	}), nil
}

func without(shares []*betapb.Share, name string) []*betapb.Share {
	var out []*betapb.Share
	for _, sh := range shares {
		if sh.GetName() != name {
			out = append(out, sh)
		}
	}
	return out
}

// UpdateShare changes a share's description, labels, capacity_gb and
// nfs_export_options.
func (b *BetaServer) UpdateShare(ctx context.Context, req *betapb.UpdateShareRequest) (*longrunningpb.Operation, error) {
	s := b.s
	in := req.GetShare()
	paths := req.GetUpdateMask().GetPaths()
	if in == nil || len(paths) == 0 {
		return nil, apierror.InvalidArgument("share and update_mask are required")
	}
	if err := s.noBackend(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	r, cur, err := s.shareLocked(in.GetName())
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	next := proto.Clone(cur).(*betapb.Share)
	for _, p := range paths {
		switch p {
		case "description":
			next.Description = in.GetDescription()
		case "labels":
			next.Labels = in.GetLabels()
		case "capacity_gb", "capacityGb":
			next.CapacityGb = in.GetCapacityGb()
		case "nfs_export_options", "nfsExportOptions":
			next.NfsExportOptions = in.GetNfsExportOptions()
		default:
			s.mu.Unlock()
			return nil, apierror.InvalidArgument("Field %q is not an updatable field of a share", p)
		}
	}
	if err := checkShare(r.Multi, next); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if err := s.readyLocked(r); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if used := r.Multi.usedGB(cur.GetName()); used+next.GetCapacityGb() > r.Multi.CapacityGB {
		s.mu.Unlock()
		return nil, apierror.FailedPrecondition("Instance %s has %d GiB of its %d GiB free: share %s cannot grow to %d GiB; increase the instance's capacity_gb first", r.I.GetName(), r.Multi.CapacityGB-used-cur.GetCapacityGb(), r.Multi.CapacityGB, cur.GetName(), next.GetCapacityGb())
	}
	key := r.Key
	s.mu.Unlock()
	if next.GetCapacityGb() < cur.GetCapacityGb() {
		used, err := s.opts.Backend.Usage(ctx, key, cur.GetMountName())
		if err == nil && used > next.GetCapacityGb()<<30 {
			return nil, apierror.FailedPrecondition("Share %s holds %d bytes: it cannot shrink to %d GiB", cur.GetName(), used, next.GetCapacityGb())
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.readyLocked(r); err != nil {
		return nil, err
	}
	nm := *r.Multi
	nm.Shares = append(without(nm.Shares, cur.GetName()), next)
	old := r.Multi
	r.Multi = &nm
	spec := r.spec()
	r.Multi = old
	ip, be := r.IP, s.opts.Backend
	reconfigure := next.GetCapacityGb() != cur.GetCapacityGb() || !proto.Equal(&betapb.Share{NfsExportOptions: next.GetNfsExportOptions()}, &betapb.Share{NfsExportOptions: cur.GetNfsExportOptions()})
	return s.startOpLocked(locationOf(r.I.GetName()), r.I.GetName(), "update", func(ctx context.Context) error {
		if !reconfigure {
			return nil
		}
		_, err := s.ensure(ctx, be, r, key, ip, spec)
		return err
	}, func(err error) proto.Message {
		if err != nil {
			return nil
		}
		r.Multi.Shares = append(without(r.Multi.Shares, cur.GetName()), next)
		_ = s.saveLocked()
		return proto.Clone(next)
	}), nil
}

// DeleteShare deletes a share and its files.
func (b *BetaServer) DeleteShare(_ context.Context, req *betapb.DeleteShareRequest) (*longrunningpb.Operation, error) {
	s := b.s
	if err := s.noBackend(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, sh, err := s.shareLocked(req.GetName())
	if err != nil {
		return nil, err
	}
	if err := s.readyLocked(r); err != nil {
		return nil, err
	}
	sh.State = betapb.Share_DELETING
	_ = s.saveLocked()
	key, ip, spec, mount, be := r.Key, r.IP, r.spec(), sh.GetMountName(), s.opts.Backend
	return s.startOpLocked(locationOf(r.I.GetName()), r.I.GetName(), "delete", func(ctx context.Context) error {
		if _, err := s.ensure(ctx, be, r, key, ip, spec); err != nil {
			return err
		}
		return be.RemoveShare(ctx, key, mount)
	}, func(err error) proto.Message {
		if err != nil {
			sh.State = betapb.Share_READY
			return nil
		}
		r.Multi.Shares = without(r.Multi.Shares, sh.GetName())
		return nil
	}), nil
}
