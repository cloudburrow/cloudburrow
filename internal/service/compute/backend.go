package compute

import "context"

// VMBackend runs VM instances and keeps their disks (#1963). The API above
// it (instances, disks, snapshots, machine types, zonal operations) is the
// same whatever runs them: today a systemd container in the instance's
// kind cluster (cmd/cloudburrow's computeVMs), later a microVM on Linux
// hosts with /dev/kvm.
//
// Every method is called without the Server's lock held, from the
// goroutine of the zonal operation that runs it, and may take as long as
// the work takes.
type VMBackend interface {
	// CreateDisk makes a disk's storage, from a snapshot's when
	// d.SourceSnapshot is set.
	CreateDisk(ctx context.Context, d DiskSpec) error
	// DeleteDisk removes a disk's storage.
	DeleteDisk(ctx context.Context, d DiskSpec) error
	// ResizeDisk grows a disk to d.SizeGb.
	ResizeDisk(ctx context.Context, d DiskSpec) error
	// Snapshot copies a disk's contents into a snapshot's storage.
	Snapshot(ctx context.Context, d DiskSpec, snapshotKey string) error
	// DeleteSnapshot removes a snapshot's storage.
	DeleteSnapshot(ctx context.Context, snapshotKey string) error
	// Start boots the instance (its boot disk seeded from its image on
	// first boot) and returns once it is running.
	Start(ctx context.Context, vm VMSpec) error
	// Stop shuts the instance down; its disks stay.
	Stop(ctx context.Context, vm VMSpec) error
	// Suspend freezes a running instance, its memory kept; Resume thaws it.
	Suspend(ctx context.Context, vm VMSpec) error
	Resume(ctx context.Context, vm VMSpec) error
	// SerialOutput is the instance's serial port 1 output since boot.
	SerialOutput(ctx context.Context, vm VMSpec) (string, error)
	// SSHAddress is the host address (host:port) that reaches the running
	// instance's port 22, or "" when there is none.
	SSHAddress(vm VMSpec) string
	// FreeBytes is the free space of the disks' file system.
	FreeBytes() (uint64, error)
}

// DiskSpec is a disk as the backend sees it.
type DiskSpec struct {
	// Key is the disk's storage name: stable, DNS-label safe and unique
	// across projects and zones.
	Key string
	// SizeGb is the disk's size.
	SizeGb int64
	// Type is the disk type (pd-balanced, pd-ssd, pd-standard, ...).
	Type string
	// Image is the image a boot disk is seeded from on first boot; nil for
	// a blank disk.
	Image *PublicImage
	// SourceSnapshot is the snapshot key the disk is made from, or "".
	SourceSnapshot string
}

// AttachedDisk is a non-boot disk of an instance.
type AttachedDisk struct {
	Disk       DiskSpec
	DeviceName string
	ReadOnly   bool
}

// VMSpec is an instance as the backend sees it.
type VMSpec struct {
	// Key is the instance's workload name: stable, DNS-label safe, unique.
	Key      string
	Project  string
	Zone     string
	Name     string
	Hostname string
	// Image is the boot disk's image.
	Image    PublicImage
	BootDisk DiskSpec
	Disks    []AttachedDisk
	// CPUs and MemoryMb are the machine type's.
	CPUs     int
	MemoryMb int
	// InternalIP is the address reserved in the instance's subnet.
	InternalIP string
	// GuestToken names the instance in its guest metadata URL
	// (/cloudburrow/guest/TOKEN/computeMetadata/v1/...).
	GuestToken string
	// ServiceAccount is the first serviceAccounts[] email, or "".
	ServiceAccount string
}
