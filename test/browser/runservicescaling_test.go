//go:build browser

package browser

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	run "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"github.com/chromedp/chromedp"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestCloudRunServiceScalingThroughTheForms (#1800), in the run shard: the
// Deploy container page offers service-level scaling (Automatic with a
// service minimum and maximum, Manual with a number of instances) and the
// revision's CPU and concurrency utilization targets. A service deployed
// with Automatic 1-2 and a concurrency target of 0.7 reads back through the
// official ServicesClient with that ServiceScaling and RevisionScaling; Edit
// and deploy new revision opens prefilled with them, and switching it to
// Manual with 1 instance reads back as MANUAL 1.
func TestCloudRunServiceScalingThroughTheForms(t *testing.T) {
	needService(t, "run")
	addr := strings.TrimPrefix(strings.TrimSpace(os.Getenv(envRun)), "http://")
	if addr == "" {
		t.Skipf("%s is not set: the service is read back through the Cloud Run API itself", envRun)
	}
	ctx := context.Background()
	c, err := run.NewServicesClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	project := uniqueProject(t)
	id := fmt.Sprintf("browser-scaling-%d", time.Now().UnixNano()%1e6)
	name := "projects/" + project + "/locations/us-central1/services/" + id
	t.Cleanup(func() {
		consoleDo(t, http.MethodDelete, "/api/resources/run?project="+project+"&name="+id, "")
	})
	// get waits for the service to settle with what the form asked for.
	get := func(ok func(*runpb.Service) bool) *runpb.Service {
		t.Helper()
		var last *runpb.Service
		for deadline := time.Now().Add(4 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
			svc, err := c.GetService(ctx, &runpb.GetServiceRequest{Name: name})
			if err == nil && !svc.GetReconciling() && ok(svc) {
				return svc
			}
			last = svc
		}
		t.Fatalf("the service never read back as the form asked: scaling %v, template scaling %v",
			last.GetScaling(), last.GetTemplate().GetScaling())
		return nil
	}

	p := open(t)
	p.navigate("/run/create?project=" + project)
	p.waitFor(`["f-serviceScalingMode", "f-serviceMinInstances", "f-serviceMaxInstances", "f-manualInstances",
		"f-cpuUtilization", "f-concurrencyUtilization"].every((id) => document.querySelector("#view #" + id) !== null)`)
	p.run(chromedp.SendKeys(chromedp.CSS(`#view #f-name`), id))
	setField(p, "#view", "f-serviceScalingMode", "AUTOMATIC")
	setField(p, "#view", "f-serviceMinInstances", "1")
	setField(p, "#view", "f-serviceMaxInstances", "2")
	setField(p, "#view", "f-concurrencyUtilization", "0.7")
	p.clickText(`#view button[type="submit"]`, "Create")

	svc := get(func(s *runpb.Service) bool { return s.GetScaling() != nil })
	if sc := svc.GetScaling(); sc.GetScalingMode() != runpb.ServiceScaling_AUTOMATIC ||
		sc.GetMinInstanceCount() != 1 || sc.GetMaxInstanceCount() != 2 {
		t.Errorf("the deployed service's scaling = %v, want AUTOMATIC 1-2", sc)
	}
	if ts := svc.GetTemplate().GetScaling(); ts.ConcurrencyUtilization == nil ||
		fmt.Sprintf("%.2f", ts.GetConcurrencyUtilization()) != "0.70" || ts.CpuUtilization != nil {
		t.Errorf("the deployed template's scaling = %v, want concurrencyUtilization 0.7 and no CPU target", ts)
	}

	p.navigate("/run/" + id + "?project=" + project)
	p.clickText("#view .page-actions button", "Edit and deploy new revision")
	p.waitFor(`document.querySelector(".modal.is-open #f-serviceScalingMode") !== null`)
	var form struct{ Mode, Min, Max, Manual, Concurrency string }
	p.eval(`(() => { const q = (s) => document.querySelector(".modal.is-open " + s).value;
		return { Mode: q("#f-serviceScalingMode"), Min: q("#f-serviceMinInstances"), Max: q("#f-serviceMaxInstances"),
		         Manual: q("#f-manualInstances"), Concurrency: q("#f-concurrencyUtilization") }; })()`, &form)
	if form.Mode != "AUTOMATIC" || form.Min != "1" || form.Max != "2" || form.Manual != "" || form.Concurrency != "0.7" {
		t.Errorf("Edit is prefilled with %+v, want AUTOMATIC 1-2 and concurrency 0.7", form)
	}

	setField(p, ".modal.is-open", "f-serviceScalingMode", "MANUAL")
	setField(p, ".modal.is-open", "f-serviceMinInstances", "")
	setField(p, ".modal.is-open", "f-serviceMaxInstances", "")
	setField(p, ".modal.is-open", "f-manualInstances", "1")
	p.run(chromedp.Click(chromedp.CSS(`.modal.is-open button[type="submit"]`)))
	svc = get(func(s *runpb.Service) bool { return s.GetScaling().GetScalingMode() == runpb.ServiceScaling_MANUAL })
	if sc := svc.GetScaling(); sc.GetManualInstanceCount() != 1 || sc.GetMinInstanceCount() != 0 || sc.GetMaxInstanceCount() != 0 {
		t.Errorf("the edited service's scaling = %v, want MANUAL 1", sc)
	}
}
