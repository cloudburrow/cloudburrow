package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	bpb "cloud.google.com/go/billing/budgets/apiv1/budgetspb"
)

// Budget programmatic notifications (#2109). A budget whose
// notifications_rule names a Pub/Sub topic gets "the current status of
// your budget" on that topic "multiple times per day", even with no
// usage, not only when a threshold is crossed, and the first "may take
// several hours" (https://docs.cloud.google.com/billing/docs/how-to/budgets-programmatic-notifications,
// read 2026-11-21). Google gives no exact interval: here a budget's
// notification is due every NotifyInterval of the Cloud Billing clock,
// the first one interval after its topic is set. The clock is moved
// forward by ClockAdvancePath, on the Resource Manager port, for tests and
// the console: Google has no such call.

// DefaultNotifyInterval is how often a budget's status is published: 6
// hours, four messages a day, Google's "multiple times per day".
const DefaultNotifyInterval = 6 * time.Hour

// Publisher publishes one message on a Pub/Sub topic
// ("projects/P/topics/T") of the instance.
type Publisher interface {
	Publish(ctx context.Context, topic string, data []byte, attrs map[string]string) error
}

// ClockPath is GET: {"now", "advancedBy", "budgetNotificationInterval"}.
const ClockPath = "/cloudburrow/v1/clock"

// ClockAdvancePath is POST {"duration": "21600s"}: the Cloud Billing clock
// moves that far forward (never back), and the budget notifications now
// due are published, one per budget whatever the number of intervals
// passed. A duration of 0s only reads the clock. The body may also set
// "budgetNotificationInterval" ("3600s"): each scheduled notification is
// then due one new interval from now.
const ClockAdvancePath = "/cloudburrow/v1/clock:advance"

// pacific is the zone every budget period begins in: "All calendar and
// custom date times begin at 12 AM US and Canadian Pacific Time (UTC-8)"
// (https://docs.cloud.google.com/billing/docs/how-to/budgets, read
// 2026-11-21).
var pacific = time.FixedZone("UTC-8", -8*60*60)

// Now is the Cloud Billing clock: the wall clock, advanced.
func (s *Server) Now() time.Time { return s.now().Add(time.Duration(s.clockOffset.Load())) }

// AdvancedBy is how far the clock has been moved forward.
func (s *Server) AdvancedBy() time.Duration { return time.Duration(s.clockOffset.Load()) }

func (s *Server) interval() time.Duration {
	if d := time.Duration(s.notifyInterval.Load()); d > 0 {
		return d
	}
	return DefaultNotifyInterval
}

// SetNotifyInterval sets the interval between a budget's notifications;
// every scheduled one is then due d from now.
func (s *Server) SetNotifyInterval(d time.Duration) {
	s.notifyInterval.Store(int64(d))
	now := s.Now()
	s.dueMu.Lock()
	defer s.dueMu.Unlock()
	for k, v := range s.due {
		v.at = now.Add(d)
		s.due[k] = v
	}
}

// AdvanceClock moves the clock d forward and publishes the notifications
// now due; it returns the new time.
func (s *Server) AdvanceClock(ctx context.Context, d time.Duration) (time.Time, error) {
	if d < 0 {
		return time.Time{}, fmt.Errorf("duration %s is negative: the clock only moves forward", d)
	}
	s.clockOffset.Add(int64(d))
	if d > 0 {
		s.NotifyDue(ctx)
	}
	return s.Now(), nil
}

// schedule sets budget name's next notification one interval from now
// when its topic is set or changed, and forgets it when there is none.
// Callers hold s.mu.
func (s *Server) schedule(name, topic string) {
	s.dueMu.Lock()
	defer s.dueMu.Unlock()
	if s.due == nil {
		s.due = map[string]dueAt{}
	}
	if topic == "" {
		delete(s.due, name)
		return
	}
	if cur, ok := s.due[name]; ok && cur.topic == topic {
		return
	}
	s.due[name] = dueAt{topic: topic, at: s.Now().Add(s.interval())}
}

type dueAt struct {
	topic string
	at    time.Time
}

// Notification is one budget's status message: its data (Google's JSON
// object) and attributes.
type Notification struct {
	Topic      string
	Data       []byte
	Attributes map[string]string
}

// notificationData is the documented message body. alertThresholdExceeded
// and forecastThresholdExceeded are omitted when no threshold is crossed,
// as documented.
type notificationData struct {
	BudgetDisplayName         string   `json:"budgetDisplayName"`
	AlertThresholdExceeded    *float64 `json:"alertThresholdExceeded,omitempty"`
	ForecastThresholdExceeded *float64 `json:"forecastThresholdExceeded,omitempty"`
	CostAmount                float64  `json:"costAmount"`
	CostIntervalStart         string   `json:"costIntervalStart"`
	BudgetAmount              float64  `json:"budgetAmount"`
	BudgetAmountType          string   `json:"budgetAmountType"`
	CurrencyCode              string   `json:"currencyCode"`
}

// PeriodStart is the start of x's current budget period at now: the
// custom period's start date, or the first day of the calendar month
// (the default), quarter or year, at midnight UTC-8.
func PeriodStart(x *bpb.Budget, now time.Time) time.Time {
	f := x.GetBudgetFilter()
	if cp := f.GetCustomPeriod(); cp != nil && cp.GetStartDate() != nil {
		d := cp.GetStartDate()
		return time.Date(int(d.GetYear()), time.Month(d.GetMonth()), int(d.GetDay()), 0, 0, 0, 0, pacific)
	}
	t := now.In(pacific)
	switch f.GetCalendarPeriod().String() {
	case "QUARTER":
		return time.Date(t.Year(), time.Month((int(t.Month())-1)/3*3+1), 1, 0, 0, 0, 0, pacific)
	case "YEAR":
		return time.Date(t.Year(), time.January, 1, 0, 0, 0, 0, pacific)
	}
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, pacific)
}

// budgetAmount is x's amount and its documented type: SPECIFIED_AMOUNT,
// or the last period's cost (0 here, as every cost is), LAST_MONTH_COST
// for a monthly budget and LAST_PERIODS_COST otherwise.
func budgetAmount(x *bpb.Budget) (float64, string) {
	if m := x.GetAmount().GetSpecifiedAmount(); m != nil {
		return float64(m.GetUnits()) + float64(m.GetNanos())/1e9, "SPECIFIED_AMOUNT"
	}
	if p := x.GetBudgetFilter().GetCalendarPeriod().String(); p == "MONTH" || (p == "CALENDAR_PERIOD_UNSPECIFIED" && x.GetBudgetFilter().GetCustomPeriod() == nil) {
		return 0, "LAST_MONTH_COST"
	}
	return 0, "LAST_PERIODS_COST"
}

// highest is the highest threshold percent of rules, nil for none.
func highest(rules []*bpb.ThresholdRule) *float64 {
	var out *float64
	for _, r := range rules {
		v := r.GetThresholdPercent()
		if out == nil || v > *out {
			out = &v
		}
	}
	return out
}

// StatusMessage is x's notification at now: its spend (0, Spend), the
// highest actual threshold it exceeds (none at 0), and its account's
// currency.
func StatusMessage(x *bpb.Budget, currency string, now time.Time) (Notification, error) {
	aid, id, err := budgetName(x.GetName())
	if err != nil {
		return Notification{}, err
	}
	amount, kind := budgetAmount(x)
	spend := Spend(x)
	d := notificationData{
		BudgetDisplayName: x.GetDisplayName(), CostAmount: spend,
		CostIntervalStart: PeriodStart(x, now).UTC().Format(time.RFC3339),
		BudgetAmount:      amount, BudgetAmountType: kind, CurrencyCode: currency,
		AlertThresholdExceeded: highest(Exceeded(x, spend)),
		// Forecasted spend is 0 too: no forecasted threshold is crossed.
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return Notification{}, err
	}
	return Notification{Topic: x.GetNotificationsRule().GetPubsubTopic(), Data: raw,
		Attributes: map[string]string{"billingAccountId": aid, "budgetId": id, "schemaVersion": "1.0"}}, nil
}

// due are the notifications due at the clock's now, each budget's next
// moved past it. Budgets not yet scheduled (an instance restarted) are
// scheduled one interval on.
func (s *Server) dueNotifications() []Notification {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys, err := s.st.List(budgetPrefix)
	if err != nil {
		return nil
	}
	sort.Strings(keys)
	now := s.Now()
	b := s.Budgets()
	var out []Notification
	seen := map[string]bool{}
	for _, k := range keys {
		aid, id, ok := strings.Cut(strings.TrimPrefix(k, budgetPrefix), "/")
		if !ok {
			continue
		}
		x, err := b.read(aid, id)
		if err != nil {
			continue
		}
		topic := x.GetNotificationsRule().GetPubsubTopic()
		if topic == "" {
			continue
		}
		seen[x.GetName()] = true
		s.dueMu.Lock()
		if s.due == nil {
			s.due = map[string]dueAt{}
		}
		d, ok := s.due[x.GetName()]
		if !ok || d.topic != topic {
			s.due[x.GetName()] = dueAt{topic: topic, at: now.Add(s.interval())}
			s.dueMu.Unlock()
			continue
		}
		if now.Before(d.at) {
			s.dueMu.Unlock()
			continue
		}
		for !now.Before(d.at) {
			d.at = d.at.Add(s.interval())
		}
		s.due[x.GetName()] = d
		s.dueMu.Unlock()
		currency := Currency
		var acct account
		if found, err := s.get(accountPrefix+aid, &acct); err == nil && found && acct.Currency != "" {
			currency = acct.Currency
		}
		n, err := StatusMessage(x, currency, now)
		if err == nil {
			out = append(out, n)
		}
	}
	s.dueMu.Lock()
	for name := range s.due {
		if !seen[name] {
			delete(s.due, name)
		}
	}
	s.dueMu.Unlock()
	return out
}

// NotifyDue publishes every notification now due. A topic that does not
// exist, or an instance without Pub/Sub, loses that message, as a topic
// Cloud Billing may not publish to does.
func (s *Server) NotifyDue(ctx context.Context) {
	ns := s.dueNotifications()
	if s.Publisher == nil {
		return
	}
	for _, n := range ns {
		if err := s.Publisher.Publish(ctx, n.Topic, n.Data, n.Attributes); err != nil {
			log.Printf("billing: budget notification to %s: %v", n.Topic, err)
		}
	}
}

// RunNotifications publishes the notifications due on the wall clock,
// checking every minute, until ctx ends.
func (s *Server) RunNotifications(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.NotifyDue(ctx)
		}
	}
}

// ServeClock serves ClockPath and ClockAdvancePath.
func (s *Server) ServeClock(w http.ResponseWriter, r *http.Request) {
	reply := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	fail := func(code int, msg string) {
		reply(code, map[string]any{"error": map[string]any{"code": code, "message": msg}})
	}
	read := func(now time.Time) {
		reply(http.StatusOK, map[string]any{"now": now.UTC().Format(time.RFC3339Nano),
			"advancedBy":                 fmt.Sprintf("%ds", int64(s.AdvancedBy()/time.Second)),
			"budgetNotificationInterval": fmt.Sprintf("%ds", int64(s.interval()/time.Second))})
	}
	switch {
	case r.URL.Path == ClockPath && r.Method == http.MethodGet:
		read(s.Now())
		return
	case r.URL.Path != ClockAdvancePath || r.Method != http.MethodPost:
		fail(http.StatusMethodNotAllowed, "GET "+ClockPath+", or POST "+ClockAdvancePath+" with {\"duration\": \"21600s\"}")
		return
	}
	var body struct {
		Duration string `json:"duration"`
		// BudgetNotificationInterval, when set, is the new interval
		// between a budget's notifications, from now on.
		BudgetNotificationInterval string `json:"budgetNotificationInterval"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(http.StatusBadRequest, "the body is not JSON: "+err.Error())
		return
	}
	d, err := time.ParseDuration(body.Duration)
	if err != nil {
		fail(http.StatusBadRequest, fmt.Sprintf("duration %q is not a duration such as 21600s", body.Duration))
		return
	}
	if body.BudgetNotificationInterval != "" {
		iv, err := time.ParseDuration(body.BudgetNotificationInterval)
		if err != nil || iv < time.Second {
			fail(http.StatusBadRequest, fmt.Sprintf("budgetNotificationInterval %q is not a duration of at least 1s", body.BudgetNotificationInterval))
			return
		}
		s.SetNotifyInterval(iv)
	}
	now, err := s.AdvanceClock(r.Context(), d)
	if err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	read(now)
}
