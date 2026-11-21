package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	bpb "cloud.google.com/go/billing/budgets/apiv1/budgetspb"
	"google.golang.org/genproto/googleapis/type/money"
)

type published struct {
	mu   sync.Mutex
	msgs []Notification
}

func (p *published) Publish(_ context.Context, topic string, data []byte, attrs map[string]string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msgs = append(p.msgs, Notification{Topic: topic, Data: data, Attributes: attrs})
	return nil
}

func (p *published) take() []Notification {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.msgs
	p.msgs = nil
	return out
}

// TestBudgetNotifications: a budget with a topic gets no message before
// one interval of the clock, then the documented status message on each
// interval, with costAmount 0 and no alertThresholdExceeded; one without
// a topic gets none, and a deleted budget no more.
func TestBudgetNotifications(t *testing.T) {
	s, _ := newTest(t)
	pub := &published{}
	s.Publisher = pub
	ctx := context.Background()
	acct := "billingAccounts/" + DefaultAccountID(org)
	x, err := s.Budgets().CreateBudget(ctx, &bpb.CreateBudgetRequest{Parent: acct, Budget: &bpb.Budget{DisplayName: "quarterly",
		Amount:            &bpb.BudgetAmount{BudgetAmount: &bpb.BudgetAmount_SpecifiedAmount{SpecifiedAmount: &money.Money{CurrencyCode: "USD", Units: 100, Nanos: 500000000}}},
		BudgetFilter:      &bpb.Filter{UsagePeriod: &bpb.Filter_CalendarPeriod{CalendarPeriod: bpb.CalendarPeriod_QUARTER}},
		ThresholdRules:    []*bpb.ThresholdRule{{ThresholdPercent: 0.5}},
		NotificationsRule: &bpb.NotificationsRule{PubsubTopic: "projects/p1/topics/budget", SchemaVersion: "1.0"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Budgets().CreateBudget(ctx, &bpb.CreateBudgetRequest{Parent: acct, Budget: &bpb.Budget{DisplayName: "silent",
		Amount:       &bpb.BudgetAmount{BudgetAmount: &bpb.BudgetAmount_LastPeriodAmount{LastPeriodAmount: &bpb.LastPeriodAmount{}}},
		BudgetFilter: &bpb.Filter{UsagePeriod: &bpb.Filter_CalendarPeriod{CalendarPeriod: bpb.CalendarPeriod_MONTH}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceClock(ctx, DefaultNotifyInterval-time.Minute); err != nil {
		t.Fatal(err)
	}
	if got := pub.take(); len(got) != 0 {
		t.Fatalf("before one interval: %d messages", len(got))
	}
	if _, err := s.AdvanceClock(ctx, 3*DefaultNotifyInterval); err != nil {
		t.Fatal(err)
	}
	got := pub.take()
	if len(got) != 1 {
		t.Fatalf("after the interval: %d messages, want 1 (one per advance)", len(got))
	}
	n := got[0]
	_, id, _ := strings.Cut(x.GetName(), "/budgets/")
	if n.Topic != "projects/p1/topics/budget" || n.Attributes["billingAccountId"] != DefaultAccountID(org) ||
		n.Attributes["budgetId"] != id || n.Attributes["schemaVersion"] != "1.0" || len(n.Attributes) != 3 {
		t.Errorf("message = %s %v", n.Topic, n.Attributes)
	}
	var data map[string]any
	if err := json.Unmarshal(n.Data, &data); err != nil {
		t.Fatal(err)
	}
	start := PeriodStart(x, s.Now()).UTC().Format(time.RFC3339)
	if data["budgetDisplayName"] != "quarterly" || data["costAmount"] != 0.0 || data["budgetAmount"] != 100.5 ||
		data["budgetAmountType"] != "SPECIFIED_AMOUNT" || data["currencyCode"] != "USD" || data["costIntervalStart"] != start {
		t.Errorf("data = %s", n.Data)
	}
	if _, ok := data["alertThresholdExceeded"]; ok {
		t.Errorf("alertThresholdExceeded set with no cost: %s", n.Data)
	}
	if !strings.HasSuffix(start, "T08:00:00Z") {
		t.Errorf("costIntervalStart = %s, want midnight UTC-8", start)
	}
	// The interval, set through the clock extension.
	srv := httptest.NewServer(http.HandlerFunc(s.ServeClock))
	defer srv.Close()
	resp, err := http.Post(srv.URL+ClockAdvancePath, "application/json", strings.NewReader(`{"duration":"0s","budgetNotificationInterval":"60s"}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("clock:advance = %v, %v", resp, err)
	}
	_ = resp.Body.Close()
	resp, err = http.Post(srv.URL+ClockAdvancePath, "application/json", strings.NewReader(`{"duration":"61s"}`))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("clock:advance = %v, %v", resp, err)
	}
	_ = resp.Body.Close()
	if got := pub.take(); len(got) != 1 {
		t.Fatalf("after the new 60s interval: %d messages", len(got))
	}
	if _, err := s.Budgets().DeleteBudget(ctx, &bpb.DeleteBudgetRequest{Name: x.GetName()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdvanceClock(ctx, time.Hour); err != nil {
		t.Fatal(err)
	}
	if got := pub.take(); len(got) != 0 {
		t.Errorf("after delete: %d messages", len(got))
	}
}

func TestPeriodStartAndAmountType(t *testing.T) {
	now := time.Date(2026, 5, 1, 7, 0, 0, 0, time.UTC) // 2026-04-30 23:00 UTC-8
	month := &bpb.Budget{}
	if got := PeriodStart(month, now).UTC().Format(time.RFC3339); got != "2026-04-01T08:00:00Z" {
		t.Errorf("month start = %s", got)
	}
	year := &bpb.Budget{BudgetFilter: &bpb.Filter{UsagePeriod: &bpb.Filter_CalendarPeriod{CalendarPeriod: bpb.CalendarPeriod_YEAR}}}
	if got := PeriodStart(year, now).UTC().Format(time.RFC3339); got != "2026-01-01T08:00:00Z" {
		t.Errorf("year start = %s", got)
	}
	last := &bpb.Budget{Amount: &bpb.BudgetAmount{BudgetAmount: &bpb.BudgetAmount_LastPeriodAmount{LastPeriodAmount: &bpb.LastPeriodAmount{}}},
		BudgetFilter: &bpb.Filter{UsagePeriod: &bpb.Filter_CalendarPeriod{CalendarPeriod: bpb.CalendarPeriod_QUARTER}}}
	if _, kind := budgetAmount(last); kind != "LAST_PERIODS_COST" {
		t.Errorf("quarterly last-period kind = %s", kind)
	}
	last.BudgetFilter.UsagePeriod = &bpb.Filter_CalendarPeriod{CalendarPeriod: bpb.CalendarPeriod_MONTH}
	if _, kind := budgetAmount(last); kind != "LAST_MONTH_COST" {
		t.Errorf("monthly last-period kind = %s", kind)
	}
}
