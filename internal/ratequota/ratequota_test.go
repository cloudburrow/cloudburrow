package ratequota

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/cloudburrow/cloudburrow/internal/quotalimit"
)

func TestChargeRefillsEachMinute(t *testing.T) {
	now := time.Date(2026, 11, 20, 10, 0, 59, 0, time.UTC)
	l := New()
	l.Now = func() time.Time { return now }
	l.Register(Metric{Service: "x.googleapis.com", Name: "x.googleapis.com/read_requests", Title: "Read requests",
		Limit: "ReadRequestsPerMinutePerProject", LimitTitle: "Read requests per minute per project", Default: 1})
	if err := l.Set("x.googleapis.com/read_requests", "lowered", 0); err != nil {
		t.Fatal(err)
	}
	if err := l.Charge("x.googleapis.com/read_requests", "p"); err != nil {
		t.Fatal(err)
	}
	err := l.Charge("x.googleapis.com/read_requests", "p")
	st := status.Convert(err)
	if st.Code() != codes.ResourceExhausted {
		t.Fatalf("second call = %v", err)
	}
	var info *errdetails.ErrorInfo
	for _, d := range st.Details() {
		if i, ok := d.(*errdetails.ErrorInfo); ok {
			info = i
		}
	}
	if info == nil || info.GetReason() != Reason || info.GetMetadata()["quota_metric"] != "x.googleapis.com/read_requests" || info.GetMetadata()["quota_limit_value"] != "1" {
		t.Errorf("ErrorInfo = %v", info)
	}
	if err := l.Charge("x.googleapis.com/read_requests", "lowered"); status.Code(err) != codes.ResourceExhausted {
		t.Errorf("a project lowered to 0 = %v", err)
	}
	now = now.Add(time.Second)
	if err := l.Charge("x.googleapis.com/read_requests", "p"); err != nil {
		t.Errorf("the next minute = %v", err)
	}
	l.Clear("", "lowered")
	if err := l.Charge("x.googleapis.com/read_requests", "lowered"); err != nil {
		t.Errorf("after Clear = %v", err)
	}
	if err := l.Set("nope", "", 1); err == nil {
		t.Error("an unknown metric was set")
	}
}

// fixedSource grants one quota a value in one project.
type fixedSource struct {
	project, service, quotaID string
	value                     int64
}

func (f fixedSource) Value(project, service, quotaID string, _ map[string]string) (int64, bool) {
	if project == f.project && service == f.service && quotaID == f.quotaID {
		return f.value, true
	}
	return 0, false
}

// TestPreferenceIsTheLimit (#2090): a granted Cloud Quotas preference is
// the project's limit, over the configured one, and is read at each call,
// so a change applies to the next one; other projects keep theirs.
func TestPreferenceIsTheLimit(t *testing.T) {
	l := New()
	m := Metric{Service: "s.googleapis.com", Name: "s.googleapis.com/read_requests", Title: "Read requests",
		Limit: "ReadRequestsPerMinutePerProject", LimitTitle: "Read requests per minute per project", Default: 100}
	l.Register(m)
	if m.QuotaID() != "ReadRequestsPerMinutePerProject" {
		t.Fatalf("QuotaID = %q", m.QuotaID())
	}
	if err := l.Set(m.Name, "", 50); err != nil {
		t.Fatal(err)
	}
	quotalimit.Use(fixedSource{"p", m.Service, m.QuotaID(), 1})
	t.Cleanup(func() { quotalimit.Use(nil) })
	if err := l.Charge(m.Name, "p"); err != nil {
		t.Fatal(err)
	}
	err := l.Charge(m.Name, "p")
	if err == nil || !strings.Contains(err.Error(), "Quota exceeded") {
		t.Fatalf("second call = %v; want the refusal at the preference's 1", err)
	}
	if v, _ := l.Configured(m.Name, "p"); v != 50 {
		t.Errorf("Configured = %d, want 50", v)
	}
	quotalimit.Use(fixedSource{"p", m.Service, m.QuotaID(), 3})
	if err := l.Charge(m.Name, "p"); err != nil {
		t.Errorf("after raising the preference: %v", err)
	}
	for i := 0; i < 50; i++ {
		if err := l.Charge(m.Name, "q"); err != nil {
			t.Fatalf("another project's call %d: %v", i+1, err)
		}
	}
	if len(l.Metrics()) != 1 {
		t.Errorf("Metrics = %v", l.Metrics())
	}
}
