package main

// Cloud Run: Delete revision on a service's Revision history (#785).
//
// The adapter's DeleteRevision is verified with the official client, and a
// revision row offered nothing, so a service's history of failed and
// superseded revisions could only grow. The action is offered on a revision
// that serves no traffic, of a service CloudBurrow created: the API refuses
// the others (the serving revision, and any revision of a service it does not
// own), so the button is absent there rather than present and failing.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	runclient "cloud.google.com/go/run/apiv2"
	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/cloudburrow/cloudburrow/internal/console"
	"github.com/cloudburrow/cloudburrow/internal/k8s"
)

const runDeleteRevision = "delete-revision"

// servingRevisions is the set of revisions the API will not delete: every one
// with a share of traffic, and the latest ready one while Knative reports no
// split yet. The same rule as the adapter's DeleteRevision.
func (s ksvcStatus) servingRevisions() map[string]bool {
	out := map[string]bool{}
	for _, t := range s.Status.Traffic {
		// A tagged revision serves its tag URL even at 0%.
		if (t.Percent > 0 || t.Tag != "") && t.RevisionName != "" {
			out[t.RevisionName] = true
		}
	}
	if len(s.Status.Traffic) == 0 && s.Status.LatestReadyRevisionName != "" {
		out[s.Status.LatestReadyRevisionName] = true
	}
	return out
}

// revisionActions is what a revision of svc offers.
func (p runProvider) revisionActions(svc *ksvcStatus, revision string) []console.Action {
	if p.runEndpoint() == "" || svc == nil || svc.Metadata.Labels[k8s.OwnedLabel] != k8s.OwnedValue ||
		svc.servingRevisions()[revision] {
		return nil
	}
	return []console.Action{{ID: runDeleteRevision, Label: "Delete revision", Destructive: true}}
}

// service reads one of project's services by its ID: nil, and no error,
// when there is none. Of two regions' services of the ID, the console's
// region's is preferred.
func (p runProvider) service(ctx context.Context, project, id string) (*ksvcStatus, error) {
	svcs, err := p.services(ctx, project)
	if err != nil {
		return nil, err
	}
	var found *ksvcStatus
	for i := range svcs {
		if svcs[i].Metadata.Name != id {
			continue
		}
		if found == nil || strings.Contains(svcs[i].FullName, "/locations/"+p.location()+"/") {
			found = &svcs[i]
		}
	}
	return found, nil
}

// DetailActions implements console.PathActor: a service's Manage traffic
// (#1224) and a revision's Delete revision.
func (p runProvider) DetailActions(ctx context.Context, project string, path []string) []console.Action {
	if len(path) < 1 || len(path) > 2 || p.runEndpoint() == "" {
		return nil
	}
	svc, err := p.service(ctx, project, path[0])
	if err != nil || svc == nil {
		return nil
	}
	if len(path) == 1 {
		return append(p.trafficAction(svc), p.sourceActions()...)
	}
	return p.revisionActions(svc, path[1])
}

// ActAt implements console.PathActor through the adapter's UpdateService
// (Manage traffic) and DeleteRevision.
func (p runProvider) ActAt(ctx context.Context, project string, path []string, action string, values map[string]string) error {
	if action == runDeploySource {
		// Its file is not in a JSON action: the form sends both to the
		// upload route, which calls ActAtFile.
		return errors.New("Deploy from source is sent with its file, to /api/actions/run/upload")
	}
	if action == runManageTraffic && len(path) == 1 {
		if p.runEndpoint() == "" {
			return errors.New("the Cloud Run adapter is not running")
		}
		return p.manageTraffic(ctx, project, path[0], values)
	}
	if action != runDeleteRevision || len(path) != 2 {
		return fmt.Errorf("unknown action %q", action)
	}
	if p.runEndpoint() == "" {
		return errors.New("the Cloud Run adapter is not running")
	}
	if project == "" {
		project = p.defaultProject
	}
	if project == "" {
		return errors.New("choose a project before deleting a revision")
	}
	c, err := runclient.NewRevisionsClient(ctx,
		option.WithEndpoint(p.runEndpoint()),
		option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
	)
	if err != nil {
		return fmt.Errorf("connect to Cloud Run: %w", err)
	}
	defer func() { _ = c.Close() }()

	service := path[0]
	if !strings.HasPrefix(service, "projects/") {
		service = fmt.Sprintf("projects/%s/locations/%s/services/%s", project, p.location(), service)
	}
	op, err := c.DeleteRevision(ctx, &runpb.DeleteRevisionRequest{Name: service + "/revisions/" + path[1]})
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

var _ console.PathActor = runProvider{}
