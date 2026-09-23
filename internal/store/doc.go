// Package store abstracts resource metadata storage behind one interface with
// an in-memory mode and a durable mode, including atomic multi-key commits and
// single-instance ownership of a data directory.
package store
