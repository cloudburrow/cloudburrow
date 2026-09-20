// Package config defines the configuration model, its documented precedence
// (flags over environment over file over defaults), and validation. Invalid
// configuration must fail here, before any listener is opened or any container
// is created.
//
// Not implemented yet; see issue #3 and docs/architecture.md.
package config
