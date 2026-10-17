package secrets

import (
	"context"
	"fmt"
	"google.golang.org/protobuf/proto"
	"slices"
	"strings"
	"time"

	"cloud.google.com/go/iam/apiv1/iampb"
	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/iampolicy"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/resource"
)

// GRPCServer serves google.cloud.secretmanager.v1 over gRPC.
//
// Embedding UnimplementedSecretManagerServiceServer means a method this
// service does not implement returns Unimplemented rather than failing to
// compile — and, crucially, rather than us writing a stub that returns a
// plausible empty success.
type GRPCServer struct {
	secretmanagerpb.UnimplementedSecretManagerServiceServer
	store *Store
}

// NewGRPCServer returns a Secret Manager gRPC service.
func NewGRPCServer(s *Store) *GRPCServer { return &GRPCServer{store: s} }

// Register adds this service to a gRPC server.
func (g *GRPCServer) Register(s *grpc.Server) {
	secretmanagerpb.RegisterSecretManagerServiceServer(s, g)
	s.RegisterService(&clockServiceDesc, g)
}

// descendingKey renders a version number so that ascending string order is
// descending numeric order. maxVersionNumber is far beyond any real version
// count and keeps every key the same width, which is what makes the string
// comparison agree with the numeric one.
func descendingKey(number int) string {
	const maxVersionNumber = 1 << 40
	return fmt.Sprintf("%013d", maxVersionNumber-number)
}

// --- conversions -------------------------------------------------------

func toProtoSecret(s Secret) *secretmanagerpb.Secret {
	out := &secretmanagerpb.Secret{
		Name:        s.Name,
		CreateTime:  timestamppb.New(s.Created),
		Labels:      s.Labels,
		Annotations: s.Annotations,
		Etag:        s.Etag,
	}
	if !s.Expire.IsZero() {
		out.Expiration = &secretmanagerpb.Secret_ExpireTime{ExpireTime: timestamppb.New(s.Expire)}
	}
	for _, t := range s.Topics {
		out.Topics = append(out.Topics, &secretmanagerpb.Topic{Name: t})
	}
	if !s.NextRotation.IsZero() || s.RotationPeriod > 0 {
		out.Rotation = &secretmanagerpb.Rotation{}
		if !s.NextRotation.IsZero() {
			out.Rotation.NextRotationTime = timestamppb.New(s.NextRotation)
		}
		if s.RotationPeriod > 0 {
			out.Rotation.RotationPeriod = durationpb.New(s.RotationPeriod)
		}
	}
	if s.VersionDestroyTTL > 0 {
		out.VersionDestroyTtl = durationpb.New(s.VersionDestroyTTL)
	}
	if s.Regional() {
		// A regional secret (#1519) has no replication; its key is the
		// secret's own.
		if len(s.KMSKeys) > 0 && s.KMSKeys[0] != "" {
			out.CustomerManagedEncryption = &secretmanagerpb.CustomerManagedEncryption{KmsKeyName: s.KMSKeys[0]}
		}
		return out
	}
	// Replication is a required field of the resource, so it is always
	// present in a response even when the caller did not set one.
	out.Replication = toProtoReplication(s)
	return out
}

func toProtoVersion(v Version) *secretmanagerpb.SecretVersion {
	state := secretmanagerpb.SecretVersion_ENABLED
	switch v.State {
	case StateDisabled:
		state = secretmanagerpb.SecretVersion_DISABLED
	case StateDestroyed:
		state = secretmanagerpb.SecretVersion_DESTROYED
	}
	out := &secretmanagerpb.SecretVersion{
		Name:       v.Name,
		CreateTime: timestamppb.New(v.Created),
		State:      state,
		Etag:       v.Etag,
		// gcloud treats a version added without a checksum as possibly
		// corrupted (#590).
		ClientSpecifiedPayloadChecksum: v.ClientChecksum,
	}
	if !v.Destroyed.IsZero() {
		out.DestroyTime = timestamppb.New(v.Destroyed)
	}
	if !v.ScheduledDestroy.IsZero() {
		out.ScheduledDestroyTime = timestamppb.New(v.ScheduledDestroy)
	}
	if scope := scopeOfName(v.Name); scope != "" {
		if _, l := SplitScope(scope); l != "" {
			// A regional secret's version (#1519) reports its key version
			// in customer_managed_encryption, not replication_status.
			out.CustomerManagedEncryption = regionalKeyStatus(v)
			return out
		}
	}
	out.ReplicationStatus = toProtoReplicationStatus(v)
	return out
}

// regionalKeyStatus is a regional secret version's customer_managed_encryption
// (#1519): the key version that encrypted it, nil for none.
func regionalKeyStatus(v Version) *secretmanagerpb.CustomerManagedEncryptionStatus {
	if len(v.Replicas) == 0 || v.Replicas[0].KeyVersion == "" {
		return nil
	}
	return &secretmanagerpb.CustomerManagedEncryptionStatus{KmsKeyVersionName: v.Replicas[0].KeyVersion}
}

// toProtoReplicationStatus is a version's replication_status (#1453): its
// replicas, with the key version that encrypted each. nil for a version
// added before it was recorded.
func toProtoReplicationStatus(v Version) *secretmanagerpb.ReplicationStatus {
	if len(v.Replicas) == 0 {
		return nil
	}
	cmek := func(r ReplicaStatus) *secretmanagerpb.CustomerManagedEncryptionStatus {
		if r.KeyVersion == "" {
			return nil
		}
		return &secretmanagerpb.CustomerManagedEncryptionStatus{KmsKeyVersionName: r.KeyVersion}
	}
	if len(v.Replicas) == 1 && v.Replicas[0].Location == "" {
		return &secretmanagerpb.ReplicationStatus{ReplicationStatus: &secretmanagerpb.ReplicationStatus_Automatic{
			Automatic: &secretmanagerpb.ReplicationStatus_AutomaticStatus{CustomerManagedEncryption: cmek(v.Replicas[0])}}}
	}
	um := &secretmanagerpb.ReplicationStatus_UserManagedStatus{}
	for _, r := range v.Replicas {
		um.Replicas = append(um.Replicas, &secretmanagerpb.ReplicationStatus_UserManagedStatus_ReplicaStatus{
			Location: r.Location, CustomerManagedEncryption: cmek(r)})
	}
	return &secretmanagerpb.ReplicationStatus{ReplicationStatus: &secretmanagerpb.ReplicationStatus_UserManaged{UserManaged: um}}
}

// toProtoReplication is a secret's replication policy as it was created:
// automatic, or user-managed with its replica locations in the order given.
func toProtoReplication(s Secret) *secretmanagerpb.Replication {
	cmek := func(i int) *secretmanagerpb.CustomerManagedEncryption {
		if i >= len(s.KMSKeys) || s.KMSKeys[i] == "" {
			return nil
		}
		return &secretmanagerpb.CustomerManagedEncryption{KmsKeyName: s.KMSKeys[i]}
	}
	if s.Replication == "user-managed" {
		um := &secretmanagerpb.Replication_UserManaged{}
		for i, l := range s.ReplicaLocations {
			um.Replicas = append(um.Replicas, &secretmanagerpb.Replication_UserManaged_Replica{Location: l, CustomerManagedEncryption: cmek(i)})
		}
		return &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_UserManaged_{UserManaged: um}}
	}
	return &secretmanagerpb.Replication{Replication: &secretmanagerpb.Replication_Automatic_{
		Automatic: &secretmanagerpb.Replication_Automatic{CustomerManagedEncryption: cmek(0)}}}
}

// replicationOf is a request's replication policy as the store keeps it:
// the kind, a user-managed policy's replica locations, and the
// customer-managed key of each replica ("" for none; one entry under
// automatic), nil when there is none.
func replicationOf(r *secretmanagerpb.Replication) (string, []string, []string) {
	um := r.GetUserManaged()
	if um == nil {
		return "automatic", nil, normalKeys([]string{r.GetAutomatic().GetCustomerManagedEncryption().GetKmsKeyName()})
	}
	var locations, keys []string
	for _, rep := range um.GetReplicas() {
		locations = append(locations, rep.GetLocation())
		keys = append(keys, rep.GetCustomerManagedEncryption().GetKmsKeyName())
	}
	return "user-managed", locations, normalKeys(keys)
}

// parseParent validates a projects/{project} parent, or the
// projects/{project}/locations/{location} of regional secrets (#1519),
// whose scope it returns.
func parseParent(parent string) (string, error) {
	const prefix = "projects/"
	if !strings.HasPrefix(parent, prefix) {
		return "", apierror.InvalidArgument("parent %q must be projects/{project}", parent)
	}
	project := strings.TrimPrefix(parent, prefix)
	if p, l, ok := strings.Cut(project, "/locations/"); ok {
		if p == "" || strings.Contains(p, "/") || !resource.ValidID(p) {
			return "", apierror.InvalidArgument("parent %q must be projects/{project}/locations/{location}", parent)
		}
		if l == "" || strings.Contains(l, "/") || !resource.ValidID(l) {
			return "", apierror.InvalidArgument("parent %q must be projects/{project}/locations/{location}", parent)
		}
		return RegionalScope(p, l), nil
	}
	if project == "" || strings.Contains(project, "/") {
		return "", apierror.InvalidArgument("parent %q must be projects/{project}", parent)
	}
	if !resource.ValidID(project) {
		return "", apierror.InvalidArgument("project %q is not a valid resource ID", project)
	}
	return project, nil
}

// --- secrets -----------------------------------------------------------

func (g *GRPCServer) CreateSecret(_ context.Context, req *secretmanagerpb.CreateSecretRequest) (*secretmanagerpb.Secret, error) {
	project, err := parseParent(req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	sec := req.GetSecret()
	_, location := SplitScope(project)
	if err := checkCreatable(sec, location != ""); err != nil {
		return nil, apierror.Wrap(err)
	}
	expire, err := expireOf(sec, g.store.now())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	sch, err := scheduleOf(sec)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	kind, locations, keys := replicationOf(sec.GetReplication())
	if location != "" {
		// A regional secret (#1519) has no replication policy: its one
		// replica is its location, and its key, if any, is the secret's own
		// customer_managed_encryption, which must be in that location.
		kind, locations = "user-managed", []string{location}
		keys = normalKeys([]string{sec.GetCustomerManagedEncryption().GetKmsKeyName()})
	}
	created, err := g.store.CreateSecretEncrypted(project, req.GetSecretId(),
		sec.GetLabels(), sec.GetAnnotations(), expire, topicNames(sec), sch, keys, kind, locations...)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return toProtoSecret(created), nil
}

func (g *GRPCServer) GetSecret(_ context.Context, req *secretmanagerpb.GetSecretRequest) (*secretmanagerpb.Secret, error) {
	project, id, err := ParseSecretName(req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	sec, err := g.store.GetSecret(project, id)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return toProtoSecret(sec), nil
}

// UpdateSecret applies only the fields the update mask names.
//
// An empty mask is rejected rather than treated as "replace everything": a
// client that forgot the mask would otherwise silently clear every label.
func (g *GRPCServer) UpdateSecret(_ context.Context, req *secretmanagerpb.UpdateSecretRequest) (*secretmanagerpb.Secret, error) {
	sec := req.GetSecret()
	project, id, err := ParseSecretName(sec.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, apierror.Wrap(apierror.InvalidArgument(
			"update_mask must name at least one field; an empty mask would clear every label"))
	}

	var updateLabels, updateAnnotations, checkReplication, updateExpire, updateTopics, updateCMEK bool
	var updateNext, updatePeriod, updateTTL bool
	for _, p := range paths {
		switch {
		case p == "rotation":
			// The whole message: a mask naming it with no rotation clears it.
			updateNext, updatePeriod = true, true
		case p == "rotation.next_rotation_time" || p == "rotation.nextRotationTime":
			updateNext = true
		case p == "rotation.rotation_period" || p == "rotation.rotationPeriod":
			updatePeriod = true
		case p == "version_destroy_ttl" || p == "versionDestroyTtl":
			updateTTL = true
		case p == "labels":
			updateLabels = true
		case p == "annotations":
			updateAnnotations = true
		case p == "expire_time" || p == "ttl" || p == "expireTime":
			// Either path names the expiration oneof; a mask naming it with
			// neither field set clears it (#1225).
			updateExpire = true
		case p == "topics":
			updateTopics = true
		case p == "replication" || strings.HasPrefix(p, "replication."):
			checkReplication = true
		case p == "customer_managed_encryption" || p == "customerManagedEncryption" ||
			strings.HasPrefix(p, "customer_managed_encryption.") || strings.HasPrefix(p, "customerManagedEncryption."):
			updateCMEK = true
		default:
			return nil, apierror.Wrap(apierror.InvalidArgument(
				"update_mask path %q is not supported; only labels, annotations, expire_time, ttl, topics, rotation and version_destroy_ttl are mutable", p))
		}
	}
	if err := g.secretEtag(project, id, sec.GetEtag()); err != nil {
		return nil, apierror.Wrap(err)
	}
	var newKeys []string
	_, location := SplitScope(project)
	switch {
	case checkReplication && location != "":
		return nil, apierror.Wrap(apierror.InvalidArgument("a regional secret has no replication policy; " +
			"its key is customer_managed_encryption"))
	case checkReplication:
		if newKeys, err = g.replicationUnchanged(project, id, sec.GetReplication()); err != nil {
			return nil, apierror.Wrap(err)
		}
	case updateCMEK && location == "":
		return nil, apierror.Wrap(apierror.InvalidArgument("customer_managed_encryption is a regional secret's; " +
			"a global secret's keys are in replication"))
	case updateCMEK:
		// The one key of a regional secret (#1519) can be changed or
		// removed, as a global secret's can (#1453).
		cur, err := g.store.GetSecret(project, id)
		if err != nil {
			return nil, apierror.Wrap(err)
		}
		if c := sec.GetCustomerManagedEncryption(); c != nil && c.GetKmsKeyName() == "" {
			return nil, apierror.Wrap(apierror.InvalidArgument("customer_managed_encryption.kms_key_name is required"))
		}
		if k := []string{sec.GetCustomerManagedEncryption().GetKmsKeyName()}; !slices.Equal(normalKeys(k), cur.KMSKeys) {
			newKeys = k
		}
	}
	var expire time.Time
	if updateExpire {
		if expire, err = expireOf(sec, g.store.now()); err != nil {
			return nil, apierror.Wrap(err)
		}
	}
	sch, err := scheduleOf(sec)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	updated, err := g.store.UpdateSecretFields(project, id, SecretUpdate{
		Schedule: sch, UpdateNextRotation: updateNext, UpdateRotationPeriod: updatePeriod,
		UpdateVersionDestroyTTL: updateTTL,
		Labels:                  sec.GetLabels(), Annotations: sec.GetAnnotations(),
		UpdateLabels: updateLabels, UpdateAnnotations: updateAnnotations,
		Expire: expire, UpdateExpire: updateExpire,
		Topics: topicNames(sec), UpdateTopics: updateTopics,
	})
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if newKeys != nil {
		// The keys are the one part of replication that can change (#1453).
		if updated, err = g.store.SetReplicationKeys(project, id, newKeys); err != nil {
			return nil, apierror.Wrap(err)
		}
	}
	return toProtoSecret(updated), nil
}

// scheduleOf is a request Secret's rotation and version_destroy_ttl.
func scheduleOf(sec *secretmanagerpb.Secret) (Schedule, error) {
	var sch Schedule
	if t := sec.GetRotation().GetNextRotationTime(); t != nil {
		if err := t.CheckValid(); err != nil {
			return Schedule{}, apierror.InvalidArgument("rotation.next_rotation_time is not a valid timestamp: %v", err)
		}
		sch.NextRotation = t.AsTime().UTC()
	}
	if d := sec.GetRotation().GetRotationPeriod(); d != nil {
		if err := d.CheckValid(); err != nil || d.AsDuration() <= 0 {
			return Schedule{}, apierror.InvalidArgument("rotation.rotation_period must be a positive duration")
		}
		sch.RotationPeriod = d.AsDuration()
	}
	if d := sec.GetVersionDestroyTtl(); d != nil {
		if err := d.CheckValid(); err != nil || d.AsDuration() < 0 {
			return Schedule{}, apierror.InvalidArgument("version_destroy_ttl must be a duration that is not negative")
		}
		sch.VersionDestroyTTL = d.AsDuration()
	}
	return sch, nil
}

// topicNames are a Secret's topics by name.
func topicNames(sec *secretmanagerpb.Secret) []string {
	var out []string
	for _, t := range sec.GetTopics() {
		out = append(out, t.GetName())
	}
	return out
}

// replicationUnchanged enforces replication's IMMUTABLE field behaviour
// (google.cloud.secretmanager.v1.Secret.replication: "The replication policy
// cannot be changed after the Secret has been created"). Per AIP-203 an
// immutable field in an update is ignored when it matches and refused with
// INVALID_ARGUMENT when it would change; documented, not measured against
// Google.
//
// The customer-managed keys are the exception docs.cloud.google.com/secret-manager/docs/cmek
// documents (#1453): `gcloud secrets replication update --set-kms-key` and
// `--remove-cmek` change them with the rest of the policy unchanged. The
// keys to set are returned (one empty entry per replica to remove them),
// nil when they do not change.
func (g *GRPCServer) replicationUnchanged(project, id string, requested *secretmanagerpb.Replication) ([]string, error) {
	current, err := g.store.GetSecret(project, id)
	if err != nil {
		return nil, err
	}
	if err := checkCMEKFields(requested); err != nil {
		return nil, err
	}
	kind, locations, keys := replicationOf(requested)
	if requested.GetReplication() == nil || kind != current.Replication || !slices.Equal(locations, current.ReplicaLocations) {
		return nil, apierror.InvalidArgument("replication is immutable: the replication policy of %s cannot be "+
			"changed after the secret is created, except for its customer-managed encryption keys; "+
			"create a new secret with the policy instead", current.Name)
	}
	if slices.Equal(keys, current.KMSKeys) {
		return nil, nil
	}
	if keys == nil {
		keys = make([]string, max(1, len(locations)))
	}
	return keys, nil
}

func (g *GRPCServer) DeleteSecret(_ context.Context, req *secretmanagerpb.DeleteSecretRequest) (*emptypb.Empty, error) {
	project, id, err := ParseSecretName(req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if err := g.secretEtag(project, id, req.GetEtag()); err != nil {
		return nil, apierror.Wrap(err)
	}
	if err := g.store.DeleteSecret(project, id); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &emptypb.Empty{}, nil
}

// secretEtag checks a request etag against the stored secret's (#580).
func (g *GRPCServer) secretEtag(project, id, etag string) error {
	if etag == "" {
		return nil
	}
	cur, err := g.store.GetSecret(project, id)
	if err != nil {
		return err
	}
	return checkEtag(etag, cur.Etag)
}

// versionEtag checks a request etag against the stored version's (#580).
func (g *GRPCServer) versionEtag(project, id, version, etag string) error {
	if etag == "" {
		return nil
	}
	cur, err := g.store.GetVersion(project, id, version)
	if err != nil {
		return err
	}
	return checkEtag(etag, cur.Etag)
}

func (g *GRPCServer) ListSecrets(_ context.Context, req *secretmanagerpb.ListSecretsRequest) (*secretmanagerpb.ListSecretsResponse, error) {
	project, err := parseParent(req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	filter, err := parseSecretFilter(req.GetFilter())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	all, err := g.store.ListSecrets(project)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	all = filterList(all, filter)

	names := make([]string, len(all))
	byName := make(map[string]Secret, len(all))
	for i, s := range all {
		names[i] = s.Name
		byName[s.Name] = s
	}
	// The scope is the parent, so a token issued for one project cannot be
	// replayed against another.
	// The filter is part of the scope, so a token cannot be replayed with
	// another filter.
	page, next, err := paging.Page(req.GetParent()+"?"+req.GetFilter(), names, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	out := make([]*secretmanagerpb.Secret, 0, len(page))
	for _, n := range page {
		out = append(out, toProtoSecret(byName[n]))
	}
	return &secretmanagerpb.ListSecretsResponse{
		Secrets:       out,
		NextPageToken: next,
		TotalSize:     listTotal(len(all), filter),
	}, nil
}

// --- versions ----------------------------------------------------------

func (g *GRPCServer) AddSecretVersion(_ context.Context, req *secretmanagerpb.AddSecretVersionRequest) (*secretmanagerpb.SecretVersion, error) {
	project, id, err := ParseSecretName(req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if err := checkPayloadCRC32C(req.GetPayload().GetData(), req.GetPayload().DataCrc32C); err != nil {
		return nil, apierror.Wrap(err)
	}
	add := g.store.AddVersion
	if req.GetPayload().DataCrc32C != nil {
		// Verified above; the version reports that the client checked it.
		add = g.store.AddVersionWithChecksum
	}
	v, err := add(project, id, req.GetPayload().GetData())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return toProtoVersion(v), nil
}

func (g *GRPCServer) GetSecretVersion(_ context.Context, req *secretmanagerpb.GetSecretVersionRequest) (*secretmanagerpb.SecretVersion, error) {
	project, id, version, err := ParseVersionName(req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	v, err := g.store.GetVersion(project, id, version)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return toProtoVersion(v), nil
}

func (g *GRPCServer) AccessSecretVersion(_ context.Context, req *secretmanagerpb.AccessSecretVersionRequest) (*secretmanagerpb.AccessSecretVersionResponse, error) {
	project, id, version, err := ParseVersionName(req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	v, err := g.store.AccessVersion(project, id, version)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return &secretmanagerpb.AccessSecretVersionResponse{
		// The concrete version name is returned even when the caller asked
		// for "latest", so a client can record which bytes it actually got.
		Name: v.Name,
		// The checksum Google's own samples verify on every access (#580).
		Payload: &secretmanagerpb.SecretPayload{Data: v.Payload, DataCrc32C: proto.Int64(payloadCRC32C(v.Payload))},
	}, nil
}

func (g *GRPCServer) ListSecretVersions(_ context.Context, req *secretmanagerpb.ListSecretVersionsRequest) (*secretmanagerpb.ListSecretVersionsResponse, error) {
	project, id, err := ParseSecretName(req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	filter, err := parseVersionFilter(req.GetFilter())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	all, err := g.store.ListVersions(project, id)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	all = filterList(all, filter)

	// Paging sorts its keys ascending, but versions are reported newest
	// first. Sorting on the complement of the version number gives that
	// ordering without reversing a page after the fact, which would break
	// the cursor: a token names the last key of a page, and reversing would
	// make that the *first* item the caller saw.
	keys := make([]string, len(all))
	byKey := make(map[string]Version, len(all))
	for i, v := range all {
		k := descendingKey(v.Number)
		keys[i] = k
		byKey[k] = v
	}
	page, next, err := paging.Page(req.GetParent()+"?"+req.GetFilter(), keys, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	out := make([]*secretmanagerpb.SecretVersion, 0, len(page))
	for _, k := range page {
		out = append(out, toProtoVersion(byKey[k]))
	}
	return &secretmanagerpb.ListSecretVersionsResponse{
		Versions:      out,
		NextPageToken: next,
		TotalSize:     listTotal(len(all), filter),
	}, nil
}

func (g *GRPCServer) EnableSecretVersion(_ context.Context, req *secretmanagerpb.EnableSecretVersionRequest) (*secretmanagerpb.SecretVersion, error) {
	return g.setState(req.GetName(), req.GetEtag(), StateEnabled)
}

func (g *GRPCServer) DisableSecretVersion(_ context.Context, req *secretmanagerpb.DisableSecretVersionRequest) (*secretmanagerpb.SecretVersion, error) {
	return g.setState(req.GetName(), req.GetEtag(), StateDisabled)
}

func (g *GRPCServer) setState(name, etag string, state VersionState) (*secretmanagerpb.SecretVersion, error) {
	project, id, version, err := ParseVersionName(name)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if err := g.versionEtag(project, id, version, etag); err != nil {
		return nil, apierror.Wrap(err)
	}
	v, err := g.store.SetVersionState(project, id, version, state)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return toProtoVersion(v), nil
}

func (g *GRPCServer) DestroySecretVersion(_ context.Context, req *secretmanagerpb.DestroySecretVersionRequest) (*secretmanagerpb.SecretVersion, error) {
	project, id, version, err := ParseVersionName(req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if err := g.versionEtag(project, id, version, req.GetEtag()); err != nil {
		return nil, apierror.Wrap(err)
	}
	v, err := g.store.DestroyVersion(project, id, version)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return toProtoVersion(v), nil
}

// GetIamPolicy returns the stored policy (ADR-0006). Stored, never enforced.
func (g *GRPCServer) GetIamPolicy(_ context.Context, req *iampb.GetIamPolicyRequest) (*iampb.Policy, error) {
	project, id, err := ParseSecretName(req.GetResource())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	p, err := g.store.GetIamPolicy(project, id)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	out, err := iampolicy.Get(p, req.GetOptions())
	return out, apierror.Wrap(err)
}

// SetIamPolicy stores a policy. Stored, never enforced.
func (g *GRPCServer) SetIamPolicy(_ context.Context, req *iampb.SetIamPolicyRequest) (*iampb.Policy, error) {
	project, id, err := ParseSecretName(req.GetResource())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	p, err := g.store.SetIamPolicy(project, id, req)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return p.Proto(), nil
}

// TestIamPermissions returns every requested permission of an existing
// secret: nothing is enforced, so nothing is denied.
func (g *GRPCServer) TestIamPermissions(_ context.Context, req *iampb.TestIamPermissionsRequest) (*iampb.TestIamPermissionsResponse, error) {
	project, id, err := ParseSecretName(req.GetResource())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if _, err := g.store.GetSecret(project, id); err != nil {
		return nil, apierror.Wrap(err)
	}
	return iampolicy.Permissions(req), nil
}
