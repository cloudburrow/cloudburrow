package compute

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Hyperdisk Balanced High Availability limits and disks.update (#2206), as
// https://docs.cloud.google.com/compute/docs/disks/hd-types/hyperdisk-balanced-ha
// and https://docs.cloud.google.com/compute/docs/disks/sharing-disks-between-vms
// document them (read 2026-11-24):
//
//   - size 4 GiB to 64 TiB; IOPS 3,000 to 100,000 (a 4 GiB volume is fixed at
//     2,000, a 5 GiB volume at 2,500), at most 500 per GiB; throughput at
//     least max(140, IOPS/256) and at most min(2,400, IOPS/4) MiB/s;
//   - defaults: IOPS 500 x size up to 6 GiB, else min(6 x size + 3,000,
//     100,000); throughput 140 MiB/s up to 6 GiB, else min(2,400,
//     1.5 x size + 140);
//   - "You can't set IOPS or throughput without setting a size"; the size
//     changes at most twice in 4 hours, the performance once in 4 hours;
//   - access modes READ_WRITE_SINGLE (the default) and READ_WRITE_MANY
//     (multi-writer) only; the mode changes only while the disk is
//     detached from every instance;
//   - multi-writer: at most 8 instances, of the C4, C4A, C3, C3D, M3, N4,
//     N4A, N4D and Z3 series; never a boot disk, never auto-deleted, never
//     made with the instance;
//   - the machine series that support the type at all (haSeries); C2D, E2,
//     N1, N2 and N2D only through Google's account team, so refused here.
//
// Wording: Google's where its pages quote it (the IOPS, throughput, size,
// machine type and rate-limit texts of
// https://docs.cloud.google.com/compute/docs/troubleshooting/troubleshooting-disk-creation
// and troubleshooting-disk-full-resize, read 2026-11-24); the rest follow
// Compute Engine's "Invalid value for field" form, unverified (gaps.md).

const hdbHA = "hyperdisk-balanced-high-availability"

// Access modes.
const (
	rwSingle = "READ_WRITE_SINGLE"
	rwMany   = "READ_WRITE_MANY"
	roMany   = "READ_ONLY_MANY"
)

// haSeries are the machine series Hyperdisk Balanced High Availability
// attaches to; haMultiWriter those that attach it in multi-writer mode.
var (
	haSeries = map[string]bool{"a3": true, "a5x": true, "c3": true, "c3d": true, "c4": true, "c4a": true, "c4d": true,
		"c4n": true, "g4": true, "m3": true, "m4n": true, "n4": true, "n4a": true, "n4d": true, "x5": true, "z3": true,
		"z4d": true, "z4m": true}
	haMultiWriter = map[string]bool{"c4": true, "c4a": true, "c3": true, "c3d": true, "m3": true, "n4": true, "n4a": true,
		"n4d": true, "z3": true}
)

// haMaxWriters is how many instances a multi-writer volume attaches to.
const haMaxWriters = 8

// perfWindow is the window of the size and performance change limits.
const perfWindow = 4 * time.Hour

// Private fields: the times of a disk's last size changes and its last
// performance change.
const (
	sizeChangesKey = "_sizeChanges"
	perfChangedKey = "_perfChanged"
)

func seriesOf(machineType string) string {
	if i := strings.Index(machineType, "-"); i > 0 {
		return machineType[:i]
	}
	return machineType
}

func isHA(d obj) bool { return lastSeg(str(d, "type")) == hdbHA }

// haDefaults are a volume's default IOPS and throughput.
func haDefaults(size int64) (iops, tput int64) {
	if size <= 6 {
		return 500 * size, 140
	}
	return min(6*size+3000, 100000), min(2400, size*3/2+140)
}

// haIopsRange is the IOPS a volume of size GiB may have.
func haIopsRange(size int64) (lo, hi int64) {
	switch {
	case size < 6:
		return 500 * size, 500 * size
	case size <= 200:
		return 3000, min(500*size, 100000)
	}
	return 3000, 100000
}

// haTputRange is the throughput a volume with iops IOPS may have.
func haTputRange(iops int64) (lo, hi int64) {
	return max(140, (iops+255)/256), min(2400, iops/4)
}

func intField(v any, field string) (int64, bool, error) {
	switch x := v.(type) {
	case nil:
		return 0, false, nil
	case float64:
		return int64(x), true, nil
	case int:
		return int64(x), true, nil
	case int64:
		return x, true, nil
	case int32:
		return int64(x), true, nil
	case string:
		n, err := strconv.ParseInt(x, 10, 64)
		if err != nil {
			return 0, false, invalid("Invalid value for field '%s': '%s'.", field, x)
		}
		return n, true, nil
	}
	return 0, false, invalid("Invalid value for field '%s'.", field)
}

// checkHAPerf validates a volume's IOPS and throughput for its size.
func checkHAPerf(size, iops, tput int64, field string) error {
	// Google's texts (troubleshooting-disk-creation, read 2026-11-24).
	lo, hi := haIopsRange(size)
	switch {
	case iops < lo:
		return invalid("Invalid value for field '%s.provisionedIops': '%d'. Requested provisioned IOPS cannot be smaller than %d.", field, iops, lo)
	case iops > 100000:
		return invalid("Invalid value for field '%s.provisionedIops': '%d'. Requested provisioned IOPS cannot be higher than 100,000.", field, iops)
	case iops > hi:
		return invalid("Invalid value for field '%s.provisionedIops': '%d'. Requested provisioned IOPS is too high for the current disk size", field, iops)
	}
	tlo, thi := haTputRange(iops)
	switch {
	case tput < tlo:
		return invalid("Invalid value for field '%s.provisionedThroughput': '%d'. Requested provisioned throughput cannot be smaller than %d.", field, tput, tlo)
	case tput > 2400:
		return invalid("Invalid value for field '%s.provisionedThroughput': '%d'. Requested provisioned throughput cannot be higher than 2,400.", field, tput)
	case tput > thi:
		return invalid("Invalid value for field '%s.provisionedThroughput': '%d'. Requested provisioned throughput is too high for the requested disk size.", field, tput)
	}
	return nil
}

// applyDiskPerf sets a new disk's provisionedIops,
// provisionedThroughput and accessMode from b: for Hyperdisk Balanced High
// Availability its documented defaults and limits; any other type takes
// none of them.
func applyDiskPerf(d, b obj, sizeSet bool, field string) error {
	typ := lastSeg(str(d, "type"))
	iops, iopsSet, err := intField(b["provisionedIops"], field+".provisionedIops")
	if err != nil {
		return err
	}
	tput, tputSet, err := intField(b["provisionedThroughput"], field+".provisionedThroughput")
	if err != nil {
		return err
	}
	mode := str(b, "accessMode")
	if typ != hdbHA {
		if strings.HasPrefix(typ, "pd-") {
			if iopsSet && typ != "pd-extreme" {
				return invalid("Invalid value for field '%s.provisionedIops': '%d'. Provisioned IOPS can't be set for disk type %s.", field, iops, typ)
			}
			if tputSet {
				return invalid("Invalid value for field '%s.provisionedThroughput': '%d'. Provisioned throughput can't be set for disk type %s.", field, tput, typ)
			}
			if mode != "" && mode != rwSingle {
				return invalid("Invalid value for field '%s.accessMode': '%s'. Access modes apply to Hyperdisk volumes only, not to disk type %s.", field, mode, typ)
			}
		}
		return nil
	}
	if (iopsSet || tputSet) && !sizeSet {
		return invalid("Invalid value for field '%s.sizeGb': ''. You can't set IOPS or throughput without setting a size.", field)
	}
	size, _, _ := sizeGb(d["sizeGb"])
	di, dt := haDefaults(size)
	if !iopsSet {
		iops = di
	}
	if !tputSet {
		tput = dt
		// The default fits the IOPS asked for.
		lo, hi := haTputRange(iops)
		tput = min(max(tput, lo), hi)
	}
	if err := checkHAPerf(size, iops, tput, field); err != nil {
		return err
	}
	if mode == "" {
		mode = rwSingle
	}
	if err := checkHAMode(mode, field+".accessMode"); err != nil {
		return err
	}
	d["provisionedIops"] = strconv.FormatInt(iops, 10)
	d["provisionedThroughput"] = strconv.FormatInt(tput, 10)
	d["accessMode"] = mode
	return nil
}

func checkHAMode(mode, field string) error {
	switch mode {
	case rwSingle, rwMany:
		return nil
	case roMany:
		return invalid("Invalid value for field '%s': '%s'. Hyperdisk Balanced High Availability supports the single-writer (READ_WRITE_SINGLE) and multi-writer (READ_WRITE_MANY) access modes only.", field, mode)
	}
	return invalid("Invalid value for field '%s': '%s'.", field, mode)
}

// haAttachOK applies the attach rules of a Hyperdisk Balanced High
// Availability volume: its machine series, and multi-writer's.
func (s *Server) haAttachOK(d obj, rel, machineType string, e map[string]any, mode string, boot bool, users []string, field string) error {
	if !isHA(d) {
		return nil
	}
	series := seriesOf(machineType)
	if machineType != "" && !haSeries[series] {
		return errf(http.StatusBadRequest, "badRequest", "%s disk type cannot be used by %s machine type.", hdbHA, machineType)
	}
	if str(d, "accessMode") != rwMany {
		return nil
	}
	if boot {
		return invalid("Invalid value for field '%s.boot': 'true'. A Hyperdisk volume in multi-writer mode can't be used as a boot disk.", field)
	}
	if ad, _ := e["autoDelete"].(bool); ad {
		return invalid("Invalid value for field '%s.autoDelete': 'true'. Auto-delete can't be enabled for a Hyperdisk volume in multi-writer mode.", field)
	}
	if machineType != "" && !haMultiWriter[series] {
		return invalid("Invalid value for field '%s.source': '%s'. Machine type %s does not support Hyperdisk Balanced High Availability in multi-writer mode, which is supported on the C3, C3D, C4, C4A, M3, N4, N4A, N4D and Z3 machine series.", field, rel, machineType)
	}
	if mode == "READ_WRITE" && len(users) >= haMaxWriters {
		return invalid("Invalid value for field '%s.source': '%s'. A Hyperdisk Balanced High Availability volume in multi-writer mode can be attached to at most %d instances.", field, rel, haMaxWriters)
	}
	return nil
}

// recentChanges are the times in l within perfWindow of now.
func recentChanges(l []string, now time.Time) []string {
	var out []string
	for _, t := range l {
		if at, err := time.Parse(time.RFC3339Nano, t); err == nil && now.Sub(at) < perfWindow {
			out = append(out, t)
		}
	}
	return out
}

// noteSizeChange applies "size can be changed at most twice in a
// 4-hour window" to a Hyperdisk Balanced High Availability volume, and
// records the change.
func (s *Server) noteSizeChange(d obj, rel string) error {
	if !isHA(d) {
		return nil
	}
	now := s.opts.Now()
	l := recentChanges(strs(d, sizeChangesKey), now)
	if len(l) >= 2 {
		return errf(http.StatusBadRequest, "rateLimitExceeded", "Disk cannot be resized due to being rate limited.")
	}
	d[sizeChangesKey] = toAny(append(l, now.UTC().Format(time.RFC3339Nano)))
	return nil
}

// updateDiskLocked is disks.update and regionDisks.update: the fields the
// update mask (paths, or updateMask) names, from the body's Disk.
func (s *Server) updateDiskLocked(c call, scope, rel string, d obj) (obj, error) {
	var paths []string
	for _, p := range c.query["paths"] {
		paths = append(paths, strings.Split(p, ",")...)
	}
	for _, p := range c.query["updateMask"] {
		paths = append(paths, strings.Split(p, ",")...)
	}
	set := map[string]bool{}
	for _, p := range paths {
		if p = strings.TrimSpace(p); p != "" {
			set[camel(p)] = true
		}
	}
	if len(set) == 0 {
		return nil, invalid("Invalid value for field 'paths': ''. Name at least one field to update in paths (the update mask).")
	}
	var names []string
	for p := range set {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		switch p {
		case "sizeGb", "provisionedIops", "provisionedThroughput", "accessMode", "architecture":
		case "licenses", "licenseCodes", "userLicenses", "guestOsFeatures":
		default:
			return nil, invalid("Invalid value for field 'paths': '%s'. The field can't be updated: disks.update updates sizeGb, provisionedIops, provisionedThroughput, accessMode, architecture, licenses, licenseCodes, userLicenses and guestOsFeatures.", p)
		}
	}
	b := c.body
	if set["licenses"] && set["licenseCodes"] {
		return nil, invalid("Invalid value for field 'paths': 'licenseCodes'. Update licenses or licenseCodes, not both.")
	}
	// Licenses, user licenses and guest OS features, on a copy until every
	// field is valid.
	lic := obj{}
	for k, v := range d {
		lic[k] = v
	}
	switch {
	case set["licenses"]:
		if err := s.updateLicensesLocked(c.project, rel, lic, b, false); err != nil {
			return nil, err
		}
	case set["licenseCodes"]:
		if err := s.updateLicensesLocked(c.project, rel, lic, b, true); err != nil {
			return nil, err
		}
	}
	if set["userLicenses"] {
		if err := updateUserLicenses(c.project, lic, b); err != nil {
			return nil, err
		}
	}
	if set["guestOsFeatures"] {
		if err := updateGuestOsFeatures(lic, b); err != nil {
			return nil, err
		}
	}
	typ := lastSeg(str(d, "type"))
	cur, _, _ := sizeGb(d["sizeGb"])
	size := cur
	if set["sizeGb"] {
		n, ok, err := sizeGb(b["sizeGb"])
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, required("diskResource.sizeGb")
		}
		if n < cur {
			return nil, invalid("Invalid value for field 'diskResource.sizeGb': '%d'. New disk size '%d' GiB must be larger than existing size '%d' GiB.", n, n, cur)
		}
		lim := diskTypes[typ]
		if isRegional(d) {
			lim = regionDiskTypes[typ]
		}
		if n > lim.max {
			return nil, invalid("Invalid value for field 'diskResource.sizeGb': '%d'. Disk size cannot be greater than %s GB for disk type %s.", n, commas(lim.max), typ)
		}
		size = n
	}
	perf := set["provisionedIops"] || set["provisionedThroughput"]
	iops, _, _ := intField(d["provisionedIops"], "provisionedIops")
	tput, _, _ := intField(d["provisionedThroughput"], "provisionedThroughput")
	if perf {
		if typ != hdbHA {
			return nil, invalid("Invalid value for field 'paths': 'provisionedIops'. Provisioned performance can't be changed for disk type %s.", typ)
		}
		if set["provisionedIops"] {
			n, ok, err := intField(b["provisionedIops"], "diskResource.provisionedIops")
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, required("diskResource.provisionedIops")
			}
			iops = n
		}
		if set["provisionedThroughput"] {
			n, ok, err := intField(b["provisionedThroughput"], "diskResource.provisionedThroughput")
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, required("diskResource.provisionedThroughput")
			}
			tput = n
		}
	}
	if typ == hdbHA && (perf || size != cur) {
		if err := checkHAPerf(size, iops, tput, "diskResource"); err != nil {
			return nil, err
		}
	}
	mode := str(d, "accessMode")
	if set["accessMode"] {
		m := str(b, "accessMode")
		if typ != hdbHA {
			return nil, invalid("Invalid value for field 'diskResource.accessMode': '%s'. Access modes apply to Hyperdisk volumes only, not to disk type %s.", m, typ)
		}
		if err := checkHAMode(m, "diskResource.accessMode"); err != nil {
			return nil, err
		}
		if m != mode && len(strs(d, "users")) > 0 {
			// "To change an existing disk to multi-writer, you must first
			// detach the disk from all instances."
			return nil, errf(http.StatusBadRequest, "resourceInUseByAnotherResource",
				"The disk resource '%s' is already being used by '%s': detach the disk from all instances before changing its access mode.", rel, strs(d, "users")[0])
		}
		mode = m
	}
	architecture := str(b, "architecture")
	if set["architecture"] && architecture != "X86_64" && architecture != "ARM64" {
		return nil, invalid("Invalid value for field 'diskResource.architecture': '%s'.", architecture)
	}
	now := s.opts.Now()
	if perf && typ == hdbHA {
		if t := str(d, perfChangedKey); t != "" {
			if at, err := time.Parse(time.RFC3339Nano, t); err == nil && now.Sub(at) < perfWindow {
				what := "iops"
				if !set["provisionedIops"] {
					what = "throughput"
				}
				return nil, errf(http.StatusBadRequest, "rateLimitExceeded", "Cannot update provisioned %s due to being rate limited.", what)
			}
		}
	}
	grow := size != cur
	if grow {
		if err := s.noteSizeChange(d, rel); err != nil {
			return nil, err
		}
	}
	if perf && typ == hdbHA {
		d[perfChangedKey] = now.UTC().Format(time.RFC3339Nano)
		d["provisionedIops"] = strconv.FormatInt(iops, 10)
		d["provisionedThroughput"] = strconv.FormatInt(tput, 10)
	}
	if mode != "" {
		d["accessMode"] = mode
	}
	if set["architecture"] {
		d["architecture"] = architecture
	}
	for _, k := range []string{"licenses", "licenseCodes", "userLicenses", "guestOsFeatures"} {
		if v, ok := lic[k]; ok {
			d[k] = v
		} else {
			delete(d, k)
		}
	}
	var specs []DiskSpec
	if grow {
		d["sizeGb"] = strconv.FormatInt(size, 10)
		specs = append(specs, s.diskSpecLocked(d))
		if d["asyncPrimaryDisk"] == nil {
			if peer := s.replicatingPeerLocked(d); peer != "" {
				if sd, ok := s.res[peer]; ok {
					sd["sizeGb"] = d["sizeGb"]
					specs = append(specs, s.diskSpecLocked(sd))
				}
			}
		}
	}
	return s.runOpLocked(c.project, scope, "update", rel, func(ctx context.Context) error {
		for _, sp := range specs {
			if err := s.opts.VMs.ResizeDisk(ctx, sp); err != nil {
				return err
			}
		}
		return nil
	}, nil)
}

// camel is a field mask path in the JSON (lowerCamelCase) form: an
// updateMask may name provisioned_iops as well as provisionedIops.
func camel(p string) string {
	parts := strings.Split(p, "_")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

// commas is n with thousands separators, as Google's messages write them.
func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
