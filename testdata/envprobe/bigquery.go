package main

// The /bigquery probe (#874): BigQuery from inside the cluster, at the
// injected CLOUDBURROW_BIGQUERY_ENDPOINT, through the official Go client.
//
// The emulator behind CloudBurrow's BigQuery checks almost nothing; the
// validating front (internal/bigqueryfront, #861) refuses what BigQuery
// refuses. A pod that reached the emulator past the front would get none
// of it, so this reports the status of each request BigQuery refuses, as the pod
// sees it: a dataset that exists (409), an invalid dataset ID (400), and
// two columns whose names differ only in case (400), and the insertErrors
// of a row missing its REQUIRED value.
//
// No client library reads CLOUDBURROW_BIGQUERY_ENDPOINT, so it is given to
// the client, as docs/credentials.md shows; the address itself is the
// injected one, the emulator's Service, whose REST port is the front in the
// emulator's pod (#902), unless ?endpoint= names another. ?storage=
// names a host:port the probe dials over TCP and reports as reachable or
// not, for the Storage Read port of that Service. The project is the
// caller's: the emulator serves the instance's one project, which is not
// the Cloud Run service's.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"cloud.google.com/go/bigquery"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

func bigqueryProbe(ctx context.Context, q url.Values) (string, error) {
	ctx, stop := context.WithTimeout(ctx, 2*time.Minute)
	defer stop()
	endpoint := os.Getenv("CLOUDBURROW_BIGQUERY_ENDPOINT")
	if e := q.Get("endpoint"); e != "" {
		endpoint = e
	}
	storage := "unchecked"
	if addr := q.Get("storage"); addr != "" {
		conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", addr)
		if err != nil {
			storage = "unreachable"
		} else {
			storage = "reachable"
			_ = conn.Close()
		}
	}
	project, dataset := q.Get("project"), q.Get("dataset")
	if endpoint == "" || project == "" || dataset == "" {
		return "", fmt.Errorf("need CLOUDBURROW_BIGQUERY_ENDPOINT (%q), ?project= and ?dataset=", endpoint)
	}
	c, err := bigquery.NewClient(ctx, project, option.WithEndpoint(endpoint), option.WithoutAuthentication())
	if err != nil {
		return "", fmt.Errorf("bigquery.NewClient: %w", err)
	}
	defer c.Close()

	// Each refused call has its own deadline: without the front, the
	// emulator answers a duplicate with a 500, which the client retries
	// until its deadline, and the probe reports that rather than hanging.
	status := func(call func(context.Context) error) string {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		err := call(cctx)
		var e *googleapi.Error
		switch {
		case err == nil:
			return "ok"
		case errors.As(err, &e):
			return fmt.Sprint(e.Code)
		case errors.Is(err, context.DeadlineExceeded):
			return "timeout"
		}
		return "error(" + err.Error() + ")"
	}

	ds := c.Dataset(dataset)
	create := status(func(ctx context.Context) error { return ds.Create(ctx, nil) })
	if create != "ok" {
		return "", fmt.Errorf("create dataset %s: %s", dataset, create)
	}
	duplicate := status(func(ctx context.Context) error { return ds.Create(ctx, nil) })
	invalidID := status(func(ctx context.Context) error { return c.Dataset(dataset+"-bad").Create(ctx, nil) })
	twice := status(func(ctx context.Context) error {
		return ds.Table("twice").Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{
			{Name: "x", Type: bigquery.StringFieldType}, {Name: "X", Type: bigquery.StringFieldType},
		}})
	})

	tbl := ds.Table("rows")
	if err := tbl.Create(ctx, &bigquery.TableMetadata{Schema: bigquery.Schema{
		{Name: "id", Type: bigquery.IntegerFieldType, Required: true},
		{Name: "note", Type: bigquery.StringFieldType},
	}}); err != nil {
		return "", fmt.Errorf("create table: %w", err)
	}
	var reasons []string
	ictx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	err = tbl.Inserter().Put(ictx, []*bigquery.ValuesSaver{{
		Schema: bigquery.Schema{{Name: "note", Type: bigquery.StringFieldType}},
		Row:    []bigquery.Value{"no id"},
	}})
	var multi bigquery.PutMultiError
	if errors.As(err, &multi) {
		for _, row := range multi {
			for _, e := range row.Errors {
				var be *bigquery.Error
				if errors.As(e, &be) {
					reasons = append(reasons, be.Reason)
				}
			}
		}
	} else if err != nil {
		reasons = append(reasons, "error("+err.Error()+")")
	} else {
		reasons = append(reasons, "inserted")
	}
	sort.Strings(reasons)

	return fmt.Sprintf("endpoint=%s create=%s duplicate=%s invalid_id=%s duplicate_column=%s missing_required=%s storage_read=%s",
		endpoint, create, duplicate, invalidID, twice, strings.Join(reasons, ","), storage), nil
}
