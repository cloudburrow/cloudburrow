//go:build compat

package compat

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	pmpb "cloud.google.com/go/parametermanager/apiv1/parametermanagerpb"
	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	gaxerr "github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// covers: google.cloud.parametermanager.v1.ParameterManager/CreateTemplate, google.cloud.parametermanager.v1.ParameterManager/GetTemplate, google.cloud.parametermanager.v1.ParameterManager/ListTemplates, google.cloud.parametermanager.v1.ParameterManager/UpdateTemplate, google.cloud.parametermanager.v1.ParameterManager/DeleteTemplate, google.cloud.parametermanager.v1.ParameterManager/CreateTemplateVersion, google.cloud.parametermanager.v1.ParameterManager/GetTemplateVersion, google.cloud.parametermanager.v1.ParameterManager/ListTemplateVersions, google.cloud.parametermanager.v1.ParameterManager/UpdateTemplateVersion, google.cloud.parametermanager.v1.ParameterManager/DeleteTemplateVersion, google.cloud.parametermanager.v1.ParameterManager/RenderTemplateVersion
//
// TestParameterManagerTemplates (#2063), with the official client over gRPC
// and over REST, global and regional: a template (YAML by default, its
// format immutable, its labels updated), a version whose {{.key}} and
// {{.key1.key2}} variables render with a YAML parameter version's values,
// its secret reference resolved first; control flow, a __REF__ in the
// template, an array and a missing key are refused; FULL and BASIC views;
// a disabled version answers no payload and cannot be rendered; a template
// with versions cannot be deleted, and can once they are.
func TestParameterManagerTemplates(t *testing.T) {
	h := New(t)
	clients := parameterManagerClients(t, h)
	sm := secretsClient(t, h)
	ctx := h.Context()
	secretVersion := createSecretWithVersion(t, h, sm, "projects/"+h.Project(), fmt.Sprintf("pm-tpl-secret-%d", time.Now().UnixNano()%1e9), "s3cret")
	for transport, c := range clients {
		for _, loc := range []string{"global", "us-central1"} {
			t.Run(transport+"/"+loc, func(t *testing.T) {
				parent := "projects/" + h.Project() + "/locations/" + loc
				id := fmt.Sprintf("tpl-%s-%d", transport, time.Now().UnixNano()%1e9)

				p, err := c.CreateParameter(ctx, &pmpb.CreateParameterRequest{Parent: parent, ParameterId: id + "-vals",
					Parameter: &pmpb.Parameter{Format: pmpb.ParameterFormat_YAML}})
				if err != nil {
					t.Fatal(err)
				}
				grantSecretAccessor(t, sm, "projects/"+h.Project()+"/secrets/"+strings.Split(secretVersion, "/")[3], p)
				pv := p.GetName() + "/versions/v1"
				values := "log_level: debug\ndb:\n  port: 5432\n  password: __REF__(\"//secretmanager.googleapis.com/" + secretVersion + "\")\nhosts: [a, b]\n"
				if _, err := c.CreateParameterVersion(ctx, &pmpb.CreateParameterVersionRequest{Parent: p.GetName(), ParameterVersionId: "v1",
					ParameterVersion: &pmpb.ParameterVersion{Payload: &pmpb.ParameterVersionPayload{Data: []byte(values)}}}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					_ = c.DeleteParameterVersion(context.Background(), &pmpb.DeleteParameterVersionRequest{Name: pv})
					_ = c.DeleteParameter(context.Background(), &pmpb.DeleteParameterRequest{Name: p.GetName()})
				})

				tpl, err := c.CreateTemplate(ctx, &pmpb.CreateTemplateRequest{Parent: parent, TemplateId: id,
					Template: &pmpb.Template{Labels: map[string]string{"team": "burrow"}}})
				if err != nil {
					t.Fatalf("CreateTemplate: %v", err)
				}
				name := parent + "/templates/" + id
				if tpl.GetName() != name || tpl.GetFormat() != pmpb.TemplateFormat_TEMPLATE_FORMAT_YAML || tpl.GetCreateTime() == nil {
					t.Fatalf("created %+v; want YAML, the default", tpl)
				}
				_, err = c.CreateTemplate(ctx, &pmpb.CreateTemplateRequest{Parent: parent, TemplateId: id, Template: &pmpb.Template{}})
				pmWantCode(t, "a second CreateTemplate", err, codes.AlreadyExists)
				if got, err := c.GetTemplate(ctx, &pmpb.GetTemplateRequest{Name: name}); err != nil || got.GetLabels()["team"] != "burrow" {
					t.Fatalf("GetTemplate = %+v, %v", got, err)
				}
				found := false
				for it := c.ListTemplates(ctx, &pmpb.ListTemplatesRequest{Parent: parent}); ; {
					x, err := it.Next()
					if err == iterator.Done {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					found = found || x.GetName() == name
				}
				if !found {
					t.Errorf("ListTemplates did not list %s", name)
				}
				up, err := c.UpdateTemplate(ctx, &pmpb.UpdateTemplateRequest{Template: &pmpb.Template{Name: name, Labels: map[string]string{"team": "core"}},
					UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"labels"}}})
				if err != nil || up.GetLabels()["team"] != "core" {
					t.Fatalf("UpdateTemplate labels = %+v, %v", up, err)
				}
				_, err = c.UpdateTemplate(ctx, &pmpb.UpdateTemplateRequest{Template: &pmpb.Template{Name: name, Format: pmpb.TemplateFormat_TEMPLATE_FORMAT_JSON},
					UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"format"}}})
				pmWantCode(t, "changing a template's format", err, codes.InvalidArgument)

				for _, bad := range []string{"a: {{if .log_level}}x{{end}}\n", "a: {{range .hosts}}\n",
					"a: __REF__(\"//secretmanager.googleapis.com/" + secretVersion + "\")\n", "- not an object\n"} {
					_, err = c.CreateTemplateVersion(ctx, &pmpb.CreateTemplateVersionRequest{Parent: name, TemplateVersionId: "bad",
						TemplateVersion: &pmpb.TemplateVersion{Payload: &pmpb.TemplateVersionPayload{Data: []byte(bad)}}})
					pmWantCode(t, fmt.Sprintf("a template version %q", bad), err, codes.InvalidArgument)
				}
				payload := "appName: forex\nlogLevel: {{.log_level}}\nport: {{.db.port}}\npassword: {{.db.password}}\n"
				tv, err := c.CreateTemplateVersion(ctx, &pmpb.CreateTemplateVersionRequest{Parent: name, TemplateVersionId: "tv1",
					TemplateVersion: &pmpb.TemplateVersion{Payload: &pmpb.TemplateVersionPayload{Data: []byte(payload)}}})
				if err != nil || tv.GetName() != name+"/versions/tv1" {
					t.Fatalf("CreateTemplateVersion = %+v, %v", tv, err)
				}
				for _, bad := range []string{"hosts: {{.hosts}}\n", "x: {{.nope}}\n", "db: {{.db}}\n"} {
					vid := fmt.Sprintf("b%d", len(bad))
					if _, err := c.CreateTemplateVersion(ctx, &pmpb.CreateTemplateVersionRequest{Parent: name, TemplateVersionId: vid,
						TemplateVersion: &pmpb.TemplateVersion{Payload: &pmpb.TemplateVersionPayload{Data: []byte(bad)}}}); err != nil {
						t.Fatal(err)
					}
					_, err := c.RenderTemplateVersion(ctx, &pmpb.RenderTemplateVersionRequest{Name: name + "/versions/" + vid, ParameterVersion: pv})
					pmWantCode(t, fmt.Sprintf("rendering %q", bad), err, codes.InvalidArgument)
					if err := c.DeleteTemplateVersion(ctx, &pmpb.DeleteTemplateVersionRequest{Name: name + "/versions/" + vid}); err != nil {
						t.Fatal(err)
					}
				}

				full, err := c.GetTemplateVersion(ctx, &pmpb.GetTemplateVersionRequest{Name: tv.GetName()})
				if err != nil || string(full.GetPayload().GetData()) != payload {
					t.Fatalf("GetTemplateVersion FULL = %+v, %v", full, err)
				}
				basic, err := c.GetTemplateVersion(ctx, &pmpb.GetTemplateVersionRequest{Name: tv.GetName(), View: pmpb.View_BASIC})
				if err != nil || basic.GetPayload() != nil {
					t.Fatalf("GetTemplateVersion BASIC = %+v, %v; want no payload", basic, err)
				}
				n := 0
				for it := c.ListTemplateVersions(ctx, &pmpb.ListTemplateVersionsRequest{Parent: name}); ; {
					x, err := it.Next()
					if err == iterator.Done {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					if x.GetPayload() != nil {
						t.Errorf("ListTemplateVersions carried a payload")
					}
					n++
				}
				if n != 1 {
					t.Errorf("ListTemplateVersions listed %d; want 1", n)
				}

				r, err := c.RenderTemplateVersion(ctx, &pmpb.RenderTemplateVersionRequest{Name: tv.GetName(), ParameterVersion: pv})
				if err != nil {
					t.Fatalf("RenderTemplateVersion: %v", err)
				}
				want := "appName: forex\nlogLevel: debug\nport: 5432\npassword: s3cret\n"
				if string(r.GetRenderedPayload()) != want || string(r.GetPayload().GetData()) != payload || r.GetTemplateVersion() != tv.GetName() ||
					r.GetParameterVersion() != pv || r.GetTemplateFormat() != pmpb.TemplateFormat_TEMPLATE_FORMAT_YAML {
					t.Errorf("RenderTemplateVersion = %q (%+v); want %q", r.GetRenderedPayload(), r, want)
				}

				dis, err := c.UpdateTemplateVersion(ctx, &pmpb.UpdateTemplateVersionRequest{TemplateVersion: &pmpb.TemplateVersion{Name: tv.GetName(), Disabled: true},
					UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"disabled"}}})
				if err != nil || !dis.GetDisabled() {
					t.Fatalf("disabling = %+v, %v", dis, err)
				}
				if got, err := c.GetTemplateVersion(ctx, &pmpb.GetTemplateVersionRequest{Name: tv.GetName()}); err != nil || got.GetPayload() != nil {
					t.Errorf("a disabled version = %+v, %v; want no payload", got, err)
				}
				_, err = c.RenderTemplateVersion(ctx, &pmpb.RenderTemplateVersionRequest{Name: tv.GetName(), ParameterVersion: pv})
				pmWantCode(t, "rendering a disabled version", err, codes.FailedPrecondition)

				err = c.DeleteTemplate(ctx, &pmpb.DeleteTemplateRequest{Name: name})
				pmWantCode(t, "deleting a template with versions", err, codes.FailedPrecondition)
				if err := c.DeleteTemplateVersion(ctx, &pmpb.DeleteTemplateVersionRequest{Name: tv.GetName()}); err != nil {
					t.Fatal(err)
				}
				if err := c.DeleteTemplate(ctx, &pmpb.DeleteTemplateRequest{Name: name}); err != nil {
					t.Fatal(err)
				}
				_, err = c.GetTemplate(ctx, &pmpb.GetTemplateRequest{Name: name})
				pmWantCode(t, "GetTemplate after delete", err, codes.NotFound)
			})
		}
	}
}

// covers: google.cloud.parametermanager.v1.ParameterManager/CreateParameter, google.cloud.parametermanager.v1.ParameterManager/GetParameter, google.cloud.parametermanager.v1.ParameterManager/DeleteParameter, google.cloud.resourcemanager.v3.TagBindings/ListTagBindings, google.cloud.resourcemanager.v3.TagBindings/ListEffectiveTags
//
// TestParameterManagerTags (#2063), through the official parametermanager
// and resourcemanager/apiv3 clients: CreateParameter with tags (a key by
// its namespaced name with a value's short name, a key by its permanent
// ID with a value's ID), global and regional, binds them to the
// parameter's full resource name,
// //parametermanager.googleapis.com/projects/PROJECT/locations/LOCATION/parameters/ID,
// where ListTagBindings and ListEffectiveTags find them; GetParameter
// returns no tags (input only). An unknown key is INVALID_ARGUMENT with no
// parameter stored, and DeleteParameter removes the bindings.
func TestParameterManagerTags(t *testing.T) {
	h := New(t)
	c := parameterManagerClients(t, h)["grpc"]
	tc := newTagClients(t, h)
	ctx := h.Context()
	mkKey := func(short string) *rmpb.TagKey {
		op, err := tc.keys.CreateTagKey(ctx, &rmpb.CreateTagKeyRequest{TagKey: &rmpb.TagKey{Parent: localOrg, ShortName: short}})
		if err != nil {
			t.Fatal(err)
		}
		k, err := op.Wait(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = tc.keys.DeleteTagKey(context.Background(), &rmpb.DeleteTagKeyRequest{Name: k.GetName()})
		})
		return k
	}
	mkValue := func(k *rmpb.TagKey, short string) *rmpb.TagValue {
		op, err := tc.values.CreateTagValue(ctx, &rmpb.CreateTagValueRequest{TagValue: &rmpb.TagValue{Parent: k.GetName(), ShortName: short}})
		if err != nil {
			t.Fatal(err)
		}
		v, err := op.Wait(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = tc.values.DeleteTagValue(context.Background(), &rmpb.DeleteTagValueRequest{Name: v.GetName()})
		})
		return v
	}
	env := mkKey(v1ID(h, "penv-"))
	prod := mkValue(env, "prod")
	team := mkKey(v1ID(h, "pteam-"))
	core := mkValue(team, "core")

	for _, loc := range []string{"global", "us-central1"} {
		parent := "projects/" + h.Project() + "/locations/" + loc
		id := fmt.Sprintf("tagged-%d", time.Now().UnixNano()%1e9)
		name := parent + "/parameters/" + id
		full := "//parametermanager.googleapis.com/" + name
		bound := func() map[string]bool {
			out := map[string]bool{}
			for it := tc.bindings.ListTagBindings(ctx, &rmpb.ListTagBindingsRequest{Parent: full}); ; {
				b, err := it.Next()
				if err == iterator.Done {
					return out
				}
				if err != nil {
					t.Fatalf("ListTagBindings(%s): %v", full, err)
				}
				out[b.GetTagValue()] = true
			}
		}
		_, err := c.CreateParameter(ctx, &pmpb.CreateParameterRequest{Parent: parent, ParameterId: id,
			Parameter: &pmpb.Parameter{Tags: map[string]string{"tagKeys/999999999": "tagValues/999999999"}}})
		pmWantCode(t, "CreateParameter with an unknown tag key", err, codes.InvalidArgument)
		_, err = c.GetParameter(ctx, &pmpb.GetParameterRequest{Name: name})
		pmWantCode(t, "the parameter refused for its tags", err, codes.NotFound)

		created, err := c.CreateParameter(ctx, &pmpb.CreateParameterRequest{Parent: parent, ParameterId: id,
			Parameter: &pmpb.Parameter{Tags: map[string]string{env.GetNamespacedName(): "prod", team.GetName(): core.GetName()}}})
		if err != nil {
			t.Fatalf("CreateParameter with tags: %v", err)
		}
		t.Cleanup(func() { _ = c.DeleteParameter(context.Background(), &pmpb.DeleteParameterRequest{Name: name}) })
		if len(created.GetTags()) != 0 {
			t.Errorf("CreateParameter returned tags %v; the field is input only", created.GetTags())
		}
		if got := bound(); !got[prod.GetName()] || !got[core.GetName()] || len(got) != 2 {
			t.Errorf("ListTagBindings(%s) = %v; want %s and %s", full, got, prod.GetName(), core.GetName())
		}
		eff := map[string]bool{}
		for it := tc.bindings.ListEffectiveTags(ctx, &rmpb.ListEffectiveTagsRequest{Parent: full}); ; {
			e, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Fatalf("ListEffectiveTags: %v", err)
			}
			if !e.GetInherited() {
				eff[e.GetNamespacedTagValue()] = true
			}
		}
		if !eff[prod.GetNamespacedName()] || !eff[core.GetNamespacedName()] {
			t.Errorf("ListEffectiveTags(%s) = %v", full, eff)
		}
		if got, err := c.GetParameter(ctx, &pmpb.GetParameterRequest{Name: name}); err != nil || len(got.GetTags()) != 0 {
			t.Errorf("GetParameter = tags %v, %v; want none (input only)", got.GetTags(), err)
		}
		if err := c.DeleteParameter(ctx, &pmpb.DeleteParameterRequest{Name: name}); err != nil {
			t.Fatal(err)
		}
		if got := bound(); len(got) != 0 {
			t.Errorf("after DeleteParameter the bindings are %v; want none", got)
		}
	}
}

// TestParameterManagerRequestQuotas (#2063): with the read quota lowered to
// 2 per minute for a project of the test's own (PUT /admin/quotas), the
// official client's third read in the minute, over gRPC and over REST,
// is refused RESOURCE_EXHAUSTED (HTTP 429) with Google's text and a
// RATE_LIMIT_EXCEEDED ErrorInfo naming the metric and the limit; the
// project's writes are another quota and still pass. GET /admin/quotas
// lists Google's defaults, 1,200 reads, 1,200 writes and 1,800 renders.
func TestParameterManagerRequestQuotas(t *testing.T) {
	h := New(t)
	clients := parameterManagerClients(t, h)
	control := h.Endpoint(EnvControl)
	if adminToken(t) == "" {
		t.Skipf("%s is not set and no instance directory is known; /admin/quotas needs the token", EnvAdminToken)
	}
	ctx := h.Context()
	code, body := adminDo(t, http.MethodGet, "http://"+control+"/admin/quotas", "", nil)
	for _, want := range []string{`"metric":"parametermanager.googleapis.com/read_requests"`, `"default":1200`, `"default":1800`} {
		if code != http.StatusOK || !strings.Contains(body, want) {
			t.Fatalf("GET /admin/quotas = %d %s; want %s", code, body, want)
		}
	}
	for transport, c := range clients {
		t.Run(transport, func(t *testing.T) {
			project := fmt.Sprintf("pm-quota-%s-%d", transport, time.Now().UnixNano()%1e6)
			const metric = "parametermanager.googleapis.com/read_requests"
			rule := `{"metric":"` + metric + `","project":"` + project + `","limit":2}`
			if code, body := adminDo(t, http.MethodPut, "http://"+control+"/admin/quotas", "application/json", strings.NewReader(rule)); code != http.StatusOK {
				t.Fatalf("PUT /admin/quotas = %d %s", code, body)
			}
			t.Cleanup(func() {
				adminDo(t, http.MethodDelete, "http://"+control+"/admin/quotas?metric="+url.QueryEscape(metric)+"&project="+project, "", nil)
			})
			// Not across a minute's end, where the quota refills.
			if s := time.Now().Second(); s > 50 {
				time.Sleep(time.Duration(61-s) * time.Second)
			}
			parent := "projects/" + project + "/locations/global"
			for i := 0; i < 2; i++ {
				if _, err := c.ListParameters(ctx, &pmpb.ListParametersRequest{Parent: parent}).Next(); err != nil && err != iterator.Done {
					t.Fatalf("read %d: %v", i+1, err)
				}
			}
			_, err := c.GetParameter(ctx, &pmpb.GetParameterRequest{Name: parent + "/parameters/x"})
			// The REST client's error is already an APIError, which
			// FromError would parse again without its details.
			var ae *gaxerr.APIError
			ok := errors.As(err, &ae)
			if !ok {
				ae, ok = gaxerr.FromError(err)
			}
			if !ok || pmCode(err) != codes.ResourceExhausted {
				t.Fatalf("the third read = %v; want RESOURCE_EXHAUSTED", err)
			}
			if transport == "rest" && ae.HTTPCode() != http.StatusTooManyRequests {
				t.Errorf("HTTP status %d; want 429", ae.HTTPCode())
			}
			if !strings.Contains(err.Error(), "Quota exceeded for quota metric 'Read requests' and limit 'Read requests per minute per project' of service 'parametermanager.googleapis.com' for consumer 'project_number:") {
				t.Errorf("message = %v", err)
			}
			if ae.Reason() != "RATE_LIMIT_EXCEEDED" || ae.Domain() != "googleapis.com" || ae.Metadata()["quota_metric"] != metric ||
				ae.Metadata()["quota_limit"] != "ReadRequestsPerMinutePerProject" || ae.Metadata()["quota_limit_value"] != "2" {
				t.Errorf("ErrorInfo = %s %s %v", ae.Reason(), ae.Domain(), ae.Metadata())
			}
			if _, err := c.CreateParameter(ctx, &pmpb.CreateParameterRequest{Parent: parent, ParameterId: "w"}); err != nil {
				t.Errorf("a write in the project (another quota) = %v", err)
			}
			_ = c.DeleteParameter(ctx, &pmpb.DeleteParameterRequest{Name: parent + "/parameters/w"})
		})
	}
}
