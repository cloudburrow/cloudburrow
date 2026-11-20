//go:build !go1.27

package main

// A CloudBurrow built with Go 1.26 has no crypto/mldsa and refuses the
// ML-DSA algorithms UNIMPLEMENTED (internal/service/kms/mldsa_go126.go), so
// the console's crypto test signs with none.
var consoleMLDSAAlgorithms []string
