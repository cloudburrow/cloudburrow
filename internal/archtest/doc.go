// Package archtest holds no code, only tests: it checks the package
// boundaries docs/architecture.md §3 states against the module as it is, so
// the rules are enforced by `make check` rather than by review alone (#599).
//
// Each rule compares what it finds with an explicit allow-list of today's
// known exceptions. A new exception fails the test; so does an entry that no
// longer matches anything, so each PR that removes an exception must also
// shrink the list.
package archtest
