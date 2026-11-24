package compute

import "context"

// encrypted are the instance's disks that have a data key: the boot disk
// and the attached disks.
func (vm VMSpec) encrypted() []DiskSpec {
	var out []DiskSpec
	if vm.BootDisk.DataKey != nil {
		out = append(out, vm.BootDisk)
	}
	for _, d := range vm.Disks {
		if d.Disk.DataKey != nil {
			out = append(out, d.Disk)
		}
	}
	return out
}

// openElsewhereLocked is whether an instance other than self that is
// running (or suspended) has the disk d attached: its storage is then
// open, and stays so.
func (s *Server) openElsewhereLocked(d obj, self string) bool {
	for _, u := range strs(d, "users") {
		if u == self {
			continue
		}
		if o, ok := s.res[Rel(u, "")]; ok {
			switch str(o, "status") {
			case stTerminated, "":
			default:
				return true
			}
		}
	}
	return false
}

// unsealDisks opens encrypted disks for an instance to use.
func (s *Server) unsealDisks(ctx context.Context, disks []DiskSpec) error {
	for _, d := range disks {
		if err := s.opts.VMs.UnsealDisk(ctx, d); err != nil {
			return err
		}
	}
	return nil
}

// sealDisks seals encrypted disks again once no running instance has
// them, except those still in keep (by key).
func (s *Server) sealDisks(ctx context.Context, disks, keep []DiskSpec) error {
next:
	for _, d := range disks {
		for _, k := range keep {
			if k.Key == d.Key {
				continue next
			}
		}
		if d.Open {
			continue
		}
		if err := s.opts.VMs.SealDisk(ctx, d); err != nil {
			return err
		}
	}
	return nil
}

// hasCSEKDiskLocked is whether an instance has a disk with a
// customer-supplied key.
func (s *Server) hasCSEKDiskLocked(o obj) bool {
	l, _ := o["disks"].([]any)
	for _, e := range l {
		ad, _ := e.(map[string]any)
		if d, ok := s.res[Rel(str(ad, "source"), "")]; ok && isCSEK(d) {
			return true
		}
	}
	return false
}

// startKeys are the customer-supplied keys of an instances.start (none)
// or startWithEncryptionKey: InstancesStartWithEncryptionKeyRequest.disks,
// each a source and its diskEncryptionKey, by the disk's relative name.
func startKeys(c call, verb string) (map[string]map[string]any, error) {
	if verb != "startWithEncryptionKey" {
		return nil, nil
	}
	l, _ := c.body["disks"].([]any)
	if len(l) == 0 {
		return nil, required("disks")
	}
	out := map[string]map[string]any{}
	for _, e := range l {
		m, _ := e.(map[string]any)
		src := str(m, "source")
		if src == "" {
			return nil, required("disks.source")
		}
		ek, _ := m["diskEncryptionKey"].(map[string]any)
		out[Rel(src, c.project)] = ek
	}
	return out, nil
}
