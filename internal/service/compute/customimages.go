package compute

import (
	"context"
	"net/http"
	"strconv"
	"strings"
)

// Custom images (#2139, https://docs.cloud.google.com/compute/docs/images/create-custom,
// deprecate-custom, delete-custom, read 2026-11-22): images.insert from a
// disk, a snapshot or another image into the caller's own project, get,
// list, getFromFamily (the newest image of the family not deprecated),
// deprecate, setLabels and delete. An image's contents are a copy of its
// source disk's storage (VMBackend.Snapshot under the image's own key), and
// a disk made from it starts as a copy of that, running the public image
// the source disk was made from.

// isPublicImageProject says whether project is one of the public image
// projects, served or refused.
func isPublicImageProject(project string) bool {
	if _, ok := refusedImageProjects[project]; ok {
		return true
	}
	for _, p := range PublicImages {
		if p.Project == project {
			return true
		}
	}
	return false
}

// imageSeed is what a disk made from sourceImage starts from: the public
// image it runs and, for a custom image, the storage key of its contents.
func (s *Server) imageSeedLocked(ref, project string) (PublicImage, obj, error) {
	rel := Rel(ref, project)
	parts := strings.Split(strings.TrimPrefix(rel, "projects/"), "/")
	if len(parts) >= 4 && parts[1] == "global" && parts[2] == "images" && !isPublicImageProject(parts[0]) {
		var o obj
		if len(parts) == 5 && parts[3] == "family" {
			o = s.familyImageLocked(parts[0], parts[4])
			if o == nil {
				return PublicImage{}, nil, notFound(rel)
			}
		} else {
			var ok bool
			if o, ok = s.res["projects/"+parts[0]+"/global/images/"+parts[3]]; !ok {
				return PublicImage{}, nil, notFound(rel)
			}
		}
		if str(o, "status") != "READY" {
			return PublicImage{}, nil, errf(http.StatusBadRequest, "resourceNotReady", "The resource '%s' is not ready", str(o, "selfLink"))
		}
		if dep, _ := o["deprecated"].(map[string]any); str(dep, "state") == "DELETED" || str(dep, "state") == "OBSOLETE" {
			return PublicImage{}, nil, invalid("Invalid value for field 'sourceImage': '%s'. The image is %s.", ref, strings.ToLower(str(dep, "state")))
		}
		img, err := LookupImage(str(o, "_sourceImage"))
		return img, o, err
	}
	img, err := LookupImage(ref)
	return img, nil, err
}

// familyImageLocked is the newest image of a project's family that is not
// deprecated, as getFromFamily answers.
func (s *Server) familyImageLocked(project, family string) obj {
	var best obj
	prefix := "projects/" + project + "/global/images/"
	for k, o := range s.res {
		if !strings.HasPrefix(k, prefix) || str(o, "family") != family || str(o, "status") != "READY" {
			continue
		}
		if dep, _ := o["deprecated"].(map[string]any); str(dep, "state") != "" && str(dep, "state") != "ACTIVE" {
			continue
		}
		if best == nil || str(o, "creationTimestamp") > str(best, "creationTimestamp") {
			best = o
		}
	}
	return best
}

func (s *Server) customImages(c call) (obj, error) {
	p, n := c.parts, len(c.parts)
	coll := "projects/" + c.project + "/global/images"
	get, post, del := c.method == http.MethodGet, c.method == http.MethodPost, c.method == http.MethodDelete
	switch {
	case n == 2 && get:
		return s.listOut("compute#imageList", coll, s.res, c.query)
	case n == 2 && post:
		return s.insertImage(c)
	case n == 4 && get && p[2] == "family":
		if o := s.familyImageLocked(c.project, p[3]); o != nil {
			return o, nil
		}
		return nil, notFound(coll + "/family/" + p[3])
	}
	if n < 3 || n > 4 || (n == 3 && !get && !del) || (n == 4 && (!post || (p[3] != "deprecate" && p[3] != "setLabels"))) {
		return nil, unimplemented("%s on images/%s is not served", c.method, strings.Join(p[2:], "/"))
	}
	rel := coll + "/" + p[2]
	o, ok := s.res[rel]
	if !ok {
		return nil, notFound(rel)
	}
	switch {
	case n == 3 && get:
		return o, nil
	case n == 3 && del:
		if s.opts.VMs == nil {
			return nil, noVMs()
		}
		key := str(o, "_storageKey")
		o["status"] = "DELETING"
		return s.runOpLocked(c.project, "global", "delete", rel, func(ctx context.Context) error {
			if key == "" {
				return nil
			}
			return s.opts.VMs.DeleteSnapshot(ctx, key)
		}, func(err error) {
			if err == nil {
				delete(s.res, rel)
			} else if cur, ok := s.res[rel]; ok {
				cur["status"] = "READY"
			}
		})
	case n == 4 && post && p[3] == "deprecate":
		st := str(c.body, "state")
		switch st {
		case "", "ACTIVE":
			delete(o, "deprecated")
		case "DEPRECATED", "OBSOLETE", "DELETED":
			dep := obj{"state": st}
			if r := str(c.body, "replacement"); r != "" {
				dep["replacement"] = Rel(r, c.project)
			}
			for _, k := range []string{"deprecated", "obsolete", "deleted"} {
				if v := str(c.body, k); v != "" {
					dep[k] = v
				}
			}
			o["deprecated"] = dep
		default:
			return nil, invalid("Invalid value for field 'state': '%s'. Must be one of ACTIVE, DEPRECATED, OBSOLETE or DELETED.", st)
		}
		return s.done(c.project, "global", "deprecate", rel, o["id"])
	case n == 4 && post && p[3] == "setLabels":
		return s.setLabels(c.project, "global", rel, c.body)
	}
	return nil, unimplemented("%s on images/%s is not served", c.method, strings.Join(p[2:], "/"))
}

func (s *Server) insertImage(c call) (obj, error) {
	if s.opts.VMs == nil {
		return nil, noVMs()
	}
	b := c.body
	name := str(b, "name")
	if err := checkName("resource.name", name); err != nil {
		return nil, err
	}
	rel := "projects/" + c.project + "/global/images/" + name
	if _, ok := s.res[rel]; ok {
		return nil, errf(http.StatusConflict, "alreadyExists", "The resource '%s' already exists", rel)
	}
	o := obj{"kind": "compute#image", "id": newID(), "name": name, "selfLink": rel, "status": "PENDING", "sourceType": "RAW",
		"creationTimestamp": s.stamp(), "architecture": arch(), "archiveSizeBytes": "0", "labelFingerprint": labelFingerprint(nil),
		"guestOsFeatures": []any{obj{"type": "UEFI_COMPATIBLE"}, obj{"type": "VIRTIO_SCSI_MULTIQUEUE"}, obj{"type": "GVNIC"}}}
	for _, k := range []string{"description", "family"} {
		if v := str(b, k); v != "" {
			o[k] = v
		}
	}
	if l, ok := b["labels"].(map[string]any); ok && len(l) > 0 {
		o["labels"], o["labelFingerprint"] = l, labelFingerprint(l)
	}
	if b["rawDisk"] != nil {
		n := 0
		for _, k := range []string{"sourceDisk", "sourceSnapshot", "sourceImage"} {
			if str(b, k) != "" {
				n++
			}
		}
		if n > 0 {
			return nil, invalid("Invalid value for field 'resource': ''. Exactly one of sourceDisk, sourceSnapshot, sourceImage or rawDisk must be specified.")
		}
		return s.insertRawImageLocked(c, rel, o)
	}
	if err := s.checkDiskBudget(); err != nil {
		return nil, err
	}
	locs := strsAny(b["storageLocations"])
	var src DiskSpec
	switch {
	case str(b, "sourceDisk") != "":
		dr := Rel(str(b, "sourceDisk"), c.project)
		d, ok := s.res[dr]
		if !ok || !strings.Contains(dr, "/disks/") {
			return nil, notFound(dr)
		}
		if str(d, "status") != "READY" {
			return nil, errf(http.StatusBadRequest, "resourceNotReady", "The resource '%s' is not ready", dr)
		}
		// Google refuses an image of a disk a running VM writes to unless
		// forced (images/create-custom: "--force"), UNVERIFIED wording (re-read 2026-11-22).
		if b["forceCreate"] != true && first(c.query, "forceCreate") != "true" {
			for _, u := range strs(d, "users") {
				if vm, ok := s.res[u]; ok && str(vm, "status") == stRunning {
					return nil, errf(http.StatusBadRequest, "resourceInUseByAnotherResource",
						"The disk resource '%s' is already being used by '%s'", dr, u)
				}
			}
		}
		o["sourceDisk"], o["sourceDiskId"], o["diskSizeGb"] = dr, d["id"], d["sizeGb"]
		o["_sourceImage"] = s.diskImageLocked(d)
		if len(locs) == 0 {
			locs = []string{regionOfZone(lastSeg(str(d, "zone")))}
		}
		src = s.diskSpecLocked(d)
	case str(b, "sourceSnapshot") != "":
		sr := Rel(str(b, "sourceSnapshot"), c.project)
		if !strings.Contains(sr, "/") {
			sr = "projects/" + c.project + "/global/snapshots/" + sr
		}
		so, ok := s.res[sr]
		if !ok {
			return nil, notFound(sr)
		}
		o["sourceSnapshot"], o["sourceSnapshotId"], o["diskSizeGb"] = sr, so["id"], so["diskSizeGb"]
		o["_sourceImage"] = str(so, "_sourceImage")
		n, _, _ := sizeGb(so["diskSizeGb"])
		src = DiskSpec{Key: vmKey("snap", so), SizeGb: n}
	case str(b, "sourceImage") != "":
		pub, co, err := s.imageSeedLocked(str(b, "sourceImage"), c.project)
		if err != nil {
			return nil, err
		}
		o["sourceImage"], o["_sourceImage"] = Rel(str(b, "sourceImage"), c.project), imageRel(pub)
		if co != nil {
			o["diskSizeGb"] = co["diskSizeGb"]
			if k := str(co, "_storageKey"); k != "" {
				n, _, _ := sizeGb(co["diskSizeGb"])
				src = DiskSpec{Key: k, SizeGb: n}
			}
		} else {
			o["diskSizeGb"] = itoa(pub.DiskSizeGb)
		}
	default:
		return nil, invalid("Invalid value for field 'resource': ''. Exactly one of sourceDisk, sourceSnapshot, sourceImage or rawDisk must be specified.")
	}
	if len(locs) == 0 {
		locs = []string{"us"}
	}
	o["storageLocations"] = toAny(locs)
	if pub := str(o, "_sourceImage"); pub != "" {
		if img, err := LookupImage(pub); err == nil {
			o["licenses"] = []any{"projects/" + img.Project + "/global/licenses/" + img.Family}
		}
	}
	key := vmKey("img", o)
	o["_storageKey"] = key
	s.res[rel] = norm(o)
	return s.runOpLocked(c.project, "global", "insert", rel, func(ctx context.Context) error {
		if src.Key == "" {
			return nil // an image of a public image runs it, seeded on boot
		}
		// Every disk, snapshot and image is a claim of its own: the image
		// is a copy of its source's.
		return s.opts.VMs.Snapshot(ctx, src, key)
	}, func(err error) {
		if err != nil {
			delete(s.res, rel)
			return
		}
		if cur, ok := s.res[rel]; ok {
			cur["status"] = "READY"
			if src.Key == "" {
				delete(cur, "_storageKey")
			}
		}
	})
}

// diskImageLocked is the public image a disk runs, as a relative name.
func (s *Server) diskImageLocked(d obj) string {
	if si := str(d, "_seedImage"); si != "" {
		return si
	}
	if si := str(d, "sourceImage"); si != "" {
		if img, err := LookupImage(si); err == nil {
			return imageRel(img)
		}
	}
	if snap, ok := s.res[str(d, "sourceSnapshot")]; ok {
		return str(snap, "_sourceImage")
	}
	return ""
}

func itoa(n int) string { return strconv.Itoa(n) }
