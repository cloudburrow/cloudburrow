package mysqlpool

import (
	"net/http/httptest"
	"testing"
	"time"
)

// The figures page reads back as the pool's Stats (#2163).
func TestParseStatus(t *testing.T) {
	f := &Front{Pool: NewPool(3)}
	release, err := f.Pool.Acquire(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	rec := httptest.NewRecorder()
	f.StatusHandler().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	st, err := ParseStatus(rec.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if st.Size != 3 || st.Active != 1 || st.Idle != 2 || st.Pending != 0 || st.Waits != 1 || st.Waited > time.Second {
		t.Errorf("figures %+v; want 3 threads, 1 active, 2 idle, 1 wait", st)
	}
	if _, err := ParseStatus([]byte("<html>")); err == nil {
		t.Error("a page that is not the pool's read as its figures")
	}
}
