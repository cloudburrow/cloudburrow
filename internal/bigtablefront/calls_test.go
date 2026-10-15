package bigtablefront

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"cloud.google.com/go/bigtable"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"

	"github.com/cloudburrow/cloudburrow/internal/calllog"
)

// Every Bigtable call through the front, the ones it refuses itself among
// them, is in its call log with its method, table, code and duration
// (#1267), read the way the CLI reads it.
func TestCallLogRecordsBigtableCalls(t *testing.T) {
	addr := serve(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ac, err := bigtable.NewAdminClient(ctx, "p", "i", option.WithGRPCConn(dial(t, addr)))
	if err != nil {
		t.Fatal(err)
	}
	if err := ac.CreateTable(ctx, "logged"); err != nil {
		t.Fatalf("CreateTable: %v", err)
	}
	if _, err := ac.Backups(ctx, "c").Next(); err == nil {
		t.Fatal("ListBackups succeeded")
	}
	c, err := bigtable.NewClient(ctx, "p", "i", option.WithGRPCConn(dial(t, addr)))
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Open("logged").ReadRows(ctx, bigtable.InfiniteRange(""), func(bigtable.Row) bool { return true })

	resp, err := http.Get("http://" + addr + calllog.Path + "?after=0")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var page struct{ Calls []calllog.Call }
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	byMethod := map[string]calllog.Call{}
	for _, c := range page.Calls {
		byMethod[c.Method] = c
	}
	if c := byMethod[admin+"CreateTable"]; c.Code != int(codes.OK) || c.Resource != "projects/p/instances/i" || c.Duration <= 0 {
		t.Errorf("CreateTable recorded as %+v", c)
	}
	if c := byMethod[admin+"ListBackups"]; c.Code != int(codes.Unimplemented) {
		t.Errorf("ListBackups recorded as %+v", c)
	}
	if c := byMethod[data+"ReadRows"]; !c.Stream || c.Resource != "projects/p/instances/i/tables/logged" || c.Transport != "grpc" {
		t.Errorf("ReadRows recorded as %+v", c)
	}
}
