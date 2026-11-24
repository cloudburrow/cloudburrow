package byoc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/byoc/keychain"
)

// Manager is bring your own cloud's one entry point, for the console and the
// CLI alike: connections, the machines made through them, and the long
// actions on those machines.
//
// What it records on disk, in Dir, is only what is not secret: each
// connection's cloud and account, and each machine's region, size and
// resource IDs. Credentials and SSH keys are keychain items.
type Manager struct {
	dir      string
	keys     keychain.Store
	drivers  map[Cloud]Driver
	engine   RemoteEngine
	ipLookup func(context.Context) (string, error)
	browser  func(string) error
	now      func() time.Time
	zone     func() (float64, float64, bool)

	mu      sync.Mutex
	signIns map[string]*signInState
	ops     map[string]*Operation
	opOrder []string
	busy    map[string]string // instance -> operation id
	tunnels map[string]Tunnel // instance -> its Session Manager tunnel
	addrMu  sync.Mutex        // one firewall update at a time
	addrLog map[string][]AddressChange
}

// Options configure a Manager.
type Options struct {
	// Dir holds connections.json and machines.json, owner-only.
	Dir string
	// Keychain holds the credentials and keys; keychain.OS in production.
	Keychain keychain.Store
	Drivers  []Driver
	// Engine is phase 1's remote-host engine; NoRemoteEngine when nil.
	Engine RemoteEngine
	// IPLookup is the user's public address, the only one SSH is opened to.
	// The default asks https://checkip.amazonaws.com.
	IPLookup func(context.Context) (string, error)
	// OpenBrowser opens a sign-in page; the default is the OS's opener.
	OpenBrowser func(string) error
	Now         func() time.Time
	// Location is where the user is, for the closest region; the default
	// reads this machine's time zone.
	Location func() (lat, lon float64, ok bool)
}

// NewManager is a Manager over o.
func NewManager(o Options) (*Manager, error) {
	if o.Dir == "" || o.Keychain == nil {
		return nil, errors.New("byoc: a state directory and a keychain are required")
	}
	m := &Manager{
		dir: o.Dir, keys: o.Keychain, drivers: map[Cloud]Driver{}, engine: o.Engine,
		ipLookup: o.IPLookup, browser: o.OpenBrowser, now: o.Now, zone: o.Location,
		signIns: map[string]*signInState{}, ops: map[string]*Operation{}, busy: map[string]string{},
		tunnels: map[string]Tunnel{},
	}
	for _, d := range o.Drivers {
		m.drivers[d.Cloud()] = d
	}
	if m.engine == nil {
		m.engine = NoRemoteEngine{}
	}
	if m.ipLookup == nil {
		m.ipLookup = CheckIP("https://checkip.amazonaws.com", http.DefaultClient)
	}
	if m.browser == nil {
		m.browser = OpenURL
	}
	if m.now == nil {
		m.now = func() time.Time { return time.Now().UTC() }
	}
	if m.zone == nil {
		m.zone = func() (float64, float64, bool) { return ZoneCoordinates(LocalZone()) }
	}
	return m, nil
}

// Clouds is every cloud this build can connect.
func (m *Manager) Clouds() []Cloud {
	out := make([]Cloud, 0, len(m.drivers))
	for c := range m.drivers {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Engine is the remote-host engine the manager hands machines to.
func (m *Manager) Engine() RemoteEngine { return m.engine }

// SecretName is the keychain item holding connection id's credentials.
func SecretName(id string) string { return "connection/" + id }

// keyName is the keychain item holding a machine's SSH key.
func keyName(instance string) string { return "machine/" + instance }

// ---- state on disk ----

type stateFile struct {
	Connections []Connection `json:"connections"`
	Machines    []Machine    `json:"machines"`
}

func (m *Manager) path() string { return filepath.Join(m.dir, "byoc.json") }

func (m *Manager) load() (stateFile, error) {
	var s stateFile
	b, err := os.ReadFile(m.path())
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("reading %s: %w", m.path(), err)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("reading %s: %w", m.path(), err)
	}
	return s, nil
}

func (m *Manager) save(s stateFile) error {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return err
	}
	sort.Slice(s.Connections, func(i, j int) bool { return s.Connections[i].ID < s.Connections[j].ID })
	sort.Slice(s.Machines, func(i, j int) bool { return s.Machines[i].Instance < s.Machines[j].Instance })
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(m.dir, ".byoc-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), m.path())
}

// update reads the state, applies f and writes it back, under the lock.
func (m *Manager) update(f func(*stateFile) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.load()
	if err != nil {
		return err
	}
	if err := f(&s); err != nil {
		return err
	}
	return m.save(s)
}

func (m *Manager) snapshot() (stateFile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.load()
}

// ---- connections ----

// Connections lists the connected accounts.
func (m *Manager) Connections() ([]Connection, error) {
	s, err := m.snapshot()
	return s.Connections, err
}

// Connection is one connection by ID.
func (m *Manager) Connection(id string) (Connection, error) {
	s, err := m.snapshot()
	if err != nil {
		return Connection{}, err
	}
	for _, c := range s.Connections {
		if c.ID == id {
			return c, nil
		}
	}
	return Connection{}, fmt.Errorf("connection %q: %w", id, ErrNotFound)
}

// ResolveConnection is id, or the only connection to cloud when id is
// empty.
func (m *Manager) ResolveConnection(cloud Cloud, id string) (Connection, error) {
	if id != "" {
		return m.Connection(id)
	}
	cs, err := m.Connections()
	if err != nil {
		return Connection{}, err
	}
	var match []Connection
	for _, c := range cs {
		if cloud == "" || c.Cloud == cloud {
			match = append(match, c)
		}
	}
	switch len(match) {
	case 0:
		if cloud == "" {
			return Connection{}, fmt.Errorf("no cloud is connected: run cloudburrow cloud connect aws|azure, or use Settings > Compute in the console")
		}
		return Connection{}, fmt.Errorf("%s is not connected: run cloudburrow cloud connect %s, or use Settings > Compute in the console", cloud.Title(), cloud)
	case 1:
		return match[0], nil
	}
	ids := make([]string, len(match))
	for i, c := range match {
		ids[i] = c.ID
	}
	return Connection{}, fmt.Errorf("several connections match; name one with --connection: %s", strings.Join(ids, ", "))
}

func (m *Manager) driver(c Cloud) (Driver, error) {
	d, ok := m.drivers[c]
	if !ok {
		return nil, fmt.Errorf("%s is not available in this build", c.Title())
	}
	return d, nil
}

func (m *Manager) addConnection(conn Connection, secret []byte) (Connection, error) {
	if conn.ID == "" {
		conn.ID = ConnectionID(conn.Cloud, conn.AccountID)
	}
	if conn.CreatedAt.IsZero() {
		conn.CreatedAt = m.now()
	}
	if err := m.keys.Set(SecretName(conn.ID), secret); err != nil {
		return Connection{}, err
	}
	err := m.update(func(s *stateFile) error {
		for i, c := range s.Connections {
			if c.ID == conn.ID {
				s.Connections[i] = conn
				return nil
			}
		}
		s.Connections = append(s.Connections, conn)
		return nil
	})
	return conn, err
}

// ConnectKey connects an account with an access key (AWS's fallback for
// accounts without IAM Identity Center).
func (m *Manager) ConnectKey(ctx context.Context, cloud Cloud, key AccessKey) (Connection, error) {
	d, err := m.driver(cloud)
	if err != nil {
		return Connection{}, err
	}
	conn, secret, err := d.ConnectKey(ctx, key)
	if err != nil {
		return Connection{}, err
	}
	return m.addConnection(conn, secret)
}

// Disconnect removes a connection and its keychain item. A connection that
// still has machines is refused unless force: without its credentials
// CloudBurrow could no longer stop or delete them.
func (m *Manager) Disconnect(id string, force bool) error {
	s, err := m.snapshot()
	if err != nil {
		return err
	}
	found := false
	for _, c := range s.Connections {
		found = found || c.ID == id
	}
	if !found {
		return fmt.Errorf("connection %q: %w", id, ErrNotFound)
	}
	var left []string
	for _, mc := range s.Machines {
		if mc.Connection == id {
			left = append(left, mc.Instance)
		}
	}
	if len(left) > 0 && !force {
		return fmt.Errorf("%s still has machines (%s): delete them first, or disconnect with force and delete them in the cloud's own console",
			id, strings.Join(left, ", "))
	}
	if err := m.keys.Delete(SecretName(id)); err != nil {
		return err
	}
	return m.update(func(s *stateFile) error {
		out := s.Connections[:0]
		for _, c := range s.Connections {
			if c.ID != id {
				out = append(out, c)
			}
		}
		s.Connections = out
		return nil
	})
}

// Provider opens connection id.
func (m *Manager) Provider(ctx context.Context, id string) (Provider, error) {
	conn, err := m.Connection(id)
	if err != nil {
		return nil, err
	}
	d, err := m.driver(conn.Cloud)
	if err != nil {
		return nil, err
	}
	secret, err := m.keys.Get(SecretName(id))
	if errors.Is(err, keychain.ErrNotFound) {
		return nil, &AuthError{Message: "the keychain no longer holds " + id + "'s credentials; connect it again"}
	}
	if err != nil {
		return nil, err
	}
	return d.Open(ctx, conn, secret, func(b []byte) error { return m.keys.Set(SecretName(id), b) })
}

// ---- browser sign-in ----

// SignInStatus is a browser sign-in's state, as the console polls it.
type SignInStatus struct {
	ID         string      `json:"id"`
	Cloud      Cloud       `json:"cloud"`
	State      string      `json:"state"` // "waiting", "choose", "connected", "failed"
	Prompt     Prompt      `json:"prompt"`
	Choices    []Choice    `json:"choices,omitempty"`
	Error      string      `json:"error,omitempty"`
	Connection *Connection `json:"connection,omitempty"`
}

type signInState struct {
	status SignInStatus
	signIn SignIn
	cancel context.CancelFunc
}

// signInLimit bounds a sign-in: the device code's own lifetime is shorter.
const signInLimit = 15 * time.Minute

// BeginSignIn starts a browser sign-in and waits on it in the background.
func (m *Manager) BeginSignIn(ctx context.Context, cloud Cloud, opts SignInOptions) (SignInStatus, error) {
	d, err := m.driver(cloud)
	if err != nil {
		return SignInStatus{}, err
	}
	if opts.OpenBrowser == nil {
		opts.OpenBrowser = m.browser
	}
	si, err := d.BeginSignIn(ctx, opts)
	if err != nil {
		return SignInStatus{}, err
	}
	bg, cancel := context.WithTimeout(context.Background(), signInLimit)
	st := &signInState{status: SignInStatus{ID: newID("signin"), Cloud: cloud, State: "waiting", Prompt: si.Prompt()}, signIn: si, cancel: cancel}
	m.mu.Lock()
	m.signIns[st.status.ID] = st
	m.mu.Unlock()
	go func() {
		choices, err := si.Wait(bg)
		m.mu.Lock()
		defer m.mu.Unlock()
		if err != nil {
			st.status.State, st.status.Error = "failed", err.Error()
			cancel()
			return
		}
		st.status.State, st.status.Choices = "choose", choices
	}()
	return st.status, nil
}

// SignInStatusOf is a sign-in's state.
func (m *Manager) SignInStatusOf(id string) (SignInStatus, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.signIns[id]
	if !ok {
		return SignInStatus{}, fmt.Errorf("sign-in %q: %w", id, ErrNotFound)
	}
	return st.status, nil
}

// WaitSignIn blocks until the sign-in leaves "waiting", for the CLI.
func (m *Manager) WaitSignIn(ctx context.Context, id string) (SignInStatus, error) {
	for {
		st, err := m.SignInStatusOf(id)
		if err != nil || st.State != "waiting" {
			return st, err
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// ChooseAccount connects one of a sign-in's choices.
func (m *Manager) ChooseAccount(ctx context.Context, id, choice string) (Connection, error) {
	m.mu.Lock()
	st, ok := m.signIns[id]
	m.mu.Unlock()
	if !ok {
		return Connection{}, fmt.Errorf("sign-in %q: %w", id, ErrNotFound)
	}
	if st.status.State != "choose" {
		return Connection{}, fmt.Errorf("the sign-in is %s, not waiting for a choice", st.status.State)
	}
	known := false
	for _, c := range st.status.Choices {
		known = known || c.ID == choice
	}
	if !known {
		return Connection{}, fmt.Errorf("%q is not one of the sign-in's accounts", choice)
	}
	conn, secret, err := st.signIn.Choose(ctx, choice)
	if err == nil {
		conn, err = m.addConnection(conn, secret)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		st.status.Error = err.Error()
		return Connection{}, err
	}
	st.status.State, st.status.Connection = "connected", &conn
	st.cancel()
	return conn, nil
}

// CancelSignIn abandons a sign-in.
func (m *Manager) CancelSignIn(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st, ok := m.signIns[id]; ok {
		st.cancel()
		delete(m.signIns, id)
	}
}

// ---- regions, sizes, permissions ----

// RegionChoice is the wizard's region list with the closest one marked.
type RegionChoice struct {
	Regions []Region `json:"regions"`
	Default string   `json:"default"`
}

// Regions lists a connection's regions, the closest first marked Default.
func (m *Manager) Regions(ctx context.Context, conn string) (RegionChoice, error) {
	p, err := m.Provider(ctx, conn)
	if err != nil {
		return RegionChoice{}, err
	}
	rs, err := p.Regions(ctx)
	if err != nil {
		return RegionChoice{}, err
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].Name < rs[j].Name })
	out := RegionChoice{Regions: rs}
	if lat, lon, ok := m.zone(); ok {
		out.Default = ClosestRegion(rs, lat, lon)
	} else if len(rs) > 0 {
		out.Default = defaultRegion(p.Cloud(), rs)
	}
	return out, nil
}

func defaultRegion(c Cloud, rs []Region) string {
	want := map[Cloud]string{AWS: "us-east-1", Azure: "eastus"}[c]
	for _, r := range rs {
		if r.Name == want {
			return want
		}
	}
	return rs[0].Name
}

// Sizes is every preset's size and price in region.
func (m *Manager) Sizes(ctx context.Context, conn, region string) ([]Size, error) {
	p, err := m.Provider(ctx, conn)
	if err != nil {
		return nil, err
	}
	return p.Sizes(ctx, region)
}

// CheckPermissions is the connection's permission report for region.
func (m *Manager) CheckPermissions(ctx context.Context, conn, region string) (PermissionReport, error) {
	p, err := m.Provider(ctx, conn)
	if err != nil {
		return PermissionReport{}, err
	}
	return p.CheckPermissions(ctx, region)
}

func sizeFor(sizes []Size, preset Preset, name string) (Size, error) {
	for _, s := range sizes {
		if (name != "" && s.Name == name) || (name == "" && s.Preset == preset) {
			return s, nil
		}
	}
	if name != "" {
		return Size{}, fmt.Errorf("%s is not one of this cloud's sizes", name)
	}
	return Size{}, fmt.Errorf("no %s size in this region", preset)
}

// ---- machines ----

// Machines lists the machines, as last recorded.
func (m *Manager) Machines() ([]Machine, error) {
	s, err := m.snapshot()
	return s.Machines, err
}

// Machine is one machine as last recorded.
func (m *Manager) Machine(instance string) (Machine, error) {
	s, err := m.snapshot()
	if err != nil {
		return Machine{}, err
	}
	for _, mc := range s.Machines {
		if mc.Instance == instance {
			return mc, nil
		}
	}
	return Machine{}, fmt.Errorf("no machine for instance %q: %w", instance, ErrNotFound)
}

func (m *Manager) putMachine(mc Machine) error {
	mc.UpdatedAt = m.now()
	return m.update(func(s *stateFile) error {
		for i, x := range s.Machines {
			if x.Instance == mc.Instance {
				s.Machines[i] = mc
				return nil
			}
		}
		s.Machines = append(s.Machines, mc)
		return nil
	})
}

func (m *Manager) dropMachine(instance string) error {
	return m.update(func(s *stateFile) error {
		out := s.Machines[:0]
		for _, x := range s.Machines {
			if x.Instance != instance {
				out = append(out, x)
			}
		}
		s.Machines = out
		return nil
	})
}

// Refresh reads a machine's state from its cloud and records it.
func (m *Manager) Refresh(ctx context.Context, instance string) (Machine, error) {
	mc, err := m.Machine(instance)
	if err != nil {
		return Machine{}, err
	}
	p, err := m.Provider(ctx, mc.Connection)
	if err != nil {
		return mc, err
	}
	got, err := p.Status(ctx, mc)
	if err != nil {
		return mc, err
	}
	return got, m.putMachine(got)
}

// Resources lists everything tagged with the instance in its cloud.
func (m *Manager) Resources(ctx context.Context, instance string) ([]Resource, error) {
	mc, err := m.Machine(instance)
	if err != nil {
		return nil, err
	}
	p, err := m.Provider(ctx, mc.Connection)
	if err != nil {
		return nil, err
	}
	return p.ListResources(ctx, instance, mc.Region)
}

// RemoteTarget is what the remote engine needs to reach a machine.
func (m *Manager) RemoteTarget(instance string) (RemoteTarget, error) {
	return m.RemoteTargetContext(context.Background(), instance, nil)
}

// ---- operations ----

// Operation is one long action, its progress as the console polls it.
type Operation struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"` // "create", "scale", "start", "stop", "delete"
	Instance string    `json:"instance"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitempty"`
	Done     bool      `json:"done"`
	Error    string    `json:"error,omitempty"`
	// ErrorKind is "permission", "quota", "auth", "leftover" or "".
	ErrorKind string   `json:"error_kind,omitempty"`
	Events    []Event  `json:"events"`
	Machine   *Machine `json:"machine,omitempty"`
}

// Operation is a copy of one operation.
func (m *Manager) Operation(id string) (Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	op, ok := m.ops[id]
	if !ok {
		return Operation{}, fmt.Errorf("operation %q: %w", id, ErrNotFound)
	}
	cp := *op
	cp.Events = append([]Event{}, op.Events...)
	return cp, nil
}

// Operations lists this process's operations, newest first.
func (m *Manager) Operations() []Operation {
	m.mu.Lock()
	ids := append([]string(nil), m.opOrder...)
	m.mu.Unlock()
	out := make([]Operation, 0, len(ids))
	for i := len(ids) - 1; i >= 0; i-- {
		if op, err := m.Operation(ids[i]); err == nil {
			out = append(out, op)
		}
	}
	return out
}

// errBusy refuses a second action on an instance while one runs.
func (m *Manager) startOp(kind, instance string) (*Operation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id, ok := m.busy[instance]; ok {
		return nil, fmt.Errorf("%s is busy with operation %s; wait for it to finish", instance, id)
	}
	op := &Operation{ID: newID("op"), Kind: kind, Instance: instance, Started: m.now(), Events: []Event{}}
	m.ops[op.ID] = op
	m.opOrder = append(m.opOrder, op.ID)
	m.busy[instance] = op.ID
	return op, nil
}

func (m *Manager) progress(op *Operation, also Progress) Progress {
	return func(e Event) {
		if e.Time.IsZero() {
			e.Time = m.now()
		}
		m.mu.Lock()
		op.Events = append(op.Events, e)
		m.mu.Unlock()
		if also != nil {
			also(e)
		}
	}
}

func (m *Manager) finishOp(op *Operation, mc *Machine, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	op.Done, op.Finished = true, m.now()
	if mc != nil {
		cp := *mc
		op.Machine = &cp
	}
	if err != nil {
		op.Error, op.ErrorKind = err.Error(), ErrorKind(err)
	}
	delete(m.busy, op.Instance)
}

// ErrorKind classifies err for the console.
func ErrorKind(err error) string {
	var pe *PermissionError
	var qe *QuotaError
	var ae *AuthError
	var le *LeftoverError
	switch {
	case errors.As(err, &pe):
		return "permission"
	case errors.As(err, &qe):
		return "quota"
	case errors.As(err, &ae):
		return "auth"
	case errors.As(err, &le):
		return "leftover"
	}
	return ""
}

// run starts f as an operation. With wait it returns when f does (the
// CLI); otherwise at once (the console polls).
func (m *Manager) run(kind, instance string, wait bool, also Progress, f func(ctx context.Context, p Progress) (*Machine, error)) (Operation, error) {
	op, err := m.startOp(kind, instance)
	if err != nil {
		return Operation{}, err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// A long action outlives the request that started it.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		mc, err := f(ctx, m.progress(op, also))
		m.finishOp(op, mc, err)
	}()
	if wait {
		<-done
	}
	got, _ := m.Operation(op.ID)
	if wait && got.Error != "" {
		return got, errors.New(got.Error)
	}
	return got, nil
}

// CreateRequest is the wizard's three choices, and where.
type CreateRequest struct {
	Instance        string `json:"instance"`
	Connection      string `json:"connection"`
	Region          string `json:"region"`
	Preset          Preset `json:"preset"`
	Size            string `json:"size,omitempty"` // overrides Preset
	AutoStopMinutes int    `json:"auto_stop_minutes"`
	DiskGiB         int    `json:"disk_gib,omitempty"`
	// ConnectMode is ConnectSSM or ConnectSSH; empty is SSM where the
	// cloud offers it (AWS), else SSH.
	ConnectMode string `json:"connect_mode,omitempty"`
}

var instanceName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,38}[a-z0-9]$`)

// ValidateInstance checks an instance name is one every cloud accepts as a
// resource-name part and a tag value.
func ValidateInstance(name string) error {
	if !instanceName.MatchString(name) {
		return fmt.Errorf("invalid instance name %q: 2 to 40 lowercase letters, digits and hyphens, starting with a letter", name)
	}
	return nil
}

// MaxAutoStopMinutes bounds auto-stop at a week.
const MaxAutoStopMinutes = 7 * 24 * 60

// Create provisions a machine. With wait it returns when the machine is up
// or the create has failed and been rolled back.
func (m *Manager) Create(ctx context.Context, req CreateRequest, wait bool, also Progress) (Operation, error) {
	if err := ValidateInstance(req.Instance); err != nil {
		return Operation{}, err
	}
	if req.AutoStopMinutes < 0 || req.AutoStopMinutes > MaxAutoStopMinutes {
		return Operation{}, fmt.Errorf("auto-stop must be between 0 (never) and %d minutes", MaxAutoStopMinutes)
	}
	if _, err := m.Machine(req.Instance); err == nil {
		return Operation{}, fmt.Errorf("instance %s already has a machine; delete it first, or scale it", req.Instance)
	}
	conn, err := m.Connection(req.Connection)
	if err != nil {
		return Operation{}, err
	}
	if req.Preset == "" && req.Size == "" {
		req.Preset = Small
	}
	return m.run("create", req.Instance, wait, also, func(ctx context.Context, progress Progress) (*Machine, error) {
		return m.create(ctx, conn, req, progress)
	})
}

func (m *Manager) create(ctx context.Context, conn Connection, req CreateRequest, progress Progress) (*Machine, error) {
	p, err := m.Provider(ctx, conn.ID)
	if err != nil {
		return nil, err
	}
	progress.Report("permissions", StepRunning, "Checking the permissions CloudBurrow needs in %s", conn.AccountID)
	rep, err := p.CheckPermissions(ctx, req.Region)
	if err != nil {
		return nil, err
	}
	if !rep.OK() {
		progress.Report("permissions", StepFailed, "%s", rep.Summary())
		return nil, &PermissionError{Missing: rep.Missing}
	}
	progress.Report("permissions", StepDone, "%s", rep.Summary())

	sizes, err := p.Sizes(ctx, req.Region)
	if err != nil {
		return nil, err
	}
	size, err := sizeFor(sizes, req.Preset, req.Size)
	if err != nil {
		return nil, err
	}

	mode, err := connectModeFor(conn.Cloud, p, req.ConnectMode)
	if err != nil {
		return nil, err
	}
	cidr := ""
	if mode == ConnectSSM {
		progress.Report("address", StepDone, "CloudBurrow connects through AWS Systems Manager Session Manager: the VM's firewall allows nothing inbound")
	} else {
		progress.Report("address", StepRunning, "Finding this machine's public address, the only one SSH will accept")
		if cidr, err = m.currentCIDR(ctx); err != nil {
			progress.Report("address", StepFailed, "%v", err)
			return nil, err
		}
		progress.Report("address", StepDone, "SSH will accept %s only; the firewall follows this address when it changes", cidr)
	}

	progress.Report("key", StepRunning, "Making CloudBurrow's SSH key and the VM's host key on this machine")
	client, err := NewKeyPair("cloudburrow-" + req.Instance)
	if err != nil {
		return nil, err
	}
	host, err := NewKeyPair("")
	if err != nil {
		return nil, err
	}
	if err := m.keys.Set(keyName(req.Instance), []byte(client.PrivatePEM)); err != nil {
		return nil, err
	}
	progress.Report("key", StepDone, "The private key is in the keychain; only its public half goes to the VM")

	user := map[Cloud]string{AWS: "ubuntu", Azure: "cloudburrow"}[conn.Cloud]
	userData, err := RenderCloudInit(CloudInitOptions{Cloud: conn.Cloud, Instance: req.Instance, User: user,
		AuthorizedKey: client.Public, HostKeyPrivate: host.PrivatePEM, HostKeyPublic: host.Public,
		GPU: size.Preset == GPU, AutoStopMinutes: req.AutoStopMinutes, SSM: mode == ConnectSSM})
	if err != nil {
		_ = m.keys.Delete(keyName(req.Instance))
		return nil, err
	}
	pending := Machine{Instance: req.Instance, Cloud: conn.Cloud, Connection: conn.ID, Region: req.Region, Size: size.Name,
		Preset: size.Preset, DiskGiB: req.DiskGiB, AutoStopMinutes: req.AutoStopMinutes, State: Provisioning,
		SSHUser: user, HostKey: host.Public, HourlyUSD: size.HourlyUSD, CreatedAt: m.now(), ConnectMode: mode, SourceCIDR: cidr}
	if cidr != "" {
		pending.SourceUpdatedAt = pending.CreatedAt
	}
	if err := m.putMachine(pending); err != nil {
		return nil, err
	}
	mc, err := p.Create(ctx, MachineSpec{Instance: req.Instance, Region: req.Region, Size: size.Name, Preset: size.Preset,
		DiskGiB: req.DiskGiB, AutoStopMinutes: req.AutoStopMinutes, SSHUser: user, SSHPublicKey: client.Public,
		SourceCIDR: cidr, ConnectMode: mode, UserData: userData}, progress)
	if err != nil {
		// The provider removed what it made; so is the record and the key.
		_ = m.dropMachine(req.Instance)
		_ = m.keys.Delete(keyName(req.Instance))
		return nil, err
	}
	mc.Connection, mc.Cloud, mc.HostKey, mc.SSHUser = conn.ID, conn.Cloud, host.Public, user
	mc.Preset, mc.AutoStopMinutes, mc.CreatedAt = size.Preset, req.AutoStopMinutes, pending.CreatedAt
	mc.ConnectMode, mc.SourceCIDR = mode, cidr
	if mc.HourlyUSD == 0 {
		mc.HourlyUSD = size.HourlyUSD
	}
	if err := m.putMachine(mc); err != nil {
		return &mc, err
	}
	m.handOver(ctx, mc, progress)
	return &mc, nil
}

// handOver gives a running machine to the remote engine.
func (m *Manager) handOver(ctx context.Context, mc Machine, progress Progress) {
	if mc.State != Running {
		return
	}
	t, err := m.RemoteTargetContext(ctx, mc.Instance, progress)
	if err != nil {
		progress.Report("remote", StepFailed, "%v", err)
		return
	}
	via := "over SSH"
	if t.Via == ConnectSSM {
		via = "over SSH through Session Manager"
	}
	progress.Report("remote", StepRunning, "Connecting the instance's Docker to %s %s", mc.PublicIP, via)
	if err := m.engine.Connect(ctx, t); err != nil {
		progress.Report("remote", StepFailed, "%v", err)
		return
	}
	progress.Report("remote", StepDone, "The instance's Docker runs on %s", mc.PublicIP)
	// The auto-stop setting is the record's; a change made while the VM
	// was stopped reaches it now.
	if err := m.pushAutoStop(ctx, t, mc.AutoStopMinutes); err != nil {
		progress.Report("auto-stop", StepFailed, "Could not set auto-stop on the VM: %v", err)
	}
}

// Scale resizes a machine to a preset or a named size.
func (m *Manager) Scale(ctx context.Context, instance string, preset Preset, size string, wait bool, also Progress) (Operation, error) {
	mc, err := m.Machine(instance)
	if err != nil {
		return Operation{}, err
	}
	return m.run("scale", instance, wait, also, func(ctx context.Context, progress Progress) (*Machine, error) {
		p, err := m.Provider(ctx, mc.Connection)
		if err != nil {
			return nil, err
		}
		sizes, err := p.Sizes(ctx, mc.Region)
		if err != nil {
			return nil, err
		}
		s, err := sizeFor(sizes, preset, size)
		if err != nil {
			return nil, err
		}
		if s.Name == mc.Size {
			progress.Report("resize", StepDone, "%s is already %s", instance, s.Name)
			return &mc, nil
		}
		if (s.Preset == GPU) != (mc.Preset == GPU) {
			return nil, fmt.Errorf("the GPU size needs the NVIDIA driver, which cloud-init installs only at create: delete and create %s again to move to or from GPU", instance)
		}
		mc.State = Resizing
		_ = m.putMachine(mc)
		got, err := p.Resize(ctx, mc, s.Name, progress)
		if err != nil {
			if cur, serr := p.Status(ctx, mc); serr == nil {
				_ = m.putMachine(cur)
			}
			return nil, err
		}
		got.Preset, got.HourlyUSD = s.Preset, s.HourlyUSD
		if err := m.putMachine(got); err != nil {
			return &got, err
		}
		m.handOver(ctx, got, progress)
		return &got, nil
	})
}

// Start starts a stopped machine.
func (m *Manager) Start(ctx context.Context, instance string, wait bool, also Progress) (Operation, error) {
	return m.power("start", instance, wait, also)
}

// Stop stops a machine; its disk, and the cluster's state on it, stay.
func (m *Manager) Stop(ctx context.Context, instance string, wait bool, also Progress) (Operation, error) {
	return m.power("stop", instance, wait, also)
}

func (m *Manager) power(kind, instance string, wait bool, also Progress) (Operation, error) {
	mc, err := m.Machine(instance)
	if err != nil {
		return Operation{}, err
	}
	return m.run(kind, instance, wait, also, func(ctx context.Context, progress Progress) (*Machine, error) {
		p, err := m.Provider(ctx, mc.Connection)
		if err != nil {
			return nil, err
		}
		var got Machine
		if kind == "start" {
			progress.Report("start", StepRunning, "Starting %s", instance)
			got, err = p.Start(ctx, mc)
		} else {
			_ = m.engine.Close(instance)
			m.closeTunnel(instance)
			progress.Report("stop", StepRunning, "Stopping %s; its disk is kept", instance)
			got, err = p.Stop(ctx, mc)
		}
		if err != nil {
			progress.Report(kind, StepFailed, "%v", err)
			return nil, err
		}
		progress.Report(kind, StepDone, "%s is %s", instance, got.State)
		if err := m.putMachine(got); err != nil {
			return &got, err
		}
		if kind == "start" {
			m.handOver(ctx, got, progress)
		}
		return &got, nil
	})
}

// Delete removes everything the machine's create made, verifies nothing
// tagged is left, and then forgets the machine and its key.
func (m *Manager) Delete(ctx context.Context, instance string, wait bool, also Progress) (Operation, error) {
	mc, err := m.Machine(instance)
	if err != nil {
		return Operation{}, err
	}
	return m.run("delete", instance, wait, also, func(ctx context.Context, progress Progress) (*Machine, error) {
		p, err := m.Provider(ctx, mc.Connection)
		if err != nil {
			return nil, err
		}
		_ = m.engine.Close(instance)
		m.closeTunnel(instance)
		mc.State = Deleting
		_ = m.putMachine(mc)
		if err := p.Delete(ctx, instance, mc.Region, progress); err != nil {
			mc.State, mc.LastError = Failed, err.Error()
			_ = m.putMachine(mc)
			return &mc, err
		}
		progress.Report("verify", StepDone, "Nothing tagged %s=%s is left", TagKey, instance)
		if err := m.dropMachine(instance); err != nil {
			return nil, err
		}
		if err := m.keys.Delete(keyName(instance)); err != nil {
			return nil, err
		}
		// The VM is gone: the engine forgets it was the instance's compute.
		if f, ok := m.engine.(interface{ Forget(string) error }); ok {
			if err := f.Forget(instance); err != nil {
				return nil, err
			}
		}
		mc.State = Deleted
		return &mc, nil
	})
}

// ---- helpers ----

func newID(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

// CheckIP is an IPLookup asking url, which answers with the caller's
// address as plain text (checkip.amazonaws.com does).
func CheckIP(url string, client *http.Client) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return "", err
		}
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, 256))
		if err != nil {
			return "", err
		}
		ip := strings.TrimSpace(string(b))
		if resp.StatusCode != http.StatusOK || net.ParseIP(ip) == nil {
			return "", fmt.Errorf("%s answered %d, not an address", url, resp.StatusCode)
		}
		return ip, nil
	}
}
