package billing

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	bpb "cloud.google.com/go/billing/budgets/apiv1/budgetspb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/cloudburrow/cloudburrow/internal/paging"
)

// Budgets is the Budget API v1 (BudgetService), over the same accounts.
type Budgets struct {
	bpb.UnimplementedBudgetServiceServer
	s *Server
}

// Budgets is the Budget API of the same accounts.
func (s *Server) Budgets() *Budgets { return &Budgets{s: s} }

var (
	topicName   = regexp.MustCompile(`^projects/[^/]+/topics/[^/]+$`)
	channelName = regexp.MustCompile(`^projects/[^/]+/notificationChannels/[^/]+$`)
	budgetID    = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
)

func newBudgetID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// budgetName parses "billingAccounts/ID/budgets/BUDGET".
func budgetName(name string) (account, id string, err error) {
	acct, rest, ok := strings.Cut(strings.TrimPrefix(name, "billingAccounts/"), "/budgets/")
	if !ok || !strings.HasPrefix(name, "billingAccounts/") || !accountID.MatchString(acct) || !budgetID.MatchString(rest) {
		return "", "", status.Errorf(codes.InvalidArgument, "Invalid budget name %q: want billingAccounts/{billing_account_id}/budgets/{budget_id}", name)
	}
	return acct, rest, nil
}

func budgetKey(account, id string) string { return budgetPrefix + account + "/" + id }

// validate checks a budget against the Budget API's documented rules, for
// an account in currency.
func (b *Budgets) validate(in *bpb.Budget, acct *account, accountName string) error {
	if n := len([]rune(in.GetDisplayName())); n > 60 {
		return status.Errorf(codes.InvalidArgument, "display_name must be at most 60 characters, got %d", n)
	}
	amt := in.GetAmount()
	if amt == nil || (amt.GetSpecifiedAmount() == nil && amt.GetLastPeriodAmount() == nil) {
		return status.Error(codes.InvalidArgument, "amount is required: set specified_amount or last_period_amount")
	}
	f := in.GetBudgetFilter()
	custom := f.GetCustomPeriod() != nil
	if m := amt.GetSpecifiedAmount(); m != nil {
		if c := m.GetCurrencyCode(); c != "" && c != acct.Currency {
			return status.Errorf(codes.InvalidArgument, "specified_amount's currency_code %q must be the billing account's currency, %s", c, acct.Currency)
		}
		if m.GetUnits() < 0 || m.GetNanos() < 0 {
			return status.Error(codes.InvalidArgument, "specified_amount must not be negative")
		}
		if m.GetNanos() >= 1e9 {
			return status.Error(codes.InvalidArgument, "specified_amount's nanos must be less than 1e9")
		}
	}
	if amt.GetLastPeriodAmount() != nil && custom {
		return status.Error(codes.InvalidArgument, "last_period_amount cannot be used with a custom_period; use a calendar_period")
	}
	for i, r := range in.GetThresholdRules() {
		if r.GetThresholdPercent() < 0 {
			return status.Errorf(codes.InvalidArgument, "threshold_rules[%d].threshold_percent must not be negative", i)
		}
		if r.GetSpendBasis() == bpb.ThresholdRule_FORECASTED_SPEND && custom {
			return status.Errorf(codes.InvalidArgument, "threshold_rules[%d]: FORECASTED_SPEND requires a calendar_period, not a custom_period", i)
		}
	}
	for _, p := range f.GetProjects() {
		id, ok := strings.CutPrefix(p, "projects/")
		if !ok || id == "" || strings.Contains(id, "/") {
			return status.Errorf(codes.InvalidArgument, "Invalid budget_filter.projects entry %q: want projects/{project}", p)
		}
	}
	for _, r := range f.GetResourceAncestors() {
		if !strings.HasPrefix(r, "folders/") && !strings.HasPrefix(r, "organizations/") {
			return status.Errorf(codes.InvalidArgument, "Invalid budget_filter.resource_ancestors entry %q: want folders/{folder} or organizations/{organization}", r)
		}
	}
	if len(f.GetCreditTypes()) > 0 && f.GetCreditTypesTreatment() != bpb.Filter_INCLUDE_SPECIFIED_CREDITS {
		return status.Error(codes.InvalidArgument, "budget_filter.credit_types may be set only with credit_types_treatment INCLUDE_SPECIFIED_CREDITS")
	}
	for _, sv := range f.GetServices() {
		id, ok := strings.CutPrefix(sv, "services/")
		if !ok || id == "" || strings.Contains(id, "/") {
			return status.Errorf(codes.InvalidArgument, "Invalid budget_filter.services entry %q: want services/{service_id}", sv)
		}
	}
	for _, sub := range f.GetSubaccounts() {
		_, a, err := b.s.load(sub)
		if err != nil {
			return status.Errorf(codes.InvalidArgument, "budget_filter.subaccounts entry %q: %v", sub, status.Convert(err).Message())
		}
		if a.Master != accountName {
			return status.Errorf(codes.InvalidArgument, "budget_filter.subaccounts entry %q is not a subaccount of %s", sub, accountName)
		}
	}
	if len(f.GetLabels()) > 1 {
		return status.Error(codes.InvalidArgument, "budget_filter.labels may hold a single label key")
	}
	for k, v := range f.GetLabels() {
		if len(v.GetValues()) != 1 {
			return status.Errorf(codes.InvalidArgument, "budget_filter.labels[%q] must hold exactly one value", k)
		}
	}
	if cp := f.GetCustomPeriod(); cp != nil {
		sd := cp.GetStartDate()
		if sd == nil {
			return status.Error(codes.InvalidArgument, "budget_filter.custom_period.start_date is required")
		}
		start := time.Date(int(sd.GetYear()), time.Month(sd.GetMonth()), int(sd.GetDay()), 0, 0, 0, 0, time.UTC)
		if sd.GetYear() == 0 || sd.GetMonth() == 0 || sd.GetDay() == 0 || !start.After(time.Date(2017, 1, 1, 0, 0, 0, 0, time.UTC)) {
			return status.Error(codes.InvalidArgument, "budget_filter.custom_period.start_date must be a full date after January 1, 2017")
		}
		if ed := cp.GetEndDate(); ed != nil {
			end := time.Date(int(ed.GetYear()), time.Month(ed.GetMonth()), int(ed.GetDay()), 0, 0, 0, 0, time.UTC)
			if end.Before(start) {
				return status.Error(codes.InvalidArgument, "budget_filter.custom_period.end_date must not be before start_date")
			}
		}
	}
	nr := in.GetNotificationsRule()
	if t := nr.GetPubsubTopic(); t != "" {
		if !topicName.MatchString(t) {
			return status.Errorf(codes.InvalidArgument, "Invalid notifications_rule.pubsub_topic %q: want projects/{project_id}/topics/{topic_id}", t)
		}
		if nr.GetSchemaVersion() != "1.0" {
			return status.Errorf(codes.InvalidArgument, "notifications_rule.schema_version must be \"1.0\" with a pubsub_topic, got %q", nr.GetSchemaVersion())
		}
	} else if v := nr.GetSchemaVersion(); v != "" && v != "1.0" {
		return status.Errorf(codes.InvalidArgument, "notifications_rule.schema_version: only \"1.0\" is accepted, got %q", v)
	}
	if n := len(nr.GetMonitoringNotificationChannels()); n > 5 {
		return status.Errorf(codes.InvalidArgument, "notifications_rule.monitoring_notification_channels: a maximum of 5 are allowed, got %d", n)
	}
	for _, c := range nr.GetMonitoringNotificationChannels() {
		if !channelName.MatchString(c) {
			return status.Errorf(codes.InvalidArgument, "Invalid notification channel %q: want projects/{project_id}/notificationChannels/{channel_id}", c)
		}
	}
	return nil
}

// normalize fills what Google fills: the filter's credit treatment
// defaults to INCLUDE_ALL_CREDITS.
func normalize(in *bpb.Budget) {
	if in.BudgetFilter == nil {
		in.BudgetFilter = &bpb.Filter{}
	}
	if in.BudgetFilter.CreditTypesTreatment == bpb.Filter_CREDIT_TYPES_TREATMENT_UNSPECIFIED {
		in.BudgetFilter.CreditTypesTreatment = bpb.Filter_INCLUDE_ALL_CREDITS
	}
}

func etag() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (b *Budgets) read(account, id string) (*bpb.Budget, error) {
	raw, err := b.s.st.Get(budgetKey(account, id))
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "Budget billingAccounts/%s/budgets/%s not found.", account, id)
	}
	out := &bpb.Budget{}
	if err := protojson.Unmarshal(raw, out); err != nil {
		return nil, status.Errorf(codes.Internal, "billing: decode budget: %v", err)
	}
	return out, nil
}

func (b *Budgets) write(account, id string, in *bpb.Budget) error {
	raw, err := protojson.Marshal(in)
	if err != nil {
		return err
	}
	return b.s.st.Put(budgetKey(account, id), raw)
}

// CreateBudget is billingAccounts.budgets.create.
func (b *Budgets) CreateBudget(_ context.Context, req *bpb.CreateBudgetRequest) (*bpb.Budget, error) {
	if req.GetBudget() == nil {
		return nil, status.Error(codes.InvalidArgument, "budget is required")
	}
	in := proto.Clone(req.GetBudget()).(*bpb.Budget)
	b.s.mu.Lock()
	defer b.s.mu.Unlock()
	aid, a, err := b.s.load(req.GetParent())
	if err != nil {
		return nil, err
	}
	if err := b.validate(in, a, "billingAccounts/"+aid); err != nil {
		return nil, err
	}
	normalize(in)
	id := newBudgetID()
	in.Name = "billingAccounts/" + aid + "/budgets/" + id
	in.Etag = etag()
	if err := b.write(aid, id, in); err != nil {
		return nil, err
	}
	b.s.schedule(in.GetName(), in.GetNotificationsRule().GetPubsubTopic())
	return in, nil
}

// GetBudget is billingAccounts.budgets.get.
func (b *Budgets) GetBudget(_ context.Context, req *bpb.GetBudgetRequest) (*bpb.Budget, error) {
	aid, id, err := budgetName(req.GetName())
	if err != nil {
		return nil, err
	}
	b.s.mu.Lock()
	defer b.s.mu.Unlock()
	return b.read(aid, id)
}

// ListBudgets is billingAccounts.budgets.list; a scope of projects/{p}
// lists the budgets scoped to that project alone.
func (b *Budgets) ListBudgets(_ context.Context, req *bpb.ListBudgetsRequest) (*bpb.ListBudgetsResponse, error) {
	scope := req.GetScope()
	if scope != "" {
		if id, ok := strings.CutPrefix(scope, "projects/"); !ok || id == "" || strings.Contains(id, "/") {
			return nil, status.Errorf(codes.InvalidArgument, "Invalid scope %q: want projects/{project}", scope)
		}
	}
	if req.GetPageSize() < 0 {
		return nil, status.Error(codes.InvalidArgument, "page_size must not be negative")
	}
	b.s.mu.Lock()
	defer b.s.mu.Unlock()
	aid, _, err := b.s.load(req.GetParent())
	if err != nil {
		return nil, err
	}
	all, err := b.list(aid)
	if err != nil {
		return nil, err
	}
	byName := map[string]*bpb.Budget{}
	var keys []string
	for _, x := range all {
		if scope != "" {
			ps := x.GetBudgetFilter().GetProjects()
			if len(ps) != 1 || ps[0] != scope {
				continue
			}
		}
		byName[x.GetName()] = x
		keys = append(keys, x.GetName())
	}
	page, next, err := paging.Page("budgets|"+aid+"|"+scope, keys, req.GetPageToken(), int(req.GetPageSize()))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	out := &bpb.ListBudgetsResponse{NextPageToken: next}
	for _, k := range page {
		out.Budgets = append(out.Budgets, byName[k])
	}
	return out, nil
}

// list reads the account's budgets. Callers hold s.mu.
func (b *Budgets) list(aid string) ([]*bpb.Budget, error) {
	keys, err := b.s.st.List(budgetPrefix + aid + "/")
	if err != nil {
		return nil, status.Errorf(codes.Internal, "billing: list budgets: %v", err)
	}
	sort.Strings(keys)
	out := make([]*bpb.Budget, 0, len(keys))
	for _, k := range keys {
		x, err := b.read(aid, strings.TrimPrefix(k, budgetPrefix+aid+"/"))
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, nil
}

// List is every budget of the account name, for the console.
func (b *Budgets) List(name string) ([]*bpb.Budget, error) {
	b.s.mu.Lock()
	defer b.s.mu.Unlock()
	aid, _, err := b.s.load(name)
	if err != nil {
		return nil, err
	}
	return b.list(aid)
}

// updatable are the fields an update mask may name, by their top-level
// field; a nested path (budget_filter.projects) updates within it.
// ownership_scope, which the Terraform provider names in every update, is
// not in the v1 proto this package serves, so it changes nothing.
var updatable = map[string]bool{"display_name": true, "budget_filter": true, "amount": true, "threshold_rules": true,
	"notifications_rule": true, "ownership_scope": true}

// UpdateBudget is billingAccounts.budgets.patch. Without an update mask,
// "only fields with non-default values from the request are updated".
func (b *Budgets) UpdateBudget(_ context.Context, req *bpb.UpdateBudgetRequest) (*bpb.Budget, error) {
	in := req.GetBudget()
	if in == nil {
		return nil, status.Error(codes.InvalidArgument, "budget is required")
	}
	aid, id, err := budgetName(in.GetName())
	if err != nil {
		return nil, err
	}
	b.s.mu.Lock()
	defer b.s.mu.Unlock()
	cur, err := b.read(aid, id)
	if err != nil {
		return nil, err
	}
	if in.GetEtag() != "" && in.GetEtag() != cur.GetEtag() {
		return nil, status.Errorf(codes.Aborted, "The budget's etag %q does not match its current etag: it changed since it was read", in.GetEtag())
	}
	_, a, err := b.s.load("billingAccounts/" + aid)
	if err != nil {
		return nil, err
	}
	next := proto.Clone(cur).(*bpb.Budget)
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		if in.GetDisplayName() != "" {
			paths = append(paths, "display_name")
		}
		if in.GetBudgetFilter() != nil {
			paths = append(paths, "budget_filter")
		}
		if in.GetAmount() != nil {
			paths = append(paths, "amount")
		}
		if len(in.GetThresholdRules()) > 0 {
			paths = append(paths, "threshold_rules")
		}
		if in.GetNotificationsRule() != nil {
			paths = append(paths, "notifications_rule")
		}
	}
	for _, p := range paths {
		top, sub, _ := strings.Cut(snake(p), ".")
		if !updatable[top] {
			return nil, status.Errorf(codes.InvalidArgument, "Invalid update_mask path %q", p)
		}
		if err := setPath(next, in, top, sub); err != nil {
			return nil, err
		}
	}
	if err := b.validate(next, a, "billingAccounts/"+aid); err != nil {
		return nil, err
	}
	normalize(next)
	next.Name = cur.GetName()
	next.Etag = etag()
	if err := b.write(aid, id, next); err != nil {
		return nil, err
	}
	b.s.schedule(next.GetName(), next.GetNotificationsRule().GetPubsubTopic())
	return next, nil
}

// snake turns a JSON field path (budgetFilter.creditTypes) into the proto's.
func snake(p string) string {
	var sb strings.Builder
	for _, r := range p {
		if r >= 'A' && r <= 'Z' {
			sb.WriteByte('_')
			r += 'a' - 'A'
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// setPath copies the field top (and, when sub is set, only its sub-field)
// from in into next.
func setPath(next, in *bpb.Budget, top, sub string) error {
	switch top {
	case "display_name":
		next.DisplayName = in.GetDisplayName()
	case "amount":
		next.Amount = in.GetAmount()
	case "threshold_rules":
		next.ThresholdRules = in.GetThresholdRules()
	case "notifications_rule":
		if sub == "" {
			next.NotificationsRule = in.GetNotificationsRule()
			return nil
		}
		return copySub(next.ProtoReflect(), in.ProtoReflect(), "notifications_rule", sub)
	case "budget_filter":
		if sub == "" {
			next.BudgetFilter = in.GetBudgetFilter()
			return nil
		}
		return copySub(next.ProtoReflect(), in.ProtoReflect(), "budget_filter", sub)
	}
	return nil
}

// copySub copies the field sub of the message field top from src into
// dst, creating dst's message when it has none.
func copySub(dst, src protoreflect.Message, top, sub string) error {
	tf := dst.Descriptor().Fields().ByName(protoreflect.Name(top))
	sf := tf.Message().Fields().ByName(protoreflect.Name(sub))
	if sf == nil {
		return status.Errorf(codes.InvalidArgument, "Invalid update_mask path %q", top+"."+sub)
	}
	from := src.Get(tf).Message()
	to := dst.Mutable(tf).Message()
	if from.Has(sf) {
		to.Set(sf, from.Get(sf))
	} else {
		to.Clear(sf)
	}
	return nil
}

// DeleteBudget is billingAccounts.budgets.delete.
func (b *Budgets) DeleteBudget(_ context.Context, req *bpb.DeleteBudgetRequest) (*emptypb.Empty, error) {
	aid, id, err := budgetName(req.GetName())
	if err != nil {
		return nil, err
	}
	b.s.mu.Lock()
	defer b.s.mu.Unlock()
	if _, err := b.read(aid, id); err != nil {
		return nil, err
	}
	if err := b.s.st.Delete(budgetKey(aid, id)); err != nil {
		return nil, fmt.Errorf("billing: delete budget: %w", err)
	}
	b.s.schedule(req.GetName(), "")
	return &emptypb.Empty{}, nil
}

// Spend is a budget's actual spend for its current period, in the
// account's currency: always 0, since every local SKU's price is 0 (the
// Catalog). The console's Budgets & alerts shows it.
func Spend(*bpb.Budget) float64 { return 0 }

// Exceeded are the threshold rules a spend of spend against amount
// exceeds. With a spend of 0 none is: a notification carries no
// alertThresholdExceeded (notify.go).
func Exceeded(x *bpb.Budget, spend float64) []*bpb.ThresholdRule {
	m := x.GetAmount().GetSpecifiedAmount()
	if m == nil {
		return nil
	}
	amount := float64(m.GetUnits()) + float64(m.GetNanos())/1e9
	var out []*bpb.ThresholdRule
	for _, r := range x.GetThresholdRules() {
		if r.GetSpendBasis() != bpb.ThresholdRule_FORECASTED_SPEND && spend > amount*r.GetThresholdPercent() {
			out = append(out, r)
		}
	}
	return out
}
