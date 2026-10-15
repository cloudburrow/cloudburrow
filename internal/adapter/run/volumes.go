package run

import (
	"fmt"
	"path"
	"strings"

	runpb "cloud.google.com/go/run/apiv2/runpb"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Volumes, volume mounts and the service account (#1297).
//
// Two volume types have a faithful Kubernetes counterpart and are rendered:
// a Secret Manager secret, mounted from the Kubernetes Secret that holds its
// versions (the one secretKeyRef env vars resolve to), and an in-memory
// emptyDir. Cloud SQL, Cloud Storage FUSE and NFS volumes stay refused: no
// Cloud SQL socket, FUSE driver or NFS server exists on the kind node.
//
// The service account is stored and returned, not acted on: every workload
// reaches the one metadata server, which names the instance's own account.

// annPodConfig records the Cloud Run volumes, each container's mounts and
// the service account, as a RevisionTemplate's JSON, so they read back as
// set rather than as the Kubernetes objects they rendered to.
const annPodConfig = "cloudburrow.dev/pod-config"

// volumeGaps names the volumes and mounts no pod renders.
func volumeGaps(prefix string, tmpl podTemplate) []string {
	var gaps []string
	names := map[string]bool{}
	for _, v := range tmpl.GetVolumes() {
		names[v.GetName()] = true
		switch {
		case v.GetSecret() != nil, v.GetEmptyDir() != nil:
		case v.GetCloudSqlInstance() != nil:
			gaps = append(gaps, prefix+".volumes.cloudSqlInstance: no Cloud SQL socket is served locally")
		case v.GetGcs() != nil:
			gaps = append(gaps, prefix+".volumes.gcs: Cloud Storage FUSE is not mounted locally")
		case v.GetNfs() != nil:
			gaps = append(gaps, prefix+".volumes.nfs: no NFS server is reachable locally")
		}
	}
	for _, c := range tmpl.GetContainers() {
		for _, m := range c.GetVolumeMounts() {
			if m.GetSubPath() != "" {
				gaps = append(gaps, "container.volumeMounts.subPath: not mapped")
			}
		}
	}
	return gaps
}

// checkVolumes validates volumes and mounts before anything renders, so a
// mistake is the caller's InvalidArgument rather than an apply failure.
func checkVolumes(tmpl podTemplate) error {
	declared := map[string]*runpb.Volume{}
	for _, v := range tmpl.GetVolumes() {
		if !serviceIDRE.MatchString(v.GetName()) || len(v.GetName()) > 63 {
			return apierror.InvalidArgument("volume name %q must be lowercase letters, digits and hyphens", v.GetName())
		}
		if declared[v.GetName()] != nil {
			return apierror.InvalidArgument("volume %q is declared twice", v.GetName())
		}
		declared[v.GetName()] = v
		if s := v.GetSecret(); s != nil && s.GetSecret() == "" {
			return apierror.InvalidArgument("volume %q: secret.secret is required", v.GetName())
		}
		if v.GetVolumeType() == nil {
			return apierror.InvalidArgument("volume %q sets no volume type", v.GetName())
		}
	}
	for _, c := range tmpl.GetContainers() {
		for _, m := range c.GetVolumeMounts() {
			if declared[m.GetName()] == nil {
				return apierror.InvalidArgument("volumeMount %q names no declared volume", m.GetName())
			}
			if !path.IsAbs(m.GetMountPath()) || path.Clean(m.GetMountPath()) == "/" {
				return apierror.InvalidArgument("volumeMount %q: mountPath %q must be an absolute path other than /",
					m.GetName(), m.GetMountPath())
			}
		}
	}
	return nil
}

// renderVolumes writes a pod spec's volumes list at the pod-spec indent.
func renderVolumes(b *strings.Builder, volumes []*runpb.Volume, o containerRender) error {
	if len(volumes) == 0 {
		return nil
	}
	b.WriteString("      volumes:\n")
	for _, v := range volumes {
		fmt.Fprintf(b, "        - name: %s\n", v.GetName())
		switch {
		case v.GetSecret() != nil:
			s := v.GetSecret()
			if o.secrets == nil {
				return apierror.FailedPrecondition(
					"volume %q mounts secret %q, but Secret Manager is not enabled on this instance", v.GetName(), s.GetSecret())
			}
			items := s.GetItems()
			if len(items) == 0 {
				// Cloud Run's default: the latest version, in a file named
				// after the secret.
				id := s.GetSecret()
				if i := strings.LastIndex(id, "/"); i >= 0 {
					id = id[i+1:]
				}
				items = []*runpb.VersionToPath{{Path: id, Version: "latest"}}
			}
			var secretName string
			var lines strings.Builder
			for _, it := range items {
				name, key, err := o.secrets.ResolveSecretRef(o.project, s.GetSecret(), it.GetVersion())
				if err != nil {
					return err
				}
				secretName = name
				fmt.Fprintf(&lines, "              - key: %s\n                path: %q\n", key, it.GetPath())
				if it.GetMode() != 0 {
					fmt.Fprintf(&lines, "                mode: %d\n", it.GetMode())
				}
			}
			fmt.Fprintf(b, "          secret:\n            secretName: %s\n", secretName)
			if m := s.GetDefaultMode(); m != 0 {
				fmt.Fprintf(b, "            defaultMode: %d\n", m)
			}
			b.WriteString("            items:\n")
			b.WriteString(lines.String())
		case v.GetEmptyDir() != nil:
			e := v.GetEmptyDir()
			if e.GetMedium() != runpb.EmptyDirVolumeSource_MEMORY && e.GetSizeLimit() == "" {
				b.WriteString("          emptyDir: {}\n")
				continue
			}
			b.WriteString("          emptyDir:\n")
			if e.GetMedium() == runpb.EmptyDirVolumeSource_MEMORY {
				b.WriteString("            medium: Memory\n")
			}
			if e.GetSizeLimit() != "" {
				fmt.Fprintf(b, "            sizeLimit: %q\n", e.GetSizeLimit())
			}
		}
	}
	return nil
}

// renderVolumeMounts writes one container's volumeMounts. Knative requires
// a secret mount to be read-only, as Cloud Run's is.
func renderVolumeMounts(b *strings.Builder, mounts []*runpb.VolumeMount, volumes []*runpb.Volume) {
	if len(mounts) == 0 {
		return
	}
	secret := map[string]bool{}
	for _, v := range volumes {
		secret[v.GetName()] = v.GetSecret() != nil
	}
	b.WriteString("          volumeMounts:\n")
	for _, m := range mounts {
		fmt.Fprintf(b, "            - name: %s\n              mountPath: %q\n", m.GetName(), m.GetMountPath())
		if secret[m.GetName()] {
			b.WriteString("              readOnly: true\n")
		}
	}
}

// podConfigAnnotation renders annPodConfig for a template, or "" when it
// sets none of the fields.
func podConfigAnnotation(tmpl podTemplate) string {
	rec := &runpb.RevisionTemplate{ServiceAccount: tmpl.GetServiceAccount(), Volumes: tmpl.GetVolumes()}
	set := rec.ServiceAccount != "" || len(rec.Volumes) > 0
	for _, c := range tmpl.GetContainers() {
		rec.Containers = append(rec.Containers, &runpb.Container{VolumeMounts: c.GetVolumeMounts()})
		set = set || len(c.GetVolumeMounts()) > 0
	}
	if !set {
		return ""
	}
	raw, _ := protojson.Marshal(rec)
	return string(raw)
}

// readPodConfig merges annPodConfig into a read-back template.
func readPodConfig(ann map[string]string, into *runpb.RevisionTemplate) {
	raw := ann[annPodConfig]
	if raw == "" {
		return
	}
	var rec runpb.RevisionTemplate
	if protojson.Unmarshal([]byte(raw), &rec) != nil {
		return
	}
	into.ServiceAccount = rec.GetServiceAccount()
	into.Volumes = rec.GetVolumes()
	for i, c := range rec.GetContainers() {
		if i < len(into.Containers) {
			into.Containers[i].VolumeMounts = c.GetVolumeMounts()
		}
	}
}
