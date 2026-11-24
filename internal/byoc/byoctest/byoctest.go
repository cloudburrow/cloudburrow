// Package byoctest is an in-memory byoc.Driver and byoc.Provider, for the
// manager's, the console's and the CLI's tests. The clouds' own fakes, which
// speak each cloud's API, are internal/byoc/awsprov/awsfake and
// internal/byoc/azureprov/azurefake.
package byoctest

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/cloudburrow/cloudburrow/internal/byoc"
)

// Cloud is a fake cloud: its driver and every connection's provider share
// its resources.
type Cloud struct {
	Kind byoc.Cloud

	mu        sync.Mutex
	Missing   []byoc.Permission // CheckPermissions reports these
	FailOn    map[string]error  // "create", "resize", "start", "stop", "delete"
	Leave     bool              // Delete leaves one resource behind
	resources map[string][]byoc.Resource
	machines  map[string]byoc.Machine
	Created   []byoc.MachineSpec
	// Boot, when set, stands a host in for the created VM: it receives the
	// spec (its cloud-init carries the VM's host key and CloudBurrow's
	// public key) and the machine, whose address it may change (#2209's
	// phase 1 test boots its Docker-in-Docker sshd stand-in so).
	Boot      func(spec byoc.MachineSpec, m *byoc.Machine) error
	approved  chan struct{}
	approveMu sync.Once
}

// New is an empty fake cloud of kind c.
func New(c byoc.Cloud) *Cloud {
	return &Cloud{Kind: c, FailOn: map[string]error{}, resources: map[string][]byoc.Resource{},
		machines: map[string]byoc.Machine{}, approved: make(chan struct{})}
}

// Approve completes the pending browser sign-in.
func (c *Cloud) Approve() { c.approveMu.Do(func() { close(c.approved) }) }

// Resources is what is tagged with instance.
func (c *Cloud) Resources(instance string) []byoc.Resource {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byoc.Resource(nil), c.resources[instance]...)
}

// Cloud implements byoc.Driver.
func (c *Cloud) Cloud() byoc.Cloud { return c.Kind }

// BeginSignIn implements byoc.Driver.
func (c *Cloud) BeginSignIn(_ context.Context, opts byoc.SignInOptions) (byoc.SignIn, error) {
	p := byoc.Prompt{URL: "https://device.example.test/?user_code=ABCD-EFGH", UserCode: "ABCD-EFGH", Message: "Confirm the code on the sign-in page"}
	if opts.OpenBrowser != nil {
		_ = opts.OpenBrowser(p.URL)
	}
	return &signIn{c: c, prompt: p}, nil
}

type signIn struct {
	c      *Cloud
	prompt byoc.Prompt
}

func (s *signIn) Prompt() byoc.Prompt { return s.prompt }

func (s *signIn) Wait(ctx context.Context) ([]byoc.Choice, error) {
	select {
	case <-s.c.approved:
		return []byoc.Choice{{ID: "111122223333", Label: "sandbox (111122223333)"}, {ID: "444455556666", Label: "prod (444455556666)"}}, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("the sign-in expired: %w", ctx.Err())
	}
}

func (s *signIn) Choose(_ context.Context, id string) (byoc.Connection, []byte, error) {
	return byoc.Connection{Cloud: s.c.Kind, Method: "sso", AccountID: id, Principal: "test-user"}, []byte(`{"token":"t-` + id + `"}`), nil
}

// ConnectKey implements byoc.Driver.
func (c *Cloud) ConnectKey(_ context.Context, k byoc.AccessKey) (byoc.Connection, []byte, error) {
	if c.Kind != byoc.AWS {
		return byoc.Connection{}, nil, byoc.ErrUnsupported
	}
	if k.AccessKeyID == "" || k.SecretAccessKey == "" {
		return byoc.Connection{}, nil, &byoc.AuthError{Message: "the access key was refused"}
	}
	b, _ := json.Marshal(k)
	return byoc.Connection{Cloud: c.Kind, Method: "access-key", AccountID: "777788889999"}, b, nil
}

// Open implements byoc.Driver.
func (c *Cloud) Open(_ context.Context, conn byoc.Connection, secret []byte, _ func([]byte) error) (byoc.Provider, error) {
	if len(secret) == 0 {
		return nil, &byoc.AuthError{Message: "no credentials"}
	}
	return &provider{c: c, conn: conn}, nil
}

type provider struct {
	c    *Cloud
	conn byoc.Connection
}

func (p *provider) Cloud() byoc.Cloud { return p.c.Kind }

func (p *provider) Connect(context.Context) (byoc.Account, error) {
	return byoc.Account{ID: p.conn.AccountID, Principal: p.conn.Principal}, nil
}

func (p *provider) CheckPermissions(context.Context, string) (byoc.PermissionReport, error) {
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	return byoc.PermissionReport{Checked: true, Missing: append([]byoc.Permission(nil), p.c.Missing...)}, nil
}

func (p *provider) Regions(context.Context) ([]byoc.Region, error) {
	if p.c.Kind == byoc.Azure {
		return []byoc.Region{{Name: "eastus", DisplayName: "East US", Latitude: 37.37, Longitude: -79.82},
			{Name: "westeurope", DisplayName: "West Europe", Latitude: 52.37, Longitude: 4.9}}, nil
	}
	return []byoc.Region{{Name: "us-east-1", DisplayName: "US East (N. Virginia)", Latitude: 38.9, Longitude: -77.4},
		{Name: "eu-west-1", DisplayName: "Europe (Ireland)", Latitude: 53.3, Longitude: -6.3}}, nil
}

// Sizes are the presets with fixed prices.
func (p *provider) Sizes(context.Context, string) ([]byoc.Size, error) {
	return []byoc.Size{
		{Preset: byoc.Small, Name: "small-4", VCPUs: 4, MemoryGiB: 16, HourlyUSD: 0.2, PriceSource: "fake"},
		{Preset: byoc.Medium, Name: "medium-16", VCPUs: 16, MemoryGiB: 64, HourlyUSD: 0.8, PriceSource: "fake"},
		{Preset: byoc.Large, Name: "large-32", VCPUs: 32, MemoryGiB: 128, HourlyUSD: 1.6, PriceSource: "fake"},
		{Preset: byoc.GPU, Name: "gpu-l4", VCPUs: 4, MemoryGiB: 16, GPU: "NVIDIA L4", GPUs: 1, HourlyUSD: 0.8, PriceSource: "fake"},
	}, nil
}

func (p *provider) fail(op string) error {
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	return p.c.FailOn[op]
}

func (p *provider) Create(_ context.Context, spec byoc.MachineSpec, progress byoc.Progress) (byoc.Machine, error) {
	p.c.mu.Lock()
	p.c.Created = append(p.c.Created, spec)
	p.c.resources[spec.Instance] = []byoc.Resource{{Type: "network", ID: "net-1"}, {Type: "firewall", ID: "fw-1"}}
	p.c.mu.Unlock()
	progress.Report("network", byoc.StepDone, "network made")
	if err := p.fail("create"); err != nil {
		p.c.mu.Lock()
		delete(p.c.resources, spec.Instance)
		p.c.mu.Unlock()
		progress.Report("rollback", byoc.StepRolledBack, "removed what was made")
		return byoc.Machine{}, err
	}
	m := byoc.Machine{Instance: spec.Instance, Region: spec.Region, Size: spec.Size, Preset: spec.Preset, State: byoc.Running,
		PublicIP: "198.51.100.7", SSHPort: 22, SSHUser: spec.SSHUser, IDs: map[string]string{"vm": "vm-" + spec.Instance}}
	if p.c.Boot != nil {
		if err := p.c.Boot(spec, &m); err != nil {
			p.c.mu.Lock()
			delete(p.c.resources, spec.Instance)
			p.c.mu.Unlock()
			return byoc.Machine{}, err
		}
	}
	p.c.mu.Lock()
	p.c.resources[spec.Instance] = append(p.c.resources[spec.Instance], byoc.Resource{Type: "vm", ID: "vm-" + spec.Instance})
	p.c.machines[spec.Instance] = m
	p.c.mu.Unlock()
	progress.Report("vm", byoc.StepDone, "VM running")
	return m, nil
}

func (p *provider) Status(_ context.Context, m byoc.Machine) (byoc.Machine, error) {
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	got, ok := p.c.machines[m.Instance]
	if !ok {
		m.State = byoc.Deleted
		return m, nil
	}
	m.State, m.Size, m.PublicIP = got.State, got.Size, got.PublicIP
	return m, nil
}

func (p *provider) set(m byoc.Machine, f func(*byoc.Machine)) byoc.Machine {
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	got := p.c.machines[m.Instance]
	f(&got)
	p.c.machines[m.Instance] = got
	m.State, m.Size = got.State, got.Size
	return m
}

func (p *provider) Resize(_ context.Context, m byoc.Machine, size string, progress byoc.Progress) (byoc.Machine, error) {
	if err := p.fail("resize"); err != nil {
		return m, err
	}
	progress.Report("resize", byoc.StepDone, "resized to %s", size)
	return p.set(m, func(x *byoc.Machine) { x.Size = size }), nil
}

func (p *provider) Start(_ context.Context, m byoc.Machine) (byoc.Machine, error) {
	if err := p.fail("start"); err != nil {
		return m, err
	}
	return p.set(m, func(x *byoc.Machine) { x.State = byoc.Running }), nil
}

func (p *provider) Stop(_ context.Context, m byoc.Machine) (byoc.Machine, error) {
	if err := p.fail("stop"); err != nil {
		return m, err
	}
	return p.set(m, func(x *byoc.Machine) { x.State = byoc.Stopped }), nil
}

func (p *provider) Delete(_ context.Context, instance, _ string, progress byoc.Progress) error {
	if err := p.fail("delete"); err != nil {
		return err
	}
	p.c.mu.Lock()
	defer p.c.mu.Unlock()
	delete(p.c.machines, instance)
	if p.c.Leave {
		left := p.c.resources[instance][:1]
		p.c.resources[instance] = left
		return &byoc.LeftoverError{Resources: left}
	}
	delete(p.c.resources, instance)
	progress.Report("delete", byoc.StepDone, "deleted")
	return nil
}

func (p *provider) ListResources(_ context.Context, instance, _ string) ([]byoc.Resource, error) {
	out := p.c.Resources(instance)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
