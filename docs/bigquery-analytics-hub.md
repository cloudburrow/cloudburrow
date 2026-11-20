# BigQuery sharing (Analytics Hub)

CloudBurrow serves BigQuery sharing, formerly Analytics Hub
(`analyticshub.googleapis.com` v1, #1960), whenever BigQuery is enabled:
`cloudburrow up --services bigquery`. It listens on `--port-analyticshub`
(default `9042`), gRPC and JSON on one port. Everything runs locally; nothing
is sent to Google.

## Clients

No client library reads an emulator variable for it, so pass the endpoint:

```go
addr := os.Getenv("CLOUDBURROW_ANALYTICSHUB_ENDPOINT") // from `cloudburrow env`
c, err := analyticshub.NewClient(ctx, option.WithEndpoint(addr), option.WithoutAuthentication(),
	option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
```

`analyticshub.NewRESTClient` works with `option.WithEndpoint("http://" + addr)`.
`cloudburrow terraform` sets `bigquery_analytics_hub_custom_endpoint`, so
`google_bigquery_analytics_hub_data_exchange`, `_listing` and
`_listing_subscription` apply locally. gcloud has no Analytics Hub command.
The console's BigQuery menu has **Analytics Hub**: exchanges, their listings
and subscriptions, Create exchange, Create listing (with the data egress
options), Subscribe, Revoke subscription and Delete.

## Linked datasets

`SubscribeListing` makes the linked dataset in the subscriber's project on the
instance's BigQuery (the project must exist in the instance's Resource
Manager). As on Google, it is a read-only pointer to the shared dataset:

- its tables are the shared dataset's (or the listing's selected ones), listed
  and described with their own type and schema, and queried live: a row or a
  table the publisher adds is read at once;
- every write into it is refused 403 `accessDenied`; deleting it is allowed;
- after `RevokeSubscription` it stays in the subscriber's project, but every
  query and read of it is refused; `DeleteSubscription` deletes it.

The local BigQuery engine has no dataset that points at another, so the
BigQuery front keeps it one: each linked table is a view of the shared table
in the engine, added or dropped before every read, and tables.list,
tables.get and tabledata.list answer from the shared table. INFORMATION_SCHEMA
of the linked dataset shows those views.

## Data egress restriction

With `restricted_export_config.enabled`, the subscriber can query the linked
dataset, but a copy, clone or snapshot job, an extract job, a query job with a
destination table, `CREATE TABLE … AS SELECT` (and any CREATE, INSERT, MERGE,
UPDATE or EXPORT DATA statement that reads it) and tabledata.list of a linked
table are refused 403 `accessDenied` before anything runs, as Google
documents. Not enforced locally: reads through the BigQuery Storage Read API,
and `restrict_query_result`'s ban on exporting a finished query's result table.
IAM is stored, not enforced (ADR-0006).

## Not served

Data clean rooms (`dcr_exchange_config`, `SubscribeDataExchange`, query
templates), Pub/Sub topic listings, routines in `selected_resources`,
multi-region listings and linked dataset replicas, and Marketplace commercial
listings are refused UNIMPLEMENTED. See
[compatibility.md](compatibility.md#bigquery-sharing-analytics-hub--googlecloudbigqueryanalyticshubv1).
