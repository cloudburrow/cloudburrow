// Command goprobe is the Go client of TestAlloyDBConnectorsSendRealSQL
// (test/compat/alloydbconnectors_test.go): Google's Go connector
// (cloud.google.com/go/alloydbconn) and pgx write and read a row, with the
// options test/compat's alloyDBDialer uses, so that the container proves
// they send nothing off the machine. Its built-in telemetry opt-out alone
// does not: NewDialer still opens a Cloud Monitoring gRPC client, at
// monitoring.googleapis.com, which WithHTTPClient makes fail to start
// (a gRPC client refuses an HTTP client). Prints "OK go <rows>" or
// "FAIL go: <error>"; runs only in the no-egress container.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"cloud.google.com/go/alloydbconn"
	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"
)

func main() {
	inst, api, tok := os.Getenv("ADB_INSTANCE"), os.Getenv("ADB_API"), os.Getenv("ADB_TOKEN")
	// Never fall back to Google's endpoint.
	if inst == "" || api == "" || tok == "" {
		fmt.Println("FAIL go: ADB_INSTANCE, ADB_API and ADB_TOKEN must be set")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	n, err := run(ctx, inst, api, tok)
	if err != nil {
		fmt.Println("FAIL go:", err)
		return
	}
	fmt.Println("OK go", n)
}

func run(ctx context.Context, inst, api, tok string) (int, error) {
	d, err := alloydbconn.NewDialer(ctx,
		alloydbconn.WithAdminAPIEndpoint("http://"+api),
		alloydbconn.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: tok, Expiry: time.Now().Add(time.Hour)})),
		alloydbconn.WithOptOutOfBuiltInTelemetry(),
		alloydbconn.WithHTTPClient(&http.Client{Transport: &http.Transport{}}))
	if err != nil {
		return 0, err
	}
	defer d.Close()
	cfg, err := pgx.ParseConfig("user=postgres password=" + os.Getenv("ADB_PASSWORD") + " dbname=postgres sslmode=disable")
	if err != nil {
		return 0, err
	}
	cfg.DialFunc = func(ctx context.Context, _, _ string) (net.Conn, error) { return d.Dial(ctx, inst) }
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return 0, err
	}
	defer conn.Close(context.Background())
	for _, q := range []string{"CREATE TABLE IF NOT EXISTS connector_probe (client text)", "INSERT INTO connector_probe VALUES ('go')"} {
		if _, err := conn.Exec(ctx, q); err != nil {
			return 0, err
		}
	}
	var n int
	err = conn.QueryRow(ctx, "SELECT count(*) FROM connector_probe WHERE client = 'go'").Scan(&n)
	return n, err
}
