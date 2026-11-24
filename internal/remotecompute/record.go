package remotecompute

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// RecordFile is where an instance remembers the host its compute runs on,
// so stop, delete, reset, status and doctor reach the same host as the up
// that created it without being told again.
const RecordFile = "compute.json"

// Record is an instance's compute placement.
type Record struct {
	Target  Target    `json:"target"`
	Created time.Time `json:"created"`
	// Host is what the host was when up last connected.
	Host HostInfo `json:"host"`
	// Provisioned is a VM `cloudburrow cloud create` or the console's
	// Create made (internal/byoc): CloudBurrow's key for it is in the OS
	// keychain and its host key is pinned, both read from the provisioner
	// at each connect, and Target follows the VM's address.
	Provisioned bool `json:"provisioned,omitempty"`
}

// SaveRecord writes the record into the instance directory, owner-only.
func SaveRecord(instanceDir string, r Record) error {
	if err := os.MkdirAll(instanceDir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	p := filepath.Join(instanceDir, RecordFile)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// LoadRecord reads the instance's record; ok is false when the instance's
// compute is this machine's own Docker.
func LoadRecord(instanceDir string) (r Record, ok bool, err error) {
	b, err := os.ReadFile(filepath.Join(instanceDir, RecordFile))
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return Record{}, false, fmt.Errorf("read %s: %w", filepath.Join(instanceDir, RecordFile), err)
	}
	if r.Target.Host == "" {
		return Record{}, false, fmt.Errorf("%s names no host", filepath.Join(instanceDir, RecordFile))
	}
	return r, true, nil
}

// RemoveRecord forgets the placement, after delete.
func RemoveRecord(instanceDir string) error {
	err := os.Remove(filepath.Join(instanceDir, RecordFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
