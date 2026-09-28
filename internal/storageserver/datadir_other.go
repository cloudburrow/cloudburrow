//go:build !unix

package storageserver

// writable is not checked where there are no Unix owners; the store's own
// opens report a failure.
func writable(string) error { return nil }

func describeOwner(string) string { return "is not writable" }

func currentUser() string { return "this user" }
