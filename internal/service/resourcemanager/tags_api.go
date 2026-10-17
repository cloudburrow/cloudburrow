package resourcemanager

// The v3 TagKeys, TagValues, TagBindings (with ListEffectiveTags) and
// TagHolds services over gRPC and JSON, and Liens over JSON on v1 and v3,
// over the records in tags.go (#1295). Every change completes before the
// call returns, so the long-running operations come back done.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"github.com/cloudburrow/cloudburrow/internal/paging"
	"github.com/cloudburrow/cloudburrow/internal/transport/rest"
)

// pageProtos pages items by name.
func pageProtos[T proto.Message](scope string, items []T, name func(T) string, token string, size int) ([]T, string, error) {
	by := map[string]T{}
	names := make([]string, 0, len(items))
	for _, it := range items {
		n := name(it)
		by[n] = it
		names = append(names, n)
	}
	page, next, err := paging.Page(scope, names, token, size)
	if err != nil {
		return nil, "", apierror.InvalidArgument("%v", err)
	}
	out := make([]T, 0, len(page))
	for _, n := range page {
		out = append(out, by[n])
	}
	return out, next, nil
}

func idOf(name string) string { return name[strings.LastIndex(name, "/")+1:] }

// --- TagKeys ---------------------------------------------------------------

// TagKeysServer serves google.cloud.resourcemanager.v3.TagKeys.
type TagKeysServer struct {
	rmpb.UnimplementedTagKeysServer
	reg *Registry
}

func (s *TagKeysServer) ListTagKeys(_ context.Context, req *rmpb.ListTagKeysRequest) (*rmpb.ListTagKeysResponse, error) {
	all, err := s.reg.TagKeys(req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	page, next, err := pageProtos("tagKeys.list:"+req.GetParent(), all, (*rmpb.TagKey).GetName, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return &rmpb.ListTagKeysResponse{TagKeys: page, NextPageToken: next}, nil
}

func (s *TagKeysServer) GetTagKey(_ context.Context, req *rmpb.GetTagKeyRequest) (*rmpb.TagKey, error) {
	k, err := s.reg.TagKey(req.GetName())
	return k, apierror.Wrap(err)
}

func (s *TagKeysServer) GetNamespacedTagKey(_ context.Context, req *rmpb.GetNamespacedTagKeyRequest) (*rmpb.TagKey, error) {
	k, err := s.reg.NamespacedTagKey(req.GetName())
	return k, apierror.Wrap(err)
}

func (s *TagKeysServer) CreateTagKey(_ context.Context, req *rmpb.CreateTagKeyRequest) (*longrunningpb.Operation, error) {
	k, err := s.reg.CreateTagKey(req.GetTagKey(), req.GetValidateOnly())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return doneOperation("ctk", idOf(k.GetName()), k)
}

func (s *TagKeysServer) UpdateTagKey(_ context.Context, req *rmpb.UpdateTagKeyRequest) (*longrunningpb.Operation, error) {
	k, err := s.reg.UpdateTagKey(req.GetTagKey(), req.GetUpdateMask().GetPaths(), req.GetValidateOnly())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return doneOperation("utk", idOf(k.GetName()), k)
}

func (s *TagKeysServer) DeleteTagKey(_ context.Context, req *rmpb.DeleteTagKeyRequest) (*longrunningpb.Operation, error) {
	k, err := s.reg.DeleteTagKey(req.GetName(), req.GetEtag(), req.GetValidateOnly())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return doneOperation("dtk", idOf(k.GetName()), k)
}

// --- TagValues -------------------------------------------------------------

// TagValuesServer serves google.cloud.resourcemanager.v3.TagValues.
type TagValuesServer struct {
	rmpb.UnimplementedTagValuesServer
	reg *Registry
}

func (s *TagValuesServer) ListTagValues(_ context.Context, req *rmpb.ListTagValuesRequest) (*rmpb.ListTagValuesResponse, error) {
	all, err := s.reg.TagValues(req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	page, next, err := pageProtos("tagValues.list:"+req.GetParent(), all, (*rmpb.TagValue).GetName, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return &rmpb.ListTagValuesResponse{TagValues: page, NextPageToken: next}, nil
}

func (s *TagValuesServer) GetTagValue(_ context.Context, req *rmpb.GetTagValueRequest) (*rmpb.TagValue, error) {
	v, err := s.reg.TagValue(req.GetName())
	return v, apierror.Wrap(err)
}

func (s *TagValuesServer) GetNamespacedTagValue(_ context.Context, req *rmpb.GetNamespacedTagValueRequest) (*rmpb.TagValue, error) {
	v, err := s.reg.NamespacedTagValue(req.GetName())
	return v, apierror.Wrap(err)
}

func (s *TagValuesServer) CreateTagValue(_ context.Context, req *rmpb.CreateTagValueRequest) (*longrunningpb.Operation, error) {
	v, err := s.reg.CreateTagValue(req.GetTagValue(), req.GetValidateOnly())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return doneOperation("ctv", idOf(v.GetName()), v)
}

func (s *TagValuesServer) UpdateTagValue(_ context.Context, req *rmpb.UpdateTagValueRequest) (*longrunningpb.Operation, error) {
	v, err := s.reg.UpdateTagValue(req.GetTagValue(), req.GetUpdateMask().GetPaths(), req.GetValidateOnly())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return doneOperation("utv", idOf(v.GetName()), v)
}

func (s *TagValuesServer) DeleteTagValue(_ context.Context, req *rmpb.DeleteTagValueRequest) (*longrunningpb.Operation, error) {
	v, err := s.reg.DeleteTagValue(req.GetName(), req.GetEtag(), req.GetValidateOnly())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return doneOperation("dtv", idOf(v.GetName()), v)
}

// --- TagBindings -----------------------------------------------------------

// TagBindingsServer serves google.cloud.resourcemanager.v3.TagBindings.
type TagBindingsServer struct {
	rmpb.UnimplementedTagBindingsServer
	reg *Registry
}

func (s *TagBindingsServer) ListTagBindings(_ context.Context, req *rmpb.ListTagBindingsRequest) (*rmpb.ListTagBindingsResponse, error) {
	all, err := s.reg.TagBindings(req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	page, next, err := pageProtos("tagBindings.list:"+req.GetParent(), all, (*rmpb.TagBinding).GetName, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return &rmpb.ListTagBindingsResponse{TagBindings: page, NextPageToken: next}, nil
}

func (s *TagBindingsServer) CreateTagBinding(_ context.Context, req *rmpb.CreateTagBindingRequest) (*longrunningpb.Operation, error) {
	b, err := s.reg.CreateTagBinding(req.GetTagBinding(), req.GetValidateOnly())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return doneOperation("ctb", idOf(b.GetTagValue()), b)
}

func (s *TagBindingsServer) DeleteTagBinding(_ context.Context, req *rmpb.DeleteTagBindingRequest) (*longrunningpb.Operation, error) {
	if err := s.reg.DeleteTagBinding(req.GetName()); err != nil {
		return nil, apierror.Wrap(err)
	}
	return doneOperation("dtb", idOf(req.GetName()), &emptypb.Empty{})
}

func (s *TagBindingsServer) ListEffectiveTags(_ context.Context, req *rmpb.ListEffectiveTagsRequest) (*rmpb.ListEffectiveTagsResponse, error) {
	all, err := s.reg.EffectiveTags(req.GetParent())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	page, next, err := pageProtos("effectiveTags.list:"+req.GetParent(), all, (*rmpb.EffectiveTag).GetTagValue, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return &rmpb.ListEffectiveTagsResponse{EffectiveTags: page, NextPageToken: next}, nil
}

// --- TagHolds --------------------------------------------------------------

// TagHoldsServer serves google.cloud.resourcemanager.v3.TagHolds.
type TagHoldsServer struct {
	rmpb.UnimplementedTagHoldsServer
	reg *Registry
}

func (s *TagHoldsServer) CreateTagHold(_ context.Context, req *rmpb.CreateTagHoldRequest) (*longrunningpb.Operation, error) {
	h, err := s.reg.CreateTagHold(req.GetParent(), req.GetTagHold(), req.GetValidateOnly())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return doneOperation("cth", idOf(h.GetName()), h)
}

func (s *TagHoldsServer) DeleteTagHold(_ context.Context, req *rmpb.DeleteTagHoldRequest) (*longrunningpb.Operation, error) {
	if !req.GetValidateOnly() {
		if err := s.reg.DeleteTagHold(req.GetName()); err != nil {
			return nil, apierror.Wrap(err)
		}
	}
	return doneOperation("dth", idOf(req.GetName()), &emptypb.Empty{})
}

func (s *TagHoldsServer) ListTagHolds(_ context.Context, req *rmpb.ListTagHoldsRequest) (*rmpb.ListTagHoldsResponse, error) {
	all, err := s.reg.TagHolds(req.GetParent(), req.GetFilter())
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	page, next, err := pageProtos("tagHolds.list:"+req.GetParent()+":"+req.GetFilter(), all, (*rmpb.TagHold).GetName, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, apierror.Wrap(err)
	}
	return &rmpb.ListTagHoldsResponse{TagHolds: page, NextPageToken: next}, nil
}

// --- REST --------------------------------------------------------------

type tagServers struct {
	reg      *Registry
	keys     *TagKeysServer
	values   *TagValuesServer
	bindings *TagBindingsServer
	holds    *TagHoldsServer
}

func newTagServers(reg *Registry) *tagServers {
	return &tagServers{reg: reg, keys: &TagKeysServer{reg: reg}, values: &TagValuesServer{reg: reg},
		bindings: &TagBindingsServer{reg: reg}, holds: &TagHoldsServer{reg: reg}}
}

func pageArgs(r *http.Request) (string, int32, error) {
	size, err := rest.QueryInt(r, "pageSize", 0)
	return r.URL.Query().Get("pageToken"), int32(size), err
}

func (t *tagServers) restRoutes(r *rest.Router) {
	// Liens: v1 (Terraform's google_resource_manager_lien) and v3 alike.
	for _, v := range []string{"v1", "v3"} {
		r.Handle("GET /"+v+"/liens", t.restListLiens)
		r.Handle("POST /"+v+"/liens", t.restCreateLien)
		r.Handle("GET /"+v+"/liens/{lien}", t.restGetLien)
		r.Handle("DELETE /"+v+"/liens/{lien}", t.restDeleteLien)
	}

	r.Handle("GET /v3/tagKeys", func(w http.ResponseWriter, req *http.Request) error {
		tok, size, err := pageArgs(req)
		if err != nil {
			return err
		}
		resp, err := t.keys.ListTagKeys(req.Context(), &rmpb.ListTagKeysRequest{Parent: req.URL.Query().Get("parent"), PageToken: tok, PageSize: size})
		if err != nil {
			return err
		}
		return writeProto(w, resp)
	})
	r.Handle("GET /v3/tagKeys/namespaced", func(w http.ResponseWriter, req *http.Request) error {
		k, err := t.keys.GetNamespacedTagKey(req.Context(), &rmpb.GetNamespacedTagKeyRequest{Name: req.URL.Query().Get("name")})
		if err != nil {
			return err
		}
		return writeProto(w, k)
	})
	r.Handle("POST /v3/tagKeys", func(w http.ResponseWriter, req *http.Request) error {
		var k rmpb.TagKey
		if err := readProto(req, &k); err != nil {
			return err
		}
		op, err := t.keys.CreateTagKey(req.Context(), &rmpb.CreateTagKeyRequest{TagKey: &k, ValidateOnly: req.URL.Query().Get("validateOnly") == "true"})
		if err != nil {
			return err
		}
		return writeProto(w, op)
	})
	r.Handle("GET /v3/tagKeys/{id}", func(w http.ResponseWriter, req *http.Request) error {
		k, err := t.keys.GetTagKey(req.Context(), &rmpb.GetTagKeyRequest{Name: "tagKeys/" + req.PathValue("id")})
		if err != nil {
			return err
		}
		return writeProto(w, k)
	})
	r.Handle("PATCH /v3/tagKeys/{id}", func(w http.ResponseWriter, req *http.Request) error {
		var k rmpb.TagKey
		if err := readProto(req, &k); err != nil {
			return err
		}
		k.Name = "tagKeys/" + req.PathValue("id")
		op, err := t.keys.UpdateTagKey(req.Context(), &rmpb.UpdateTagKeyRequest{TagKey: &k, UpdateMask: splitMask(req.URL.Query().Get("updateMask")),
			ValidateOnly: req.URL.Query().Get("validateOnly") == "true"})
		if err != nil {
			return err
		}
		return writeProto(w, op)
	})
	r.Handle("DELETE /v3/tagKeys/{id}", func(w http.ResponseWriter, req *http.Request) error {
		q := req.URL.Query()
		op, err := t.keys.DeleteTagKey(req.Context(), &rmpb.DeleteTagKeyRequest{Name: "tagKeys/" + req.PathValue("id"), Etag: q.Get("etag"),
			ValidateOnly: q.Get("validateOnly") == "true"})
		if err != nil {
			return err
		}
		return writeProto(w, op)
	})
	// The three IAM methods (#1486): POST /v3/tagKeys/{id}:{verb}.
	r.Handle("POST /v3/tagKeys/{id}", func(w http.ResponseWriter, req *http.Request) error {
		id, verb, _ := strings.Cut(req.PathValue("id"), ":")
		if !isIAMVerb(verb) {
			return apierror.NotFound("no method %s on tagKeys", verb)
		}
		return serveIAM(w, req, t.keys, "tagKeys/"+id, verb)
	})

	r.Handle("GET /v3/tagValues", func(w http.ResponseWriter, req *http.Request) error {
		tok, size, err := pageArgs(req)
		if err != nil {
			return err
		}
		resp, err := t.values.ListTagValues(req.Context(), &rmpb.ListTagValuesRequest{Parent: req.URL.Query().Get("parent"), PageToken: tok, PageSize: size})
		if err != nil {
			return err
		}
		return writeProto(w, resp)
	})
	r.Handle("GET /v3/tagValues/namespaced", func(w http.ResponseWriter, req *http.Request) error {
		v, err := t.values.GetNamespacedTagValue(req.Context(), &rmpb.GetNamespacedTagValueRequest{Name: req.URL.Query().Get("name")})
		if err != nil {
			return err
		}
		return writeProto(w, v)
	})
	r.Handle("POST /v3/tagValues", func(w http.ResponseWriter, req *http.Request) error {
		var v rmpb.TagValue
		if err := readProto(req, &v); err != nil {
			return err
		}
		op, err := t.values.CreateTagValue(req.Context(), &rmpb.CreateTagValueRequest{TagValue: &v, ValidateOnly: req.URL.Query().Get("validateOnly") == "true"})
		if err != nil {
			return err
		}
		return writeProto(w, op)
	})
	r.Handle("GET /v3/tagValues/{id}", func(w http.ResponseWriter, req *http.Request) error {
		v, err := t.values.GetTagValue(req.Context(), &rmpb.GetTagValueRequest{Name: "tagValues/" + req.PathValue("id")})
		if err != nil {
			return err
		}
		return writeProto(w, v)
	})
	r.Handle("PATCH /v3/tagValues/{id}", func(w http.ResponseWriter, req *http.Request) error {
		var v rmpb.TagValue
		if err := readProto(req, &v); err != nil {
			return err
		}
		v.Name = "tagValues/" + req.PathValue("id")
		op, err := t.values.UpdateTagValue(req.Context(), &rmpb.UpdateTagValueRequest{TagValue: &v, UpdateMask: splitMask(req.URL.Query().Get("updateMask")),
			ValidateOnly: req.URL.Query().Get("validateOnly") == "true"})
		if err != nil {
			return err
		}
		return writeProto(w, op)
	})
	r.Handle("DELETE /v3/tagValues/{id}", func(w http.ResponseWriter, req *http.Request) error {
		q := req.URL.Query()
		op, err := t.values.DeleteTagValue(req.Context(), &rmpb.DeleteTagValueRequest{Name: "tagValues/" + req.PathValue("id"), Etag: q.Get("etag"),
			ValidateOnly: q.Get("validateOnly") == "true"})
		if err != nil {
			return err
		}
		return writeProto(w, op)
	})
	// The three IAM methods (#1486): POST /v3/tagValues/{id}:{verb}.
	r.Handle("POST /v3/tagValues/{id}", func(w http.ResponseWriter, req *http.Request) error {
		id, verb, _ := strings.Cut(req.PathValue("id"), ":")
		if !isIAMVerb(verb) {
			return apierror.NotFound("no method %s on tagValues", verb)
		}
		return serveIAM(w, req, t.values, "tagValues/"+id, verb)
	})
	r.Handle("GET /v3/tagValues/{id}/tagHolds", func(w http.ResponseWriter, req *http.Request) error {
		tok, size, err := pageArgs(req)
		if err != nil {
			return err
		}
		resp, err := t.holds.ListTagHolds(req.Context(), &rmpb.ListTagHoldsRequest{Parent: "tagValues/" + req.PathValue("id"),
			Filter: req.URL.Query().Get("filter"), PageToken: tok, PageSize: size})
		if err != nil {
			return err
		}
		return writeProto(w, resp)
	})
	r.Handle("POST /v3/tagValues/{id}/tagHolds", func(w http.ResponseWriter, req *http.Request) error {
		var h rmpb.TagHold
		if err := readProto(req, &h); err != nil {
			return err
		}
		op, err := t.holds.CreateTagHold(req.Context(), &rmpb.CreateTagHoldRequest{Parent: "tagValues/" + req.PathValue("id"), TagHold: &h,
			ValidateOnly: req.URL.Query().Get("validateOnly") == "true"})
		if err != nil {
			return err
		}
		return writeProto(w, op)
	})
	r.Handle("DELETE /v3/tagValues/{id}/tagHolds/{hold}", func(w http.ResponseWriter, req *http.Request) error {
		op, err := t.holds.DeleteTagHold(req.Context(), &rmpb.DeleteTagHoldRequest{
			Name:         "tagValues/" + req.PathValue("id") + "/tagHolds/" + req.PathValue("hold"),
			ValidateOnly: req.URL.Query().Get("validateOnly") == "true"})
		if err != nil {
			return err
		}
		return writeProto(w, op)
	})

	// Terraform's google_tags_tag_binding lists at tagBindings/?parent=.
	r.Handle("GET /v3/tagBindings/{$}", func(w http.ResponseWriter, req *http.Request) error { return t.restListBindings(w, req) })
	r.Handle("GET /v3/tagBindings", func(w http.ResponseWriter, req *http.Request) error { return t.restListBindings(w, req) })
	r.Handle("POST /v3/tagBindings", func(w http.ResponseWriter, req *http.Request) error {
		var b rmpb.TagBinding
		if err := readProto(req, &b); err != nil {
			return err
		}
		op, err := t.bindings.CreateTagBinding(req.Context(), &rmpb.CreateTagBindingRequest{TagBinding: &b, ValidateOnly: req.URL.Query().Get("validateOnly") == "true"})
		if err != nil {
			return err
		}
		return writeProto(w, op)
	})
	r.Handle("DELETE /v3/tagBindings/{binding...}", func(w http.ResponseWriter, req *http.Request) error {
		// The parent inside the name is escaped; read the path as sent.
		raw := strings.TrimPrefix(req.URL.EscapedPath(), "/v3/")
		op, err := t.bindings.DeleteTagBinding(req.Context(), &rmpb.DeleteTagBindingRequest{Name: raw})
		if err != nil {
			return err
		}
		return writeProto(w, op)
	})
	r.Handle("GET /v3/effectiveTags", func(w http.ResponseWriter, req *http.Request) error {
		tok, size, err := pageArgs(req)
		if err != nil {
			return err
		}
		resp, err := t.bindings.ListEffectiveTags(req.Context(), &rmpb.ListEffectiveTagsRequest{Parent: req.URL.Query().Get("parent"), PageToken: tok, PageSize: size})
		if err != nil {
			return err
		}
		return writeProto(w, resp)
	})
}

func writeJSON(w http.ResponseWriter, v any) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	return json.NewEncoder(w).Encode(v)
}

func (t *tagServers) restListLiens(w http.ResponseWriter, req *http.Request) error {
	parent := req.URL.Query().Get("parent")
	if parent == "" {
		return apierror.InvalidArgument("parent is required: projects/{project_number}")
	}
	all, err := t.reg.Liens(parent)
	if err != nil {
		return err
	}
	tok, size, err := pageArgs(req)
	if err != nil {
		return err
	}
	by := map[string]Lien{}
	names := make([]string, 0, len(all))
	for _, l := range all {
		by[l.Name] = l
		names = append(names, l.Name)
	}
	page, next, err := paging.Page("liens.list:"+parent, names, tok, int(size))
	if err != nil {
		return apierror.InvalidArgument("%v", err)
	}
	out := struct {
		Liens         []Lien `json:"liens,omitempty"`
		NextPageToken string `json:"nextPageToken,omitempty"`
	}{NextPageToken: next}
	for _, n := range page {
		out.Liens = append(out.Liens, by[n])
	}
	return writeJSON(w, out)
}

func (t *tagServers) restCreateLien(w http.ResponseWriter, req *http.Request) error {
	var in Lien
	b, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	if err != nil {
		return apierror.InvalidArgument("read body: %v", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return apierror.InvalidArgument("decode lien: %v", err)
	}
	l, err := t.reg.CreateLien(in)
	if err != nil {
		return err
	}
	return writeJSON(w, l)
}

func (t *tagServers) restGetLien(w http.ResponseWriter, req *http.Request) error {
	l, err := t.reg.GetLien("liens/" + req.PathValue("lien"))
	if err != nil {
		return err
	}
	return writeJSON(w, l)
}

func (t *tagServers) restDeleteLien(w http.ResponseWriter, req *http.Request) error {
	if err := t.reg.DeleteLien("liens/" + req.PathValue("lien")); err != nil {
		return err
	}
	return writeJSON(w, struct{}{})
}

// NewTagKeysServer returns the TagKeys service over reg.
func NewTagKeysServer(reg *Registry) *TagKeysServer { return &TagKeysServer{reg: reg} }

// NewTagValuesServer returns the TagValues service over reg.
func NewTagValuesServer(reg *Registry) *TagValuesServer { return &TagValuesServer{reg: reg} }

// NewTagBindingsServer returns the TagBindings service over reg.
func NewTagBindingsServer(reg *Registry) *TagBindingsServer { return &TagBindingsServer{reg: reg} }

// NewTagHoldsServer returns the TagHolds service over reg.
func NewTagHoldsServer(reg *Registry) *TagHoldsServer { return &TagHoldsServer{reg: reg} }

func (t *tagServers) restListBindings(w http.ResponseWriter, req *http.Request) error {
	tok, size, err := pageArgs(req)
	if err != nil {
		return err
	}
	resp, err := t.bindings.ListTagBindings(req.Context(), &rmpb.ListTagBindingsRequest{Parent: req.URL.Query().Get("parent"), PageToken: tok, PageSize: size})
	if err != nil {
		return err
	}
	return writeProto(w, resp)
}
