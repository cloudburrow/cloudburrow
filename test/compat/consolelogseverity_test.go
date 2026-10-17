//go:build compat

package compat

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"cloud.google.com/go/logging"
	"google.golang.org/api/option"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// TestConsoleFoldsLoggingSeverities (#1576): an entry at each of Cloud
// Logging's nine severities, written with the official client, is in the
// console's Logs Explorer at one of the console's four levels: DEFAULT and
// DEBUG as DEFAULT, INFO and NOTICE as INFO, WARNING as WARNING, and ERROR,
// CRITICAL, ALERT and EMERGENCY as ERROR.
// covers: google.logging.v2.LoggingServiceV2/WriteLogEntries
func TestConsoleFoldsLoggingSeverities(t *testing.T) {
	h := New(t)
	ctx := h.Context()
	opts := []option.ClientOption{option.WithEndpoint(h.Endpoint(EnvLogging)), option.WithoutAuthentication(),
		option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))}
	addr := consoleAddr(t, h)
	parent := "projects/" + h.Project()
	c, err := logging.NewClient(ctx, parent, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	lg := c.Logger("severity-fold", logging.CommonResource(&mrpb.MonitoredResource{Type: "global"}))
	want := map[logging.Severity]string{
		logging.Default: "DEFAULT", logging.Debug: "DEFAULT",
		logging.Info: "INFO", logging.Notice: "INFO",
		logging.Warning: "WARNING",
		logging.Error:   "ERROR", logging.Critical: "ERROR", logging.Alert: "ERROR", logging.Emergency: "ERROR",
	}
	for sev := range want {
		if err := lg.LogSync(ctx, logging.Entry{Severity: sev, Payload: "fold-" + sev.String()}); err != nil {
			t.Fatalf("LogSync %v: %v", sev, err)
		}
	}

	q := url.Values{"project": {h.Project()}, "resource": {parent + "/logs/severity-fold"}, "limit": {"100"}}
	var got map[string]string
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		code, body := consoleDo(t, addr, http.MethodGet, "/api/logs?"+q.Encode(), "")
		var resp struct {
			Entries []struct{ Severity, Message string } `json:"entries"`
		}
		if code != http.StatusOK || json.Unmarshal([]byte(body), &resp) != nil {
			t.Fatalf("/api/logs = %d %s", code, body)
		}
		got = map[string]string{}
		for _, e := range resp.Entries {
			got[e.Message] = e.Severity
		}
		if len(got) >= len(want) || time.Now().After(deadline) {
			break
		}
	}
	for sev, level := range want {
		if m := "fold-" + sev.String(); got[m] != level {
			t.Errorf("%s in the Logs Explorer at %q, want %q", sev, got[m], level)
		}
	}
}
