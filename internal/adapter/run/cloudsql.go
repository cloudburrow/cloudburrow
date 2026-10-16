package run

import (
	"fmt"
	"strings"

	runpb "cloud.google.com/go/run/apiv2/runpb"

	"github.com/cloudburrow/cloudburrow/internal/apierror"
)

// Cloud SQL volumes (#1346), the way Cloud Run provides them: for each
// connection name a volume lists, a Unix socket at <mountPath>/<connection
// name> (PostgreSQL's at <mountPath>/<connection name>/.s.PGSQL.5432), served
// by the Cloud SQL Auth Proxy.
//
// Google runs the Auth Proxy beside the container; so does this adapter. A
// Cloud SQL volume renders as an emptyDir, and a sidecar running Google's
// own proxy image mounts it at /cloudsql with --unix-socket /cloudsql and the
// connection names. The proxy does what it does on Cloud Run: it asks the
// Cloud SQL Admin API (--sqladmin-api-endpoint, the instance's sqladmin
// service as a pod reaches it) for connectSettings and an ephemeral
// certificate, with a token from the metadata server, and dials the
// connector on port 3307 over TLS 1.3. Nothing in the proxy is replaced.
//
// In a service the proxy is a second container, as a Cloud Run sidecar is;
// in a job's task it is a Kubernetes native sidecar (an init container that
// keeps running), so the task completes when the job's own container exits.

// CloudSQLProxyImage is the Cloud SQL Auth Proxy v2.19.0, Google's image,
// pinned by the index digest (linux/amd64 and linux/arm64).
const CloudSQLProxyImage = "gcr.io/cloud-sql-connectors/cloud-sql-proxy:2.19.0@sha256:5943ac94c0309ddf879ab11384b85781f1381524312911e38789daf4b61faa8a"

// cloudSQLSocketDir is where the proxy writes its sockets in its own
// container: the volume's mount there.
const cloudSQLSocketDir = "/cloudsql"

// CloudSQLConfig is what a Cloud SQL volume needs from the instance.
type CloudSQLConfig struct {
	// Endpoint is the Cloud SQL Admin API's URL as a pod reaches it.
	Endpoint string
	// Region is the one region the instances are in.
	Region string
	// Instances are the instance IDs served, in any project.
	Instances []string
}

// CloudSQL returns the instance's Cloud SQL configuration, or nil when the
// Admin API is not published to pods. It is read per request, as the
// environment is: the Admin API is published after the adapter starts.
type CloudSQL func() *CloudSQLConfig

// WithCloudSQL enables Cloud SQL volumes.
func (s *Server) WithCloudSQL(f CloudSQL) *Server {
	s.cloudSQL = f
	return s
}

func (s *Server) cloudSQLConfig() *CloudSQLConfig {
	if s.cloudSQL == nil {
		return nil
	}
	return s.cloudSQL()
}

// checkCloudSQLVolume validates one Cloud SQL volume's connection names.
func checkCloudSQLVolume(field string, v *runpb.Volume, sql *CloudSQLConfig) error {
	names := v.GetCloudSqlInstance().GetInstances()
	if len(names) == 0 {
		return apierror.InvalidArgument("%s.cloudSqlInstance.instances: at least one Cloud SQL connection name is required", field)
	}
	if sql == nil || sql.Endpoint == "" {
		return apierror.FailedPrecondition("volume %q mounts Cloud SQL, but neither Cloud SQL nor the Cloud SQL Admin API "+
			"is enabled on this instance; start it with --services including cloudsql (or cloudsql-mysql) and run", v.GetName())
	}
	seen := map[string]bool{}
	for i, n := range names {
		f := fmt.Sprintf("%s.cloudSqlInstance.instances[%d]", field, i)
		parts := strings.Split(n, ":")
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
			return apierror.InvalidArgument("%s: Cloud SQL connection name %q is invalid; "+
				"it must be in the form project:region:instance", f, n)
		}
		known := false
		for _, id := range sql.Instances {
			known = known || id == parts[2]
		}
		if parts[1] != sql.Region || !known {
			return apierror.InvalidArgument("%s: Cloud SQL instance %q does not exist or you do not have "+
				"permission to access it. CloudBurrow's instances are %s in region %s.",
				f, n, strings.Join(sql.Instances, ", "), sql.Region)
		}
		if seen[n] {
			return apierror.InvalidArgument("%s: Cloud SQL connection name %q is listed twice", f, n)
		}
		seen[n] = true
	}
	return nil
}

// cloudSQLVolumes are a template's Cloud SQL volumes.
func cloudSQLVolumes(volumes []*runpb.Volume) []*runpb.Volume {
	var out []*runpb.Volume
	for _, v := range volumes {
		if v.GetCloudSqlInstance() != nil {
			out = append(out, v)
		}
	}
	return out
}

// proxyContainerName is the sidecar's container name for a volume, within
// Kubernetes' 63 characters.
func proxyContainerName(volume string) string {
	n := "cloud-sql-proxy-" + volume
	if len(n) > 63 {
		n = strings.TrimRight(n[:63], "-")
	}
	return n
}

// isCloudSQLProxy reports whether a rendered container is an Auth Proxy
// sidecar renderCloudSQLProxies added for a Cloud SQL volume: the pinned
// image under its sidecar name. The volume reads back as the volume, so the
// sidecar is never one of the caller's containers; were it read back, a
// read-modify-write (gcloud run services update) would send it as a plain
// container without the socket directory, and it would crash.
func isCloudSQLProxy(name, image string) bool {
	return image == CloudSQLProxyImage && strings.HasPrefix(name, "cloud-sql-proxy-")
}

// renderCloudSQLProxies writes one Auth Proxy container per Cloud SQL
// volume, as list items at the containers indent. native makes each a
// Kubernetes native sidecar, for a batch Job's initContainers.
func renderCloudSQLProxies(b *strings.Builder, volumes []*runpb.Volume, o containerRender, native bool) {
	for _, v := range cloudSQLVolumes(volumes) {
		fmt.Fprintf(b, "        - name: %s\n          image: %s\n          imagePullPolicy: IfNotPresent\n",
			proxyContainerName(v.GetName()), CloudSQLProxyImage)
		if native {
			b.WriteString("          restartPolicy: Always\n")
		}
		args := []string{"--unix-socket", cloudSQLSocketDir, "--sqladmin-api-endpoint", o.cloudSQL.Endpoint,
			"--exit-zero-on-sigterm"}
		args = append(args, v.GetCloudSqlInstance().GetInstances()...)
		fmt.Fprintf(b, "          args: [%s]\n", quote(args))
		// The proxy authenticates as a Cloud Run container does: through
		// the metadata server, which the injected GCE_METADATA_HOST names.
		var env []injectedEnv
		for _, e := range o.injected {
			if e.name == "GCE_METADATA_HOST" || e.name == "GOOGLE_CLOUD_PROJECT" {
				env = append(env, e)
			}
		}
		if len(env) > 0 {
			b.WriteString("          env:\n")
			for _, e := range env {
				fmt.Fprintf(b, "            - name: %s\n              value: %q\n", e.name, e.value)
			}
		}
		fmt.Fprintf(b, "          volumeMounts:\n            - name: %s\n              mountPath: %q\n",
			v.GetName(), cloudSQLSocketDir)
	}
}

// Cloud Run v1 names a revision's Cloud SQL instances in one template
// annotation, which `gcloud run deploy --add-cloudsql-instances`,
// --set-cloudsql-instances and --remove-cloudsql-instances write: a
// comma-separated list of connection names, each served, as Google
// documents, as a socket at /cloudsql/<connection name> in every container.
// v2 reads that as a Cloud SQL volume named cloudsql that every container
// mounts at /cloudsql, and v1 reads a v2 template's Cloud SQL volumes back
// as the annotation (v1 has no Cloud SQL volume of its own).
const (
	v1CloudSQLInstances = "run.googleapis.com/cloudsql-instances"
	v1CloudSQLVolume    = "cloudsql"
)

// fromV1CloudSQL adds the annotation's connection names to a template as
// the Cloud SQL volume, mounted at /cloudsql in each container.
func fromV1CloudSQL(field, value string, volumes []*runpb.Volume, containers []*runpb.Container) ([]*runpb.Volume, error) {
	var conns []string
	for _, c := range strings.Split(value, ",") {
		if c = strings.TrimSpace(c); c != "" {
			conns = append(conns, c)
		}
	}
	if len(conns) == 0 {
		return volumes, nil
	}
	for _, v := range volumes {
		if v.GetName() == v1CloudSQLVolume {
			return nil, apierror.InvalidArgument("%s: a volume named %q already exists; "+
				"Cloud SQL instances are mounted as that volume", field, v1CloudSQLVolume)
		}
	}
	for _, c := range containers {
		for _, m := range c.GetVolumeMounts() {
			if m.GetMountPath() == cloudSQLSocketDir {
				return nil, apierror.InvalidArgument("%s: container %q already mounts %s; "+
					"Cloud SQL instances are mounted there", field, c.GetName(), cloudSQLSocketDir)
			}
		}
		c.VolumeMounts = append(c.VolumeMounts, &runpb.VolumeMount{Name: v1CloudSQLVolume, MountPath: cloudSQLSocketDir})
	}
	return append(volumes, newCloudSQLVolume(v1CloudSQLVolume, conns)), nil
}

func newCloudSQLVolume(name string, conns []string) *runpb.Volume {
	return &runpb.Volume{Name: name, VolumeType: &runpb.Volume_CloudSqlInstance{
		CloudSqlInstance: &runpb.CloudSqlInstance{Instances: conns}}}
}

// cloudSQLConns are the connection names a template's Cloud SQL volumes
// list, each once, in order.
func cloudSQLConns(volumes []*runpb.Volume) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range cloudSQLVolumes(volumes) {
		for _, c := range v.GetCloudSqlInstance().GetInstances() {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	return out
}

// toV1CloudSQL sets the annotation from a template's Cloud SQL volumes.
func toV1CloudSQL(ann map[string]string, volumes []*runpb.Volume) map[string]string {
	conns := cloudSQLConns(volumes)
	if len(conns) == 0 {
		return ann
	}
	if ann == nil {
		ann = map[string]string{}
	}
	ann[v1CloudSQLInstances] = strings.Join(conns, ",")
	return ann
}

// keepCloudSQLLayout keeps a template's Cloud SQL volumes and mounts as
// they were when a v1 replace names the same connections: v1 cannot say
// which volume or path a v2 template mounted them at, so a gcloud update
// that leaves the instances alone leaves their layout alone too.
func keepCloudSQLLayout(next, prior *runpb.RevisionTemplate) {
	was := cloudSQLConns(prior.GetVolumes())
	now := cloudSQLConns(next.GetVolumes())
	if len(was) == 0 || len(was) != len(now) {
		return
	}
	set := map[string]bool{}
	for _, c := range was {
		set[c] = true
	}
	for _, c := range now {
		if !set[c] {
			return
		}
	}
	var vols []*runpb.Volume
	for _, v := range next.GetVolumes() {
		if v.GetCloudSqlInstance() == nil {
			vols = append(vols, v)
		}
	}
	names := map[string]bool{}
	for _, v := range cloudSQLVolumes(prior.GetVolumes()) {
		names[v.GetName()] = true
		vols = append(vols, v)
	}
	next.Volumes = vols
	for i, c := range next.GetContainers() {
		var mounts []*runpb.VolumeMount
		for _, m := range c.GetVolumeMounts() {
			if m.GetName() != v1CloudSQLVolume {
				mounts = append(mounts, m)
			}
		}
		if i < len(prior.GetContainers()) {
			for _, m := range prior.GetContainers()[i].GetVolumeMounts() {
				if names[m.GetName()] {
					mounts = append(mounts, m)
				}
			}
		}
		c.VolumeMounts = mounts
	}
}
