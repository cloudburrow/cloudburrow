package billing

import (
	"context"
	"errors"
	"slices"
	"testing"

	pb "cloud.google.com/go/billing/apiv1/billingpb"
	bpb "cloud.google.com/go/billing/budgets/apiv1/budgetspb"
	"google.golang.org/genproto/googleapis/type/money"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/cloudburrow/cloudburrow/internal/store"
)

const org = "organizations/123456789012"

type projects map[string]bool

func (p projects) Resolve(s string) (string, error) {
	if p[s] {
		return s, nil
	}
	return "", errors.New("no such project")
}

type apis struct {
	enabled map[string][]string
}

func (a *apis) DisableBillable(project string) ([]string, error) {
	out := a.enabled[project]
	a.enabled[project] = nil
	return out, nil
}

func (a *apis) Enable(project string, names []string) error {
	a.enabled[project] = append(a.enabled[project], names...)
	return nil
}

func code(err error) codes.Code { return status.Code(err) }

func newTest(t *testing.T) (*Server, *apis) {
	t.Helper()
	s := NewServer(store.NewMemory(), projects{"p1": true, "p2": true}, org)
	a := &apis{enabled: map[string][]string{"p1": {"run.googleapis.com"}}}
	s.APIs = a
	return s, a
}

func TestAccountsSubaccountsAndMove(t *testing.T) {
	s, _ := newTest(t)
	ctx := context.Background()
	list, err := s.ListBillingAccounts(ctx, &pb.ListBillingAccountsRequest{})
	if err != nil || len(list.GetBillingAccounts()) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
	top := list.GetBillingAccounts()[0]
	if top.GetName() != "billingAccounts/"+DefaultAccountID(org) || !top.GetOpen() || top.GetParent() != org || top.GetCurrencyCode() != "USD" {
		t.Fatalf("default account = %v", top)
	}
	sub, err := s.CreateBillingAccount(ctx, &pb.CreateBillingAccountRequest{Parent: top.GetName(), BillingAccount: &pb.BillingAccount{DisplayName: "team"}})
	if err != nil || sub.GetMasterBillingAccount() != top.GetName() || sub.GetCurrencyCode() != "USD" {
		t.Fatalf("subaccount = %v, %v", sub, err)
	}
	if _, err := s.CreateBillingAccount(ctx, &pb.CreateBillingAccountRequest{Parent: sub.GetName(), BillingAccount: &pb.BillingAccount{DisplayName: "x"}}); code(err) != codes.FailedPrecondition {
		t.Errorf("a subaccount of a subaccount = %v", err)
	}
	if _, err := s.CreateBillingAccount(ctx, &pb.CreateBillingAccountRequest{Parent: org, BillingAccount: &pb.BillingAccount{DisplayName: "x"}}); code(err) != codes.InvalidArgument {
		t.Errorf("an organization parent = %v", err)
	}
	f, err := s.ListBillingAccounts(ctx, &pb.ListBillingAccountsRequest{Filter: "masterBillingAccount=" + top.GetName()})
	if err != nil || len(f.GetBillingAccounts()) != 1 || f.GetBillingAccounts()[0].GetName() != sub.GetName() {
		t.Errorf("filtered = %v, %v", f, err)
	}
	if _, err := s.ListBillingAccounts(ctx, &pb.ListBillingAccountsRequest{Filter: "open=true"}); code(err) != codes.InvalidArgument {
		t.Errorf("an unsupported filter = %v", err)
	}
	if _, err := s.MoveBillingAccount(ctx, &pb.MoveBillingAccountRequest{Name: sub.GetName(), DestinationParent: org}); code(err) != codes.FailedPrecondition {
		t.Errorf("moving a subaccount = %v", err)
	}
	if _, err := s.MoveBillingAccount(ctx, &pb.MoveBillingAccountRequest{Name: top.GetName(), DestinationParent: "organizations/1"}); code(err) != codes.NotFound {
		t.Errorf("an unknown organization = %v", err)
	}
	if m, err := s.MoveBillingAccount(ctx, &pb.MoveBillingAccountRequest{Name: top.GetName(), DestinationParent: org}); err != nil || m.GetParent() != org {
		t.Errorf("move = %v, %v", m, err)
	}
	up, err := s.UpdateBillingAccount(ctx, &pb.UpdateBillingAccountRequest{Name: sub.GetName(), Account: &pb.BillingAccount{DisplayName: "renamed"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
	if err != nil || up.GetDisplayName() != "renamed" {
		t.Errorf("update = %v, %v", up, err)
	}
	if _, err := s.UpdateBillingAccount(ctx, &pb.UpdateBillingAccountRequest{Name: sub.GetName(), Account: &pb.BillingAccount{DisplayName: "x"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"open"}}}); code(err) != codes.InvalidArgument {
		t.Errorf("updating open = %v", err)
	}
	if _, err := s.GetBillingAccount(ctx, &pb.GetBillingAccountRequest{Name: "billingAccounts/000000-000000-000000"}); code(err) != codes.NotFound {
		t.Errorf("unknown account = %v", err)
	}
	if _, err := s.GetBillingAccount(ctx, &pb.GetBillingAccountRequest{Name: "billingAccounts/nope"}); code(err) != codes.InvalidArgument {
		t.Errorf("malformed name = %v", err)
	}
}

func TestLinkAndUnlinkStopBillableAPIs(t *testing.T) {
	s, a := newTest(t)
	ctx := context.Background()
	acct := "billingAccounts/" + DefaultAccountID(org)
	info, err := s.GetProjectBillingInfo(ctx, &pb.GetProjectBillingInfoRequest{Name: "projects/p1"})
	if err != nil || info.GetBillingEnabled() || info.GetName() != "projects/p1/billingInfo" {
		t.Fatalf("unlinked info = %v, %v", info, err)
	}
	info, err = s.UpdateProjectBillingInfo(ctx, &pb.UpdateProjectBillingInfoRequest{Name: "projects/p1", ProjectBillingInfo: &pb.ProjectBillingInfo{BillingAccountName: acct}})
	if err != nil || !info.GetBillingEnabled() || info.GetBillingAccountName() != acct {
		t.Fatalf("link = %v, %v", info, err)
	}
	ps, err := s.ListProjectBillingInfo(ctx, &pb.ListProjectBillingInfoRequest{Name: acct})
	if err != nil || len(ps.GetProjectBillingInfo()) != 1 {
		t.Fatalf("projects.list = %v, %v", ps, err)
	}
	if _, err := s.UpdateProjectBillingInfo(ctx, &pb.UpdateProjectBillingInfoRequest{Name: "projects/p1", ProjectBillingInfo: &pb.ProjectBillingInfo{}}); err != nil {
		t.Fatal(err)
	}
	if len(a.enabled["p1"]) != 0 {
		t.Errorf("after unlink the billable APIs are %v", a.enabled["p1"])
	}
	if _, err := s.UpdateProjectBillingInfo(ctx, &pb.UpdateProjectBillingInfoRequest{Name: "projects/p1", ProjectBillingInfo: &pb.ProjectBillingInfo{BillingAccountName: acct}}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(a.enabled["p1"], []string{"run.googleapis.com"}) {
		t.Errorf("after relink the APIs are %v", a.enabled["p1"])
	}
	if _, err := s.GetProjectBillingInfo(ctx, &pb.GetProjectBillingInfoRequest{Name: "projects/nope"}); code(err) != codes.NotFound {
		t.Errorf("unknown project = %v", err)
	}
	// A closed account's projects keep the link with billing disabled.
	if _, err := s.SetOpen(acct, false); err != nil {
		t.Fatal(err)
	}
	if info, _ := s.GetProjectBillingInfo(ctx, &pb.GetProjectBillingInfoRequest{Name: "projects/p1"}); info.GetBillingEnabled() || info.GetBillingAccountName() != acct {
		t.Errorf("on a closed account = %v", info)
	}
}

func TestBudgets(t *testing.T) {
	s, _ := newTest(t)
	b := s.Budgets()
	ctx := context.Background()
	acct := "billingAccounts/" + DefaultAccountID(org)
	usd := func(u int64) *bpb.BudgetAmount {
		return &bpb.BudgetAmount{BudgetAmount: &bpb.BudgetAmount_SpecifiedAmount{SpecifiedAmount: &money.Money{CurrencyCode: "USD", Units: u}}}
	}
	in := &bpb.Budget{DisplayName: "monthly", Amount: usd(100),
		BudgetFilter:      &bpb.Filter{Projects: []string{"projects/p1"}},
		ThresholdRules:    []*bpb.ThresholdRule{{ThresholdPercent: 0.5}, {ThresholdPercent: 1}},
		NotificationsRule: &bpb.NotificationsRule{PubsubTopic: "projects/p1/topics/budget", SchemaVersion: "1.0"}}
	x, err := b.CreateBudget(ctx, &bpb.CreateBudgetRequest{Parent: acct, Budget: in})
	if err != nil {
		t.Fatal(err)
	}
	if x.GetEtag() == "" || x.GetBudgetFilter().GetCreditTypesTreatment() != bpb.Filter_INCLUDE_ALL_CREDITS {
		t.Errorf("created = %v", x)
	}
	if len(Exceeded(x, Spend(x))) != 0 {
		t.Error("a budget with no spend has an exceeded threshold")
	}
	if len(Exceeded(x, 60)) != 1 {
		t.Error("a spend of 60 of 100 should exceed the 50% rule")
	}
	bad := []*bpb.Budget{
		{Amount: nil},
		{DisplayName: string(make([]rune, 61)), Amount: usd(1)},
		{Amount: &bpb.BudgetAmount{BudgetAmount: &bpb.BudgetAmount_SpecifiedAmount{SpecifiedAmount: &money.Money{CurrencyCode: "EUR", Units: 1}}}},
		{Amount: usd(1), ThresholdRules: []*bpb.ThresholdRule{{ThresholdPercent: -1}}},
		{Amount: usd(1), NotificationsRule: &bpb.NotificationsRule{PubsubTopic: "projects/p1/topics/t", SchemaVersion: "2.0"}},
		{Amount: usd(1), NotificationsRule: &bpb.NotificationsRule{MonitoringNotificationChannels: []string{"a", "b", "c", "d", "e", "f"}}},
		{Amount: usd(1), BudgetFilter: &bpb.Filter{CreditTypes: []string{"PROMOTION"}}},
	}
	for i, in := range bad {
		if _, err := b.CreateBudget(ctx, &bpb.CreateBudgetRequest{Parent: acct, Budget: in}); code(err) != codes.InvalidArgument {
			t.Errorf("bad[%d] = %v", i, err)
		}
	}
	up, err := b.UpdateBudget(ctx, &bpb.UpdateBudgetRequest{Budget: &bpb.Budget{Name: x.GetName(), DisplayName: "renamed", Amount: usd(5)},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"display_name"}}})
	if err != nil || up.GetDisplayName() != "renamed" || up.GetAmount().GetSpecifiedAmount().GetUnits() != 100 || up.GetEtag() == x.GetEtag() {
		t.Fatalf("update = %v, %v", up, err)
	}
	if _, err := b.UpdateBudget(ctx, &bpb.UpdateBudgetRequest{Budget: &bpb.Budget{Name: x.GetName(), DisplayName: "y", Etag: x.GetEtag()}}); code(err) != codes.Aborted {
		t.Errorf("a stale etag = %v", err)
	}
	l, err := b.ListBudgets(ctx, &bpb.ListBudgetsRequest{Parent: acct, Scope: "projects/p1"})
	if err != nil || len(l.GetBudgets()) != 1 {
		t.Errorf("scoped list = %v, %v", l, err)
	}
	if l, _ := b.ListBudgets(ctx, &bpb.ListBudgetsRequest{Parent: acct, Scope: "projects/p2"}); len(l.GetBudgets()) != 0 {
		t.Errorf("another project's scope = %v", l)
	}
	if _, err := b.DeleteBudget(ctx, &bpb.DeleteBudgetRequest{Name: x.GetName()}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.GetBudget(ctx, &bpb.GetBudgetRequest{Name: x.GetName()}); code(err) != codes.NotFound {
		t.Errorf("after delete = %v", err)
	}
}

func TestCatalog(t *testing.T) {
	c := &Catalog{APIs: func() []API {
		return []API{{"compute.googleapis.com", "Compute Engine API"}, {"run.googleapis.com", "Cloud Run Admin API"}}
	}}
	ctx := context.Background()
	l, err := c.ListServices(ctx, &pb.ListServicesRequest{})
	if err != nil || len(l.GetServices()) != 2 {
		t.Fatalf("services = %v, %v", l, err)
	}
	if ServiceID("compute.googleapis.com") != "6F81-5844-456A" {
		t.Error("Compute Engine's documented ID")
	}
	sk, err := c.ListSkus(ctx, &pb.ListSkusRequest{Parent: "services/" + ServiceID("run.googleapis.com")})
	if err != nil || len(sk.GetSkus()) != 1 {
		t.Fatalf("skus = %v, %v", sk, err)
	}
	if p := sk.GetSkus()[0].GetPricingInfo()[0].GetPricingExpression().GetTieredRates()[0].GetUnitPrice(); p.GetUnits() != 0 || p.GetNanos() != 0 {
		t.Errorf("a local SKU's price = %v", p)
	}
	if _, err := c.ListSkus(ctx, &pb.ListSkusRequest{Parent: "services/0000-0000-0000"}); code(err) != codes.NotFound {
		t.Errorf("unknown service = %v", err)
	}
}
