package filestore

import (
	"context"
	"fmt"
	"sync"
)

// MemoryBackend is a Backend that keeps each share's files as one string
// in memory, for tests of the API and the console without Docker.
type MemoryBackend struct {
	mu      sync.Mutex
	servers map[string]Spec
	data    map[string]string // key/share
	snaps   map[string]string // key/share/snap
	backups map[string]string
	next    int
}

func (m *MemoryBackend) init() {
	if m.servers == nil {
		m.servers, m.data, m.snaps, m.backups = map[string]Spec{}, map[string]string{}, map[string]string{}, map[string]string{}
	}
}

// Ensure records the server and answers an address.
func (m *MemoryBackend) Ensure(_ context.Context, key, ip string, spec Spec) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.servers[key] = spec
	if ip == "" {
		m.next++
		ip = fmt.Sprintf("10.255.0.%d", m.next)
	}
	return ip, nil
}

// Remove forgets the server and its data.
func (m *MemoryBackend) Remove(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	for _, s := range m.servers[key].Shares {
		delete(m.data, key+"/"+s.Name)
	}
	delete(m.servers, key)
	return nil
}

// Usage is the length of the share's data.
func (m *MemoryBackend) Usage(_ context.Context, key, share string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	return int64(len(m.data[key+"/"+share])), nil
}

// Snapshot copies the share's data.
func (m *MemoryBackend) Snapshot(_ context.Context, key, share, snap string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.snaps[key+"/"+share+"/"+snap] = m.data[key+"/"+share]
	return nil
}

// DeleteSnapshot forgets a snapshot.
func (m *MemoryBackend) DeleteSnapshot(_ context.Context, key, share, snap string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	delete(m.snaps, key+"/"+share+"/"+snap)
	return nil
}

// Revert copies a snapshot's data back.
func (m *MemoryBackend) Revert(_ context.Context, key, share, snap string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.data[key+"/"+share] = m.snaps[key+"/"+share+"/"+snap]
	return nil
}

// Backup copies the share's data out.
func (m *MemoryBackend) Backup(_ context.Context, key, share, backup string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.backups[backup] = m.data[key+"/"+share]
	return int64(len(m.backups[backup])), nil
}

// Restore copies a backup's data in.
func (m *MemoryBackend) Restore(_ context.Context, key, share, backup string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.data[key+"/"+share] = m.backups[backup]
	return nil
}

// DeleteBackup forgets a backup.
func (m *MemoryBackend) DeleteBackup(_ context.Context, backup string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	delete(m.backups, backup)
	return nil
}

// Write sets the share's data, as a client writing to it would.
func (m *MemoryBackend) Write(key, share, data string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	m.data[key+"/"+share] = data
}

// Read answers the share's data.
func (m *MemoryBackend) Read(key, share string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.init()
	return m.data[key+"/"+share]
}

// Running is whether key's server is up.
func (m *MemoryBackend) Running(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.servers[key]
	return ok
}
