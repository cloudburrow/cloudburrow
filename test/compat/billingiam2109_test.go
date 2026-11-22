//go:build compat

package compat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	billing "cloud.google.com/go/billing/apiv1"
	pb "cloud.google.com/go/billing/apiv1/billingpb"
	budgets "cloud.google.com/go/billing/budgets/apiv1"
	bpb "cloud.google.com/go/billing/budgets/apiv1/budgetspb"
	"cloud.google.com/go/iam/apiv1/iampb"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"google.golang.org/api/iterator"
	"google.golang.org/genproto/googleapis/type/money"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// advanceBillingClock posts body to the Cloud Billing clock extension on
// the Resource Manager port (#2109).
func advanceBillingClock(t *testing.T, h *Harness, rmEndpoint, body string) {
	t.Helper()
	req, err := http.NewRequestWithContext(h.Context(), http.MethodPost, "http://"+stripScheme(rmEndpoint)+"/cloudburrow/v1/clock:advance", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("clock:advance %s = %s", body, resp.Status)
	}
}

// covers: google.cloud.billing.budgets.v1.BudgetService/CreateBudget, google.pubsub.v1.Subscriber/Pull
//
// TestBillingBudgetNotifications (#2109): a budget with a Pub/Sub topic,
// created with the official Budget API client, gets the documented status
// message on its topic once the Cloud Billing clock passes the
// notification interval (set to 60s through the clock extension), pulled
// with the official Pub/Sub client: attributes billingAccountId, budgetId
// and schemaVersion 1.0, and a JSON body with the budget's name, amount
// and currency, costAmount 0, the period's start at midnight UTC-8, and no
// alertThresholdExceeded (no cost crosses a threshold).
func TestBillingBudgetNotifications(t *testing.T) {
	h := New(t)
	rmEndpoint := h.Endpoint(EnvResourceManager)
	ps := pubsubClient(t, h)
	ctx := h.Context()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1000000000)
	topicName := topic(t, h, ps, "budget-"+suffix)
	sub := subscription(t, h, ps, "budget-sub-"+suffix, topicName)
	bc, err := billing.NewCloudBillingClient(ctx, rmOptionsAt(rmEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Close()
	c, err := budgets.NewBudgetClient(ctx, rmOptionsAt(rmEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	acct := topBillingAccount(t, h, bc)
	x, err := c.CreateBudget(ctx, &bpb.CreateBudgetRequest{Parent: acct.GetName(), Budget: &bpb.Budget{DisplayName: "notify " + suffix,
		Amount:            &bpb.BudgetAmount{BudgetAmount: &bpb.BudgetAmount_SpecifiedAmount{SpecifiedAmount: &money.Money{CurrencyCode: "USD", Units: 250}}},
		ThresholdRules:    []*bpb.ThresholdRule{{ThresholdPercent: 0.5}, {ThresholdPercent: 0.9}},
		NotificationsRule: &bpb.NotificationsRule{PubsubTopic: topicName, SchemaVersion: "1.0"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.DeleteBudget(ctx, &bpb.DeleteBudgetRequest{Name: x.GetName()}) })
	pull := func() []*pubsubpb.ReceivedMessage {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		resp, err := ps.SubscriptionAdminClient.Pull(pctx, &pubsubpb.PullRequest{Subscription: sub, MaxMessages: 10})
		if err != nil {
			return nil
		}
		var ids []string
		for _, m := range resp.GetReceivedMessages() {
			ids = append(ids, m.GetAckId())
		}
		if len(ids) > 0 {
			_ = ps.SubscriptionAdminClient.Acknowledge(ctx, &pubsubpb.AcknowledgeRequest{Subscription: sub, AckIds: ids})
		}
		return resp.GetReceivedMessages()
	}
	// Before the interval passes, nothing.
	advanceBillingClock(t, h, rmEndpoint, `{"duration":"0s","budgetNotificationInterval":"60s"}`)
	if got := pull(); len(got) != 0 {
		t.Fatalf("before the interval: %d messages", len(got))
	}
	advanceBillingClock(t, h, rmEndpoint, `{"duration":"61s"}`)
	var got []*pubsubpb.ReceivedMessage
	for i := 0; i < 10 && len(got) == 0; i++ {
		got = pull()
	}
	if len(got) != 1 {
		t.Fatalf("after the interval: %d messages, want 1", len(got))
	}
	m := got[0].GetMessage()
	_, id, _ := strings.Cut(x.GetName(), "/budgets/")
	attrs := m.GetAttributes()
	if attrs["billingAccountId"] != strings.TrimPrefix(acct.GetName(), "billingAccounts/") || attrs["budgetId"] != id || attrs["schemaVersion"] != "1.0" {
		t.Errorf("attributes = %v", attrs)
	}
	var data map[string]any
	if err := json.Unmarshal(m.GetData(), &data); err != nil {
		t.Fatalf("data %q: %v", m.GetData(), err)
	}
	start, _ := data["costIntervalStart"].(string)
	if data["budgetDisplayName"] != "notify "+suffix || data["costAmount"] != 0.0 || data["budgetAmount"] != 250.0 ||
		data["budgetAmountType"] != "SPECIFIED_AMOUNT" || data["currencyCode"] != "USD" || !strings.HasSuffix(start, "-01T08:00:00Z") {
		t.Errorf("data = %s", m.GetData())
	}
	if _, ok := data["alertThresholdExceeded"]; ok {
		t.Errorf("alertThresholdExceeded with no cost: %s", m.GetData())
	}
	// The next interval, the next status message.
	advanceBillingClock(t, h, rmEndpoint, `{"duration":"61s"}`)
	got = nil
	for i := 0; i < 10 && len(got) == 0; i++ {
		got = pull()
	}
	if len(got) != 1 {
		t.Errorf("after the second interval: %d messages, want 1", len(got))
	}
	advanceBillingClock(t, h, rmEndpoint, `{"duration":"0s","budgetNotificationInterval":"21600s"}`)
}

// billingDenied is Cloud Billing's refusal.
const billingDenied = "The caller does not have permission"

// covers: google.cloud.billing.v1.CloudBilling/GetBillingAccount, google.cloud.billing.v1.CloudBilling/ListBillingAccounts, google.cloud.billing.v1.CloudBilling/TestIamPermissions, google.cloud.billing.v1.CloudBilling/UpdateProjectBillingInfo, google.cloud.billing.budgets.v1.BudgetService/ListBudgets, google.cloud.billing.budgets.v1.BudgetService/CreateBudget
//
// TestIAMEnforcementBilling (#2109), with the official Cloud Billing and
// Budget API clients and a minted token: without a role on a subaccount,
// GetBillingAccount is PERMISSION_DENIED (billing.accounts.get), listing
// leaves it out, TestIamPermissions answers nothing and ListBudgets is
// refused (billing.budgets.list); Billing Account Viewer on it lets them
// through. Linking a project needs both
// resourcemanager.projects.createBillingAssignment on the project (Project
// Billing Manager) and billing.resourceAssociations.create on the account
// (Billing Account User); unlinking the first's
// resourcemanager.projects.deleteBillingAssignment. A budget needs
// billing.budgets.create (Billing Account Costs Manager), and one with a
// Pub/Sub topic pubsub.topics.setIamPolicy on the topic (Pub/Sub Admin).
func TestIAMEnforcementBilling(t *testing.T) {
	h := New(t)
	rmEndpoint := h.Endpoint(EnvResourceManager)
	h.Endpoint(EnvMetadata)
	requireIAMEnforcement(t)
	ctx := h.Context()
	project := iamProject(t, h, "iobill-")
	p := newIAMPrincipal(t, h, project, "iobill")
	member := "serviceAccount:" + p.email
	owner, err := billing.NewCloudBillingClient(ctx, rmOptionsAt(rmEndpoint)...)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	as, err := billing.NewCloudBillingClient(ctx, grpcAs(rmEndpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	defer as.Close()
	asBudgets, err := budgets.NewBudgetClient(ctx, grpcAs(rmEndpoint, p.token)...)
	if err != nil {
		t.Fatal(err)
	}
	defer asBudgets.Close()
	acct := billingSubaccount(t, h, "iam enforcement")
	grantOnAccount := func(role string) {
		t.Helper()
		pol, err := owner.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: acct})
		if err != nil {
			t.Fatal(err)
		}
		pol.Bindings = append(pol.Bindings, &iampb.Binding{Role: role, Members: []string{member}})
		if _, err := owner.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: acct, Policy: pol}); err != nil {
			t.Fatal(err)
		}
	}
	lists := func() bool {
		it := as.ListBillingAccounts(ctx, &pb.ListBillingAccountsRequest{})
		for {
			a, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return false
			}
			if err != nil {
				t.Fatalf("ListBillingAccounts: %v", err)
			}
			if a.GetName() == acct {
				return true
			}
		}
	}
	get := func() error {
		_, err := as.GetBillingAccount(ctx, &pb.GetBillingAccountRequest{Name: acct})
		return err
	}
	listBudgets := func() error {
		_, err := asBudgets.ListBudgets(ctx, &bpb.ListBudgetsRequest{Parent: acct}).Next()
		if errors.Is(err, iterator.Done) {
			return nil
		}
		return err
	}
	tested := func() []string {
		r, err := as.TestIamPermissions(ctx, &iampb.TestIamPermissionsRequest{Resource: acct,
			Permissions: []string{"billing.accounts.get", "billing.accounts.update"}})
		if err != nil {
			t.Fatalf("TestIamPermissions: %v", err)
		}
		return r.GetPermissions()
	}
	if _, err := owner.GetBillingAccount(ctx, &pb.GetBillingAccountRequest{Name: acct}); err != nil {
		t.Fatalf("the instance's own account: GetBillingAccount = %v", err)
	}
	wantDenied(t, "GetBillingAccount without a role", get(), billingDenied, "billing.accounts.get", true)
	wantDenied(t, "ListBudgets without a role", listBudgets(), billingDenied, "billing.budgets.list", true)
	if lists() {
		t.Error("ListBillingAccounts without a role lists the account")
	}
	if got := tested(); len(got) != 0 {
		t.Errorf("TestIamPermissions without a role = %v", got)
	}

	grantOnAccount("roles/billing.viewer")
	waitGranted(t, "GetBillingAccount as a Billing Account Viewer", get)
	if err := listBudgets(); err != nil {
		t.Errorf("ListBudgets as a Billing Account Viewer: %v", err)
	}
	if !lists() {
		t.Error("ListBillingAccounts as a Billing Account Viewer leaves the account out")
	}
	if got := tested(); len(got) != 1 || got[0] != "billing.accounts.get" {
		t.Errorf("TestIamPermissions as a Billing Account Viewer = %v; want billing.accounts.get alone", got)
	}

	link := func(account string) error {
		_, err := as.UpdateProjectBillingInfo(ctx, &pb.UpdateProjectBillingInfoRequest{Name: "projects/" + project,
			ProjectBillingInfo: &pb.ProjectBillingInfo{BillingAccountName: account}})
		return err
	}
	wantDenied(t, "linking without a project role", link(acct), billingDenied, "resourcemanager.projects.createBillingAssignment", true)
	p.grant("roles/billing.projectManager")
	t.Cleanup(func() { p.revoke("roles/billing.projectManager") })
	wantDenied(t, "linking as a Project Billing Manager alone", link(acct), billingDenied, "billing.resourceAssociations.create", true)
	grantOnAccount("roles/billing.user")
	waitGranted(t, "linking as a Project Billing Manager and Billing Account User", func() error { return link(acct) })
	info, err := owner.GetProjectBillingInfo(ctx, &pb.GetProjectBillingInfoRequest{Name: "projects/" + project})
	if err != nil || info.GetBillingAccountName() != acct || !info.GetBillingEnabled() {
		t.Errorf("after the link: %v, %v", info, err)
	}
	if err := link(""); err != nil {
		t.Errorf("unlinking as a Project Billing Manager: %v", err)
	}

	// A budget's Pub/Sub topic needs pubsub.topics.setIamPolicy on it,
	// which Pub/Sub answers, besides billing.budgets.create.
	ps := pubsubClient(t, h)
	// In the test's project, where the principal is granted Pub/Sub Admin.
	topicName := "projects/" + project + "/topics/iobill-" + fmt.Sprint(time.Now().UnixNano()%1000000000)
	// Unlinking disabled the project's billable APIs, Pub/Sub among them
	// ("Any billable activity of your in-use services will stop"), so the
	// topic is SERVICE_DISABLED until the project is linked again, which
	// enables them again.
	// Pub/Sub's gate in the cluster caches the API states for a second
	// (apienable.CacheFor), so each is waited for.
	disabled := func(err error) bool {
		return status.Code(err) == codes.PermissionDenied && strings.Contains(err.Error(), "SERVICE_DISABLED")
	}
	for i := 0; i < 25; i++ {
		probe := fmt.Sprintf("%s-off%d", topicName, i)
		if _, err = ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: probe}); disabled(err) {
			break
		}
		if err == nil {
			_ = ps.TopicAdminClient.DeleteTopic(ctx, &pubsubpb.DeleteTopicRequest{Topic: probe})
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !disabled(err) {
		t.Fatalf("CreateTopic with billing disabled = %v; want PERMISSION_DENIED with SERVICE_DISABLED", err)
	}
	if err := link(acct); err != nil {
		t.Fatalf("linking again as a Project Billing Manager and Billing Account User: %v", err)
	}
	for i := 0; i < 25; i++ {
		if _, err = ps.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topicName}); !disabled(err) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("CreateTopic once billing is linked again: %v", err)
	}
	t.Cleanup(func() {
		_ = ps.TopicAdminClient.DeleteTopic(context.Background(), &pubsubpb.DeleteTopicRequest{Topic: topicName})
	})
	create := func() error {
		x, err := asBudgets.CreateBudget(ctx, &bpb.CreateBudgetRequest{Parent: acct, Budget: &bpb.Budget{DisplayName: "iam",
			Amount:            &bpb.BudgetAmount{BudgetAmount: &bpb.BudgetAmount_SpecifiedAmount{SpecifiedAmount: &money.Money{CurrencyCode: "USD", Units: 10}}},
			NotificationsRule: &bpb.NotificationsRule{PubsubTopic: topicName, SchemaVersion: "1.0"}}})
		if err == nil {
			_ = asBudgets.DeleteBudget(ctx, &bpb.DeleteBudgetRequest{Name: x.GetName()})
		}
		return err
	}
	wantDenied(t, "CreateBudget as a Billing Account Viewer", create(), billingDenied, "billing.budgets.create", true)
	grantOnAccount("roles/billing.costsManager")
	waitGranted(t, "a budget without a topic as a Billing Account Costs Manager", func() error {
		x, err := asBudgets.CreateBudget(ctx, &bpb.CreateBudgetRequest{Parent: acct, Budget: &bpb.Budget{DisplayName: "iam",
			Amount: &bpb.BudgetAmount{BudgetAmount: &bpb.BudgetAmount_SpecifiedAmount{SpecifiedAmount: &money.Money{CurrencyCode: "USD", Units: 10}}}}})
		if err == nil {
			_ = asBudgets.DeleteBudget(ctx, &bpb.DeleteBudgetRequest{Name: x.GetName()})
		}
		return err
	})
	wantDenied(t, "a budget with a topic, without Pub/Sub Admin", create(), billingDenied, "pubsub.topics.setIamPolicy", true)
	p.grant("roles/pubsub.admin")
	t.Cleanup(func() { p.revoke("roles/pubsub.admin") })
	waitGranted(t, "a budget with a topic as a Pub/Sub Admin", create)
}
