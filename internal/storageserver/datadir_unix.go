//go:build unix

package storageserver

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// writable asks the kernel whether this process may write path.
func writable(path string) error { return unix.Access(path, unix.W_OK) }

// describeOwner is "is owned by uid 0 (root) with mode -rw-r--r--".
func describeOwner(path string) string {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Sprintf("cannot be read (%v)", err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Sprintf("has mode %s", info.Mode())
	}
	return fmt.Sprintf("is owned by %s with mode %s", uidName(int(st.Uid)), info.Mode())
}

// currentUser is "uid 65532 (nonroot)".
func currentUser() string { return uidName(os.Geteuid()) }

// uidName is "uid N", with the user's name when the system knows it.
func uidName(uid int) string {
	id := strconv.Itoa(uid)
	if u, err := user.LookupId(id); err == nil && u.Username != "" {
		return fmt.Sprintf("uid %s (%s)", id, u.Username)
	}
	return "uid " + id
}
