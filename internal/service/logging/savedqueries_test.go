package logging

import "testing"

func TestSavedQueryFilter(t *testing.T) {
	logPriv := &savedQuery{LoggingQuery: &loggingQuery{}, Visibility: "PRIVATE"}
	sqlShared := &savedQuery{OpsAnalyticsQuery: &opsAnalyticsQuery{}, Visibility: "SHARED"}
	for f, want := range map[string][2]bool{
		`type("Logging")`:                          {true, false},
		`type("OpsAnalytics")`:                     {false, true},
		`visibility = PRIVATE`:                     {true, false},
		`visibility=SHARED`:                        {false, true},
		`type("Logging") AND visibility = PRIVATE`: {true, false},
		`visibility = SHARED type("OpsAnalytics")`: {false, true},
		`type("Logging") visibility = SHARED`:      {false, false},
	} {
		keep, err := savedQueryFilter(f)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		if got := [2]bool{keep(logPriv), keep(sqlShared)}; got != want {
			t.Errorf("%s keeps %v, want %v", f, got, want)
		}
	}
	for _, f := range []string{`type("Other")`, `visibility = PUBLIC`, `type("Logging") type("Logging")`, `displayName = "x"`, `type("Logging") OR visibility = SHARED`, `AND type("Logging")`} {
		if _, err := savedQueryFilter(f); err == nil {
			t.Errorf("%s: accepted, want INVALID_ARGUMENT", f)
		}
	}
}
