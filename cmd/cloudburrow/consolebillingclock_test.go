package main

import (
	"context"
	"sync"
	"testing"

	budgetspb "cloud.google.com/go/billing/budgets/apiv1/budgetspb"
	"google.golang.org/genproto/googleapis/type/money"

	"github.com/cloudburrow/cloudburrow/internal/service/billing"
	"github.com/cloudburrow/cloudburrow/internal/store"
)

type billingTopics struct {
	mu     sync.Mutex
	topics []string
}

func (b *billingTopics) Publish(_ context.Context, topic string, _ []byte, _ map[string]string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.topics = append(b.topics, topic)
	return nil
}

type anyProject struct{}

func (anyProject) Resolve(s string) (string, error) { return s, nil }

// TestConsoleBillingAdvanceClock: Budgets & alerts' Advance Cloud Billing
// clock (#2109) sets the interval and publishes the status message due.
func TestConsoleBillingAdvanceClock(t *testing.T) {
	s := billing.NewServer(store.NewMemory(), anyProject{}, "organizations/1")
	pub := &billingTopics{}
	s.Publisher = pub
	ctx := context.Background()
	acct := "billingAccounts/" + billing.DefaultAccountID("organizations/1")
	if _, err := s.Budgets().CreateBudget(ctx, &budgetspb.CreateBudgetRequest{Parent: acct, Budget: &budgetspb.Budget{DisplayName: "b",
		Amount:            &budgetspb.BudgetAmount{BudgetAmount: &budgetspb.BudgetAmount_SpecifiedAmount{SpecifiedAmount: &money.Money{CurrencyCode: "USD", Units: 1}}},
		NotificationsRule: &budgetspb.NotificationsRule{PubsubTopic: "projects/p/topics/t", SchemaVersion: "1.0"}}}); err != nil {
		t.Fatal(err)
	}
	p := billingBudgetsProvider{billingConsole{srv: s}}
	if len(p.ListActions(ctx, "p")) != 1 {
		t.Fatal("no Advance clock action")
	}
	out, err := p.ActAtResult(ctx, "p", nil, actBillingClock, map[string]string{"duration": "2m", "interval": "1m"})
	if err != nil || out == nil || out.Total != 1 {
		t.Fatalf("advance = %v, %v", out, err)
	}
	if len(pub.topics) != 1 || pub.topics[0] != "projects/p/topics/t" {
		t.Errorf("published = %v", pub.topics)
	}
	if _, err := p.ActAtResult(ctx, "p", nil, actBillingClock, map[string]string{"duration": "-1h"}); err == nil {
		t.Error("a negative advance was accepted")
	}
}
