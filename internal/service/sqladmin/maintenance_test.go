package sqladmin

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// rescheduleMaintenance checks its body as Google documents it, then is
// 501: no maintenance event is ever scheduled here (#1524).
func TestRescheduleMaintenance(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{}`, "400"},
		{`{"reschedule":{}}`, "400"},
		{`{"reschedule":{"rescheduleType":"SOON"}}`, "400"},
		{`{"reschedule":{"rescheduleType":"SPECIFIC_TIME"}}`, "400"},
		{`{"reschedule":{"rescheduleType":"SPECIFIC_TIME","scheduleTime":"tomorrow"}}`, "400"},
		{`{"reschedule":{"rescheduleType":"SPECIFIC_TIME","scheduleTime":"2026-12-01T10:00:00Z"}}`, "501"},
		{`{"reschedule":{"rescheduleType":"IMMEDIATE"}}`, "501"},
		{`{"reschedule":{"rescheduleType":"NEXT_AVAILABLE_WINDOW"}}`, "501"},
	} {
		r := httptest.NewRequest("POST", "/x", strings.NewReader(tc.body))
		_, err := rescheduleMaintenance(r)
		if err == nil || !strings.Contains(errStatus(err), tc.want) {
			t.Errorf("%s: %v, want %s", tc.body, err, tc.want)
		}
	}
}
