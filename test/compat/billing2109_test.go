//go:build compat

package compat

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	billing "cloud.google.com/go/billing/apiv1"
	pb "cloud.google.com/go/billing/apiv1/billingpb"
	budgets "cloud.google.com/go/billing/budgets/apiv1"
	bpb "cloud.google.com/go/billing/budgets/apiv1/budgetspb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/genproto/googleapis/type/money"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// billingClients are the official cloud.google.com/go/billing clients
// (CloudBilling, CloudCatalog and the Budget API's BudgetClient) over gRPC
// and JSON on the Resource Manager port (#2109).
type billingClients struct {
	billing *billing.CloudBillingClient
	catalog *billing.CloudCatalogClient
	budgets *budgets.BudgetClient
}

func newBillingClients(t *testing.T, h *Harness) map[string]billingClients {
	t.Helper()
	ep := h.Endpoint(EnvResourceManager)
	rest := []option.ClientOption{option.WithEndpoint("http://" + ep), option.WithoutAuthentication(), option.WithHTTPClient(&http.Client{})}
	out := map[string]billingClients{}
	var err error
	var g, r billingClients
	if g.billing, err = billing.NewCloudBillingClient(h.Context(), rmOptionsAt(ep)...); err != nil {
		t.Fatal(err)
	}
	if g.catalog, err = billing.NewCloudCatalogClient(h.Context(), rmOptionsAt(ep)...); err != nil {
		t.Fatal(err)
	}
	if g.budgets, err = budgets.NewBudgetClient(h.Context(), rmOptionsAt(ep)...); err != nil {
		t.Fatal(err)
	}
	if r.billing, err = billing.NewCloudBillingRESTClient(h.Context(), rest...); err != nil {
		t.Fatal(err)
	}
	if r.catalog, err = billing.NewCloudCatalogRESTClient(h.Context(), rest...); err != nil {
		t.Fatal(err)
	}
	if r.budgets, err = budgets.NewBudgetRESTClient(h.Context(), rest...); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, c := range []billingClients{g, r} {
			_ = c.billing.Close()
			_ = c.catalog.Close()
			_ = c.budgets.Close()
		}
	})
	out["grpc"], out["rest"] = g, r
	return out
}

// topBillingAccount is the instance's first open top-level account.
func topBillingAccount(t *testing.T, h *Harness, c *billing.CloudBillingClient) *pb.BillingAccount {
	t.Helper()
	it := c.ListBillingAccounts(h.Context(), &pb.ListBillingAccountsRequest{})
	for {
		a, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if a.GetOpen() && a.GetMasterBillingAccount() == "" {
			return a
		}
	}
	t.Fatal("no open top-level billing account")
	return nil
}

// covers: google.cloud.billing.v1.CloudBilling/ListBillingAccounts, google.cloud.billing.v1.CloudBilling/GetBillingAccount, google.cloud.billing.v1.CloudBilling/CreateBillingAccount, google.cloud.billing.v1.CloudBilling/UpdateBillingAccount, google.cloud.billing.v1.CloudBilling/MoveBillingAccount, google.cloud.billing.v1.CloudBilling/GetIamPolicy, google.cloud.billing.v1.CloudBilling/SetIamPolicy, google.cloud.billing.v1.CloudBilling/TestIamPermissions
//
// TestCloudBillingAccounts (#2109), with cloud.google.com/go/billing over
// gRPC and JSON: the instance's account, a subaccount created, listed by
// its parent's filter, read and renamed; moving a subaccount refused and
// a top-level account moved within the organization; the account's IAM
// policy set, read and tested.
func TestCloudBillingAccounts(t *testing.T) {
	h := New(t)
	for transport, c := range newBillingClients(t, h) {
		t.Run(transport, func(t *testing.T) {
			h := h.For(t)
			ctx := h.Context()
			top := topBillingAccount(t, h, c.billing)
			if top.GetCurrencyCode() != "USD" || !strings.HasPrefix(top.GetParent(), "organizations/") {
				t.Errorf("top-level account = %v", top)
			}
			sub, err := c.billing.CreateBillingAccount(ctx, &pb.CreateBillingAccountRequest{Parent: top.GetName(),
				BillingAccount: &pb.BillingAccount{DisplayName: "compat " + transport}})
			if err != nil {
				t.Fatal(err)
			}
			if sub.GetMasterBillingAccount() != top.GetName() || !sub.GetOpen() || sub.GetCurrencyCode() != top.GetCurrencyCode() {
				t.Errorf("subaccount = %v", sub)
			}
			found := false
			it := c.billing.ListBillingAccounts(ctx, &pb.ListBillingAccountsRequest{Filter: "masterBillingAccount=" + top.GetName()})
			for {
				a, err := it.Next()
				if err == iterator.Done {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if a.GetMasterBillingAccount() != top.GetName() {
					t.Errorf("the filter listed %v", a)
				}
				found = found || a.GetName() == sub.GetName()
			}
			if !found {
				t.Errorf("the subaccount %s is not listed under its parent", sub.GetName())
			}
			up, err := c.billing.UpdateBillingAccount(ctx, &pb.UpdateBillingAccountRequest{Name: sub.GetName(),
				Account: &pb.BillingAccount{DisplayName: "renamed " + transport}, UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
			if err != nil || up.GetDisplayName() != "renamed "+transport {
				t.Fatalf("update = %v, %v", up, err)
			}
			if got, err := c.billing.GetBillingAccount(ctx, &pb.GetBillingAccountRequest{Name: sub.GetName()}); err != nil || got.GetDisplayName() != up.GetDisplayName() {
				t.Errorf("get = %v, %v", got, err)
			}
			if _, err := c.billing.GetBillingAccount(ctx, &pb.GetBillingAccountRequest{Name: "billingAccounts/000000-000000-000000"}); quotaCode(err) != codes.NotFound {
				t.Errorf("an unknown account = %v", err)
			}
			if _, err := c.billing.MoveBillingAccount(ctx, &pb.MoveBillingAccountRequest{Name: sub.GetName(), DestinationParent: top.GetParent()}); quotaCode(err) != codes.FailedPrecondition {
				t.Errorf("moving a subaccount = %v", err)
			}
			if m, err := c.billing.MoveBillingAccount(ctx, &pb.MoveBillingAccountRequest{Name: top.GetName(), DestinationParent: top.GetParent()}); err != nil || m.GetParent() != top.GetParent() {
				t.Errorf("move = %v, %v", m, err)
			}
			pol, err := c.billing.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: sub.GetName()})
			if err != nil {
				t.Fatal(err)
			}
			pol.Bindings = append(pol.Bindings, &iampb.Binding{Role: "roles/billing.viewer", Members: []string{"user:viewer@example.com"}})
			set, err := c.billing.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: sub.GetName(), Policy: pol})
			if err != nil || len(set.GetBindings()) != 1 || len(set.GetEtag()) == 0 {
				t.Fatalf("setIamPolicy = %v, %v", set, err)
			}
			if got, err := c.billing.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: sub.GetName()}); err != nil || rolesOf(got)["roles/billing.viewer"] != "user:viewer@example.com" {
				t.Errorf("getIamPolicy after the set = %v, %v", got, err)
			}
			tp, err := c.billing.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: sub.GetName(), Permissions: []string{"billing.accounts.get"}})
			if err != nil || len(tp.GetPermissions()) != 1 {
				t.Errorf("testIamPermissions = %v, %v", tp, err)
			}
		})
	}
}

// covers: google.cloud.billing.v1.CloudBilling/GetProjectBillingInfo, google.cloud.billing.v1.CloudBilling/UpdateProjectBillingInfo, google.cloud.billing.v1.CloudBilling/ListProjectBillingInfo
//
// TestCloudBillingLinkProject (#2109): a new project starts with billing
// disabled; linking it enables billing and lists it under the account;
// unlinking disables billing and its billable APIs (Pub/Sub's), which a
// link enables again, as Service Usage reads them.
func TestCloudBillingLinkProject(t *testing.T) {
	h := New(t)
	su := serviceUsageClient(t, h)
	for transport, c := range newBillingClients(t, h) {
		t.Run(transport, func(t *testing.T) {
			h := h.For(t)
			ctx := h.Context()
			project := iamProject(t, h, "bill-"+transport[:1]+"-")
			top := topBillingAccount(t, h, c.billing)
			info, err := c.billing.GetProjectBillingInfo(ctx, &pb.GetProjectBillingInfoRequest{Name: "projects/" + project})
			if err != nil || info.GetBillingEnabled() || info.GetBillingAccountName() != "" || info.GetProjectId() != project {
				t.Fatalf("a new project's billing = %v, %v", info, err)
			}
			info, err = c.billing.UpdateProjectBillingInfo(ctx, &pb.UpdateProjectBillingInfoRequest{Name: "projects/" + project,
				ProjectBillingInfo: &pb.ProjectBillingInfo{BillingAccountName: top.GetName()}})
			if err != nil || !info.GetBillingEnabled() || info.GetBillingAccountName() != top.GetName() {
				t.Fatalf("link = %v, %v", info, err)
			}
			listed := false
			it := c.billing.ListProjectBillingInfo(ctx, &pb.ListProjectBillingInfoRequest{Name: top.GetName()})
			for {
				p, err := it.Next()
				if err == iterator.Done {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				listed = listed || p.GetProjectId() == project
			}
			if !listed {
				t.Errorf("%s is not listed under %s", project, top.GetName())
			}
			before := serviceState(t, h, su, project, "pubsub.googleapis.com")
			if _, err := c.billing.UpdateProjectBillingInfo(ctx, &pb.UpdateProjectBillingInfoRequest{Name: "projects/" + project,
				ProjectBillingInfo: &pb.ProjectBillingInfo{}}); err != nil {
				t.Fatal(err)
			}
			if info, _ := c.billing.GetProjectBillingInfo(ctx, &pb.GetProjectBillingInfoRequest{Name: "projects/" + project}); info.GetBillingEnabled() {
				t.Errorf("after unlink = %v", info)
			}
			if before == "ENABLED" {
				if s := serviceState(t, h, su, project, "pubsub.googleapis.com"); s != "DISABLED" {
					t.Errorf("pubsub.googleapis.com after unlink = %s, want DISABLED", s)
				}
				if s := serviceState(t, h, su, project, "cloudresourcemanager.googleapis.com"); s != "ENABLED" {
					t.Errorf("a free API after unlink = %s, want ENABLED", s)
				}
			}
			if _, err := c.billing.UpdateProjectBillingInfo(ctx, &pb.UpdateProjectBillingInfoRequest{Name: "projects/" + project,
				ProjectBillingInfo: &pb.ProjectBillingInfo{BillingAccountName: top.GetName()}}); err != nil {
				t.Fatal(err)
			}
			if s := serviceState(t, h, su, project, "pubsub.googleapis.com"); s != before {
				t.Errorf("pubsub.googleapis.com after relink = %s, want %s", s, before)
			}
			if _, err := c.billing.GetProjectBillingInfo(ctx, &pb.GetProjectBillingInfoRequest{Name: "projects/no-such-project-2109"}); quotaCode(err) != codes.NotFound {
				t.Errorf("an unknown project = %v", err)
			}
		})
	}
}

// covers: google.cloud.billing.v1.CloudCatalog/ListServices, google.cloud.billing.v1.CloudCatalog/ListSkus
//
// TestCloudBillingCatalog (#2109): the catalog lists the served APIs, and
// a service's SKUs carry the local list price, 0.
func TestCloudBillingCatalog(t *testing.T) {
	h := New(t)
	for transport, c := range newBillingClients(t, h) {
		t.Run(transport, func(t *testing.T) {
			h := h.For(t)
			ctx := h.Context()
			var rm *pb.Service
			it := c.catalog.ListServices(ctx, &pb.ListServicesRequest{})
			for {
				s, err := it.Next()
				if err == iterator.Done {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if s.GetDisplayName() == "Cloud Resource Manager" {
					rm = s
				}
			}
			if rm == nil || rm.GetName() != "services/"+rm.GetServiceId() || rm.GetBusinessEntityName() != "businessEntities/GCP" {
				t.Fatalf("Cloud Resource Manager's catalog entry = %v", rm)
			}
			n := 0
			sit := c.catalog.ListSkus(ctx, &pb.ListSkusRequest{Parent: rm.GetName()})
			for {
				sk, err := sit.Next()
				if err == iterator.Done {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				n++
				price := sk.GetPricingInfo()[0].GetPricingExpression().GetTieredRates()[0].GetUnitPrice()
				if price.GetCurrencyCode() != "USD" || price.GetUnits() != 0 || price.GetNanos() != 0 || !strings.HasPrefix(sk.GetName(), rm.GetName()+"/skus/") {
					t.Errorf("sku = %v", sk)
				}
			}
			if n == 0 {
				t.Error("no SKUs")
			}
			if _, err := c.catalog.ListSkus(ctx, &pb.ListSkusRequest{Parent: "services/0000-0000-0000"}).Next(); quotaCode(err) != codes.NotFound {
				t.Errorf("an unknown service = %v", err)
			}
		})
	}
}

// covers: google.cloud.billing.budgets.v1.BudgetService/CreateBudget, google.cloud.billing.budgets.v1.BudgetService/GetBudget, google.cloud.billing.budgets.v1.BudgetService/ListBudgets, google.cloud.billing.budgets.v1.BudgetService/UpdateBudget, google.cloud.billing.budgets.v1.BudgetService/DeleteBudget
//
// TestCloudBillingBudgets (#2109), with cloud.google.com/go/billing/budgets
// over gRPC and JSON: a budget with thresholds, a project filter and a
// Pub/Sub topic created, read, listed by its project scope, updated with a
// mask and deleted; the documented validation refused.
func TestCloudBillingBudgets(t *testing.T) {
	h := New(t)
	for transport, c := range newBillingClients(t, h) {
		t.Run(transport, func(t *testing.T) {
			h := h.For(t)
			ctx := h.Context()
			top := topBillingAccount(t, h, c.billing)
			project := h.Project()
			usd := func(u int64) *bpb.BudgetAmount {
				return &bpb.BudgetAmount{BudgetAmount: &bpb.BudgetAmount_SpecifiedAmount{SpecifiedAmount: &money.Money{CurrencyCode: "USD", Units: u}}}
			}
			in := &bpb.Budget{DisplayName: "compat " + transport, Amount: usd(250),
				BudgetFilter:   &bpb.Filter{Projects: []string{"projects/" + project}, UsagePeriod: &bpb.Filter_CalendarPeriod{CalendarPeriod: bpb.CalendarPeriod_MONTH}},
				ThresholdRules: []*bpb.ThresholdRule{{ThresholdPercent: 0.5}, {ThresholdPercent: 0.9}, {ThresholdPercent: 1, SpendBasis: bpb.ThresholdRule_FORECASTED_SPEND}},
				NotificationsRule: &bpb.NotificationsRule{PubsubTopic: fmt.Sprintf("projects/%s/topics/budget-%s", project, transport),
					SchemaVersion: "1.0"}}
			b, err := c.budgets.CreateBudget(ctx, &bpb.CreateBudgetRequest{Parent: top.GetName(), Budget: in})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.budgets.DeleteBudget(ctx, &bpb.DeleteBudgetRequest{Name: b.GetName()}) })
			if !strings.HasPrefix(b.GetName(), top.GetName()+"/budgets/") || b.GetEtag() == "" ||
				b.GetNotificationsRule().GetPubsubTopic() != in.GetNotificationsRule().GetPubsubTopic() || len(b.GetThresholdRules()) != 3 ||
				b.GetBudgetFilter().GetCreditTypesTreatment() != bpb.Filter_INCLUDE_ALL_CREDITS {
				t.Errorf("created = %v", b)
			}
			got, err := c.budgets.GetBudget(ctx, &bpb.GetBudgetRequest{Name: b.GetName()})
			if err != nil || got.GetDisplayName() != in.GetDisplayName() || got.GetAmount().GetSpecifiedAmount().GetUnits() != 250 {
				t.Fatalf("get = %v, %v", got, err)
			}
			found := false
			it := c.budgets.ListBudgets(ctx, &bpb.ListBudgetsRequest{Parent: top.GetName(), Scope: "projects/" + project})
			for {
				x, err := it.Next()
				if err == iterator.Done {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				found = found || x.GetName() == b.GetName()
			}
			if !found {
				t.Error("the budget is not listed in its project's scope")
			}
			up, err := c.budgets.UpdateBudget(ctx, &bpb.UpdateBudgetRequest{Budget: &bpb.Budget{Name: b.GetName(), Amount: usd(300), DisplayName: "ignored"},
				UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"amount"}}})
			if err != nil || up.GetAmount().GetSpecifiedAmount().GetUnits() != 300 || up.GetDisplayName() != in.GetDisplayName() || up.GetEtag() == b.GetEtag() {
				t.Fatalf("update = %v, %v", up, err)
			}
			if _, err := c.budgets.UpdateBudget(ctx, &bpb.UpdateBudgetRequest{Budget: &bpb.Budget{Name: b.GetName(), DisplayName: "stale", Etag: b.GetEtag()}}); quotaCode(err) != codes.Aborted {
				t.Errorf("a stale etag = %v", err)
			}
			for i, bad := range []*bpb.Budget{
				{DisplayName: strings.Repeat("x", 61), Amount: usd(1)},
				{Amount: usd(1), NotificationsRule: &bpb.NotificationsRule{PubsubTopic: "projects/" + project + "/topics/t", SchemaVersion: "2.0"}},
				{Amount: usd(1), ThresholdRules: []*bpb.ThresholdRule{{ThresholdPercent: -0.1}}},
				{Amount: &bpb.BudgetAmount{BudgetAmount: &bpb.BudgetAmount_SpecifiedAmount{SpecifiedAmount: &money.Money{CurrencyCode: "EUR", Units: 1}}}},
			} {
				if _, err := c.budgets.CreateBudget(ctx, &bpb.CreateBudgetRequest{Parent: top.GetName(), Budget: bad}); quotaCode(err) != codes.InvalidArgument {
					t.Errorf("bad budget %d = %v", i, err)
				}
			}
			if err := c.budgets.DeleteBudget(ctx, &bpb.DeleteBudgetRequest{Name: b.GetName()}); err != nil {
				t.Fatal(err)
			}
			if _, err := c.budgets.GetBudget(ctx, &bpb.GetBudgetRequest{Name: b.GetName()}); quotaCode(err) != codes.NotFound {
				t.Errorf("after delete = %v", err)
			}
		})
	}
}

// TestGcloudBilling (#2109): gcloud billing accounts list, describe and
// get-iam-policy, and gcloud billing projects link, describe, list and
// unlink, against the cloudbilling override on the Resource Manager port.
func TestGcloudBilling(t *testing.T) {
	h := New(t)
	clients := newBillingClients(t, h)
	g := newGcloudSession(t, h)
	top := topBillingAccount(t, h, clients["grpc"].billing)
	id := strings.TrimPrefix(top.GetName(), "billingAccounts/")
	if out := g.must("billing", "accounts", "list", "--format=value(name)"); !strings.Contains(out, id) {
		t.Errorf("accounts list:\n%s", out)
	}
	if out := g.must("billing", "accounts", "describe", id, "--format=value(displayName)"); strings.TrimSpace(out) != top.GetDisplayName() {
		t.Errorf("accounts describe = %q", out)
	}
	g.must("billing", "accounts", "get-iam-policy", id, "--format=json")
	project := iamProject(t, h, "gbill-")
	if out := g.must("billing", "projects", "link", project, "--billing-account="+id, "--format=value(billingEnabled)"); strings.TrimSpace(out) != "True" {
		t.Errorf("projects link = %q", out)
	}
	if out := g.must("billing", "projects", "describe", project, "--format=value(billingAccountName)"); strings.TrimSpace(out) != top.GetName() {
		t.Errorf("projects describe = %q", out)
	}
	if out := g.must("billing", "projects", "list", "--billing-account="+id, "--format=value(projectId)"); !strings.Contains(out, project) {
		t.Errorf("projects list:\n%s", out)
	}
	g.must("billing", "projects", "unlink", project)
	// billingEnabled false is proto3's default, so JSON leaves it out.
	if out := g.must("billing", "projects", "describe", project, "--format=value(billingEnabled)"); strings.TrimSpace(out) != "" && strings.TrimSpace(out) != "False" {
		t.Errorf("after unlink billingEnabled = %q", out)
	}
}

// TestTerraformBillingProjectInfoAndBudget (#2109): google_billing_project_info
// links a project and google_billing_budget creates a budget with a
// Pub/Sub topic; both apply and plan clean, the budget updates in place,
// and destroy unlinks the project.
func TestTerraformBillingProjectInfoAndBudget(t *testing.T) {
	h := New(t)
	clients := newBillingClients(t, h)
	c := clients["rest"]
	m := newTFModule(t)
	top := topBillingAccount(t, h, c.billing)
	id := strings.TrimPrefix(top.GetName(), "billingAccounts/")
	project := iamProject(t, h, "tfbill-")
	module := func(units int) string {
		return fmt.Sprintf(`
resource "google_billing_project_info" "p" {
  project         = %q
  billing_account = %q
}

resource "google_billing_budget" "b" {
  billing_account = %q
  display_name    = "tf budget"
  amount {
    specified_amount {
      currency_code = "USD"
      units         = "%d"
    }
  }
  threshold_rules {
    threshold_percent = 0.5
  }
  threshold_rules {
    threshold_percent = 0.9
  }
  all_updates_rule {
    pubsub_topic   = "projects/%s/topics/tf-budget"
    schema_version = "1.0"
  }
}
`, project, id, id, units, project)
	}
	m.write(module(100))
	m.must("init", "-input=false", "-no-color")
	m.apply()
	m.planClean()
	info, err := c.billing.GetProjectBillingInfo(h.Context(), &pb.GetProjectBillingInfoRequest{Name: "projects/" + project})
	if err != nil || info.GetBillingAccountName() != top.GetName() || !info.GetBillingEnabled() {
		t.Fatalf("after apply the project's billing = %v, %v", info, err)
	}
	m.write(module(150))
	m.apply()
	m.planClean()
	n := 0
	it := c.budgets.ListBudgets(h.Context(), &bpb.ListBudgetsRequest{Parent: top.GetName()})
	for {
		x, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if x.GetDisplayName() == "tf budget" && x.GetAmount().GetSpecifiedAmount().GetUnits() == 150 {
			n++
		}
	}
	if n != 1 {
		t.Errorf("found %d updated tf budgets, want 1", n)
	}
	m.must("destroy", "-auto-approve", "-input=false", "-no-color")
	if info, err := c.billing.GetProjectBillingInfo(h.Context(), &pb.GetProjectBillingInfoRequest{Name: "projects/" + project}); err != nil || info.GetBillingAccountName() != "" {
		t.Errorf("after destroy = %v, %v", info, err)
	}
}
