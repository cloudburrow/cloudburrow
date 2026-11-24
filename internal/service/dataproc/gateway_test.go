package dataproc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cloud.google.com/go/dataproc/v2/apiv1/dataprocpb"
)

// TestKernelGatewaySpecs: the kernel gateway's kernelspecs are the
// region's Jupyter session templates; Spark Connect templates, other
// regions' and unknown kernels are not.
func TestKernelGatewaySpecs(t *testing.T) {
	s, err := NewServer(Options{Engine: &fakeEngine{}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := WithAuthority(context.Background(), "127.0.0.1:9066")
	mk := func(id string, cfg *dataprocpb.SessionTemplate) {
		cfg.Name = parent + "/sessionTemplates/" + id
		cfg.RuntimeConfig = &dataprocpb.RuntimeConfig{Version: "2.2"}
		if _, err := s.CreateSessionTemplate(ctx, &dataprocpb.CreateSessionTemplateRequest{Parent: parent, SessionTemplate: cfg}); err != nil {
			t.Fatal(err)
		}
	}
	mk("py-22", &dataprocpb.SessionTemplate{SessionConfig: &dataprocpb.SessionTemplate_JupyterSession{JupyterSession: &dataprocpb.JupyterConfig{DisplayName: "PySpark 2.2"}}})
	mk("sc-22", &dataprocpb.SessionTemplate{SessionConfig: &dataprocpb.SessionTemplate_JupyterSession{JupyterSession: &dataprocpb.JupyterConfig{Kernel: dataprocpb.JupyterConfig_SCALA}}})
	mk("conn-22", &dataprocpb.SessionTemplate{SessionConfig: &dataprocpb.SessionTemplate_SparkConnectSession{SparkConnectSession: &dataprocpb.SparkConnectConfig{}}})
	p := strings.Split(parent, "/")
	h := s.WebUI(http.NotFoundHandler())
	get := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}
	w := get("/kernels/" + p[1] + "-dot-" + p[3] + "/api/kernelspecs")
	var specs struct {
		Default     string `json:"default"`
		Kernelspecs map[string]struct {
			Spec struct {
				DisplayName string `json:"display_name"`
				Language    string `json:"language"`
			} `json:"spec"`
		} `json:"kernelspecs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &specs); err != nil || w.Code != http.StatusOK {
		t.Fatalf("kernelspecs: %d %s", w.Code, w.Body)
	}
	if len(specs.Kernelspecs) != 2 || specs.Kernelspecs["py-22"].Spec.DisplayName != "PySpark 2.2" || specs.Kernelspecs["py-22"].Spec.Language != "python" ||
		specs.Kernelspecs["sc-22"].Spec.DisplayName != "sc-22" || specs.Kernelspecs["sc-22"].Spec.Language != "scala" || specs.Default != "py-22" {
		t.Errorf("kernelspecs: %+v", specs)
	}
	if w := get("/kernels/" + p[1] + "-dot-europe-west1/api/kernelspecs"); !strings.Contains(w.Body.String(), `"kernelspecs":{}`) {
		t.Errorf("another region's kernelspecs: %s", w.Body)
	}
	if w := get("/kernels/" + p[1] + "-dot-" + p[3] + "/api/kernels/nope"); w.Code != http.StatusNotFound {
		t.Errorf("an unknown kernel: %d %s", w.Code, w.Body)
	}
	if w := get("/kernels/" + p[1] + "-dot-" + p[3] + "/api/kernels"); w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Errorf("kernels: %d %s", w.Code, w.Body)
	}
	pw := httptest.NewRecorder()
	h.ServeHTTP(pw, httptest.NewRequest(http.MethodPost, "/kernels/"+p[1]+"-dot-"+p[3]+"/api/kernels", strings.NewReader(`{"name":"conn-22"}`)))
	if pw.Code != http.StatusNotFound {
		t.Errorf("a Spark Connect template's kernel: %d %s", pw.Code, pw.Body)
	}
}
