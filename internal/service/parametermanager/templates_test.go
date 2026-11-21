package parametermanager

import (
	"context"
	"strings"
	"testing"

	pmpb "cloud.google.com/go/parametermanager/apiv1/parametermanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/ratequota"
)

func newTestServer(t *testing.T, opts Options) *Server {
	t.Helper()
	s, err := NewServer("", opts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRenderTemplateVersion(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t, Options{})
	parent := "projects/p/locations/global"
	if _, err := s.CreateParameter(ctx, &pmpb.CreateParameterRequest{Parent: parent, ParameterId: "vals",
		Parameter: &pmpb.Parameter{Format: pmpb.ParameterFormat_JSON}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateParameterVersion(ctx, &pmpb.CreateParameterVersionRequest{Parent: parent + "/parameters/vals", ParameterVersionId: "v1",
		ParameterVersion: &pmpb.ParameterVersion{Payload: &pmpb.ParameterVersionPayload{Data: []byte(`{"log_level":"debug","db":{"port":5432,"host":"a\"b"},"list":[1]}`)}}}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		format   pmpb.TemplateFormat
		payload  string
		rendered string
		code     codes.Code
	}{
		{pmpb.TemplateFormat_TEMPLATE_FORMAT_YAML, "appName: forex\nlogLevel: {{.log_level}}\nport: {{ .db.port }}\n", "appName: forex\nlogLevel: debug\nport: 5432\n", codes.OK},
		{pmpb.TemplateFormat_TEMPLATE_FORMAT_JSON, `{"host": "{{.db.host}}", "port": {{.db.port}}}`, `{"host": "a\"b", "port": 5432}`, codes.OK},
		{pmpb.TemplateFormat_TEMPLATE_FORMAT_YAML, "x: {{.missing}}\n", "", codes.InvalidArgument},
		{pmpb.TemplateFormat_TEMPLATE_FORMAT_YAML, "x: {{.db}}\n", "", codes.InvalidArgument},
		{pmpb.TemplateFormat_TEMPLATE_FORMAT_YAML, "x: {{.list}}\n", "", codes.InvalidArgument},
	} {
		id := "t" + strings.ToLower(tc.format.String()[16:]) + string(rune('a'+len(s.templates)))
		if _, err := s.CreateTemplate(ctx, &pmpb.CreateTemplateRequest{Parent: parent, TemplateId: id, Template: &pmpb.Template{Format: tc.format}}); err != nil {
			t.Fatal(err)
		}
		tname := parent + "/templates/" + id
		if _, err := s.CreateTemplateVersion(ctx, &pmpb.CreateTemplateVersionRequest{Parent: tname, TemplateVersionId: "tv1",
			TemplateVersion: &pmpb.TemplateVersion{Payload: &pmpb.TemplateVersionPayload{Data: []byte(tc.payload)}}}); err != nil {
			t.Fatalf("CreateTemplateVersion %q: %v", tc.payload, err)
		}
		resp, err := s.RenderTemplateVersion(ctx, &pmpb.RenderTemplateVersionRequest{Name: tname + "/versions/tv1", ParameterVersion: parent + "/parameters/vals/versions/v1"})
		if status.Code(err) != tc.code {
			t.Fatalf("render %q = %v; want %v", tc.payload, err, tc.code)
		}
		if err == nil && (string(resp.GetRenderedPayload()) != tc.rendered || resp.GetTemplateFormat() != tc.format || string(resp.GetPayload().GetData()) != tc.payload) {
			t.Errorf("render %q = %q (%v)", tc.payload, resp.GetRenderedPayload(), resp.GetTemplateFormat())
		}
	}
}

func TestTemplateVersionLimits(t *testing.T) {
	ctx := context.Background()
	s := newTestServer(t, Options{})
	parent := "projects/p/locations/us-central1"
	if _, err := s.CreateTemplate(ctx, &pmpb.CreateTemplateRequest{Parent: parent, TemplateId: "t"}); err != nil {
		t.Fatal(err)
	}
	var many strings.Builder
	for i := 0; i < 65; i++ {
		many.WriteString("k" + strings.Repeat("x", i) + ": {{.v}}\n")
	}
	for _, bad := range []string{
		"{{if .x}}a: 1{{end}}\n",
		"a: {{range .x}}\n",
		"- 1\n- 2\n",
		`a: __REF__("//secretmanager.googleapis.com/projects/p/secrets/s/versions/1")` + "\n",
		many.String(),
	} {
		_, err := s.CreateTemplateVersion(ctx, &pmpb.CreateTemplateVersionRequest{Parent: parent + "/templates/t", TemplateVersionId: "v",
			TemplateVersion: &pmpb.TemplateVersion{Payload: &pmpb.TemplateVersionPayload{Data: []byte(bad)}}})
		if status.Code(err) != codes.InvalidArgument {
			t.Errorf("CreateTemplateVersion %.40q = %v; want INVALID_ARGUMENT", bad, err)
		}
	}
	if _, err := s.CreateTemplateVersion(ctx, &pmpb.CreateTemplateVersionRequest{Parent: parent + "/templates/t", TemplateVersionId: "-v",
		TemplateVersion: &pmpb.TemplateVersion{Payload: &pmpb.TemplateVersionPayload{Data: []byte("a: 1\n")}}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("a version ID beginning with a dash = %v", err)
	}
	if _, err := s.CreateTemplate(ctx, &pmpb.CreateTemplateRequest{Parent: parent, TemplateId: "u", Template: &pmpb.Template{Format: 7}}); status.Code(err) != codes.InvalidArgument {
		t.Errorf("format 7 = %v", err)
	}
}

func TestRequestQuotas(t *testing.T) {
	ctx := context.Background()
	l := ratequota.New()
	s := newTestServer(t, Options{Quotas: l})
	if err := l.Set(quotaRead, "p", 2); err != nil {
		t.Fatal(err)
	}
	req := &pmpb.ListParametersRequest{Parent: "projects/p/locations/global"}
	for i := 0; i < 2; i++ {
		if _, err := s.ListParameters(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.ListParameters(ctx, req)
	if status.Code(err) != codes.ResourceExhausted || !strings.Contains(status.Convert(err).Message(), "Quota exceeded for quota metric 'Read requests' and limit 'Read requests per minute per project' of service 'parametermanager.googleapis.com'") {
		t.Fatalf("third read = %v", err)
	}
	// Another project, and another family, are not charged.
	if _, err := s.ListParameters(ctx, &pmpb.ListParametersRequest{Parent: "projects/q/locations/global"}); err != nil {
		t.Errorf("another project: %v", err)
	}
	if _, err := s.CreateTemplate(ctx, &pmpb.CreateTemplateRequest{Parent: "projects/p/locations/global", TemplateId: "t"}); err != nil {
		t.Errorf("a write: %v", err)
	}
}
