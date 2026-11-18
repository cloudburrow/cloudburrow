//go:build !race

package embedding

// opusCheckptr: without -race, checkptr is off and the decoder runs (#1808).
func opusCheckptr() error { return nil }
