//go:build !linux && !darwin

package main

import "errors"

// procStartTime is not read on this platform: `up` records none, and a
// runtime file without one is checked by the process's name, as before #820.
func procStartTime(int) (string, error) { return "", errors.ErrUnsupported }
