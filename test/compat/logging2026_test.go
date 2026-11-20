//go:build compat

package compat

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/logging"
	vlogging "cloud.google.com/go/logging/apiv2"
	"cloud.google.com/go/logging/apiv2/loggingpb"
	"cloud.google.com/go/logging/logadmin"
	"google.golang.org/api/iterator"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// covers: google.logging.v2.LoggingServiceV2/ListLogEntries, google.logging.v2.ConfigServiceV2/CreateSink, google.logging.v2.ConfigServiceV2/CreateExclusion
//
// TestLoggingQueryLanguage (#2026): the Logging query language in full
// through the official clients. logadmin's Entries reads with OR, NOT, the
// ":" has operator, parentheses, a value list, jsonPayload paths, SEARCH and
// a regular expression; a malformed filter is INVALID_ARGUMENT with the
// language's message; a sink created with logadmin routes, to another
// project, exactly the entries its OR/NOT/":" filter matches; and an
// exclusion made with the official ConfigClient keeps what its OR filter
// matches out of the bucket.
func TestLoggingQueryLanguage(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	opts := loggingClientOptions(h)
	project := h.Project()
	parent := "projects/" + project
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_000)
	logID := "qlang" + suffix

	c, err := logging.NewClient(ctx, parent, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ac, err := logadmin.NewClient(ctx, parent, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer ac.Close()
	cc, err := vlogging.NewConfigClient(ctx, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()

	// The sink and the exclusion exist before the entries are written.
	other := "qlang-other-" + suffix
	sinkID := "qlang-sink-" + suffix
	sinkFilter := fmt.Sprintf(`logName:"/logs/%s" AND (severity >= ERROR OR jsonPayload.kind:"keep") AND NOT jsonPayload.skip = true`, logID)
	if _, err := ac.CreateSink(ctx, &logadmin.Sink{ID: sinkID, Destination: "logging.googleapis.com/projects/" + other, Filter: sinkFilter}); err != nil {
		t.Fatalf("CreateSink: %v", err)
	}
	t.Cleanup(func() { _ = ac.DeleteSink(ctx, sinkID) })
	if s, err := ac.Sink(ctx, sinkID); err != nil || s.Filter != sinkFilter {
		t.Errorf("Sink = %+v, %v; want the filter kept", s, err)
	}
	exID := "qlang-ex-" + suffix
	if _, err := cc.CreateExclusion(ctx, &loggingpb.CreateExclusionRequest{Parent: parent, Exclusion: &loggingpb.LogExclusion{
		Name: exID, Filter: fmt.Sprintf(`log_id("%s") AND (jsonPayload.kind = "noise" OR jsonPayload.msg =~ "^debug:")`, logID)}}); err != nil {
		t.Fatalf("CreateExclusion: %v", err)
	}
	t.Cleanup(func() {
		_ = cc.DeleteExclusion(ctx, &loggingpb.DeleteExclusionRequest{Name: parent + "/exclusions/" + exID})
	})

	lg := c.Logger(logID, logging.CommonResource(&mrpb.MonitoredResource{Type: "global"}))
	for _, e := range []logging.Entry{
		{InsertID: "e1-" + suffix, Severity: logging.Info, Payload: map[string]any{"msg": "boot complete", "kind": "keep", "user": map[string]any{"id": "u1"}}},
		{InsertID: "e2-" + suffix, Severity: logging.Warning, Payload: map[string]any{"msg": "disk nearly full", "kind": "other"}},
		{InsertID: "e3-" + suffix, Severity: logging.Error, Payload: map[string]any{"msg": "payment crashed", "code": 7}},
		{InsertID: "e4-" + suffix, Severity: logging.Critical, Payload: map[string]any{"msg": "skipped crash", "skip": true}},
		{InsertID: "e5-" + suffix, Severity: logging.Info, Payload: map[string]any{"msg": "chatter", "kind": "noise"}},
		{InsertID: "e6-" + suffix, Severity: logging.Info, Payload: map[string]any{"msg": "debug: verbose"}},
	} {
		if err := lg.LogSync(ctx, e); err != nil {
			t.Fatalf("LogSync: %v", err)
		}
	}

	read := func(c *logadmin.Client, filter string) (string, error) {
		it := c.Entries(ctx, logadmin.Filter(filter))
		var got []string
		for {
			e, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				return "", err
			}
			got = append(got, strings.TrimSuffix(e.InsertID, "-"+suffix))
		}
		sort.Strings(got)
		return strings.Join(got, ","), nil
	}
	logName := parent + "/logs/" + logID
	scope := fmt.Sprintf(`logName = "%s" AND `, logName)
	for filter, want := range map[string]string{
		// e5 and e6 were excluded: never stored.
		fmt.Sprintf(`logName = "%s"`, logName):                         "e1,e2,e3,e4",
		fmt.Sprintf(`logName:"/logs/%s"`, logID):                       "e1,e2,e3,e4",
		scope + `(severity = WARNING OR severity = CRITICAL)`:          "e2,e4",
		scope + `severity = (WARNING OR CRITICAL)`:                     "e2,e4",
		scope + `NOT severity >= ERROR`:                                "e1,e2",
		scope + `-jsonPayload.kind:*`:                                  "e3,e4",
		scope + `jsonPayload.msg:"CRASH"`:                              "e3,e4",
		scope + `jsonPayload.code = 7 OR jsonPayload.user.id = u1`:     "e1,e3",
		scope + `jsonPayload.msg =~ "^(boot|disk)"`:                    "e1,e2",
		scope + `SEARCH("payment")`:                                    "e3",
		scope + `"nearly"`:                                             "e2",
		scope + `severity >= INFO severity < ERROR`:                    "e1,e2",
		fmt.Sprintf(`log_id("%s") AND jsonPayload.skip = true`, logID): "e4",
	} {
		got, err := read(ac, filter)
		if err != nil || got != want {
			t.Errorf("Entries(%q) = [%s], %v; want [%s]", filter, got, err, want)
		}
	}

	// A filter the language refuses.
	for filter, want := range map[string]string{
		scope + `(severity = ERROR`:    "Unparseable filter: syntax error",
		scope + `nosuchfield = 1`:      `field "nosuchfield" is not defined`,
		scope + `jsonPayload.msg =~ x`: "must be double-quoted",
	} {
		_, err := read(ac, filter)
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), want) {
			t.Errorf("Entries(%q) = %v; want INVALID_ARGUMENT with %q", filter, err, want)
		}
	}

	// The sink routed e1 (kind keep) and e3 (ERROR), not e2 (WARNING, other)
	// nor e4 (skip), nor e5 and e6, which match neither side of its OR.
	oc, err := logadmin.NewClient(ctx, "projects/"+other, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer oc.Close()
	deadline := time.Now().Add(15 * time.Second)
	for {
		got, err := read(oc, fmt.Sprintf(`logName = "%s"`, logName))
		if err == nil && got == "e1,e3" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("projects/%s holds [%s], %v; want the sink's e1,e3", other, got, err)
		}
		time.Sleep(300 * time.Millisecond)
	}

	// Sinks and exclusions refuse a filter the language cannot parse.
	_, err = ac.CreateSink(ctx, &logadmin.Sink{ID: sinkID + "-bad", Destination: "logging.googleapis.com/projects/" + other, Filter: "severity = ERROR OR"})
	wantLoggingCode(t, "a sink with an unparseable filter", err, codes.InvalidArgument)
	_, err = cc.CreateExclusion(ctx, &loggingpb.CreateExclusionRequest{Parent: parent, Exclusion: &loggingpb.LogExclusion{
		Name: exID + "-bad", Filter: "NOT"}})
	wantLoggingCode(t, "an exclusion with an unparseable filter", err, codes.InvalidArgument)
}
