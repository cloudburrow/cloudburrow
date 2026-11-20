//go:build go1.27

package main

// consoleMLDSAAlgorithms are the ML-DSA algorithms the console's crypto
// test signs with: a CloudBurrow built with Go 1.27 computes ML-DSA with
// crypto/mldsa (internal/service/kms/mldsa_go127.go).
var consoleMLDSAAlgorithms = []string{"PQ_SIGN_ML_DSA_65"}
