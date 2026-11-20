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
options), Subscribe, Revoke subscription and Delete; Edit exchange and Edit
listing (everything but the shared dataset), Upload icon, a listing's own
page, Refresh subscription and Delete subscription, **My subscriptions**
(the project's subscriptions, and the exchanges of its organization), and a
Permissions tab on an exchange and a listing (#1989).

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
documents. So is a BigQuery Storage Read API session of a linked table
(`restrict_direct_table_access`: "read api/tabledata.list"); a linked table
without restriction is read through it from the shared table. With
`restrict_query_result`, a copy or extract job of the result table of a query
that read the linked dataset is refused too; the result itself is read as
usual. Without it, query results may be copied and exported (#1989).

## IAM

Policies on exchanges, listings and subscriptions are stored, and with
`cloudburrow up --iam-enforcement=on` enforced (#1989): each method checks
the `analyticshub.*` permission Google documents for it, on the resource,
its exchange (for a listing) and its project, folders and organization; a
subscription also needs `bigquery.datasets.create` on the subscriber's
project. Grant `roles/analyticshub.viewer`, `subscriber`, `publisher`,
`listingAdmin`, `subscriptionOwner` or `admin` as on Google. A request with no
access token is the instance's own account, which holds every permission.

## Not served

Data clean rooms (`dcr_exchange_config`, `SubscribeDataExchange`, query
templates), Pub/Sub topic listings, routines in `selected_resources`,
multi-region listings and linked dataset replicas, and Marketplace commercial
listings are refused UNIMPLEMENTED. See
[compatibility.md](compatibility.md#bigquery-sharing-analytics-hub--googlecloudbigqueryanalyticshubv1).
