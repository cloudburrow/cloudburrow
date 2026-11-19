package run

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	longrunningpb "cloud.google.com/go/longrunning/autogen/longrunningpb"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"github.com/cloudburrow/cloudburrow/internal/apierror"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// A build's Deploy step (#1852): the step Cloud Run's "Connect repo"
// trigger ends with, as Google's "Continuous deployment from git using
// Cloud Build" page shows it,
//
//	name: gcr.io/google.com/cloudsdktool/cloud-sdk:slim
//	entrypoint: gcloud
//	args: [run, services, update, SERVICE, --platform=managed,
//	       --image=IMAGE, --labels=K=V,..., --region=REGION, --quiet]
//
// runs here as the docker build and push steps do: the build itself
// performs it, as Cloud Run's UpdateService on this instance, so the step
// reaches no endpoint but this instance's and needs no gcloud image. Any
// other gcloud command in that image runs as an arbitrary container step,
// which the host's Docker runs (#1382).

// gcloudImages are Google's Cloud SDK images a Deploy step names.
var gcloudImages = map[string]bool{
	"gcr.io/google.com/cloudsdktool/cloud-sdk":               true,
	"gcr.io/google.com/cloudsdktool/cloud-sdk:slim":          true,
	"gcr.io/google.com/cloudsdktool/cloud-sdk:latest":        true,
	"gcr.io/google.com/cloudsdktool/google-cloud-cli":        true,
	"gcr.io/google.com/cloudsdktool/google-cloud-cli:slim":   true,
	"gcr.io/google.com/cloudsdktool/google-cloud-cli:latest": true,
	"gcr.io/cloud-builders/gcloud":                           true,
	"gcr.io/cloud-builders/gcloud:latest":                    true,
}

// gcloudRunStep is a Deploy step's `gcloud run services update`.
type gcloudRunStep struct {
	service, image, region, project string
	labels                          map[string]string
}

// planGcloudRun reads a step as `gcloud run services update`, or reports
// that it is not one.
func planGcloudRun(name string, st interface {
	GetEntrypoint() string
	GetScript() string
}, args []string) (*gcloudRunStep, bool, error) {
	if !gcloudImages[name] || st.GetScript() != "" {
		return nil, false, nil
	}
	// gcr.io/cloud-builders/gcloud's entrypoint is gcloud; the Cloud SDK
	// images' is the shell, so the step names gcloud.
	if ep := st.GetEntrypoint(); ep != "gcloud" && !(ep == "" && strings.HasPrefix(name, "gcr.io/cloud-builders/gcloud")) {
		return nil, false, nil
	}
	if len(args) < 4 || args[0] != "run" || args[1] != "services" || args[2] != "update" || strings.HasPrefix(args[3], "-") {
		return nil, false, nil
	}
	g := &gcloudRunStep{service: args[3], labels: map[string]string{}}
	for j := 4; j < len(args); j++ {
		flag, val, hasVal := strings.Cut(args[j], "=")
		if !hasVal && j+1 < len(args) && !strings.HasPrefix(args[j+1], "-") && flag != "--quiet" && flag != "-q" {
			j++
			val, hasVal = args[j], true
		}
		switch flag {
		case "--image":
			g.image = val
		case "--region":
			g.region = val
		case "--project":
			g.project = val
		case "--platform":
			if val != "managed" {
				return nil, true, fmt.Errorf("--platform=%s is not supported: Cloud Run here is the managed platform", val)
			}
		case "--labels", "--update-labels":
			for _, kv := range strings.Split(val, ",") {
				if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
					g.labels[k] = v
				}
			}
		case "--quiet", "-q":
		default:
			return nil, true, fmt.Errorf("gcloud run services update flag %s is not supported in a build step here", flag)
		}
		_ = hasVal
	}
	if g.image == "" {
		return nil, true, fmt.Errorf("gcloud run services update needs --image")
	}
	return g, true, nil
}

// gcloudRun performs a Deploy step: the service's container image and
// labels are replaced, and the step ends when the new revision is ready.
func (r *buildRun) gcloudRun(ctx context.Context, g *gcloudRunStep, project, location string, w io.Writer) error {
	if r.c.builds == nil || r.c.builds.s == nil {
		return fmt.Errorf("Cloud Run is not served with this instance's builds")
	}
	s := r.c.builds.s
	if g.project != "" {
		project = g.project
	}
	region := g.region
	if region == "" {
		region = location
	}
	if region == "" || region == "global" {
		return fmt.Errorf("gcloud run services update needs --region")
	}
	name := "projects/" + project + "/locations/" + region + "/services/" + g.service
	fmt.Fprintf(w, "Deploying...\n")
	svc, err := s.GetService(ctx, &runpb.GetServiceRequest{Name: name})
	if err != nil {
		return fmt.Errorf("ERROR: (gcloud.run.services.update) %v", err)
	}
	tmpl := svc.GetTemplate()
	if tmpl == nil || len(tmpl.GetContainers()) == 0 {
		return fmt.Errorf("ERROR: (gcloud.run.services.update) service %s has no container", g.service)
	}
	tmpl.Containers[0].Image = g.image
	tmpl.Revision = ""
	labels := svc.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	for k, v := range g.labels {
		labels[k] = v
	}
	if tmpl.Labels == nil {
		tmpl.Labels = map[string]string{}
	}
	for k, v := range g.labels {
		tmpl.Labels[k] = v
	}
	op, err := s.UpdateService(ctx, &runpb.UpdateServiceRequest{
		Service:    &runpb.Service{Name: name, Template: tmpl, Labels: labels},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"template", "labels"}},
	})
	if err != nil {
		return fmt.Errorf("ERROR: (gcloud.run.services.update) %v", err)
	}
	done, err := waitRunOperation(ctx, s, op)
	if err != nil {
		return fmt.Errorf("ERROR: (gcloud.run.services.update) %v", err)
	}
	if e := done.GetError(); e != nil {
		return fmt.Errorf("ERROR: (gcloud.run.services.update) %s", e.GetMessage())
	}
	got, err := s.GetService(ctx, &runpb.GetServiceRequest{Name: name})
	if err == nil {
		fmt.Fprintf(w, "Service [%s] revision [%s] has been deployed and is serving traffic.\nService URL: %s\n",
			g.service, got.GetLatestReadyRevision()[strings.LastIndex(got.GetLatestReadyRevision(), "/")+1:], got.GetUri())
	}
	return nil
}

// waitRunOperation waits for one of Cloud Run's operations to finish.
func waitRunOperation(ctx context.Context, s *Server, op *longrunningpb.Operation) (*longrunningpb.Operation, error) {
	ops := NewOperationsServer(s)
	for !op.GetDone() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var err error
		if op, err = ops.WaitOperation(ctx, &longrunningpb.WaitOperationRequest{Name: op.GetName(),
			Timeout: durationpb.New(30 * time.Second)}); err != nil {
			return nil, err
		}
	}
	return op, nil
}

// refuseGcloudRun is a Deploy step's refusal, as the build's.
func refuseGcloudRun(i int, err error) error {
	return apierror.InvalidArgument("invalid build: step %d: %v", i, err)
}
