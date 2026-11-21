package ratequota

import (
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
