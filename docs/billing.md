# Cloud Billing

CloudBurrow serves the Cloud Billing API v1 (`cloudbilling.googleapis.com`) and the Budget API v1
(`billingbudgets.googleapis.com`) locally (#2109), on the Resource Manager port. Status per method is
in [compatibility.md](compatibility.md#cloud-billing--googlecloudbillingv1-and-googlecloudbillingbudgetsv1).

## Accounts are local records

A billing account here is a record and nothing more: no payment profile, no payment details, no
money. CloudBurrow never contacts Google for billing. Each instance starts with one open top-level
account, "My Billing Account", under the instance's organization; its ID is derived from the
organization, so it is stable across restarts. The API creates subaccounts under it, as Google's
`billingAccounts.create` does; the console's Create account also creates top-level accounts.

## Projects

A project starts with billing disabled. Linking it to an open account enables billing. Unlinking
it, or closing its account, disables billing. Google then stops "all billable services within that
project" ([modify-project](https://docs.cloud.google.com/billing/docs/how-to/modify-project)).
CloudBurrow disables the project's enabled billable APIs in Service Usage and enables them again on
the next link. The APIs that need no billing account stay enabled (`freeAPIs` in
`cmd/cloudburrow/billing.go`).

## Spend, pricing and budgets

CloudBurrow measures usage but has no pricing model. Every service in the local Cloud Catalog
has one SKU at a list price of 0, so the measured usage costs 0. A budget's actual spend is 0, and
no threshold rule is exceeded. Nothing is invented: if a pricing model is added, `billing.Spend` is
where it plugs in.

## Budget notifications

A budget with a Pub/Sub topic gets Google's programmatic notification, the budget's current status,
"multiple times per day" and with no usage too
([budgets-programmatic-notifications](https://docs.cloud.google.com/billing/docs/how-to/budgets-programmatic-notifications)):
here every 6 hours of the Cloud Billing clock, the first one interval after the topic is set. Each
message has the attributes `billingAccountId`, `budgetId` and `schemaVersion` (`1.0`) and Google's
JSON body, with `costAmount` 0 and no `alertThresholdExceeded`. Its `costIntervalStart` is the
period's start, at midnight UTC-8. Email alerts are not sent.

The clock is CloudBurrow's own extension on the Resource Manager port, for tests:

```sh
# publish what is due after 6 hours
curl -X POST http://127.0.0.1:9007/cloudburrow/v1/clock:advance -d '{"duration": "21600s"}'
# a message every minute from now on
curl -X POST http://127.0.0.1:9007/cloudburrow/v1/clock:advance -d '{"duration": "0s", "budgetNotificationInterval": "60s"}'
```

`GET /cloudburrow/v1/clock` reads the clock and the interval. The console's Budgets & alerts page
has the same Advance clock action.

## Access control

With `up --iam-enforcement=on`, Cloud Billing checks Google's documented permissions
(`billing.accounts.get`, `billing.budgets.*`, ...) against each account's own policy, a
subaccount's parent account's and the organization's. Linking a project needs
`resourcemanager.projects.createBillingAssignment` on the project and
`billing.resourceAssociations.create` on the account. Cloud Logging's `billingAccounts/*`
resources must be accounts Cloud Billing holds.

## gcloud

`gcloud billing accounts` and `gcloud billing projects` reach CloudBurrow through the
`cloudbilling` override that `gcloud-setup` writes. `gcloud billing budgets` cannot: gcloud (593.0.0)
has no override property for `billingbudgets`, so use the client libraries or Terraform.

## Console

Billing in the drawer has four pages:

- **Billing accounts:** create, rename, close and reopen accounts, link and unlink projects, and
  grant access.
- **Account management:** each project's account, with Change billing and Disable billing.
- **Budgets & alerts:** create, edit and delete budgets, with their actual spend.
- **Pricing:** the local SKUs.
