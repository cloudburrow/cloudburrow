// Package byoc is bring your own cloud (#2209, phases 2 and 3): CloudBurrow
// provisions, scales and deletes the VM that runs an instance's compute in
// the user's own AWS account or Azure subscription, point and click.
//
// The API surface, the console and every endpoint stay on the user's
// machine. Only the instance's Docker (its kind cluster and the containers it
// starts) moves to the VM, through the remote-host engine of phase 1, which
// this package reaches through RemoteEngine.
//
// Each cloud is one Driver (sign-in) and one Provider (the account's VMs),
// in internal/byoc/awsprov and internal/byoc/azureprov. Credentials live in
// the OS keychain (internal/byoc/keychain), never on disk in plain text and
// never in a log. Every resource a provider creates carries the tag
// TagKey=<instance>, and Delete removes everything with it, then verifies
// that nothing is left.
package byoc

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Cloud names a cloud CloudBurrow can provision in.
type Cloud string

// The clouds.
const (
	AWS   Cloud = "aws"
	Azure Cloud = "azure"
)

// Title is the cloud's name as the console shows it.
func (c Cloud) Title() string {
	switch c {
	case AWS:
		return "AWS"
	case Azure:
		return "Azure"
	}
	return string(c)
}

// ParseCloud reads a cloud's name as the CLI and the console send it.
func ParseCloud(s string) (Cloud, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "aws", "amazon":
		return AWS, nil
	case "azure", "microsoft":
		return Azure, nil
	}
	return "", fmt.Errorf("unknown cloud %q: want aws or azure", s)
}

// TagKey is the tag every resource CloudBurrow creates carries, its value
// the instance's name. Delete and ListResources find resources by it.
const TagKey = "cloudburrow-instance"

// Preset is one of the wizard's sizes.
type Preset string

// The size presets (#2209): Small 4 vCPU/16 GB, Medium 16/64, Large 32/128,
// and GPU, one NVIDIA L4 or the nearest the cloud offers.
const (
	Small  Preset = "small"
	Medium Preset = "medium"
	Large  Preset = "large"
	GPU    Preset = "gpu"
)

// Presets is every preset in the order the size slider shows them.
var Presets = []Preset{Small, Medium, Large, GPU}

// Title is the preset's name as the console shows it.
func (p Preset) Title() string {
	switch p {
	case Small:
		return "Small"
	case Medium:
		return "Medium"
	case Large:
		return "Large"
	case GPU:
		return "GPU"
	}
	return string(p)
}

// ParsePreset reads a preset's name.
func ParsePreset(s string) (Preset, error) {
	p := Preset(strings.ToLower(strings.TrimSpace(s)))
	for _, q := range Presets {
		if p == q {
			return p, nil
		}
	}
	return "", fmt.Errorf("unknown size %q: want small, medium, large or gpu", s)
}

// Region is one of a cloud's regions.
type Region struct {
	Name        string  `json:"name"`
	DisplayName string  `json:"display_name"`
	Latitude    float64 `json:"latitude"`
	Longitude   float64 `json:"longitude"`
}

// Size is a machine size in the cloud's own terms, with its price.
type Size struct {
	Preset    Preset  `json:"preset"`
	Name      string  `json:"name"` // the instance type or VM size: m7i.xlarge, Standard_D4s_v5
	VCPUs     int     `json:"vcpus"`
	MemoryGiB float64 `json:"memory_gib"`
	GPU       string  `json:"gpu,omitempty"` // "NVIDIA L4"
	GPUs      int     `json:"gpus,omitempty"`
	// HourlyUSD is the on-demand Linux price per hour, from the cloud's
	// pricing API; 0 with PriceError set when it could not be read.
	HourlyUSD   float64 `json:"hourly_usd"`
	PriceSource string  `json:"price_source,omitempty"`
	PriceError  string  `json:"price_error,omitempty"`
	// DiskHourlyUSD is the boot disk's price per hour, billed while the VM
	// is stopped too.
	DiskHourlyUSD float64 `json:"disk_hourly_usd,omitempty"`
	// Note says how the size departs from the preset (Azure's GPU preset is
	// not an L4).
	Note string `json:"note,omitempty"`
}

// Account is who a connection signed in as.
type Account struct {
	ID        string `json:"id"`        // the AWS account ID or Azure subscription ID
	Name      string `json:"name"`      // the account alias or subscription name
	Principal string `json:"principal"` // the caller ARN or the signed-in user
}

// Permission is one action CloudBurrow needs, with why, in plain words.
type Permission struct {
	Action string `json:"action"`
	Why    string `json:"why"`
}

// PermissionReport is CheckPermissions' answer.
type PermissionReport struct {
	// Checked is false when the cloud would not say (the check itself was
	// refused); Note then says so.
	Checked bool         `json:"checked"`
	Missing []Permission `json:"missing,omitempty"`
	Note    string       `json:"note,omitempty"`
}

// OK reports whether nothing is known to be missing.
func (r PermissionReport) OK() bool { return len(r.Missing) == 0 }

// Summary is the report in plain words.
func (r PermissionReport) Summary() string {
	if len(r.Missing) == 0 {
		if !r.Checked {
			return "The permissions could not be checked: " + r.Note
		}
		return "Every permission CloudBurrow needs is granted."
	}
	var b strings.Builder
	b.WriteString("Missing permissions: ")
	for i, p := range r.Missing {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(p.Action + " (" + p.Why + ")")
	}
	return b.String()
}

// MachineSpec is what Create builds.
type MachineSpec struct {
	Instance string `json:"instance"`
	Region   string `json:"region"`
	Size     string `json:"size"`
	Preset   Preset `json:"preset"`
	DiskGiB  int    `json:"disk_gib"`
	// AutoStopMinutes is the idle time after which the VM stops itself; 0
	// never.
	AutoStopMinutes int `json:"auto_stop_minutes"`
	// SSHUser and SSHPublicKey are the login CloudBurrow's own key opens;
	// the private half stays in the keychain.
	SSHUser      string `json:"ssh_user"`
	SSHPublicKey string `json:"-"`
	// SourceCIDR is the only address SSH is open to: the user's own, /32.
	// Empty in ConnectSSM mode, where nothing inbound is open.
	SourceCIDR string `json:"source_cidr"`
	// ConnectMode is how CloudBurrow reaches the VM: ConnectSSM (AWS
	// Systems Manager Session Manager, no inbound port) or ConnectSSH.
	ConnectMode string `json:"connect_mode"`
	// UserData is the rendered cloud-init (RenderCloudInit).
	UserData string `json:"-"`
}

// State is a machine's state, the same words for every cloud.
type State string

// The states.
const (
	Provisioning State = "provisioning"
	Starting     State = "starting"
	Running      State = "running"
	Stopping     State = "stopping"
	Stopped      State = "stopped"
	Resizing     State = "resizing"
	Deleting     State = "deleting"
	Deleted      State = "deleted"
	Failed       State = "failed"
	Unknown      State = "unknown"
)

// Machine is one VM CloudBurrow made for an instance.
type Machine struct {
	Instance        string `json:"instance"`
	Cloud           Cloud  `json:"cloud"`
	Connection      string `json:"connection"`
	Region          string `json:"region"`
	Size            string `json:"size"`
	Preset          Preset `json:"preset"`
	DiskGiB         int    `json:"disk_gib"`
	AutoStopMinutes int    `json:"auto_stop_minutes"`
	State           State  `json:"state"`
	PublicIP        string `json:"public_ip,omitempty"`
	PrivateIP       string `json:"private_ip,omitempty"`
	// ConnectMode is ConnectSSM or ConnectSSH ("" is SSH, for a machine
	// recorded before modes existed).
	ConnectMode string `json:"connect_mode,omitempty"`
	// SourceCIDR is the address the firewall lets SSH in from, in SSH mode;
	// it follows the user's public address (Manager.FollowAddress).
	SourceCIDR string `json:"source_cidr,omitempty"`
	// SourceUpdatedAt is when the firewall's address last changed.
	SourceUpdatedAt time.Time `json:"source_updated_at,omitempty"`
	SSHUser         string    `json:"ssh_user,omitempty"`
	SSHPort         int       `json:"ssh_port,omitempty"`
	// HostKey is the VM's SSH host key (authorized_keys form), generated
	// here and installed by cloud-init, so the first connection is verified.
	HostKey string `json:"host_key,omitempty"`
	// IDs are the provider's own resource IDs (vpc, subnet, instance, ...).
	IDs       map[string]string `json:"ids,omitempty"`
	HourlyUSD float64           `json:"hourly_usd"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
	LastError string            `json:"last_error,omitempty"`
}

// Resource is one cloud resource carrying the instance's tag.
type Resource struct {
	Type   string `json:"type"` // "ec2:instance", "Microsoft.Network/virtualNetworks"
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
	Region string `json:"region,omitempty"`
	State  string `json:"state,omitempty"`
}

// EventStatus is how far a step got.
type EventStatus string

// The statuses.
const (
	StepRunning    EventStatus = "running"
	StepDone       EventStatus = "done"
	StepFailed     EventStatus = "failed"
	StepRolledBack EventStatus = "rolled-back"
)

// Event is one line of a create, resize or delete's progress.
type Event struct {
	Time    time.Time   `json:"time"`
	Step    string      `json:"step"` // "network", "firewall", "vm", "boot", ...
	Status  EventStatus `json:"status"`
	Message string      `json:"message"`
}

// Progress receives a long action's events, as they happen.
type Progress func(Event)

// Report sends an event if p is not nil.
func (p Progress) Report(step string, status EventStatus, format string, args ...any) {
	if p == nil {
		return
	}
	p(Event{Time: time.Now().UTC(), Step: step, Status: status, Message: fmt.Sprintf(format, args...)})
}

// Provider is one connected cloud account.
type Provider interface {
	Cloud() Cloud
	// Connect checks the credentials work and says who they are.
	Connect(ctx context.Context) (Account, error)
	// CheckPermissions says, in plain words, what the account may not do
	// that Create, Resize, Start, Stop or Delete needs.
	CheckPermissions(ctx context.Context, region string) (PermissionReport, error)
	Regions(ctx context.Context) ([]Region, error)
	// Sizes is every preset's size in region, with its hourly price from
	// the cloud's pricing API.
	Sizes(ctx context.Context, region string) ([]Size, error)
	// Create builds the network, firewall, VM and disk, tagged; on a
	// failure it removes what it made before returning the error.
	Create(ctx context.Context, spec MachineSpec, progress Progress) (Machine, error)
	// Status reads the machine's state and addresses.
	Status(ctx context.Context, m Machine) (Machine, error)
	// Resize changes the VM's size, stopping and starting it if the cloud
	// cannot resize it while it runs.
	Resize(ctx context.Context, m Machine, size string, progress Progress) (Machine, error)
	Start(ctx context.Context, m Machine) (Machine, error)
	Stop(ctx context.Context, m Machine) (Machine, error)
	// Delete removes every resource tagged with the instance, then lists
	// again and returns a *LeftoverError if anything is still there.
	Delete(ctx context.Context, instance, region string, progress Progress) error
	// ListResources is every resource tagged with the instance.
	ListResources(ctx context.Context, instance, region string) ([]Resource, error)
	// UpdateSourceAddress makes the firewall let SSH in from cidr only,
	// in place of m.SourceCIDR: the user's public address changed.
	UpdateSourceAddress(ctx context.Context, m Machine, cidr string) (Machine, error)
}

// The ways CloudBurrow reaches a VM.
const (
	// ConnectSSM is AWS Systems Manager Session Manager port forwarding to
	// the VM's sshd: the SSM agent on the VM dials out, and the security
	// group allows nothing inbound.
	ConnectSSM = "ssm"
	// ConnectSSH is SSH to the VM's public address, the firewall open to
	// the user's own address only.
	ConnectSSH = "ssh"
)

// Tunnel is a local listener that carries connections to a VM's port.
type Tunnel interface {
	// Port is the listener's port on 127.0.0.1.
	Port() int
	// LastError is why the latest connection could not be carried.
	LastError() error
	Close() error
}

// Tunneler is a Provider that reaches a VM's port without an inbound rule
// (AWS, through Session Manager).
type Tunneler interface {
	// OpenTunnel waits until the VM can be reached (its agent registered),
	// then listens on 127.0.0.1 for connections to its port.
	OpenTunnel(ctx context.Context, m Machine, port int, progress Progress) (Tunnel, error)
}

// Connection is one signed-in cloud account. It holds no secret: the
// credentials are the keychain item SecretName(ID).
type Connection struct {
	ID    string `json:"id"`
	Cloud Cloud  `json:"cloud"`
	// Method is how it signed in: "sso" (IAM Identity Center), "access-key"
	// or "browser" (Microsoft sign-in).
	Method      string            `json:"method"`
	AccountID   string            `json:"account_id"`
	AccountName string            `json:"account_name,omitempty"`
	Principal   string            `json:"principal,omitempty"`
	Detail      map[string]string `json:"detail,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
}

// ConnectionID is the ID a connection to account on cloud gets.
func ConnectionID(c Cloud, account string) string { return string(c) + "-" + account }

// SignInOptions start a browser sign-in.
type SignInOptions struct {
	// StartURL and Region are the AWS access portal and its IAM Identity
	// Center region.
	StartURL string `json:"start_url,omitempty"`
	Region   string `json:"region,omitempty"`
	// Tenant is the Microsoft Entra tenant; empty for the user's home one.
	Tenant string `json:"tenant,omitempty"`
	// OpenBrowser opens url in the user's browser.
	OpenBrowser func(url string) error `json:"-"`
}

// Prompt is what the user sees while a sign-in waits on them.
type Prompt struct {
	URL       string    `json:"url,omitempty"`
	UserCode  string    `json:"user_code,omitempty"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	Message   string    `json:"message"`
}

// Choice is an account or subscription the user may connect.
type Choice struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// SignIn is a browser sign-in under way.
type SignIn interface {
	Prompt() Prompt
	// Wait blocks until the user has signed in, and lists what they may
	// connect: AWS accounts and roles, or Azure subscriptions.
	Wait(ctx context.Context) ([]Choice, error)
	// Choose connects one of Wait's choices; the secret goes to the keychain.
	Choose(ctx context.Context, id string) (Connection, []byte, error)
}

// AccessKey is the AWS access-key fallback.
type AccessKey struct {
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
}

// Driver signs in to one cloud and opens its connections.
type Driver interface {
	Cloud() Cloud
	BeginSignIn(ctx context.Context, opts SignInOptions) (SignIn, error)
	// ConnectKey is the access-key fallback; a cloud without one refuses.
	ConnectKey(ctx context.Context, key AccessKey) (Connection, []byte, error)
	// Open is the connection's Provider. save stores refreshed credentials.
	Open(ctx context.Context, conn Connection, secret []byte, save func([]byte) error) (Provider, error)
}

// PermissionError is a refusal for want of a permission.
type PermissionError struct {
	Missing []Permission
	Message string
}

func (e *PermissionError) Error() string {
	if len(e.Missing) > 0 {
		return PermissionReport{Checked: true, Missing: e.Missing}.Summary()
	}
	return "permission denied: " + e.Message
}

// QuotaError is a refusal for want of quota or capacity.
type QuotaError struct{ Message string }

func (e *QuotaError) Error() string { return "quota or capacity: " + e.Message }

// AuthError says the credentials are no longer accepted: sign in again.
type AuthError struct{ Message string }

func (e *AuthError) Error() string { return "sign in again: " + e.Message }

// LeftoverError is a Delete whose check found resources still tagged.
type LeftoverError struct{ Resources []Resource }

func (e *LeftoverError) Error() string {
	names := make([]string, 0, len(e.Resources))
	for _, r := range e.Resources {
		names = append(names, r.Type+" "+r.ID)
	}
	sort.Strings(names)
	return fmt.Sprintf("delete left %d resources behind: %s", len(e.Resources), strings.Join(names, ", "))
}

// ErrNotFound is a machine or connection that does not exist.
var ErrNotFound = errors.New("not found")

// ErrUnsupported is an action a cloud does not offer.
var ErrUnsupported = errors.New("not supported")
