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
	"encoding/json"
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
		if t.Percent > 0 && t.RevisionName != "" {
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

// service reads one Knative Service by name.
func (p runProvider) service(ctx context.Context, name string) (*ksvcStatus, error) {
	out, err := kubectlJSON(ctx, p.kubeconfig, p.namespace, "ksvc")
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []ksvcStatus `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("decode services: %w", err)
	}
	for i := range list.Items {
		if list.Items[i].Metadata.Name == name {
			return &list.Items[i], nil
		}
	}
	return nil, fmt.Errorf("no service named %s", name)
}

// DetailActions implements console.PathActor: a revision's Delete revision.
func (p runProvider) DetailActions(ctx context.Context, _ string, path []string) []console.Action {
	if len(path) != 2 || p.runEndpoint() == "" {
		return nil
	}
	svc, err := p.service(ctx, path[0])
	if err != nil {
		return nil
	}
	return p.revisionActions(svc, path[1])
}

// ActAt implements console.PathActor through the adapter's DeleteRevision.
func (p runProvider) ActAt(ctx context.Context, project string, path []string, action string, _ map[string]string) error {
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
