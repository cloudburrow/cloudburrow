package main

// Cloud Run: the service account, Secret Manager files and in-memory
// volumes on the deploy and job forms, and their edit forms (#1297).
//
// The adapter mounts a Secret Manager volume from the Kubernetes Secret
// holding the secret's versions and an in-memory emptyDir as a tmpfs, and
// stores and returns the service account (docs/compatibility.md:
// TestRunServiceVolumesAndServiceAccount, TestRunJobMountsSecretAndMemoryVolumes).

import (
	"fmt"
	"path"
	"strings"

	runpb "cloud.google.com/go/run/apiv2/runpb"

	"github.com/cloudburrow/cloudburrow/internal/console"
)

// runVolumeFields are the service account and volume fields.
func runVolumeFields(section string) []console.Field {
	return []console.Field{
		{Name: "serviceAccount", Label: "Service account", Type: "text", Section: section,
			Help: "Optional. The email of the identity the workload runs as. It is stored and returned; locally " +
				"every workload's metadata server names the instance's own account."},
		{Name: "secretFiles", Label: "Files from Secret Manager", Type: "map", Section: section,
			Help: "Optional. One /dir/file=secret per line, or /dir/file=secret:version; the version is latest " +
				"when not given. The file holds the version's payload, read-only. Files in one directory come from one secret."},
		{Name: "memoryVolumes", Label: "In-memory volumes", Type: "map", Section: section,
			Help: `Optional. One /mount/path=size per line, as Kubernetes quantities ("64Mi"), or /mount/path= for no limit. ` +
				"The volume is a writable tmpfs that counts toward the container's memory."},
	}
}

// podFields is what a service's RevisionTemplate and a job's TaskTemplate
// share for the form.
type podFields struct {
	serviceAccount *string
	volumes        *[]*runpb.Volume
}

// applyRunVolumes sets the form's service account, volumes and mounts on a
// template and its one container. A field the request did not send leaves
// that part as it was, so an older form edits nothing it did not show.
func applyRunVolumes(p podFields, c *runpb.Container, values map[string]string) error {
	if v, ok := values["serviceAccount"]; ok {
		*p.serviceAccount = strings.TrimSpace(v)
	}
	rawSecret, sentSecret := values["secretFiles"]
	rawMemory, sentMemory := values["memoryVolumes"]
	if !sentSecret && !sentMemory {
		return nil
	}
	var keep []*runpb.Volume
	kept := map[string]bool{}
	for _, v := range *p.volumes {
		if (v.GetSecret() != nil && sentSecret) || (v.GetEmptyDir() != nil && sentMemory) {
			continue
		}
		keep = append(keep, v)
		kept[v.GetName()] = true
	}
	var mounts []*runpb.VolumeMount
	for _, m := range c.GetVolumeMounts() {
		if kept[m.GetName()] {
			mounts = append(mounts, m)
		}
	}
	if sentSecret {
		m, err := console.ParseMap(rawSecret)
		if err != nil {
			return fmt.Errorf("files from Secret Manager: %w", err)
		}
		byDir := map[string]*runpb.SecretVolumeSource{}
		var dirs []string
		for _, file := range sortedKeys(m) {
			ref := strings.TrimSpace(m[file])
			secret, version := ref, "latest"
			if i := strings.LastIndex(ref, ":"); i >= 0 {
				secret, version = ref[:i], ref[i+1:]
			}
			if !path.IsAbs(file) || path.Base(file) == "/" || secret == "" || version == "" {
				return fmt.Errorf("files from Secret Manager: %s=%q is not /dir/file=secret[:version]", file, ref)
			}
			dir := path.Dir(path.Clean(file))
			s := byDir[dir]
			if s == nil {
				s = &runpb.SecretVolumeSource{Secret: secret}
				byDir[dir] = s
				dirs = append(dirs, dir)
			} else if s.Secret != secret {
				return fmt.Errorf("files from Secret Manager: %s holds files from %s and %s; use one secret per directory", dir, s.Secret, secret)
			}
			s.Items = append(s.Items, &runpb.VersionToPath{Path: path.Base(file), Version: version})
		}
		for i, dir := range dirs {
			name := fmt.Sprintf("secret-%d", i+1)
			keep = append(keep, &runpb.Volume{Name: name, VolumeType: &runpb.Volume_Secret{Secret: byDir[dir]}})
			mounts = append(mounts, &runpb.VolumeMount{Name: name, MountPath: dir})
		}
	}
	if sentMemory {
		m, err := console.ParseMap(rawMemory)
		if err != nil {
			return fmt.Errorf("in-memory volumes: %w", err)
		}
		for i, dir := range sortedKeys(m) {
			if !path.IsAbs(dir) || path.Clean(dir) == "/" {
				return fmt.Errorf("in-memory volumes: %q is not an absolute path other than /", dir)
			}
			name := fmt.Sprintf("memory-%d", i+1)
			keep = append(keep, &runpb.Volume{Name: name, VolumeType: &runpb.Volume_EmptyDir{EmptyDir: &runpb.EmptyDirVolumeSource{
				Medium: runpb.EmptyDirVolumeSource_MEMORY, SizeLimit: strings.TrimSpace(m[dir])}}})
			mounts = append(mounts, &runpb.VolumeMount{Name: name, MountPath: path.Clean(dir)})
		}
	}
	for _, mt := range mounts {
		for _, other := range mounts {
			if mt != other && mt.GetMountPath() == other.GetMountPath() {
				return fmt.Errorf("%s is mounted twice", mt.GetMountPath())
			}
		}
	}
	*p.volumes = keep
	c.VolumeMounts = mounts
	return nil
}

// runVolumeDefaults are the volume fields' prefill from a template's
// volumes and its one container's mounts.
func runVolumeDefaults(volumes []*runpb.Volume, mounts []*runpb.VolumeMount) (secretFiles, memoryVolumes string) {
	byName := map[string]*runpb.Volume{}
	for _, v := range volumes {
		byName[v.GetName()] = v
	}
	secret, memory := map[string]string{}, map[string]string{}
	for _, m := range mounts {
		v := byName[m.GetName()]
		switch {
		case v.GetSecret() != nil:
			s := v.GetSecret()
			items := s.GetItems()
			if len(items) == 0 {
				id := s.GetSecret()[strings.LastIndex(s.GetSecret(), "/")+1:]
				items = []*runpb.VersionToPath{{Path: id, Version: "latest"}}
			}
			for _, it := range items {
				secret[path.Join(m.GetMountPath(), it.GetPath())] = formatRunSecretRef(s.GetSecret(), it.GetVersion())
			}
		case v.GetEmptyDir() != nil:
			memory[m.GetMountPath()] = v.GetEmptyDir().GetSizeLimit()
		}
	}
	return console.FormatMap(secret), console.FormatMap(memory)
}
