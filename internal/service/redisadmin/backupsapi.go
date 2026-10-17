package redisadmin

import (
	"context"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	memorystorepb "cloud.google.com/go/memorystore/apiv1/memorystorepb"
	clusterpb "cloud.google.com/go/redis/cluster/apiv1/clusterpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
)

// The Redis Cluster and Memorystore for Valkey methods of token auth
// (clusterauth.go) and backups (backups.go). Their messages have the same
// fields by number in both APIs, so the Valkey API's are the Redis
// Cluster API's, converted on the wire, with the node type set apart.

// convert copies src into dst by their wire form.
func convert[T proto.Message](src proto.Message, dst T) T {
	b, err := proto.Marshal(src)
	if err == nil {
		_ = proto.Unmarshal(b, dst)
	}
	return dst
}

func valkeyMetadata(now time.Time, target, verb string) *memorystorepb.OperationMetadata {
	return &memorystorepb.OperationMetadata{CreateTime: timestamppb.New(now), Target: target, Verb: verb, ApiVersion: "v1"}
}

// backupOut is b as c's API returns it.
func (c *clusters) backupOut(b *backupRecord) proto.Message {
	out := renderBackup(b)
	if !c.valkey {
		out.NodeType = clusterpb.NodeType(clusterpb.NodeType_value[b.NodeType])
		return out
	}
	v := convert(out, &memorystorepb.Backup{})
	v.NodeType = memorystorepb.Instance_NodeType(memorystorepb.Instance_NodeType_value[b.NodeType])
	return v
}

func renderTokenUser(r *clusterRecord, user, state string) *clusterpb.TokenAuthUser {
	return &clusterpb.TokenAuthUser{Name: tokenUserName(r.Name, user), State: clusterpb.TokenAuthUser_State(clusterpb.TokenAuthUser_State_value[state])}
}

func renderToken(r *clusterRecord, user string, t authToken, state string) *clusterpb.AuthToken {
	if state == "UPDATING" {
		state = "CREATING"
	}
	return &clusterpb.AuthToken{Name: authTokenName(r.Name, user, t.ID), Token: t.Token, CreateTime: timestamppb.New(t.Created),
		State: clusterpb.AuthToken_State(clusterpb.AuthToken_State_value[state])}
}

// --- Redis Cluster ---

func (s *ClusterServer) AddTokenAuthUser(ctx context.Context, req *clusterpb.AddTokenAuthUserRequest) (*longrunningpb.Operation, error) {
	c, r, err := s.find(req.GetCluster())
	if err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	op, err := c.addTokenUser(r, req.GetTokenAuthUser(), func() proto.Message { return c.render(r) })
	if err != nil {
		return nil, err
	}
	return s.api.operation(op)
}

func (s *ClusterServer) DeleteTokenAuthUser(ctx context.Context, req *clusterpb.DeleteTokenAuthUserRequest) (*longrunningpb.Operation, error) {
	owner, user, _, err := splitTokenName(req.GetName(), false)
	if err != nil {
		return nil, err
	}
	c, r, err := s.find(owner)
	if err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	op, err := c.deleteTokenUser(r, user, req.GetForce(), func() proto.Message { return &emptypb.Empty{} })
	if err != nil {
		return nil, err
	}
	return s.api.operation(op)
}

func (s *ClusterServer) GetTokenAuthUser(ctx context.Context, req *clusterpb.GetTokenAuthUserRequest) (*clusterpb.TokenAuthUser, error) {
	owner, user, _, err := splitTokenName(req.GetName(), false)
	if err != nil {
		return nil, err
	}
	c, r, err := s.find(owner)
	if err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	if _, ok := r.TokenUsers[user]; !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", req.GetName())
	}
	return renderTokenUser(r, user, c.tokenUserState(r)), nil
}

func (s *ClusterServer) ListTokenAuthUsers(ctx context.Context, req *clusterpb.ListTokenAuthUsersRequest) (*clusterpb.ListTokenAuthUsersResponse, error) {
	if req.GetFilter() != "" || req.GetOrderBy() != "" {
		return nil, apierror.Unimplemented("filter and order_by are not served for token auth users here")
	}
	c, r, err := s.find(req.GetParent())
	if err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	page, next, err := tokenUsersPage(r, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	resp := &clusterpb.ListTokenAuthUsersResponse{NextPageToken: next}
	for _, u := range page {
		resp.TokenAuthUsers = append(resp.TokenAuthUsers, renderTokenUser(r, u, c.tokenUserState(r)))
	}
	return resp, nil
}

func (s *ClusterServer) AddAuthToken(ctx context.Context, req *clusterpb.AddAuthTokenRequest) (*longrunningpb.Operation, error) {
	owner, user, _, err := splitTokenName(req.GetTokenAuthUser(), false)
	if err != nil {
		return nil, err
	}
	c, r, err := s.find(owner)
	if err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	id := ""
	if n := req.GetAuthToken().GetName(); n != "" {
		_, _, id, _ = splitTokenName(n, true)
	}
	op, err := c.addAuthToken(r, user, id, func() proto.Message { return renderTokenUser(r, user, "ACTIVE") })
	if err != nil {
		return nil, err
	}
	return s.api.operation(op)
}

func (s *ClusterServer) DeleteAuthToken(ctx context.Context, req *clusterpb.DeleteAuthTokenRequest) (*longrunningpb.Operation, error) {
	owner, user, id, err := splitTokenName(req.GetName(), true)
	if err != nil {
		return nil, err
	}
	c, r, err := s.find(owner)
	if err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	op, err := c.deleteAuthToken(r, user, id, func() proto.Message { return &emptypb.Empty{} })
	if err != nil {
		return nil, err
	}
	return s.api.operation(op)
}

func (s *ClusterServer) GetAuthToken(ctx context.Context, req *clusterpb.GetAuthTokenRequest) (*clusterpb.AuthToken, error) {
	owner, user, id, err := splitTokenName(req.GetName(), true)
	if err != nil {
		return nil, err
	}
	c, r, err := s.find(owner)
	if err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	t, ok := findToken(r, user, id)
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", req.GetName())
	}
	return renderToken(r, user, t, c.tokenUserState(r)), nil
}

func (s *ClusterServer) ListAuthTokens(ctx context.Context, req *clusterpb.ListAuthTokensRequest) (*clusterpb.ListAuthTokensResponse, error) {
	if req.GetFilter() != "" || req.GetOrderBy() != "" {
		return nil, apierror.Unimplemented("filter and order_by are not served for auth tokens here")
	}
	owner, user, _, err := splitTokenName(req.GetParent(), false)
	if err != nil {
		return nil, err
	}
	c, r, err := s.find(owner)
	if err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	page, next, err := authTokensPage(r, user, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	resp := &clusterpb.ListAuthTokensResponse{NextPageToken: next}
	for _, t := range page {
		resp.AuthTokens = append(resp.AuthTokens, renderToken(r, user, t, c.tokenUserState(r)))
	}
	return resp, nil
}

func (s *ClusterServer) BackupCluster(ctx context.Context, req *clusterpb.BackupClusterRequest) (*longrunningpb.Operation, error) {
	c, r, err := s.find(req.GetName())
	if err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	c.loadBackups()
	op, err := c.startBackup(r, req.GetBackupId(), req.GetTtl().AsDuration(), false, func() proto.Message { return c.render(r) })
	if err != nil {
		return nil, err
	}
	return s.api.operation(op)
}

func (s *ClusterServer) ListBackupCollections(ctx context.Context, req *clusterpb.ListBackupCollectionsRequest) (*clusterpb.ListBackupCollectionsResponse, error) {
	m := parentRE.FindStringSubmatch(req.GetParent())
	if m == nil {
		return nil, apierror.InvalidArgument("parent must be projects/{project}/locations/{location}, not %q", req.GetParent())
	}
	c, err := s.clusters()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	cols := c.listCollections(m[1], m[2])
	c.mu.Unlock()
	names := make([]string, len(cols))
	for i, col := range cols {
		names[i] = col.Name
	}
	page, next, err := paging.Page("backup-collections:"+req.GetParent(), names, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("invalid page_token %q", req.GetPageToken())
	}
	resp := &clusterpb.ListBackupCollectionsResponse{NextPageToken: next}
	for i := range page {
		resp.BackupCollections = append(resp.BackupCollections, renderCollection(cols[i+indexOf(names, page[0])]))
	}
	return resp, nil
}

func indexOf(names []string, n string) int {
	for i, x := range names {
		if x == n {
			return i
		}
	}
	return 0
}

func (s *ClusterServer) GetBackupCollection(ctx context.Context, req *clusterpb.GetBackupCollectionRequest) (*clusterpb.BackupCollection, error) {
	c, err := s.clusters()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loadBackups()
	col := c.collections[req.GetName()]
	if col == nil {
		return nil, apierror.NotFound("Resource '%s' was not found", req.GetName())
	}
	return renderCollection(col), nil
}

func (s *ClusterServer) ListBackups(ctx context.Context, req *clusterpb.ListBackupsRequest) (*clusterpb.ListBackupsResponse, error) {
	c, err := s.clusters()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	bs, err := c.listBackups(req.GetParent())
	var out []*clusterpb.Backup
	for _, b := range bs {
		out = append(out, c.backupOut(b).(*clusterpb.Backup))
	}
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	names := make([]string, len(out))
	for i, b := range out {
		names[i] = b.Name
	}
	page, next, err := paging.Page("backups:"+req.GetParent(), names, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("invalid page_token %q", req.GetPageToken())
	}
	resp := &clusterpb.ListBackupsResponse{NextPageToken: next}
	if len(page) > 0 {
		at := indexOf(names, page[0])
		resp.Backups = out[at : at+len(page)]
	}
	return resp, nil
}

func (s *ClusterServer) GetBackup(ctx context.Context, req *clusterpb.GetBackupRequest) (*clusterpb.Backup, error) {
	c, err := s.clusters()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := c.findBackup(req.GetName())
	if err != nil {
		return nil, err
	}
	return c.backupOut(b).(*clusterpb.Backup), nil
}

func (s *ClusterServer) DeleteBackup(ctx context.Context, req *clusterpb.DeleteBackupRequest) (*longrunningpb.Operation, error) {
	c, err := s.clusters()
	if err != nil {
		return nil, err
	}
	op, err := c.deleteBackup(req.GetName())
	if err != nil {
		return nil, err
	}
	c.finish(op, &emptypb.Empty{}, nil)
	return s.api.operation(op)
}

func (s *ClusterServer) ExportBackup(ctx context.Context, req *clusterpb.ExportBackupRequest) (*longrunningpb.Operation, error) {
	c, err := s.clusters()
	if err != nil {
		return nil, err
	}
	op, err := c.exportBackup(ctx, req.GetName(), req.GetGcsBucket())
	if err != nil {
		return nil, err
	}
	return s.api.operation(op)
}

// --- Memorystore for Valkey ---

func toValkeyUser(u *clusterpb.TokenAuthUser) *memorystorepb.TokenAuthUser {
	return convert(u, &memorystorepb.TokenAuthUser{})
}

func (s *MemorystoreServer) AddTokenAuthUser(ctx context.Context, req *memorystorepb.AddTokenAuthUserRequest) (*longrunningpb.Operation, error) {
	c, r, err := s.find(req.GetInstance())
	if err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	op, err := c.addTokenUser(r, req.GetTokenAuthUser(), func() proto.Message { return renderValkey(r) })
	if err != nil {
		return nil, err
	}
	return s.api.operation(op)
}

func (s *MemorystoreServer) DeleteTokenAuthUser(ctx context.Context, req *memorystorepb.DeleteTokenAuthUserRequest) (*longrunningpb.Operation, error) {
	owner, user, _, err := splitTokenName(req.GetName(), false)
	if err != nil {
		return nil, err
	}
	c, r, err := s.find(owner)
	if err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	op, err := c.deleteTokenUser(r, user, req.GetForce(), func() proto.Message { return &emptypb.Empty{} })
	if err != nil {
		return nil, err
	}
	return s.api.operation(op)
}

func (s *MemorystoreServer) GetTokenAuthUser(ctx context.Context, req *memorystorepb.GetTokenAuthUserRequest) (*memorystorepb.TokenAuthUser, error) {
	owner, user, _, err := splitTokenName(req.GetName(), false)
	if err != nil {
		return nil, err
	}
	c, r, err := s.find(owner)
	if err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	if _, ok := r.TokenUsers[user]; !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", req.GetName())
	}
	return toValkeyUser(renderTokenUser(r, user, c.tokenUserState(r))), nil
}

func (s *MemorystoreServer) ListTokenAuthUsers(ctx context.Context, req *memorystorepb.ListTokenAuthUsersRequest) (*memorystorepb.ListTokenAuthUsersResponse, error) {
	if req.GetFilter() != "" || req.GetOrderBy() != "" {
		return nil, apierror.Unimplemented("filter and order_by are not served for token auth users here")
	}
	c, r, err := s.find(req.GetParent())
	if err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	page, next, err := tokenUsersPage(r, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	resp := &memorystorepb.ListTokenAuthUsersResponse{NextPageToken: next}
	for _, u := range page {
		resp.TokenAuthUsers = append(resp.TokenAuthUsers, toValkeyUser(renderTokenUser(r, u, c.tokenUserState(r))))
	}
	return resp, nil
}

func (s *MemorystoreServer) AddAuthToken(ctx context.Context, req *memorystorepb.AddAuthTokenRequest) (*longrunningpb.Operation, error) {
	owner, user, _, err := splitTokenName(req.GetTokenAuthUser(), false)
	if err != nil {
		return nil, err
	}
	c, r, err := s.find(owner)
	if err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	id := ""
	if n := req.GetAuthToken().GetName(); n != "" {
		_, _, id, _ = splitTokenName(n, true)
	}
	op, err := c.addAuthToken(r, user, id, func() proto.Message { return toValkeyUser(renderTokenUser(r, user, "ACTIVE")) })
	if err != nil {
		return nil, err
	}
	return s.api.operation(op)
}

func (s *MemorystoreServer) DeleteAuthToken(ctx context.Context, req *memorystorepb.DeleteAuthTokenRequest) (*longrunningpb.Operation, error) {
	owner, user, id, err := splitTokenName(req.GetName(), true)
	if err != nil {
		return nil, err
	}
	c, r, err := s.find(owner)
	if err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	op, err := c.deleteAuthToken(r, user, id, func() proto.Message { return &emptypb.Empty{} })
	if err != nil {
		return nil, err
	}
	return s.api.operation(op)
}

func (s *MemorystoreServer) GetAuthToken(ctx context.Context, req *memorystorepb.GetAuthTokenRequest) (*memorystorepb.AuthToken, error) {
	owner, user, id, err := splitTokenName(req.GetName(), true)
	if err != nil {
		return nil, err
	}
	c, r, err := s.find(owner)
	if err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	t, ok := findToken(r, user, id)
	if !ok {
		return nil, apierror.NotFound("Resource '%s' was not found", req.GetName())
	}
	return convert(renderToken(r, user, t, c.tokenUserState(r)), &memorystorepb.AuthToken{}), nil
}

func (s *MemorystoreServer) ListAuthTokens(ctx context.Context, req *memorystorepb.ListAuthTokensRequest) (*memorystorepb.ListAuthTokensResponse, error) {
	if req.GetFilter() != "" || req.GetOrderBy() != "" {
		return nil, apierror.Unimplemented("filter and order_by are not served for auth tokens here")
	}
	owner, user, _, err := splitTokenName(req.GetParent(), false)
	if err != nil {
		return nil, err
	}
	c, r, err := s.find(owner)
	if err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	page, next, err := authTokensPage(r, user, req.GetPageToken(), req.GetPageSize())
	if err != nil {
		return nil, err
	}
	resp := &memorystorepb.ListAuthTokensResponse{NextPageToken: next}
	for _, t := range page {
		resp.AuthTokens = append(resp.AuthTokens, convert(renderToken(r, user, t, c.tokenUserState(r)), &memorystorepb.AuthToken{}))
	}
	return resp, nil
}

func (s *MemorystoreServer) BackupInstance(ctx context.Context, req *memorystorepb.BackupInstanceRequest) (*longrunningpb.Operation, error) {
	c, r, err := s.find(req.GetName())
	if err != nil {
		return nil, err
	}
	defer c.mu.Unlock()
	c.loadBackups()
	op, err := c.startBackup(r, req.GetBackupId(), req.GetTtl().AsDuration(), false, func() proto.Message { return renderValkey(r) })
	if err != nil {
		return nil, err
	}
	return s.api.operation(op)
}

func (s *MemorystoreServer) ListBackupCollections(ctx context.Context, req *memorystorepb.ListBackupCollectionsRequest) (*memorystorepb.ListBackupCollectionsResponse, error) {
	m := parentRE.FindStringSubmatch(req.GetParent())
	if m == nil {
		return nil, apierror.InvalidArgument("parent must be projects/{project}/locations/{location}, not %q", req.GetParent())
	}
	c, err := s.instances()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	cols := c.listCollections(m[1], m[2])
	c.mu.Unlock()
	names := make([]string, len(cols))
	for i, col := range cols {
		names[i] = col.Name
	}
	page, next, err := paging.Page("valkey-backup-collections:"+req.GetParent(), names, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("invalid page_token %q", req.GetPageToken())
	}
	resp := &memorystorepb.ListBackupCollectionsResponse{NextPageToken: next}
	for i := range page {
		resp.BackupCollections = append(resp.BackupCollections, c.valkeyCollection(cols[i+indexOf(names, page[0])]))
	}
	return resp, nil
}

// valkeyCollection is col as the Valkey API shapes it, with its counts.
// Call with mu held or after listCollections.
func (c *clusters) valkeyCollection(col *collectionRecord) *memorystorepb.BackupCollection {
	out := convert(renderCollection(col), &memorystorepb.BackupCollection{})
	out.CreateTime = timestamppb.New(col.Created)
	return out
}

func (s *MemorystoreServer) GetBackupCollection(ctx context.Context, req *memorystorepb.GetBackupCollectionRequest) (*memorystorepb.BackupCollection, error) {
	c, err := s.instances()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loadBackups()
	col := c.collections[req.GetName()]
	if col == nil {
		return nil, apierror.NotFound("Resource '%s' was not found", req.GetName())
	}
	out := c.valkeyCollection(col)
	for _, b := range c.backups {
		if b.OwnerUID == col.OwnerUID && !b.creating {
			out.TotalBackupCount++
			for _, f := range b.Files {
				out.TotalBackupSizeBytes += f.Size
			}
			if out.LastBackupTime == nil || b.Created.After(out.LastBackupTime.AsTime()) {
				out.LastBackupTime = timestamppb.New(b.Created)
			}
		}
	}
	return out, nil
}

func (s *MemorystoreServer) ListBackups(ctx context.Context, req *memorystorepb.ListBackupsRequest) (*memorystorepb.ListBackupsResponse, error) {
	c, err := s.instances()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	bs, err := c.listBackups(req.GetParent())
	var out []*memorystorepb.Backup
	for _, b := range bs {
		out = append(out, c.backupOut(b).(*memorystorepb.Backup))
	}
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	names := make([]string, len(out))
	for i, b := range out {
		names[i] = b.Name
	}
	page, next, err := paging.Page("valkey-backups:"+req.GetParent(), names, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.InvalidArgument("invalid page_token %q", req.GetPageToken())
	}
	resp := &memorystorepb.ListBackupsResponse{NextPageToken: next}
	if len(page) > 0 {
		at := indexOf(names, page[0])
		resp.Backups = out[at : at+len(page)]
	}
	return resp, nil
}

func (s *MemorystoreServer) GetBackup(ctx context.Context, req *memorystorepb.GetBackupRequest) (*memorystorepb.Backup, error) {
	c, err := s.instances()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	b, err := c.findBackup(req.GetName())
	if err != nil {
		return nil, err
	}
	return c.backupOut(b).(*memorystorepb.Backup), nil
}

func (s *MemorystoreServer) DeleteBackup(ctx context.Context, req *memorystorepb.DeleteBackupRequest) (*longrunningpb.Operation, error) {
	c, err := s.instances()
	if err != nil {
		return nil, err
	}
	op, err := c.deleteBackup(req.GetName())
	if err != nil {
		return nil, err
	}
	c.finish(op, &emptypb.Empty{}, nil)
	return s.api.operation(op)
}

func (s *MemorystoreServer) ExportBackup(ctx context.Context, req *memorystorepb.ExportBackupRequest) (*longrunningpb.Operation, error) {
	c, err := s.instances()
	if err != nil {
		return nil, err
	}
	op, err := c.exportBackup(ctx, req.GetName(), req.GetGcsBucket())
	if err != nil {
		return nil, err
	}
	return s.api.operation(op)
}
