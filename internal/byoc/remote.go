package byoc

import (
	"context"
	"errors"
	"time"
)

// RemoteTarget is a VM the remote-host engine (#2209 phase 1) runs an
// instance's Docker on, over SSH.
type RemoteTarget struct {
	Instance string
	Host     string
	Port     int
	User     string
	// PrivateKey is CloudBurrow's own key for the VM (OpenSSH PEM), read
	// from the keychain for the call and never written to disk.
	PrivateKey []byte
	// HostKey is the VM's host key in authorized_keys form; the engine
	// refuses any other.
	HostKey string
	// Via is how Host:Port reaches the VM's sshd: ConnectSSH (the VM's
	// address) or ConnectSSM (a Session Manager tunnel on 127.0.0.1).
	Via string
	// BeforeConnect, when set, runs before each connection and reconnection
	// attempt: in SSH mode it moves the firewall to the user's current
	// address.
	BeforeConnect func(context.Context) error `json:"-"`
}

// RemoteRunner is an engine that runs a shell command on the VM over its
// SSH connection, connecting for the call when the instance is not
// connected from this process.
type RemoteRunner interface {
	Run(ctx context.Context, t RemoteTarget, command string) (string, error)
}

// RemoteStatus is the engine's view of one instance's connection.
type RemoteStatus struct {
	Connected bool      `json:"connected"`
	Message   string    `json:"message"`
	Since     time.Time `json:"since,omitempty"`
}

// RemoteEvent is one of the engine's progress events.
type RemoteEvent struct {
	Time     time.Time `json:"time"`
	Instance string    `json:"instance"`
	Kind     string    `json:"kind"` // "connecting", "connected", "lost", "reconnected", "closed"
	Message  string    `json:"message"`
}

// RemoteEngine is phase 1's remote-host engine, as this package uses it. A
// later landing wires internal/remotecompute in through it; until then
// NoRemoteEngine answers.
type RemoteEngine interface {
	Connect(ctx context.Context, t RemoteTarget) error
	Status(ctx context.Context, instance string) (RemoteStatus, error)
	Reconnect(ctx context.Context, instance string) error
	Close(instance string) error
	Events(instance string) <-chan RemoteEvent
}

// ErrNoRemoteEngine is NoRemoteEngine's refusal.
var ErrNoRemoteEngine = errors.New("the remote-host engine (#2209 phase 1) is not part of this build yet: " +
	"the VM is provisioned and reachable over SSH, but the instance's Docker cannot be moved onto it from here")

// NoRemoteEngine is the engine of a build without phase 1.
type NoRemoteEngine struct{}

// Connect refuses.
func (NoRemoteEngine) Connect(context.Context, RemoteTarget) error { return ErrNoRemoteEngine }

// Status says the engine is missing.
func (NoRemoteEngine) Status(context.Context, string) (RemoteStatus, error) {
	return RemoteStatus{Message: ErrNoRemoteEngine.Error()}, nil
}

// Reconnect refuses.
func (NoRemoteEngine) Reconnect(context.Context, string) error { return ErrNoRemoteEngine }

// Close has nothing to close.
func (NoRemoteEngine) Close(string) error { return nil }

// Events is a closed channel.
func (NoRemoteEngine) Events(string) <-chan RemoteEvent {
	c := make(chan RemoteEvent)
	close(c)
	return c
}
