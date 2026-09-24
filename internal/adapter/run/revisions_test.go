package run

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// scriptedRunner answers kubectl with canned output, keyed by the verb and
// the object it names, and records deletes.
type scriptedRunner struct {
	answers map[string]string
	deleted []string
}

func (s *scriptedRunner) Run(_ context.Context, _, _ string, args ...string) (string, error) {
	joined := strings.Join(args, " ")
	if i := strings.Index(joined, " delete revision "); i >= 0 {
		s.deleted = append(s.deleted, strings.Fields(joined[i:])[2])
		return "", nil
	}
	for k, v := range s.answers {
		if strings.Contains(joined, k) {
			return v, nil
		}
	}
	return "", errors.New("NotFound")
}

const svcJSON = `{"metadata":{"name":"hello","labels":{"cloudburrow.dev/owned":"true"}},
 "status":{"latestReadyRevisionName":"hello-00002","traffic":[{"revisionName":"hello-00002","percent":100,"latestRevision":true}]}}`

func revJSON(name, gen, ready string) string {
	return `{"metadata":{"name":"` + name + `","uid":"u-` + name + `","creationTimestamp":"2026-09-24T10:00:00Z","generation":1,
	  "labels":{"serving.knative.dev/service":"hello","serving.knative.dev/configurationGeneration":"` + gen + `"}},
	 "spec":{"containerConcurrency":80,"containers":[{"image":"ghcr.io/x/y@sha256:abc","env":[{"name":"A","value":"b"}],"ports":[{"containerPort":8080}]}]},
	 "status":{"observedGeneration":1,"conditions":[{"type":"Ready","status":"` + ready + `"},{"type":"ContainerHealthy","status":"True"}]}}`
}

func revisionsServer() (*RevisionsServer, *scriptedRunner) {
	r := &scriptedRunner{answers: map[string]string{
		"get ksvc hello": "" + svcJSON,
		"get revisions -l serving.knative.dev/service=hello": `{"items":[` + revJSON("hello-00001", "1", "True") + `,` + revJSON("hello-00002", "2", "True") + `]}`,
		"get revision hello-00001":                           revJSON("hello-00001", "1", "True"),
		"get revision hello-00002":                           revJSON("hello-00002", "2", "True"),
	}}
	s := NewServer(&Knative{Kubeconfig: "k", Namespace: "default", Runner: r}, "inst", time.Second)
	return s.Revisions(), r
}

const svcParent = "projects/demo-project/locations/us-central1/services/hello"

func TestListAndGetRevisionsMapKnative(t *testing.T) {
	rs, _ := revisionsServer()
	ctx := context.Background()
	resp, err := rs.ListRevisions(ctx, &runpb.ListRevisionsRequest{Parent: svcParent})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetRevisions()) != 2 {
		t.Fatalf("ListRevisions = %v", resp.GetRevisions())
	}
	// Newest first.
	if resp.GetRevisions()[0].GetGeneration() != 2 || resp.GetRevisions()[1].GetGeneration() != 1 {
		t.Errorf("ListRevisions order = %d, %d; want newest first", resp.GetRevisions()[0].GetGeneration(), resp.GetRevisions()[1].GetGeneration())
	}
	r := resp.GetRevisions()[0]
	if r.GetName() != svcParent+"/revisions/hello-00002" || r.GetService() != svcParent || r.GetGeneration() != 2 ||
		r.GetUid() != "u-hello-00002" || r.GetMaxInstanceRequestConcurrency() != 80 ||
		r.GetContainers()[0].GetImage() != "ghcr.io/x/y@sha256:abc" || r.GetCreateTime().AsTime().Year() != 2026 {
		t.Errorf("mapped revision = %v", r)
	}
	if len(r.GetConditions()) != 2 || r.GetConditions()[0].GetState() != runpb.Condition_CONDITION_SUCCEEDED || r.GetReconciling() {
		t.Errorf("conditions = %v, reconciling %v", r.GetConditions(), r.GetReconciling())
	}

	got, err := rs.GetRevision(ctx, &runpb.GetRevisionRequest{Name: svcParent + "/revisions/hello-00001"})
	if err != nil || got.GetGeneration() != 1 {
		t.Errorf("GetRevision = %v, %v", got, err)
	}
	if _, err := rs.GetRevision(ctx, &runpb.GetRevisionRequest{Name: svcParent + "/revisions/absent"}); status.Code(err) != codes.NotFound {
		t.Errorf("GetRevision(absent) = %v, want NotFound", err)
	}
	if _, err := rs.ListRevisions(ctx, &runpb.ListRevisionsRequest{Parent: "projects/demo-project/locations/us-central1/services/nope"}); status.Code(err) != codes.NotFound {
		t.Errorf("ListRevisions(absent service) = %v, want NotFound", err)
	}
}

func TestDeleteRevisionRefusesTheServingRevision(t *testing.T) {
	rs, runner := revisionsServer()
	ctx := context.Background()
	_, err := rs.DeleteRevision(ctx, &runpb.DeleteRevisionRequest{Name: svcParent + "/revisions/hello-00002"})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "serving traffic") {
		t.Fatalf("DeleteRevision(serving) = %v, want FailedPrecondition", err)
	}
	if len(runner.deleted) != 0 {
		t.Fatalf("the serving revision was deleted: %v", runner.deleted)
	}
	op, err := rs.DeleteRevision(ctx, &runpb.DeleteRevisionRequest{Name: svcParent + "/revisions/hello-00001"})
	if err != nil || !op.GetDone() {
		t.Fatalf("DeleteRevision(retired) = %v, %v", op, err)
	}
	if len(runner.deleted) != 1 || runner.deleted[0] != "hello-00001" {
		t.Errorf("deleted = %v", runner.deleted)
	}
}
