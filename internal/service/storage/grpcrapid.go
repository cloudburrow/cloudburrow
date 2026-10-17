package storage

// google.storage.control.v2.StorageControl's Rapid Cache methods and the
// managed-folder methods (#1505), served as grpccontrol.go serves Anywhere
// Caches: each call is the JSON API's handler (rapidcache.go,
// managedfolders.go), so the two APIs share one store.

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/storage/control/apiv2/controlpb"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/emptypb"
)

// resourceName splits projects/{project}/buckets/{bucket}/{kind}/{id}.
func resourceName(name, kind string, slashes bool) (string, string, error) {
	rest, ok := strings.CutPrefix(name, "projects/")
	if ok {
		if _, r, found := strings.Cut(rest, "/buckets/"); found {
			if b, id, found := strings.Cut(r, "/"+kind+"/"); found && b != "" && id != "" && (slashes || !strings.Contains(id, "/")) {
				return b, id, nil
			}
		}
	}
	return "", "", grpcstatus.Errorf(codes.InvalidArgument, "name %q is not projects/{project}/buckets/{bucket}/%s/{id}", name, kind)
}

func rapidPath(b, id string) string { return bucketPath(b) + "/rapidCaches/" + url.PathEscape(id) }

// rapidFromJSON is a JSON API RapidCache as the proto.
func rapidFromJSON(m map[string]any) *controlpb.RapidCache {
	str := func(k string) string { s, _ := m[k].(string); return s }
	c := &controlpb.RapidCache{
		Name: folderResource(str("bucket")) + "/rapidCaches/" + str("rapidCacheId"),
		Zone: str("zone"), CacheType: str("cacheType"), AdmissionPolicy: str("admissionPolicy"), State: str("state"),
		CreateTime: jsonTime(str("createTime")), UpdateTime: jsonTime(str("updateTime")),
	}
	c.PendingUpdate, _ = m["pendingUpdate"].(bool)
	if n, err := strconv.ParseInt(strings.TrimSuffix(str("ttl"), "s"), 10, 64); err == nil {
		c.Ttl = durationpb.New(time.Duration(n) * time.Second)
	}
	return c
}

func (c *grpcControl) CreateRapidCache(ctx context.Context, req *controlpb.CreateRapidCacheRequest) (*longrunningpb.Operation, error) {
	b, err := bucketID(req.GetParent())
	if err != nil {
		return nil, err
	}
	in := req.GetRapidCache()
	body := map[string]any{"zone": in.GetZone()}
	if in.GetCacheType() != "" {
		body["cacheType"] = in.GetCacheType()
	}
	if in.GetTtl() != nil {
		body["ttl"] = durationSeconds(in.GetTtl())
	}
	if in.GetAdmissionPolicy() != "" {
		body["admissionPolicy"] = in.GetAdmissionPolicy()
	}
	hdr := http.Header{}
	if req.GetRequestId() != "" {
		hdr.Set(cacheRequestHeader, req.GetRequestId())
	}
	var out map[string]any
	if err := c.g.controlDo(ctx, http.MethodPost, bucketPath(b)+"/rapidCaches", nil, hdr, body, &out); err != nil {
		return nil, err
	}
	return operationFromJSON(out)
}

func (c *grpcControl) UpdateRapidCache(ctx context.Context, req *controlpb.UpdateRapidCacheRequest) (*longrunningpb.Operation, error) {
	in := req.GetRapidCache()
	b, id, err := resourceName(in.GetName(), "rapidCaches", false)
	if err != nil {
		return nil, err
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, grpcstatus.Error(codes.InvalidArgument, "update_mask is required")
	}
	body := map[string]any{}
	for _, p := range paths {
		all := p == "*"
		if all || p == "ttl" {
			body["ttl"] = durationSeconds(in.GetTtl())
		}
		if all || p == "admission_policy" {
			body["admissionPolicy"] = in.GetAdmissionPolicy()
		}
		if !all && p != "ttl" && p != "admission_policy" {
			// RapidCache has no ingest_on_write field in this proto, though
			// the request's documentation lists it as mutable.
			return nil, grpcstatus.Errorf(codes.InvalidArgument, "update_mask path %q is not a mutable RapidCache field: ttl and admission_policy are", p)
		}
	}
	var out map[string]any
	if err := c.g.controlDo(ctx, http.MethodPatch, rapidPath(b, id), nil, nil, body, &out); err != nil {
		return nil, err
	}
	return operationFromJSON(out)
}

func (c *grpcControl) DisableRapidCache(ctx context.Context, req *controlpb.DisableRapidCacheRequest) (*longrunningpb.Operation, error) {
	b, id, err := resourceName(req.GetName(), "rapidCaches", false)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := c.g.controlDo(ctx, http.MethodPost, rapidPath(b, id)+"/disable", nil, nil, nil, &out); err != nil {
		return nil, err
	}
	return operationFromJSON(out)
}

func (c *grpcControl) GetRapidCache(ctx context.Context, req *controlpb.GetRapidCacheRequest) (*controlpb.RapidCache, error) {
	b, id, err := resourceName(req.GetName(), "rapidCaches", false)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := c.g.controlDo(ctx, http.MethodGet, rapidPath(b, id), nil, nil, nil, &out); err != nil {
		return nil, err
	}
	return rapidFromJSON(out), nil
}

func (c *grpcControl) ListRapidCaches(ctx context.Context, req *controlpb.ListRapidCachesRequest) (*controlpb.ListRapidCachesResponse, error) {
	b, err := bucketID(req.GetParent())
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if req.GetPageSize() > 0 {
		q.Set("pageSize", strconv.Itoa(int(req.GetPageSize())))
	}
	if req.GetPageToken() != "" {
		q.Set("pageToken", req.GetPageToken())
	}
	var out struct {
		Items []map[string]any `json:"items"`
		Next  string           `json:"nextPageToken"`
	}
	if err := c.g.controlDo(ctx, http.MethodGet, bucketPath(b)+"/rapidCaches", q, nil, nil, &out); err != nil {
		return nil, err
	}
	resp := &controlpb.ListRapidCachesResponse{NextPageToken: out.Next}
	for _, m := range out.Items {
		resp.RapidCaches = append(resp.RapidCaches, rapidFromJSON(m))
	}
	return resp, nil
}

// --- managed folders ---

func managedFolderPath(b, name string) string {
	return bucketPath(b) + "/managedFolders/" + url.PathEscape(name)
}

func managedFolderFromJSON(m map[string]any) *controlpb.ManagedFolder {
	str := func(k string) string { s, _ := m[k].(string); return s }
	f := &controlpb.ManagedFolder{
		Name:       folderResource(str("bucket")) + "/managedFolders/" + str("name"),
		CreateTime: jsonTime(str("createTime")), UpdateTime: jsonTime(str("updateTime")),
	}
	f.Metageneration, _ = strconv.ParseInt(str("metageneration"), 10, 64)
	if cfg, ok := m["rapidCacheConfig"].(map[string]any); ok {
		f.RapidCacheConfig = &controlpb.ManagedFolder_RapidCacheConfig{Policies: map[string]*controlpb.ManagedFolder_RapidCacheConfig_RapidCachePolicy{}}
		pols, _ := cfg["policies"].(map[string]any)
		for k, v := range pols {
			p, _ := v.(map[string]any)
			id, _ := p["rapidCacheId"].(string)
			pol := &controlpb.ManagedFolder_RapidCacheConfig_RapidCachePolicy{RapidCacheId: id}
			if iw, _ := p["ingestOnWrite"].(string); iw == "enabled" {
				pol.IngestOnWrite = controlpb.ManagedFolder_RapidCacheConfig_RapidCachePolicy_INGEST_ON_WRITE_ENABLED
			}
			f.RapidCacheConfig.Policies[k] = pol
		}
	}
	return f
}

func rapidConfigToJSON(cfg *controlpb.ManagedFolder_RapidCacheConfig) map[string]any {
	pols := map[string]any{}
	for k, p := range cfg.GetPolicies() {
		iw := "unspecified"
		if p.GetIngestOnWrite() == controlpb.ManagedFolder_RapidCacheConfig_RapidCachePolicy_INGEST_ON_WRITE_ENABLED {
			iw = "enabled"
		}
		pols[k] = map[string]any{"rapidCacheId": p.GetRapidCacheId(), "ingestOnWrite": iw}
	}
	return map[string]any{"policies": pols}
}

func metagenerationQuery(match, notMatch *int64) url.Values {
	q := url.Values{}
	if match != nil {
		q.Set("ifMetagenerationMatch", strconv.FormatInt(*match, 10))
	}
	if notMatch != nil {
		q.Set("ifMetagenerationNotMatch", strconv.FormatInt(*notMatch, 10))
	}
	return q
}

func (c *grpcControl) CreateManagedFolder(ctx context.Context, req *controlpb.CreateManagedFolderRequest) (*controlpb.ManagedFolder, error) {
	b, err := bucketID(req.GetParent())
	if err != nil {
		return nil, err
	}
	body := map[string]any{"name": req.GetManagedFolderId()}
	if cfg := req.GetManagedFolder().GetRapidCacheConfig(); cfg != nil {
		body["rapidCacheConfig"] = rapidConfigToJSON(cfg)
	}
	var out map[string]any
	if err := c.g.controlDo(ctx, http.MethodPost, bucketPath(b)+"/managedFolders", nil, nil, body, &out); err != nil {
		return nil, err
	}
	return managedFolderFromJSON(out), nil
}

func (c *grpcControl) GetManagedFolder(ctx context.Context, req *controlpb.GetManagedFolderRequest) (*controlpb.ManagedFolder, error) {
	b, name, err := resourceName(req.GetName(), "managedFolders", true)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := c.g.controlDo(ctx, http.MethodGet, managedFolderPath(b, name), metagenerationQuery(req.IfMetagenerationMatch, req.IfMetagenerationNotMatch), nil, nil, &out); err != nil {
		return nil, err
	}
	return managedFolderFromJSON(out), nil
}

func (c *grpcControl) DeleteManagedFolder(ctx context.Context, req *controlpb.DeleteManagedFolderRequest) (*emptypb.Empty, error) {
	b, name, err := resourceName(req.GetName(), "managedFolders", true)
	if err != nil {
		return nil, err
	}
	q := metagenerationQuery(req.IfMetagenerationMatch, req.IfMetagenerationNotMatch)
	if req.GetAllowNonEmpty() {
		q.Set("allowNonEmpty", "true")
	}
	if err := c.g.controlDo(ctx, http.MethodDelete, managedFolderPath(b, name), q, nil, nil, nil); err != nil {
		return nil, err
	}
	return &emptypb.Empty{}, nil
}

func (c *grpcControl) ListManagedFolders(ctx context.Context, req *controlpb.ListManagedFoldersRequest) (*controlpb.ListManagedFoldersResponse, error) {
	b, err := bucketID(req.GetParent())
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if req.GetPageSize() > 0 {
		q.Set("pageSize", strconv.Itoa(int(req.GetPageSize())))
	}
	if req.GetPageToken() != "" {
		q.Set("pageToken", req.GetPageToken())
	}
	if req.GetPrefix() != "" {
		q.Set("prefix", req.GetPrefix())
	}
	var out struct {
		Items []map[string]any `json:"items"`
		Next  string           `json:"nextPageToken"`
	}
	if err := c.g.controlDo(ctx, http.MethodGet, bucketPath(b)+"/managedFolders", q, nil, nil, &out); err != nil {
		return nil, err
	}
	resp := &controlpb.ListManagedFoldersResponse{NextPageToken: out.Next}
	for _, m := range out.Items {
		resp.ManagedFolders = append(resp.ManagedFolders, managedFolderFromJSON(m))
	}
	return resp, nil
}

// UpdateManagedFolder sets rapid_cache_config, the only field the RPC
// updates. update_mask may name rapid_cache_config (replace every policy),
// rapid_cache_config.policies (the same), or rapid_cache_config.policies.<key>
// (set or, when the request has no such key, remove that policy); a path
// within a policy is refused, as the request's documentation says.
func (c *grpcControl) UpdateManagedFolder(ctx context.Context, req *controlpb.UpdateManagedFolderRequest) (*controlpb.ManagedFolder, error) {
	in := req.GetManagedFolder()
	b, name, err := resourceName(in.GetName(), "managedFolders", true)
	if err != nil {
		return nil, err
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		paths = []string{"rapid_cache_config"}
	}
	cfg := in.GetRapidCacheConfig()
	pols := map[string]any{}
	whole := false
	for _, p := range paths {
		switch {
		case p == "rapid_cache_config" || p == "rapid_cache_config.policies" || p == "*":
			whole = true
		case strings.HasPrefix(p, "rapid_cache_config.policies."):
			key := strings.TrimPrefix(p, "rapid_cache_config.policies.")
			if strings.Contains(key, ".") {
				return nil, grpcstatus.Errorf(codes.InvalidArgument, "update_mask path %q: a field within a rapid cache policy cannot be patched", p)
			}
			if pol, ok := cfg.GetPolicies()[key]; ok {
				pols[key] = rapidConfigToJSON(&controlpb.ManagedFolder_RapidCacheConfig{
					Policies: map[string]*controlpb.ManagedFolder_RapidCacheConfig_RapidCachePolicy{key: pol}})["policies"].(map[string]any)[key]
			} else {
				pols[key] = nil
			}
		default:
			return nil, grpcstatus.Errorf(codes.InvalidArgument, "update_mask path %q: only rapid_cache_config can be updated", p)
		}
	}
	body := map[string]any{"rapidCacheConfig": map[string]any{"policies": pols}}
	hdr := http.Header{}
	if whole {
		body = map[string]any{"rapidCacheConfig": rapidConfigToJSON(cfg)}
		hdr.Set(replacePoliciesHeader, "true")
	}
	var out map[string]any
	q := metagenerationQuery(req.IfMetagenerationMatch, req.IfMetagenerationNotMatch)
	if err := c.g.controlDo(ctx, http.MethodPatch, managedFolderPath(b, name), q, hdr, body, &out); err != nil {
		return nil, err
	}
	return managedFolderFromJSON(out), nil
}
