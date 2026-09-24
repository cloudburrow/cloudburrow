package resourcemanager

import (
	"context"
	"errors"
	"net/http"
	"testing"

	rmpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	crmv1 "google.golang.org/api/cloudresourcemanager/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// The v1 surface gcloud and Terraform use, through the official v1 client,
// over the registry v3 and the console read.
func TestV1ProjectsThroughTheOfficialClient(t *testing.T) {
	reg, srv, v3 := startAPI(t)
	ctx := context.Background()
	c, err := crmv1.NewService(ctx, option.WithEndpoint("http://"+srv.Addr()+"/"),
		option.WithoutAuthentication(), option.WithHTTPClient(http.DefaultClient))
	if err != nil {
		t.Fatal(err)
	}

	op, err := c.Projects.Create(&crmv1.Project{ProjectId: "v1-made-one", Name: "Made by v1",
		Labels: map[string]string{"goog-terraform-provisioned": "true"}}).Context(ctx).Do()
	if err != nil {
		t.Fatalf("v1 create: %v", err)
	}
	if !op.Done || op.Name == "" {
		t.Errorf("v1 create operation = %+v, want done", op)
	}
	if got, err := c.Operations.Get(op.Name).Context(ctx).Do(); err != nil || !got.Done {
		t.Errorf("operations.get(%s) = %+v, %v", op.Name, got, err)
	}

	p, err := c.Projects.Get("v1-made-one").Context(ctx).Do()
	if err != nil || p.Name != "Made by v1" || p.LifecycleState != "ACTIVE" || p.ProjectNumber == 0 {
		t.Errorf("v1 get = %+v, %v", p, err)
	}
	// One store: v3 and the registry the console lists see it.
	if g, err := v3.GetProject(ctx, &rmpb.GetProjectRequest{Name: "projects/v1-made-one"}); err != nil || g.GetDisplayName() != "Made by v1" {
		t.Errorf("v3 does not see the v1-created project: %v, %v", g, err)
	}
	if _, err := reg.Get("v1-made-one"); err != nil {
		t.Errorf("the registry does not hold it: %v", err)
	}

	if _, err := reg.Create(Project{ProjectID: "other-one", DisplayName: "Other"}); err != nil {
		t.Fatal(err)
	}
	list, err := c.Projects.List().Filter("lifecycleState:ACTIVE labels.goog-terraform-provisioned:true").Context(ctx).Do()
	if err != nil || len(list.Projects) != 1 || list.Projects[0].ProjectId != "v1-made-one" {
		t.Errorf("v1 list with filter = %+v, %v", list, err)
	}
	all, err := c.Projects.List().Context(ctx).Do()
	if err != nil || len(all.Projects) != 2 {
		t.Errorf("v1 list = %+v, %v", all, err)
	}
	if _, err := c.Projects.List().Filter("id:a OR id:b").Context(ctx).Do(); code(err) != http.StatusBadRequest {
		t.Errorf("v1 list with OR = %v, want 400", err)
	}

	bi, err := http.Get("http://" + srv.Addr() + "/v1/projects/v1-made-one/billingInfo")
	if err != nil || bi.StatusCode != http.StatusOK {
		t.Errorf("billingInfo = %v, %v", bi, err)
	} else {
		_ = bi.Body.Close()
	}

	if _, err := c.Projects.GetIamPolicy("v1-made-one", &crmv1.GetIamPolicyRequest{}).Context(ctx).Do(); code(err) != http.StatusNotImplemented {
		t.Errorf("v1 getIamPolicy = %v, want 501", err)
	}
	if _, err := c.Projects.Update("v1-made-one", &crmv1.Project{Name: "x"}).Context(ctx).Do(); code(err) != http.StatusNotImplemented {
		t.Errorf("v1 update = %v, want 501", err)
	}
	if _, err := c.Organizations.Get("organizations/1").Context(ctx).Do(); code(err) != http.StatusNotImplemented {
		t.Errorf("v1 organizations.get = %v, want 501", err)
	}

	if _, err := c.Projects.Delete("v1-made-one").Context(ctx).Do(); err != nil {
		t.Fatalf("v1 delete: %v", err)
	}
	if _, err := c.Projects.Get("v1-made-one").Context(ctx).Do(); code(err) != http.StatusNotFound {
		t.Errorf("v1 get after delete = %v, want 404", err)
	}
}

func code(err error) int {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return ge.Code
	}
	return 0
}
