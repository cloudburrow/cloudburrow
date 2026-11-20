package compute

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// guestFS is the build context of every VM image (#1963): a Dockerfile per
// distribution family and the guest scripts (guest/root) every image
// carries: the account manager that applies ssh-keys, the startup-script
// runner and the network setup.
//
//go:embed guest
var guestFS embed.FS

// PublicImage is one public image CloudBurrow serves, backed by a container
// image of the same distribution built locally from the official image's
// pinned digest, plus systemd, sshd and the guest scripts.
type PublicImage struct {
	// Project is the image project (debian-cloud, ...).
	Project string
	// Name is the image's name, the family and the base's version.
	Name string
	// Family is the image family.
	Family string
	// Families are other family names that resolve to this image (an
	// architecture-qualified one).
	Families []string
	// Description is the image's description.
	Description string
	// Base is the official container image, by digest.
	Base string
	// Dockerfile names the build's Dockerfile in guest/.
	Dockerfile string
	// DiskSizeGb is the image's disk size.
	DiskSizeGb int
}

// PublicImages are the images served: the official container images of
// Debian 12, Ubuntu 24.04 LTS and Rocky Linux 9, pinned by their index
// digests (multi-architecture, read 2026-11-19 from Docker Hub).
var PublicImages = []PublicImage{
	{Project: "debian-cloud", Name: "debian-12-bookworm-v20261119", Family: "debian-12",
		Description: "Debian, Debian GNU/Linux, 12 (bookworm), built on 2026-11-19 (CloudBurrow: a systemd container of debian:12)",
		Base:        "docker.io/library/debian:12@sha256:268fb24c39bca92d326dd83ea3abebe4a3f7751c804ffef23bb6d857dd343ff8",
		Dockerfile:  "Dockerfile.debian", DiskSizeGb: 10},
	{Project: "ubuntu-os-cloud", Name: "ubuntu-2404-noble-v20261119", Family: "ubuntu-2404-lts",
		Families:    []string{"ubuntu-2404-lts-amd64", "ubuntu-2404-lts-arm64"},
		Description: "Canonical, Ubuntu, 24.04 LTS, built on 2026-11-19 (CloudBurrow: a systemd container of ubuntu:24.04)",
		Base:        "docker.io/library/ubuntu:24.04@sha256:73b999393d07f2bf65a40373778a44dc9a6d54ad7718d29247d8ff9413c4d6a8",
		Dockerfile:  "Dockerfile.debian", DiskSizeGb: 10},
	{Project: "rocky-linux-cloud", Name: "rocky-linux-9-v20261119", Family: "rocky-linux-9",
		Description: "Rocky Linux, Rocky Linux, 9, built on 2026-11-19 (CloudBurrow: a systemd container of rockylinux/rockylinux:9)",
		Base:        "docker.io/rockylinux/rockylinux:9@sha256:bb5755f96bec05ff26165e779ec843d7167544be9e50770508f307019a6d05c4",
		Dockerfile:  "Dockerfile.rocky", DiskSizeGb: 20},
}

// refusedImageProjects are public image projects whose images a container
// cannot run, with the reason.
var refusedImageProjects = map[string]string{
	"cos-cloud":                     "Container-Optimized OS boots its own kernel and read-only, verified root file system, which a VM run as a container cannot give (#1963)",
	"windows-cloud":                 "Windows Server images need a Windows kernel; CloudBurrow runs VMs as Linux containers (#1963)",
	"windows-sql-cloud":             "Windows Server images need a Windows kernel; CloudBurrow runs VMs as Linux containers (#1963)",
	"fedora-coreos-cloud":           "Fedora CoreOS boots its own kernel through Ignition, which a VM run as a container cannot give (#1963)",
	"ubuntu-os-pro-cloud":           "Ubuntu Pro images are licensed images; CloudBurrow serves ubuntu-os-cloud's ubuntu-2404-lts (#1963)",
	"rhel-cloud":                    "Red Hat Enterprise Linux images are licensed images; CloudBurrow serves rocky-linux-cloud's rocky-linux-9 (#1963)",
	"suse-cloud":                    "SUSE Linux Enterprise images are licensed images; CloudBurrow serves debian-12, ubuntu-2404-lts and rocky-linux-9 (#1963)",
	"deeplearning-platform-release": "Deep Learning VM images need GPUs and Google's drivers (#1963)",
}

// ImageRef is the container image tag a public image is built as: its
// family and a hash of its build context, so a changed guest script builds
// a new image.
func (p PublicImage) ImageRef() string {
	h := sha256.New()
	h.Write([]byte(p.Base))
	_ = fs.WalkDir(guestFS, "guest", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.HasPrefix(path, "guest/Dockerfile") && path != "guest/"+p.Dockerfile {
			return nil
		}
		b, _ := guestFS.ReadFile(path)
		h.Write([]byte(path))
		h.Write(b)
		return nil
	})
	return "cloudburrow-vm-" + p.Family + ":" + hex.EncodeToString(h.Sum(nil))[:12]
}

// BuildContext is the image's build context: path to contents, the
// Dockerfile at "Dockerfile".
func (p PublicImage) BuildContext() map[string][]byte {
	out := map[string][]byte{}
	_ = fs.WalkDir(guestFS, "guest", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := guestFS.ReadFile(path)
		rel := strings.TrimPrefix(path, "guest/")
		switch {
		case rel == p.Dockerfile:
			out["Dockerfile"] = b
		case strings.HasPrefix(rel, "Dockerfile"):
		default:
			out[rel] = b
		}
		return nil
	})
	return out
}

// arch is the images' architecture: the host's, as the containers run on it.
func arch() string {
	if runtime.GOARCH == "arm64" {
		return "ARM64"
	}
	return "X86_64"
}

func imageRel(p PublicImage) string {
	return "projects/" + p.Project + "/global/images/" + p.Name
}

func imageObj(p PublicImage) obj {
	fams := append([]string{p.Family}, p.Families...)
	sort.Strings(fams)
	return obj{"kind": "compute#image", "id": stableID(imageRel(p)), "name": p.Name, "family": p.Family,
		"description": p.Description, "selfLink": imageRel(p), "status": "READY", "sourceType": "RAW",
		"architecture": arch(), "diskSizeGb": strconv.Itoa(p.DiskSizeGb), "archiveSizeBytes": "0",
		"creationTimestamp": "2026-11-19T00:00:00.000-08:00", "storageLocations": []any{"us", "eu", "asia"},
		"licenses":        []any{"projects/" + p.Project + "/global/licenses/" + p.Family},
		"guestOsFeatures": []any{obj{"type": "UEFI_COMPATIBLE"}, obj{"type": "VIRTIO_SCSI_MULTIQUEUE"}, obj{"type": "GVNIC"}},
		"labels":          obj{}, "labelFingerprint": "42WmSpB8rSM=",
		"shieldedInstanceInitialState": obj{}}
}

// LookupImage is the public image a sourceImage names: an image
// (projects/P/global/images/NAME) or a family
// (projects/P/global/images/family/F), as a URL or a relative name. A
// zonal image family view (projects/P/zones/Z/imageFamilyViews/F), which
// gcloud compute instances create --image-family sends with the zone "-",
// names the family.
func LookupImage(ref string) (PublicImage, error) {
	rel := Rel(ref, "")
	parts := strings.Split(strings.TrimPrefix(rel, "projects/"), "/")
	if len(parts) == 5 && parts[1] == "zones" && parts[3] == "imageFamilyViews" && (parts[2] == "-" || IsZone(parts[2])) {
		parts = []string{parts[0], "global", "images", "family", parts[4]}
	}
	if len(parts) < 4 || parts[1] != "global" || parts[2] != "images" {
		return PublicImage{}, invalid("Invalid value for field 'sourceImage': '%s'. The URL is malformed.", ref)
	}
	project := parts[0]
	if why, ok := refusedImageProjects[project]; ok {
		return PublicImage{}, unimplemented("images of %s are not served: %s.", project, why)
	}
	if len(parts) == 5 && parts[3] == "family" {
		for _, p := range PublicImages {
			if p.Project != project {
				continue
			}
			if p.Family == parts[4] {
				return p, nil
			}
			for _, f := range p.Families {
				if f == parts[4] {
					return p, nil
				}
			}
		}
		return PublicImage{}, notFound("projects/" + project + "/global/images/family/" + parts[4])
	}
	for _, p := range PublicImages {
		if p.Project == project && p.Name == parts[3] {
			return p, nil
		}
	}
	return PublicImage{}, notFound(rel)
}

// images serves images.list, images.get and images.getFromFamily of a
// public image project; images.insert and every other method is 501.
func (s *Server) images(c call) (obj, error) {
	p := c.parts
	n := len(p)
	if why, ok := refusedImageProjects[c.project]; ok {
		return nil, unimplemented("images of %s are not served: %s.", c.project, why)
	}
	switch {
	case n == 2 && c.method == http.MethodGet:
		var all []obj
		f, err := parseFilter(first(c.query, "filter"))
		if err != nil {
			return nil, err
		}
		for _, img := range PublicImages {
			if img.Project == c.project {
				if o := imageObj(img); f.match(o) {
					all = append(all, o)
				}
			}
		}
		items, next, err := page(all, c.query)
		if err != nil {
			return nil, err
		}
		out := obj{"kind": "compute#imageList", "id": "projects/" + c.project + "/global/images", "items": items,
			"selfLink": "projects/" + c.project + "/global/images"}
		if items == nil {
			out["items"] = []obj{}
		}
		if next != "" {
			out["nextPageToken"] = next
		}
		return out, nil
	case n == 3 && c.method == http.MethodGet, n == 4 && c.method == http.MethodGet && p[2] == "family":
		img, err := LookupImage("projects/" + c.project + "/global/images/" + strings.Join(p[2:], "/"))
		if err != nil {
			return nil, err
		}
		return imageObj(img), nil
	case n == 2 && c.method == http.MethodPost:
		return nil, unimplemented("images.insert is not served: custom images are not built from a VM's disk yet (#1963); use the public images of debian-cloud, ubuntu-os-cloud and rocky-linux-cloud.")
	}
	return nil, unimplemented("%s on images of %s is not served (#1963)", c.method, c.project)
}

// RefusedImageProjects are the public image projects whose images are not
// served, each with the reason, by project name.
func RefusedImageProjects() [][2]string {
	var out [][2]string
	for p, why := range refusedImageProjects {
		out = append(out, [2]string{p, why})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}
