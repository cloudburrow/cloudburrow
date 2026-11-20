package compute

import (
	"net/http"
	"strconv"
	"strings"
)

// MachineType is one predefined machine type
// (https://docs.cloud.google.com/compute/docs/machine-resource).
type MachineType struct {
	Name     string
	CPUs     int
	MemoryMb int
	Shared   bool
}

// MachineTypes are the machine types every zone offers here: the general
// purpose families' predefined types. A VM's container is given the type's
// vCPUs and memory as its limits.
var MachineTypes = func() []MachineType {
	out := []MachineType{
		{"e2-micro", 2, 1024, true}, {"e2-small", 2, 2048, true}, {"e2-medium", 2, 4096, true},
		{"f1-micro", 1, 614, true}, {"g1-small", 1, 1740, true},
	}
	for _, n := range []int{2, 4, 8, 16, 32} {
		out = append(out, MachineType{"e2-standard-" + strconv.Itoa(n), n, n * 4096, false},
			MachineType{"e2-highmem-" + strconv.Itoa(n), n, n * 8192, false},
			MachineType{"e2-highcpu-" + strconv.Itoa(n), n, n * 1024, false})
	}
	for _, n := range []int{1, 2, 4, 8, 16, 32} {
		out = append(out, MachineType{"n1-standard-" + strconv.Itoa(n), n, n * 3840, false})
	}
	for _, n := range []int{2, 4, 8, 16, 32} {
		out = append(out, MachineType{"n2-standard-" + strconv.Itoa(n), n, n * 4096, false},
			MachineType{"n2d-standard-" + strconv.Itoa(n), n, n * 4096, false})
	}
	for _, n := range []int{4, 8, 22} {
		out = append(out, MachineType{"c3-standard-" + strconv.Itoa(n), n, n * 4096, false})
	}
	return out
}()

// LookupMachineType is the machine type a name names.
func LookupMachineType(name string) (MachineType, bool) {
	for _, m := range MachineTypes {
		if m.Name == name {
			return m, true
		}
	}
	return MachineType{}, false
}

func machineTypeObj(project, zone, name string) obj {
	m, _ := LookupMachineType(name)
	desc := strconv.Itoa(m.CPUs) + " vCPUs, " + strconv.FormatFloat(float64(m.MemoryMb)/1024, 'f', -1, 64) + " GB RAM"
	if m.Shared {
		desc = strconv.Itoa(m.CPUs) + " vCPU (shared physical core) and " + strconv.FormatFloat(float64(m.MemoryMb)/1024, 'f', -1, 64) + " GB RAM"
	}
	rel := "projects/" + project + "/zones/" + zone + "/machineTypes/" + name
	return obj{"kind": "compute#machineType", "id": stableID(name), "name": name, "description": desc,
		"guestCpus": float64(m.CPUs), "memoryMb": float64(m.MemoryMb), "isSharedCpu": m.Shared,
		"maximumPersistentDisks": 128.0, "maximumPersistentDisksSizeGb": "263168",
		"imageSpaceGb": 0.0, "zone": zone, "selfLink": rel, "creationTimestamp": "1969-12-31T16:00:00.000-08:00",
		"architecture": arch()}
}

func (s *Server) machineTypes(c call, zone string) (obj, error) {
	p := c.parts
	if c.method != http.MethodGet {
		return nil, unimplemented("%s on machineTypes", c.method)
	}
	if len(p) == 3 {
		names := make([]string, 0, len(MachineTypes))
		for _, m := range MachineTypes {
			names = append(names, m.Name)
		}
		return s.listFixed("compute#machineTypeList", c, names, func(project, name string) obj {
			return machineTypeObj(project, zone, name)
		})
	}
	if len(p) == 4 {
		if _, ok := LookupMachineType(p[3]); !ok {
			if strings.HasPrefix(p[3], "custom-") {
				return nil, unimplemented("custom machine types are not served (#1963); use a predefined type")
			}
			return nil, notFound("projects/" + c.project + "/zones/" + zone + "/machineTypes/" + p[3])
		}
		return machineTypeObj(c.project, zone, p[3]), nil
	}
	return nil, unimplemented("machineTypes/%s", strings.Join(p[3:], "/"))
}
