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
has one SKU at a list price of 0, so the measured usage costs 0. A budget's actual spend is 0. No
threshold rule is exceeded and no Pub/Sub notification is published. Google's periodic status messages
are not sent either. Nothing is invented: if a pricing model is added, `billing.Spend` is where it
plugs in.

## Console

Billing in the drawer has four pages:

- **Billing accounts:** create, rename, close and reopen accounts, link and unlink projects, and
  grant access.
- **Account management:** each project's account, with Change billing and Disable billing.
- **Budgets & alerts:** create, edit and delete budgets, with their actual spend.
- **Pricing:** the local SKUs.
