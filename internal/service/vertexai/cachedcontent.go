package vertexai

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"cloud.google.com/go/aiplatform/apiv1/aiplatformpb"
	betapb "cloud.google.com/go/aiplatform/apiv1beta1/aiplatformpb"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// Context caching (#2107): google.cloud.aiplatform.v1 and v1beta1
// GenAiCacheService, over gRPC and REST, and generateContent's
// cachedContent, which prefixes the request with the cache's system
// instruction, tools, tool config and contents, so the model answers exactly
// as it would the expanded request. usageMetadata then reports the cache's
// tokens as cachedContentTokenCount, inside promptTokenCount.
//
// The runtime keeps no KV cache between requests (cloudburrow_generate is one
// process per generation), so the cached prefix is prefilled again each time:
// the answer is the one Vertex gives, the latency saving is not reproduced.
//
// Documented behaviour (https://docs.cloud.google.com/vertex-ai/generative-ai/docs/context-cache/context-cache-overview
// and .../context-cache-use, re-read 2026-11-21): "The default expireTime is
// 60 minutes after the createTime"; the TTL's minimum is 1 minute and "There
// isn't a maximum cache duration"; content cached as text or a blob is at
// most 10 MB; a request using a cache cannot also set system_instruction,
// tools or tool_config; after a cache expires "it's marked for deletion",
// and is answered NOT_FOUND here; only the expiration can be updated.

const (
	defaultCacheTTL = time.Hour
	minCacheTTL     = time.Minute
	maxCacheBytes   = 10 << 20
)

// minCacheTokens is the smallest cache accepted. The overview gives 2,048
// tokens for the Gemini 2 family and 4,096 for Gemini 3 and 4; Vertex lists
// no figure for Gemma, which it does not cache. The Gemini 2 family's is
// used: it is the smallest documented, and the local model's context window
// holds it with room for the request. UNVERIFIED (re-read 2026-11-21, the
// overview): the wording of Vertex's refusal.
var minCacheTokens = 2048

// cacheStore holds the cached contents, by name.
type cacheStore struct {
	mu     sync.Mutex
	file   string
	caches map[string]*cacheEntry
	now    func() time.Time
}

// cacheEntry is one cached content and what its expansion needs.
type cacheEntry struct {
	CC *aiplatformpb.CachedContent
	// Tokens is the cache's prompt tokens, counted by the model's tokenizer
	// at creation (usageMetadata.totalTokenCount).
	Tokens int
	Images bool
}

// cacheUse is what a generation that used a cache reports.
type cacheUse struct {
	tokens int
	images bool
}

// SetCachedContentState keeps the cached contents in file, loading those
// already there.
func (s *Server) SetCachedContentState(file string) {
	st := s.cacheStore()
	st.mu.Lock()
	defer st.mu.Unlock()
	st.file = file
	b, err := os.ReadFile(file)
	if err != nil {
		return
	}
	var saved []struct {
		CachedContent json.RawMessage `json:"cachedContent"`
		Tokens        int             `json:"tokens"`
		Images        bool            `json:"images"`
	}
	if json.Unmarshal(b, &saved) != nil {
		return
	}
	for _, e := range saved {
		cc := &aiplatformpb.CachedContent{}
		if lenient.Unmarshal(e.CachedContent, cc) == nil && cc.GetName() != "" {
			st.caches[cc.GetName()] = &cacheEntry{CC: cc, Tokens: e.Tokens, Images: e.Images}
		}
	}
}

func (s *Server) cacheStore() *cacheStore {
	s.cacheOnce.Do(func() {
		s.caches = &cacheStore{caches: map[string]*cacheEntry{}, now: time.Now}
	})
	return s.caches
}

// save writes the caches; the caller holds st.mu.
func (st *cacheStore) save() {
	if st.file == "" {
		return
	}
	type saved struct {
		CachedContent json.RawMessage `json:"cachedContent"`
		Tokens        int             `json:"tokens"`
		Images        bool            `json:"images"`
	}
	out := []saved{}
	for _, name := range st.sortedNames() {
		e := st.caches[name]
		b, err := protojson.Marshal(e.CC)
		if err != nil {
			continue
		}
		out = append(out, saved{CachedContent: b, Tokens: e.Tokens, Images: e.Images})
	}
	b, _ := json.Marshal(out)
	_ = os.MkdirAll(filepath.Dir(st.file), 0o755)
	tmp := st.file + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, st.file)
	}
}

// sortedNames are the caches by creation, oldest first, then by name.
func (st *cacheStore) sortedNames() []string {
	names := make([]string, 0, len(st.caches))
	for n := range st.caches {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := st.caches[names[i]].CC.GetCreateTime().AsTime(), st.caches[names[j]].CC.GetCreateTime().AsTime()
		if !a.Equal(b) {
			return a.Before(b)
		}
		return names[i] < names[j]
	})
	return names
}

// expire drops every cache past its expireTime; the caller holds st.mu.
func (st *cacheStore) expire() {
	now := st.now()
	changed := false
	for n, e := range st.caches {
		if !now.Before(e.CC.GetExpireTime().AsTime()) {
			delete(st.caches, n)
			changed = true
		}
	}
	if changed {
		st.save()
	}
}

// get is a live cache, or NOT_FOUND; the caller holds st.mu.
func (st *cacheStore) get(name string) (*cacheEntry, error) {
	if _, _, _, err := parseChild("name", "cachedContents", name); err != nil {
		return nil, err
	}
	st.expire()
	e, ok := st.caches[name]
	if !ok {
		return nil, apierror.NotFound("CachedContent %q not found", name)
	}
	return e, nil
}

// cachedParts is a cache's content in this package's request types, decoded
// strictly so a part or tool this endpoint does not serve is refused by name
// when the cache is made, not when it is used.
type cachedParts struct {
	SystemInstruction *Content    `json:"systemInstruction,omitempty"`
	Contents          []Content   `json:"contents,omitempty"`
	Tools             []Tool      `json:"tools,omitempty"`
	ToolConfig        *ToolConfig `json:"toolConfig,omitempty"`
}

func partsOf(cc *aiplatformpb.CachedContent) (*cachedParts, error) {
	b, err := protojson.Marshal(&aiplatformpb.CachedContent{
		SystemInstruction: cc.GetSystemInstruction(), Contents: cc.GetContents(),
		Tools: cc.GetTools(), ToolConfig: cc.GetToolConfig(),
	})
	if err != nil {
		return nil, apierror.InvalidArgument("%v", err)
	}
	var p cachedParts
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, apierror.InvalidArgument("cachedContent: %v; this endpoint caches what generateContent serves", err)
	}
	return &p, nil
}

// cacheModel parses a cache's model,
// projects/{p}/locations/{l}/publishers/google/models/{m} (or the
// publishers/google/models/{m} short form), to its model ID.
func cacheModel(name string) (string, error) {
	p := strings.Split(name, "/")
	switch {
	case len(p) == 8 && p[0] == "projects" && p[2] == "locations" && p[4] == "publishers" && p[6] == "models" && p[7] != "":
		if p[5] != "google" {
			return "", apierror.NotFound("publisher %q is not served; only \"google\" is", p[5])
		}
		return p[7], nil
	case len(p) == 4 && p[0] == "publishers" && p[1] == "google" && p[2] == "models" && p[3] != "":
		return p[3], nil
	}
	return "", apierror.InvalidArgument(
		"cachedContent.model %q is not projects/{project}/locations/{location}/publishers/google/models/{model}", name)
}

// expiration applies ttl or expireTime, refusing an expiration under the
// documented minimum of one minute.
func expiration(now time.Time, cc *aiplatformpb.CachedContent, set bool) (*timestamppb.Timestamp, error) {
	switch e := cc.GetExpiration().(type) {
	case *aiplatformpb.CachedContent_Ttl:
		if err := e.Ttl.CheckValid(); err != nil {
			return nil, apierror.InvalidArgument("cachedContent.ttl: %v", err)
		}
		d := e.Ttl.AsDuration()
		if d < minCacheTTL {
			return nil, apierror.InvalidArgument("cachedContent.ttl %s is below the minimum of 1 minute", d)
		}
		return timestamppb.New(now.Add(d)), nil
	case *aiplatformpb.CachedContent_ExpireTime:
		if err := e.ExpireTime.CheckValid(); err != nil {
			return nil, apierror.InvalidArgument("cachedContent.expireTime: %v", err)
		}
		t := e.ExpireTime.AsTime()
		if t.Sub(now) < minCacheTTL {
			return nil, apierror.InvalidArgument("cachedContent.expireTime %s is less than 1 minute from now", t.Format(time.RFC3339))
		}
		return timestamppb.New(t), nil
	}
	if set {
		return nil, apierror.InvalidArgument("ttl or expireTime is required")
	}
	return timestamppb.New(now.Add(defaultCacheTTL)), nil
}

// createCache serves CreateCachedContent.
func (s *Server) createCache(ctx context.Context, req *aiplatformpb.CreateCachedContentRequest) (*aiplatformpb.CachedContent, error) {
	project, location, err := parseParent("parent", req.GetParent())
	if err != nil {
		return nil, err
	}
	in := req.GetCachedContent()
	if in == nil {
		return nil, apierror.InvalidArgument("cachedContent is required")
	}
	if in.GetName() != "" {
		// The name is output only (CachedContent proto: "Immutable.
		// Identifier"; the service assigns it).
		in = proto.Clone(in).(*aiplatformpb.CachedContent)
		in.Name = ""
	}
	if in.GetEncryptionSpec() != nil {
		return nil, apierror.Unimplemented("cachedContent.encryptionSpec is not supported: cached contents are kept unencrypted in the instance's state directory")
	}
	if in.GetModel() == "" {
		return nil, apierror.InvalidArgument("cachedContent.model is required")
	}
	modelID, err := cacheModel(in.GetModel())
	if err != nil {
		return nil, err
	}
	if _, err := s.resolveModel(modelID); err != nil {
		return nil, err
	}
	if n := proto.Size(in); n > maxCacheBytes {
		return nil, apierror.InvalidArgument("cachedContent is %d bytes; the maximum is 10 MB", n)
	}
	parts, err := partsOf(in)
	if err != nil {
		return nil, err
	}
	if len(parts.Contents) == 0 {
		return nil, apierror.InvalidArgument("cachedContent.contents is required")
	}
	g := &GenerateContentRequest{Contents: parts.Contents, SystemInstruction: parts.SystemInstruction,
		Tools: parts.Tools, ToolConfig: parts.ToolConfig}
	if err := validateTools(g.Tools, g.ToolConfig); err != nil {
		return nil, err
	}
	if err := validateConversation(g, false); err != nil {
		return nil, err
	}
	// The cache's tokens, counted by the model's tokenizer as countTokens
	// counts them.
	body, err := json.Marshal(CountTokensRequest{Contents: g.Contents, SystemInstruction: g.SystemInstruction, Tools: g.Tools})
	if err != nil {
		return nil, apierror.Internal(err, "encoding the cache: %v", err)
	}
	counted, err := s.countTokens(ctx, modelID, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	tokens := int(counted.TotalTokens)
	if tokens < minCacheTokens {
		return nil, apierror.InvalidArgument(
			"The cached content is of %d total tokens. The minimum token count to start caching is %d.", tokens, minCacheTokens)
	}

	st := s.cacheStore()
	st.mu.Lock()
	defer st.mu.Unlock()
	now := st.now()
	exp, err := expiration(now, in, false)
	if err != nil {
		return nil, err
	}
	cc := proto.Clone(in).(*aiplatformpb.CachedContent)
	cc.Name = "projects/" + project + "/locations/" + location + "/cachedContents/" + cacheID()
	cc.CreateTime = timestamppb.New(now)
	cc.UpdateTime = cc.CreateTime
	cc.Expiration = &aiplatformpb.CachedContent_ExpireTime{ExpireTime: exp}
	cc.UsageMetadata = usageOfCache(parts, tokens)
	images := cc.UsageMetadata.GetImageCount() > 0
	st.caches[cc.Name] = &cacheEntry{CC: cc, Tokens: tokens, Images: images}
	st.save()
	return proto.Clone(cc).(*aiplatformpb.CachedContent), nil
}

// usageOfCache is CachedContent.usage_metadata: the tokens, the text
// characters and the images cached.
func usageOfCache(p *cachedParts, tokens int) *aiplatformpb.CachedContent_UsageMetadata {
	u := &aiplatformpb.CachedContent_UsageMetadata{TotalTokenCount: int32(tokens)}
	count := func(c *Content) {
		for _, part := range c.Parts {
			u.TextCount += int32(utf8.RuneCountInString(part.Text))
			if part.InlineData != nil || part.FileData != nil {
				u.ImageCount++
			}
		}
	}
	if p.SystemInstruction != nil {
		count(p.SystemInstruction)
	}
	for i := range p.Contents {
		count(&p.Contents[i])
	}
	return u
}

// cacheID is a cache's numeric ID, as Vertex assigns them.
func cacheID() string {
	n, _ := rand.Int(rand.Reader, big.NewInt(8e18))
	return strconv.FormatInt(n.Int64()+1e18, 10)
}

func (s *Server) getCache(name string) (*aiplatformpb.CachedContent, error) {
	st := s.cacheStore()
	st.mu.Lock()
	defer st.mu.Unlock()
	e, err := st.get(name)
	if err != nil {
		return nil, err
	}
	return proto.Clone(e.CC).(*aiplatformpb.CachedContent), nil
}

// updateCache serves UpdateCachedContent: only the expiration (ttl or
// expireTime) can change. Without an update mask the fields the request sets
// are the ones updated (the Gen AI SDK sends none).
func (s *Server) updateCache(req *aiplatformpb.UpdateCachedContentRequest) (*aiplatformpb.CachedContent, error) {
	in := req.GetCachedContent()
	if in == nil {
		return nil, apierror.InvalidArgument("cachedContent is required")
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		switch in.GetExpiration().(type) {
		case *aiplatformpb.CachedContent_Ttl:
			paths = []string{"ttl"}
		case *aiplatformpb.CachedContent_ExpireTime:
			paths = []string{"expire_time"}
		}
		probe := proto.Clone(in).(*aiplatformpb.CachedContent)
		probe.Name, probe.Expiration = "", nil
		if !proto.Equal(probe, &aiplatformpb.CachedContent{}) {
			return nil, apierror.InvalidArgument("only ttl and expireTime can be updated on a CachedContent")
		}
	}
	if len(paths) == 0 {
		return nil, apierror.InvalidArgument("update_mask is required: only ttl and expireTime can be updated")
	}
	for _, p := range paths {
		switch p {
		case "ttl", "expire_time", "expireTime":
		default:
			return nil, apierror.InvalidArgument("update_mask path %q cannot be updated: only ttl and expireTime can be updated", p)
		}
	}
	st := s.cacheStore()
	st.mu.Lock()
	defer st.mu.Unlock()
	e, err := st.get(in.GetName())
	if err != nil {
		return nil, err
	}
	now := st.now()
	exp, err := expiration(now, in, true)
	if err != nil {
		return nil, err
	}
	e.CC.Expiration = &aiplatformpb.CachedContent_ExpireTime{ExpireTime: exp}
	e.CC.UpdateTime = timestamppb.New(now)
	st.save()
	return proto.Clone(e.CC).(*aiplatformpb.CachedContent), nil
}

func (s *Server) deleteCache(name string) error {
	st := s.cacheStore()
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, err := st.get(name); err != nil {
		return err
	}
	delete(st.caches, name)
	st.save()
	return nil
}

// maxCachePage bounds a list page. UNVERIFIED (re-read 2026-11-21,
// ListCachedContentsRequest in cloud.google.com/go/aiplatform v1.127.0,
// which says only "The maximum number of cached contents to return"): the
// default and maximum page size.
const maxCachePage = 1000

func (s *Server) listCaches(req *aiplatformpb.ListCachedContentsRequest) (*aiplatformpb.ListCachedContentsResponse, error) {
	project, location, err := parseParent("parent", req.GetParent())
	if err != nil {
		return nil, err
	}
	size := int(req.GetPageSize())
	if size <= 0 || size > maxCachePage {
		size = maxCachePage
	}
	start := 0
	if t := req.GetPageToken(); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n < 0 {
			return nil, apierror.InvalidArgument("page_token %q is not valid", t)
		}
		start = n
	}
	st := s.cacheStore()
	st.mu.Lock()
	defer st.mu.Unlock()
	st.expire()
	prefix := "projects/" + project + "/locations/" + location + "/cachedContents/"
	var all []*aiplatformpb.CachedContent
	for _, n := range st.sortedNames() {
		if strings.HasPrefix(n, prefix) {
			all = append(all, st.caches[n].CC)
		}
	}
	out := &aiplatformpb.ListCachedContentsResponse{}
	for i := start; i < len(all) && i < start+size; i++ {
		out.CachedContents = append(out.CachedContents, proto.Clone(all[i]).(*aiplatformpb.CachedContent))
	}
	if start+size < len(all) {
		out.NextPageToken = strconv.Itoa(start + size)
	}
	return out, nil
}

// expandCachedContent replaces a request's cachedContent with what it
// caches: its system instruction, tools and tool config, and its contents
// before the request's own. Vertex refuses a request that sets those
// itself alongside a cache, and one whose model is not the cache's.
// UNVERIFIED (re-read 2026-11-21, context-cache-use, which states the rule
// but gives no error text): both refusals' wording.
func (s *Server) expandCachedContent(ctx context.Context, req *GenerateContentRequest, model string) (*cacheUse, error) {
	if req.CachedContent == "" {
		return nil, nil
	}
	if !honoursOptions(s.gen) {
		return nil, apierror.Unimplemented("cachedContent needs cloudburrow_generate, which takes whole conversations; this runtime takes one prompt")
	}
	if req.SystemInstruction != nil || len(req.Tools) > 0 || req.ToolConfig != nil {
		return nil, apierror.InvalidArgument("CachedContent can not be used with GenerateContent request setting system_instruction, tools or tool_config.\n\n" +
			"Proposed fix: move those values to CachedContent from GenerateContent request.")
	}
	st := s.cacheStore()
	st.mu.Lock()
	e, err := st.get(req.CachedContent)
	var cc *aiplatformpb.CachedContent
	if err == nil {
		cc = proto.Clone(e.CC).(*aiplatformpb.CachedContent)
	}
	st.mu.Unlock()
	if err != nil {
		return nil, err
	}
	cacheModelID, err := cacheModel(cc.GetModel())
	if err != nil {
		return nil, err
	}
	resolved, err := s.resolveModel(cacheModelID)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(resolved, model) {
		return nil, apierror.InvalidArgument("Model used by GenerateContent request (%s) and CachedContent (%s) has to be the same.", model, cc.GetModel())
	}
	parts, err := partsOf(cc)
	if err != nil {
		return nil, err
	}
	req.SystemInstruction = parts.SystemInstruction
	req.Tools = parts.Tools
	req.ToolConfig = parts.ToolConfig
	req.Contents = append(append([]Content(nil), parts.Contents...), req.Contents...)
	req.CachedContent = ""
	return &cacheUse{tokens: e.Tokens, images: e.Images}, nil
}

// ---- gRPC: v1, and v1beta1 through the JSON mapping (betabridge.go)

type cacheService struct {
	aiplatformpb.UnimplementedGenAiCacheServiceServer
	s *Server
}

func (c *cacheService) CreateCachedContent(ctx context.Context, req *aiplatformpb.CreateCachedContentRequest) (*aiplatformpb.CachedContent, error) {
	out, err := c.s.createCache(ctx, req)
	return out, grpcError(err)
}

func (c *cacheService) GetCachedContent(_ context.Context, req *aiplatformpb.GetCachedContentRequest) (*aiplatformpb.CachedContent, error) {
	out, err := c.s.getCache(req.GetName())
	return out, grpcError(err)
}

func (c *cacheService) UpdateCachedContent(_ context.Context, req *aiplatformpb.UpdateCachedContentRequest) (*aiplatformpb.CachedContent, error) {
	out, err := c.s.updateCache(req)
	return out, grpcError(err)
}

func (c *cacheService) DeleteCachedContent(_ context.Context, req *aiplatformpb.DeleteCachedContentRequest) (*emptypb.Empty, error) {
	if err := c.s.deleteCache(req.GetName()); err != nil {
		return nil, grpcError(err)
	}
	return &emptypb.Empty{}, nil
}

func (c *cacheService) ListCachedContents(_ context.Context, req *aiplatformpb.ListCachedContentsRequest) (*aiplatformpb.ListCachedContentsResponse, error) {
	out, err := c.s.listCaches(req)
	return out, grpcError(err)
}

type betaCacheService struct {
	betapb.UnimplementedGenAiCacheServiceServer
	s *Server
}

func (b *betaCacheService) v1() *cacheService { return &cacheService{s: b.s} }

func (b *betaCacheService) CreateCachedContent(ctx context.Context, req *betapb.CreateCachedContentRequest) (*betapb.CachedContent, error) {
	return unary(req, &aiplatformpb.CreateCachedContentRequest{}, &betapb.CachedContent{}, func(v *aiplatformpb.CreateCachedContentRequest) (*aiplatformpb.CachedContent, error) {
		return b.v1().CreateCachedContent(ctx, v)
	})
}

func (b *betaCacheService) GetCachedContent(ctx context.Context, req *betapb.GetCachedContentRequest) (*betapb.CachedContent, error) {
	return unary(req, &aiplatformpb.GetCachedContentRequest{}, &betapb.CachedContent{}, func(v *aiplatformpb.GetCachedContentRequest) (*aiplatformpb.CachedContent, error) {
		return b.v1().GetCachedContent(ctx, v)
	})
}

func (b *betaCacheService) UpdateCachedContent(ctx context.Context, req *betapb.UpdateCachedContentRequest) (*betapb.CachedContent, error) {
	return unary(req, &aiplatformpb.UpdateCachedContentRequest{}, &betapb.CachedContent{}, func(v *aiplatformpb.UpdateCachedContentRequest) (*aiplatformpb.CachedContent, error) {
		return b.v1().UpdateCachedContent(ctx, v)
	})
}

func (b *betaCacheService) DeleteCachedContent(ctx context.Context, req *betapb.DeleteCachedContentRequest) (*emptypb.Empty, error) {
	var v aiplatformpb.DeleteCachedContentRequest
	if err := toV1(req, &v); err != nil {
		return nil, grpcError(err)
	}
	return b.v1().DeleteCachedContent(ctx, &v)
}

func (b *betaCacheService) ListCachedContents(ctx context.Context, req *betapb.ListCachedContentsRequest) (*betapb.ListCachedContentsResponse, error) {
	return unary(req, &aiplatformpb.ListCachedContentsRequest{}, &betapb.ListCachedContentsResponse{}, func(v *aiplatformpb.ListCachedContentsRequest) (*aiplatformpb.ListCachedContentsResponse, error) {
		return b.v1().ListCachedContents(ctx, v)
	})
}

// ---- REST: GenAiCacheService's google.api.http bindings, v1 and v1beta1

// cacheREST is the transcoder for the cachedContents paths.
func (s *Server) cacheREST() *rest.Transcoder {
	s.cacheRESTOnce.Do(func() {
		t := &rest.Transcoder{Packages: []string{"google.cloud.aiplatform.v1", "google.cloud.aiplatform.v1beta1"}}
		aiplatformpb.RegisterGenAiCacheServiceServer(t, &cacheService{s: s})
		betapb.RegisterGenAiCacheServiceServer(t, &betaCacheService{s: s})
		s.cacheTranscoder = t
	})
	return s.cacheTranscoder
}
