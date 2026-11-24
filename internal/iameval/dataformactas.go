package iameval

// DataformActAs is Dataform's table for its strict act-as checks (#2173,
// https://docs.cloud.google.com/dataform/docs/strict-act-as-mode, read
// 2026-11-22): the service has no other IAM table here, so only the
// actAs on a repository's, workflow configuration's or invocation's
// service account is checked, by the Dataform server itself
// (internal/service/dataform/runas.go), and refused as Google's newer
// services word a denial.
var DataformActAs = &Service{Name: "dataform.googleapis.com", Deny: newerDenyOf}
