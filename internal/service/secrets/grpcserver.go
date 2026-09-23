package secrets

import (
	"context"
	"fmt"
	"strings"

	secretmanagerpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
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
	// Replication is a required field of the resource, so it is always
	// present in a response even when the caller did not set one.
	if s.Replication == "user-managed" {
		out.Replication = &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_UserManaged_{
				UserManaged: &secretmanagerpb.Replication_UserManaged{},
			},
		}
	} else {
		out.Replication = &secretmanagerpb.Replication{
			Replication: &secretmanagerpb.Replication_Automatic_{
				Automatic: &secretmanagerpb.Replication_Automatic{},
			},
		}
	}
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
	}
	if !v.Destroyed.IsZero() {
		out.DestroyTime = timestamppb.New(v.Destroyed)
	}
	return out
}

func replicationOf(r *secretmanagerpb.Replication) string {
	if r.GetUserManaged() != nil {
		return "user-managed"
	}
	return "automatic"
}

// parseParent validates a projects/{project} parent.
func parseParent(parent string) (string, error) {
	const prefix = "projects/"
	if !strings.HasPrefix(parent, prefix) {
		return "", apierror.InvalidArgument("parent %q must be projects/{project}", parent)
	}
	project := strings.TrimPrefix(parent, prefix)
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
	created, err := g.store.CreateSecret(project, req.GetSecretId(),
		sec.GetLabels(), sec.GetAnnotations(), replicationOf(sec.GetReplication()))
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

	var updateLabels, updateAnnotations bool
	for _, p := range paths {
		switch p {
		case "labels":
			updateLabels = true
		case "annotations":
			updateAnnotations = true
		default:
			return nil, apierror.Wrap(apierror.InvalidArgument(
				"update_mask path %q is not supported; only labels and annotations are mutable", p))
		}
	}
	updated, err := g.store.UpdateSecret(project, id, sec.GetLabels(), sec.GetAnnotations(),
		updateLabels, updateAnnotations)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return toProtoSecret(updated), nil
}

func (g *GRPCServer) DeleteSecret(_ context.Context, req *secretmanagerpb.DeleteSecretRequest) (*emptypb.Empty, error) {
	project, id, err := ParseSecretName(req.GetName())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	if err := g.store.DeleteSecret(project, id); err != nil {
		return nil, apierror.Wrap(err)
	}
	return &emptypb.Empty{}, nil
}

func (g *GRPCServer) ListSecrets(_ context.Context, req *secretmanagerpb.ListSecretsRequest) (*secretmanagerpb.ListSecretsResponse, error) {
	project, err := parseParent(req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	all, err := g.store.ListSecrets(project)
	if err != nil {
		return nil, apierror.Wrap(err)
	}

	names := make([]string, len(all))
	byName := make(map[string]Secret, len(all))
	for i, s := range all {
		names[i] = s.Name
		byName[s.Name] = s
	}
	// The scope is the parent, so a token issued for one project cannot be
	// replayed against another.
	page, next, err := paging.Page(req.GetParent(), names, req.GetPageToken(), int(req.GetPageSize()))
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
		TotalSize:     int32(len(all)),
	}, nil
}

// --- versions ----------------------------------------------------------

func (g *GRPCServer) AddSecretVersion(_ context.Context, req *secretmanagerpb.AddSecretVersionRequest) (*secretmanagerpb.SecretVersion, error) {
	project, id, err := ParseSecretName(req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	v, err := g.store.AddVersion(project, id, req.GetPayload().GetData())
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
		Name:    v.Name,
		Payload: &secretmanagerpb.SecretPayload{Data: v.Payload},
	}, nil
}

func (g *GRPCServer) ListSecretVersions(_ context.Context, req *secretmanagerpb.ListSecretVersionsRequest) (*secretmanagerpb.ListSecretVersionsResponse, error) {
	project, id, err := ParseSecretName(req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	all, err := g.store.ListVersions(project, id)
	if err != nil {
		return nil, apierror.Wrap(err)
	}

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
	page, next, err := paging.Page(req.GetParent(), keys, req.GetPageToken(), int(req.GetPageSize()))
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
		TotalSize:     int32(len(all)),
	}, nil
}

func (g *GRPCServer) EnableSecretVersion(_ context.Context, req *secretmanagerpb.EnableSecretVersionRequest) (*secretmanagerpb.SecretVersion, error) {
	return g.setState(req.GetName(), StateEnabled)
}

func (g *GRPCServer) DisableSecretVersion(_ context.Context, req *secretmanagerpb.DisableSecretVersionRequest) (*secretmanagerpb.SecretVersion, error) {
	return g.setState(req.GetName(), StateDisabled)
}

func (g *GRPCServer) setState(name string, state VersionState) (*secretmanagerpb.SecretVersion, error) {
	project, id, version, err := ParseVersionName(name)
	if err != nil {
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
	v, err := g.store.DestroyVersion(project, id, version)
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return toProtoVersion(v), nil
}
