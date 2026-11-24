package compute

// A disk's licenses, license codes, user licenses and guest OS features
// through disks.update and regionDisks.update (#2206), as
// https://docs.cloud.google.com/compute/docs/licenses/manage documents
// them (read 2026-11-24):
//
//   - paths=licenses replaces the disk's licenses with the body's (gcloud
//     --append-licenses, --remove-licenses and --replace-license read the
//     disk and send the result; an empty list removes them all);
//     paths=licenseCodes does the same by license code;
//   - "Only certain OS licenses are eligible to be appended, removed, or
//     replaced": RHEL, RHEL for SAP, SLES, SLES for SAP, Ubuntu, Ubuntu
//     Pro, Windows Server and SQL Server; a license of another OS (Debian,
//     Rocky Linux) is neither added nor removed;
//   - to change the license of a disk attached to a running instance,
//     "first either stop the instance or detach the disk";
//   - paths=userLicenses replaces the user licenses; paths=guestOsFeatures
//     the guest OS features (gcloud --add-guest-os-features adds GVNIC to
//     those the disk has).
//
// The licenses known here are those of the public images served, and
// Ubuntu Pro's for the Ubuntu image (the documented Ubuntu to Ubuntu Pro
// upgrade); a license's code is a stable number derived from its name.

import (
	"net/http"
	"strconv"
	"strings"
)

// eligibleLicenseProjects are the projects of the licenses that can be
// appended, removed or replaced.
var eligibleLicenseProjects = map[string]bool{"rhel-cloud": true, "rhel-sap-cloud": true, "suse-cloud": true,
	"suse-sap-cloud": true, "ubuntu-os-cloud": true, "ubuntu-os-pro-cloud": true, "windows-cloud": true,
	"windows-sql-cloud": true}

// extraLicenses are licenses no served image carries that a disk can
// take: Ubuntu Pro's.
var extraLicenses = []string{"projects/ubuntu-os-pro-cloud/global/licenses/ubuntu-pro-2404-lts"}

// guestOsFeatureTypes are GuestOsFeature.type's values.
var guestOsFeatureTypes = map[string]bool{"BARE_METAL_LINUX_COMPATIBLE": true, "GVNIC": true, "IDPF": true,
	"MULTI_IP_SUBNET": true, "SECURE_BOOT": true, "SEV_CAPABLE": true, "SEV_LIVE_MIGRATABLE": true,
	"SEV_LIVE_MIGRATABLE_V2": true, "SEV_SNP_CAPABLE": true, "SNP_SVSM_CAPABLE": true, "TDX_CAPABLE": true,
	"UEFI_COMPATIBLE": true, "VIRTIO_SCSI_MULTIQUEUE": true, "WINDOWS": true}

// knownLicenses are the licenses here, by relative name.
func knownLicenses() map[string]bool {
	out := map[string]bool{}
	for _, p := range PublicImages {
		out["projects/"+p.Project+"/global/licenses/"+p.Family] = true
	}
	for _, l := range extraLicenses {
		out[l] = true
	}
	return out
}

// licenseCode is a license's code.
func licenseCode(rel string) string { return stableID(rel) }

// licenseRel is a license reference as a relative name.
func licenseRel(ref, project string) string {
	rel := Rel(ref, project)
	if !strings.Contains(rel, "/") {
		rel = "projects/" + project + "/global/licenses/" + rel
	}
	return rel
}

func licenseProject(rel string) string {
	if p := strings.Split(rel, "/"); len(p) > 1 {
		return p[1]
	}
	return ""
}

// syncLicenseCodes sets a disk's licenseCodes from its licenses.
func syncLicenseCodes(d obj) {
	l := strs(d, "licenses")
	if len(l) == 0 {
		delete(d, "licenseCodes")
		return
	}
	codes := make([]any, len(l))
	for i, r := range l {
		codes[i] = licenseCode(Rel(r, ""))
	}
	d["licenseCodes"] = codes
}

// diskRunningUserLocked is an instance using d that is not stopped, or "".
func (s *Server) diskRunningUserLocked(d obj) string {
	for _, u := range strs(d, "users") {
		if o, ok := s.res[Rel(u, "")]; ok && str(o, "status") != stTerminated {
			return u
		}
	}
	return ""
}

// updateLicensesLocked applies paths licenses or licenseCodes to d.
func (s *Server) updateLicensesLocked(project, rel string, d, b obj, byCode bool) error {
	known := knownLicenses()
	byCodeMap := map[string]string{}
	for l := range known {
		byCodeMap[licenseCode(l)] = l
	}
	var next []string
	if byCode {
		l, _ := b["licenseCodes"].([]any)
		for i, v := range l {
			var c string
			switch x := v.(type) {
			case string:
				c = x
			case float64:
				c = strconv.FormatInt(int64(x), 10)
			}
			lr, ok := byCodeMap[c]
			if !ok {
				return invalid("Invalid value for field 'diskResource.licenseCodes[%d]': '%s'. The license code is not valid.", i, c)
			}
			next = append(next, lr)
		}
	} else {
		for _, ref := range strs(b, "licenses") {
			lr := licenseRel(ref, project)
			if !known[lr] {
				return notFound(lr)
			}
			if !hasStr(next, lr) {
				next = append(next, lr)
			}
		}
	}
	cur := strs(d, "licenses")
	var changed []string
	for _, l := range next {
		if !hasStr(cur, l) {
			changed = append(changed, l)
		}
	}
	for _, l := range cur {
		if !hasStr(next, l) {
			changed = append(changed, l)
		}
	}
	field := "licenses"
	if byCode {
		field = "licenseCodes"
	}
	for _, l := range changed {
		if !eligibleLicenseProjects[licenseProject(l)] {
			return invalid("Invalid value for field 'diskResource.%s': '%s'. Only certain OS licenses are eligible to be appended, removed, or replaced: RHEL, RHEL for SAP, SLES, SLES for SAP, Ubuntu, Ubuntu Pro, Windows Server and SQL Server.", field, l)
		}
	}
	if len(changed) > 0 {
		if u := s.diskRunningUserLocked(d); u != "" {
			return errf(http.StatusBadRequest, "resourceInUseByAnotherResource",
				"The disk resource '%s' is already being used by '%s': to change the license of a disk attached to a running instance, first either stop the instance or detach the disk.", rel, u)
		}
	}
	if len(next) == 0 {
		delete(d, "licenses")
	} else {
		d["licenses"] = toAny(next)
	}
	syncLicenseCodes(d)
	return nil
}

// updateUserLicenses applies paths userLicenses to d.
func updateUserLicenses(project string, d, b obj) error {
	var next []string
	for i, ref := range strs(b, "userLicenses") {
		lr := licenseRel(ref, project)
		if p := strings.Split(lr, "/"); len(p) != 5 || p[0] != "projects" || p[2] != "global" || p[3] != "licenses" || checkName("", p[4]) != nil {
			return invalid("Invalid value for field 'diskResource.userLicenses[%d]': '%s'.", i, ref)
		}
		if !hasStr(next, lr) {
			next = append(next, lr)
		}
	}
	if len(next) == 0 {
		delete(d, "userLicenses")
	} else {
		d["userLicenses"] = toAny(next)
	}
	return nil
}

// updateGuestOsFeatures applies paths guestOsFeatures to d.
func updateGuestOsFeatures(d, b obj) error {
	l, _ := b["guestOsFeatures"].([]any)
	var out []any
	seen := map[string]bool{}
	for i, e := range l {
		f, _ := e.(map[string]any)
		t := str(f, "type")
		if !guestOsFeatureTypes[t] {
			return invalid("Invalid value for field 'diskResource.guestOsFeatures[%d].type': '%s'.", i, t)
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, obj{"type": t})
		}
	}
	if len(out) == 0 {
		delete(d, "guestOsFeatures")
	} else {
		d["guestOsFeatures"] = out
	}
	return nil
}

// finishNewDiskLocked completes a new disk: the license codes and guest OS
// features of its image or source disk, and its customer-managed key.
func (s *Server) finishNewDiskLocked(d, b obj, field string) error {
	if str(d, "sourceImage") != "" {
		d["guestOsFeatures"] = imageGuestOsFeatures()
	}
	for _, src := range []string{str(d, "sourceDisk"), str(d, "sourceSnapshot")} {
		if so, ok := s.res[src]; ok && src != "" {
			for _, k := range []string{"licenses", "guestOsFeatures", "userLicenses"} {
				if v, ok := so[k]; ok {
					d[k] = v
				}
			}
		}
	}
	syncLicenseCodes(d)
	return s.applyDiskKeyLocked(d, b, field)
}

// imageGuestOsFeatures are the guest OS features of the public images.
func imageGuestOsFeatures() []any {
	return []any{obj{"type": "UEFI_COMPATIBLE"}, obj{"type": "VIRTIO_SCSI_MULTIQUEUE"}, obj{"type": "GVNIC"}}
}
