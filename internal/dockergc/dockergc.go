// Package dockergc keeps the host's Docker free of what CloudBurrow
// instances leave behind (#2152).
//
// Every image CloudBurrow builds or loads into the host's Docker for an
// instance (the builtin storage and BigQuery images, Compute Engine VM
// images, Cloud Run source and Cloud Build outputs, Dataproc custom
// containers) carries the label io.cloudburrow.instance=<name>, and so does
// every volume it creates, anonymous ones included. `cloudburrow stop` and
// `cloudburrow delete` then remove that instance's images and volumes by the
// label (CleanInstance), unless a container or another running instance
// still uses them. `cloudburrow gc` lists, and with --yes removes, what
// builds older than the label left: images under CloudBurrow's known
// repository prefixes that carry no instance label (Collect). Nothing
// without one of CloudBurrow's labels or prefixes is ever touched.
package dockergc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// InstanceLabel is the label naming the instance an image, volume or
// container was made for.
const InstanceLabel = "io.cloudburrow.instance"

// legacyVolumeLabel is the label Cloud Build's host workspace volumes have
// carried since #1382; they are CloudBurrow's too.
const legacyVolumeLabel = "cloudburrow.instance"

// ownedLabel marks what CloudBurrow builds with no instance in mind
// (config.OwnerLabels), the Compute Engine VM images before #2152 among
// them.
const ownedLabel = "cloudburrow.dev/owned"

// kindClusterLabel is the label kind puts on a cluster's node containers.
const kindClusterLabel = "io.x-k8s.kind.cluster"

// Label is the label=value pair for an instance.
func Label(instance string) string { return InstanceLabel + "=" + instance }

// LabelArgs is the --label flag docker build, run and volume create take
// for an instance, or nothing for no instance.
func LabelArgs(instance string) []string {
	if instance == "" {
		return nil
	}
	return []string{"--label", Label(instance)}
}

// LabelDockerfile is a Dockerfile that only labels from for an instance:
// no layer is added, so the image's filesystem is from's.
func LabelDockerfile(from, instance string) string {
	return "FROM " + from + "\nLABEL " + InstanceLabel + "=" + quoteLabel(instance) + "\n"
}

func quoteLabel(v string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ClusterName is the kind cluster an instance runs, as config.ClusterName
// names it: "cloudburrow" for the default instance, cloudburrow-<name>
// otherwise.
func ClusterName(instance string) string {
	if instance == "cloudburrow" {
		return instance
	}
	return "cloudburrow-" + instance
}

// Runner runs docker; injected for tests.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// Image is one image in the host's Docker.
type Image struct {
	ID     string
	Tags   []string
	Labels map[string]string
	Size   int64
	// Reason is why it is removable, for gc's listing.
	Reason string
}

// Instance is the instance the image is labelled for, if any.
func (i Image) Instance() string { return i.Labels[InstanceLabel] }

// Short is the image's ID as docker images prints it.
func (i Image) Short() string {
	id := strings.TrimPrefix(i.ID, "sha256:")
	if len(id) > 12 {
		id = id[:12]
	}
	return id
}

// Kept is an image left in place, and why.
type Kept struct {
	Image  Image
	Reason string
}

// Result is what a cleanup removed and left.
type Result struct {
	Removed []Image
	Kept    []Kept
	Volumes []string
	// Errors are removals docker refused; the rest still ran.
	Errors []error
}

// lines splits output into its non-empty trimmed lines.
func lines(out string) []string {
	var l []string
	for _, s := range strings.Split(out, "\n") {
		if s = strings.TrimSpace(s); s != "" {
			l = append(l, s)
		}
	}
	return l
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// inspectImages reads ids' tags, labels and sizes.
func inspectImages(ctx context.Context, r Runner, ids []string) ([]Image, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	// .Config, not .Config.Labels: Docker 29 leaves out a Config's empty
	// Labels, and the template then fails on the missing key.
	args := append([]string{"image", "inspect", "--format", "{{json .Id}}\t{{json .RepoTags}}\t{{json .Config}}\t{{.Size}}"}, ids...)
	out, err := r.Run(ctx, "docker", args...)
	if err != nil {
		return nil, fmt.Errorf("docker image inspect: %w", err)
	}
	var imgs []Image
	for _, l := range lines(out) {
		f := strings.Split(l, "\t")
		if len(f) != 4 {
			continue
		}
		var img Image
		if json.Unmarshal([]byte(f[0]), &img.ID) != nil || img.ID == "" {
			continue
		}
		_ = json.Unmarshal([]byte(f[1]), &img.Tags)
		var cfg struct{ Labels map[string]string }
		_ = json.Unmarshal([]byte(f[2]), &cfg)
		img.Labels = cfg.Labels
		fmt.Sscan(f[3], &img.Size)
		imgs = append(imgs, img)
	}
	return imgs, nil
}

// labelledImages is every image carrying the instance label.
func labelledImages(ctx context.Context, r Runner) ([]Image, error) {
	out, err := r.Run(ctx, "docker", "image", "ls", "-q", "--no-trunc", "--filter", "label="+InstanceLabel)
	if err != nil {
		return nil, fmt.Errorf("docker image ls: %w", err)
	}
	return inspectImages(ctx, r, uniq(lines(out)))
}

// inUse is what containers on the host use: image IDs and the references
// they were created from.
type inUse map[string]bool

func containerImages(ctx context.Context, r Runner) (inUse, error) {
	out, err := r.Run(ctx, "docker", "ps", "-a", "-q", "--no-trunc")
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}
	used := inUse{}
	ids := lines(out)
	if len(ids) == 0 {
		return used, nil
	}
	out, err = r.Run(ctx, "docker", append([]string{"container", "inspect", "--format", "{{.Image}}\t{{.Config.Image}}"}, ids...)...)
	if err != nil {
		// A container removed between the two calls fails the inspect;
		// that is no reason to remove what the others use, so give up.
		return nil, fmt.Errorf("docker container inspect: %w", err)
	}
	for _, l := range lines(out) {
		for _, f := range strings.Split(l, "\t") {
			if f = strings.TrimSpace(f); f != "" {
				used[f] = true
			}
		}
	}
	return used, nil
}

func (u inUse) uses(img Image) bool {
	if u[img.ID] {
		return true
	}
	for _, t := range img.Tags {
		if u[t] {
			return true
		}
	}
	return false
}

// RunningClusters is the CloudBurrow kind clusters with a running node, by
// name, each with its running node containers.
func RunningClusters(ctx context.Context, r Runner) (map[string][]string, error) {
	out, err := r.Run(ctx, "docker", "ps", "--filter", "label="+kindClusterLabel,
		"--format", "{{.Label \""+kindClusterLabel+"\"}}\t{{.Names}}")
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}
	clusters := map[string][]string{}
	for _, l := range lines(out) {
		c, node, ok := strings.Cut(l, "\t")
		if !ok || !(c == "cloudburrow" || strings.HasPrefix(c, "cloudburrow-")) {
			continue
		}
		clusters[c] = append(clusters[c], node)
	}
	return clusters, nil
}

// existingClusters is every CloudBurrow kind cluster with a node container,
// running or not.
func existingClusters(ctx context.Context, r Runner) (map[string]bool, error) {
	out, err := r.Run(ctx, "docker", "ps", "-a", "--filter", "label="+kindClusterLabel,
		"--format", "{{.Label \""+kindClusterLabel+"\"}}")
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}
	c := map[string]bool{}
	for _, l := range lines(out) {
		c[l] = true
	}
	return c, nil
}

// NodeImages is what a cluster's nodes' container runtime holds: image IDs,
// tags and repo digests. A node that cannot be read contributes nothing.
func NodeImages(ctx context.Context, r Runner, nodes []string) map[string]bool {
	have := map[string]bool{}
	for _, n := range nodes {
		out, err := r.Run(ctx, "docker", "exec", n, "crictl", "images", "-o", "json")
		if err != nil {
			continue
		}
		var doc struct {
			Images []struct {
				ID          string   `json:"id"`
				RepoTags    []string `json:"repoTags"`
				RepoDigests []string `json:"repoDigests"`
			} `json:"images"`
		}
		if json.Unmarshal([]byte(out), &doc) != nil {
			continue
		}
		for _, i := range doc.Images {
			have[i.ID] = true
			for _, t := range i.RepoTags {
				have[t] = true
			}
			for _, d := range i.RepoDigests {
				if _, dg, ok := strings.Cut(d, "@"); ok {
					have[dg] = true
				}
			}
		}
	}
	return have
}

// held reports whether a node image set has img.
func held(have map[string]bool, img Image) bool {
	if have[img.ID] {
		return true
	}
	for _, t := range img.Tags {
		if have[t] || have["docker.io/library/"+t] {
			return true
		}
	}
	return false
}

// otherInstanceHolder is the running CloudBurrow cluster, other than
// instance's own and its Kubernetes Engine clusters, whose nodes hold img,
// or "".
func otherInstanceHolder(img Image, instance string, nodeImages map[string]map[string]bool) string {
	own := ClusterName(instance)
	names := make([]string, 0, len(nodeImages))
	for c := range nodeImages {
		names = append(names, c)
	}
	sort.Strings(names)
	for _, c := range names {
		if c == own || strings.HasPrefix(c, own+"-gke-") {
			continue
		}
		if held(nodeImages[c], img) {
			return c
		}
	}
	return ""
}

// instanceRunning reports whether instance's cluster has a running node.
func instanceRunning(instance string, running map[string][]string) bool {
	_, ok := running[ClusterName(instance)]
	return ok
}

// CleanInstance removes the images and volumes labelled for instance, once
// its cluster is stopped or gone: every image labelled for it, and every
// image labelled for an instance that no longer runs that this instance's
// nodes held (used, read before they stopped; a shared, content-addressed
// image another instance built, which that instance left because this one
// still used it). An image any container uses, or another running
// instance's nodes hold, stays. Volumes labelled for the instance go
// unless a container still mounts them, which docker refuses.
func CleanInstance(ctx context.Context, r Runner, instance string, used map[string]bool) (Result, error) {
	var res Result
	if instance == "" {
		return res, errors.New("no instance name")
	}
	imgs, err := labelledImages(ctx, r)
	if err != nil {
		return res, err
	}
	running, err := RunningClusters(ctx, r)
	if err != nil {
		return res, err
	}
	var candidates []Image
	for _, img := range imgs {
		switch owner := img.Instance(); {
		case owner == instance:
			candidates = append(candidates, img)
		case held(used, img) && !instanceRunning(owner, running):
			candidates = append(candidates, img)
		}
	}
	if len(candidates) > 0 {
		containers, err := containerImages(ctx, r)
		if err != nil {
			return res, err
		}
		nodeImages := map[string]map[string]bool{}
		for c, nodes := range running {
			if c != ClusterName(instance) && !strings.HasPrefix(c, ClusterName(instance)+"-gke-") {
				nodeImages[c] = NodeImages(ctx, r, nodes)
			}
		}
		for _, img := range candidates {
			if containers.uses(img) {
				res.Kept = append(res.Kept, Kept{img, "a container uses it"})
				continue
			}
			if c := otherInstanceHolder(img, instance, nodeImages); c != "" {
				res.Kept = append(res.Kept, Kept{img, "cluster " + c + " uses it"})
				continue
			}
			if err := RemoveImage(ctx, r, img); err != nil {
				res.Errors = append(res.Errors, err)
				continue
			}
			res.Removed = append(res.Removed, img)
		}
	}
	vols, errs := removeVolumes(ctx, r, instance)
	res.Volumes, res.Errors = vols, append(res.Errors, errs...)
	return res, nil
}

// removeVolumes removes the volumes labelled for instance, by either of
// CloudBurrow's instance labels.
func removeVolumes(ctx context.Context, r Runner, instance string) ([]string, []error) {
	var names []string
	for _, key := range []string{InstanceLabel, legacyVolumeLabel} {
		out, err := r.Run(ctx, "docker", "volume", "ls", "-q", "--filter", "label="+key+"="+instance)
		if err != nil {
			return nil, []error{fmt.Errorf("docker volume ls: %w", err)}
		}
		names = append(names, lines(out)...)
	}
	var removed []string
	var errs []error
	for _, v := range uniq(names) {
		if out, err := r.Run(ctx, "docker", "volume", "rm", v); err != nil {
			errs = append(errs, fmt.Errorf("docker volume rm %s: %w: %s", v, err, strings.TrimSpace(out)))
			continue
		}
		removed = append(removed, v)
	}
	return removed, errs
}

// RemoveImage removes img: each of its tags, then its ID if that is still
// there. Never forced, so docker refuses an image a container uses.
func RemoveImage(ctx context.Context, r Runner, img Image) error {
	for _, t := range img.Tags {
		if out, err := r.Run(ctx, "docker", "image", "rm", t); err != nil {
			return fmt.Errorf("docker image rm %s: %w: %s", t, err, strings.TrimSpace(out))
		}
	}
	if _, err := r.Run(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", img.ID); err != nil {
		return nil // its last tag took it
	}
	if out, err := r.Run(ctx, "docker", "image", "rm", img.ID); err != nil {
		return fmt.Errorf("docker image rm %s: %w: %s", img.Short(), err, strings.TrimSpace(out))
	}
	return nil
}

// KnownRepository reports whether ref is under one of the repositories
// CloudBurrow's own builds are tagged in: dev.local/cloudburrow-* (the
// storage and BigQuery images, test probes), cloudburrow.local/* (Dataproc
// custom containers), cloudburrow-vm-* (Compute Engine VM images), and
// dev.local/<Google registry>/... (Cloud Run source builds and Cloud Build
// outputs, under Artifact Registry's {location}-docker.pkg.dev or Container
// Registry's [region.]gcr.io). A user's own dev.local/<name> is not one.
func KnownRepository(ref string) bool {
	switch {
	case strings.HasPrefix(ref, "dev.local/cloudburrow-"),
		strings.HasPrefix(ref, "cloudburrow.local/"),
		strings.HasPrefix(ref, "cloudburrow-vm-"):
		return true
	}
	rest, ok := strings.CutPrefix(ref, "dev.local/")
	if !ok {
		return false
	}
	host, path, ok := strings.Cut(rest, "/")
	if !ok || path == "" {
		return false
	}
	return strings.HasSuffix(host, "-docker.pkg.dev") || strings.HasSuffix(host, ".pkg.dev") ||
		host == "gcr.io" || strings.HasSuffix(host, ".gcr.io")
}

// Collect lists what `cloudburrow gc` removes:
//   - images without the instance label whose every tag is under a
//     KnownRepository, or that carry CloudBurrow's cloudburrow.dev/owned
//     label: leftovers of builds older than the label;
//   - images labelled for an instance whose cluster no longer exists, which
//     its stop or delete never removed (killed, or a CLI before #2152).
//
// It skips any image a container (running or not) uses, and any a running
// CloudBurrow cluster's nodes hold. An image with no tag and no CloudBurrow
// label is never listed: nothing shows it is CloudBurrow's.
func Collect(ctx context.Context, r Runner) ([]Image, error) {
	out, err := r.Run(ctx, "docker", "image", "ls", "-q", "--no-trunc")
	if err != nil {
		return nil, fmt.Errorf("docker image ls: %w", err)
	}
	all, err := inspectImages(ctx, r, uniq(lines(out)))
	if err != nil {
		return nil, err
	}
	clusters, err := existingClusters(ctx, r)
	if err != nil {
		return nil, err
	}
	var candidates []Image
	for _, img := range all {
		switch owner := img.Instance(); {
		case owner != "":
			if !clusters[ClusterName(owner)] {
				img.Reason = "instance " + owner + " no longer exists"
				candidates = append(candidates, img)
			}
		case img.Labels[ownedLabel] == "true":
			img.Reason = "built by CloudBurrow (" + ownedLabel + ")"
			candidates = append(candidates, img)
		case len(img.Tags) > 0 && allKnown(img.Tags):
			img.Reason = "unlabelled CloudBurrow build"
			candidates = append(candidates, img)
		}
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	containers, err := containerImages(ctx, r)
	if err != nil {
		return nil, err
	}
	running, err := RunningClusters(ctx, r)
	if err != nil {
		return nil, err
	}
	nodeImages := map[string]map[string]bool{}
	for c, nodes := range running {
		nodeImages[c] = NodeImages(ctx, r, nodes)
	}
	var keep []Image
	for _, img := range candidates {
		if containers.uses(img) || otherInstanceHolder(img, "", nodeImages) != "" {
			continue
		}
		keep = append(keep, img)
	}
	sort.Slice(keep, func(i, j int) bool {
		a, b := keep[i], keep[j]
		if len(a.Tags) > 0 && len(b.Tags) > 0 && a.Tags[0] != b.Tags[0] {
			return a.Tags[0] < b.Tags[0]
		}
		return a.ID < b.ID
	})
	return keep, nil
}

func allKnown(tags []string) bool {
	for _, t := range tags {
		if !KnownRepository(t) {
			return false
		}
	}
	return true
}

// OrphanVolumes is the volumes labelled for an instance whose cluster no
// longer exists, which gc removes too.
func OrphanVolumes(ctx context.Context, r Runner) ([]string, error) {
	clusters, err := existingClusters(ctx, r)
	if err != nil {
		return nil, err
	}
	var orphans []string
	for _, key := range []string{InstanceLabel, legacyVolumeLabel} {
		out, err := r.Run(ctx, "docker", "volume", "ls", "--filter", "label="+key,
			"--format", "{{.Name}}\t{{.Label \""+key+"\"}}")
		if err != nil {
			return nil, fmt.Errorf("docker volume ls: %w", err)
		}
		for _, l := range lines(out) {
			name, owner, _ := strings.Cut(l, "\t")
			if owner != "" && !clusters[ClusterName(owner)] {
				orphans = append(orphans, name)
			}
		}
	}
	return uniq(orphans), nil
}
