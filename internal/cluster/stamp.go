package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The stamp records which versions a cluster was built and last brought up
// with (#601).
//
// `up` against an existing cluster used to leave it as it was: Create
// returns early for a running or stopped cluster, and Knative was skipped
// whenever its namespace existed. A release that bumped the node image or
// Knative never reached an existing instance, and nothing on the cluster
// said which versions it had, so the drift could not even be diagnosed.
//
// The stamp is one ConfigMap in kube-system, not in the managed namespace:
// `reset` deletes the managed namespace and keeps the cluster, and what the
// stamp describes is the cluster. Its values are annotations, because a
// ConfigMap data key cannot carry the cloudburrow.dev/ prefix.
const (
	StampNamespace = "kube-system"
	StampName      = "cloudburrow-stamp"

	// AnnotationCLIVersion is the version of the CLI that last ran `up`.
	AnnotationCLIVersion = "cloudburrow.dev/cli-version"
	// AnnotationNodeImage is the kind node image the cluster was created
	// with. A node image cannot change in place.
	AnnotationNodeImage = "cloudburrow.dev/node-image"
	// AnnotationKnativeVersion is the Knative release last applied; absent
	// when Knative was never installed by a stamping CLI.
	AnnotationKnativeVersion = "cloudburrow.dev/knative-version"
)

// ErrNodeImageDrift means the cluster runs a node image other than the one
// this CLI would create it with. A kind node cannot be upgraded in place.
var ErrNodeImageDrift = errors.New("cluster node image differs from the pinned one")

// ErrKnativeDowngrade means the cluster has a newer Knative than this CLI
// pins. Knative does not support downgrading in place.
var ErrKnativeDowngrade = errors.New("cluster Knative is newer than the pinned one")

// Stamp is what a cluster's stamp records. An empty field was not recorded.
type Stamp struct {
	// Present is false for a cluster no stamping CLI has brought up.
	Present        bool
	CLIVersion     string
	NodeImage      string
	KnativeVersion string
}

// Kubectl runs kubectl against one cluster; the caller supplies
// --kubeconfig.
type Kubectl func(ctx context.Context, args ...string) (string, error)

// ReadStamp reads a cluster's stamp. A cluster without one is not an error:
// it returns a Stamp with Present false.
func ReadStamp(ctx context.Context, k Kubectl) (Stamp, error) {
	out, err := k(ctx, "-n", StampNamespace, "get", "configmap", StampName, "--ignore-not-found", "-o", "json")
	if err != nil {
		return Stamp{}, fmt.Errorf("read the cluster stamp: %w", err)
	}
	return parseStamp(out)
}

func parseStamp(out string) (Stamp, error) {
	if strings.TrimSpace(out) == "" {
		return Stamp{}, nil
	}
	var cm struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(out), &cm); err != nil {
		return Stamp{}, fmt.Errorf("parse the cluster stamp: %w", err)
	}
	a := cm.Metadata.Annotations
	return Stamp{
		Present:        true,
		CLIVersion:     a[AnnotationCLIVersion],
		NodeImage:      a[AnnotationNodeImage],
		KnativeVersion: a[AnnotationKnativeVersion],
	}, nil
}

// WriteStamp records s's non-empty fields on the cluster, creating the
// stamp if it is absent. Fields left empty keep what is recorded.
func WriteStamp(ctx context.Context, k Kubectl, s Stamp) error {
	var pairs []string
	for _, kv := range [][2]string{
		{AnnotationCLIVersion, s.CLIVersion},
		{AnnotationNodeImage, s.NodeImage},
		{AnnotationKnativeVersion, s.KnativeVersion},
	} {
		if kv[1] != "" {
			pairs = append(pairs, kv[0]+"="+kv[1])
		}
	}
	if len(pairs) == 0 {
		return nil
	}
	out, err := k(ctx, "-n", StampNamespace, "get", "configmap", StampName, "--ignore-not-found", "-o", "name")
	if err != nil {
		return fmt.Errorf("write the cluster stamp: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		if _, err := k(ctx, "-n", StampNamespace, "create", "configmap", StampName); err != nil {
			return fmt.Errorf("create the cluster stamp: %w", err)
		}
		if _, err := k(ctx, "-n", StampNamespace, "label", "--overwrite", "configmap/"+StampName,
			"cloudburrow.dev/owned=true"); err != nil {
			return fmt.Errorf("label the cluster stamp: %w", err)
		}
	}
	args := append([]string{"-n", StampNamespace, "annotate", "--overwrite", "configmap/" + StampName}, pairs...)
	if _, err := k(ctx, args...); err != nil {
		return fmt.Errorf("write the cluster stamp: %w", err)
	}
	return nil
}

// SameNodeImage reports whether two node image references name the same
// image.
//
// When both carry a digest the digests decide. Otherwise the repository and
// tag do: #597 added a digest to the pin, and a cluster created before it
// records "kindest/node:v1.36.4", which is the same image as
// "kindest/node:v1.36.4@sha256:...". Refusing it would have told every
// existing user to delete a cluster that is exactly what the pin wants.
func SameNodeImage(a, b string) bool {
	an, at, ad := splitImage(a)
	bn, bt, bd := splitImage(b)
	if ad != "" && bd != "" {
		return ad == bd
	}
	return an == bn && at == bt && at != ""
}

// splitImage splits "repo:tag@digest" into its parts; tag and digest may be
// empty.
func splitImage(ref string) (name, tag, digest string) {
	ref = strings.TrimSpace(ref)
	if i := strings.Index(ref, "@"); i >= 0 {
		ref, digest = ref[:i], ref[i+1:]
	}
	// A colon after the last slash is a tag; one before it is a registry
	// port.
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref, tag = ref[:i], ref[i+1:]
	}
	return strings.TrimPrefix(ref, "docker.io/"), tag, digest
}

// CompareKnative orders two Knative release names such as
// "knative-v1.23.0". ok is false when either does not parse, and the caller
// then knows only that they differ.
func CompareKnative(a, b string) (cmp int, ok bool) {
	pa, oka := knativeParts(a)
	pb, okb := knativeParts(b)
	if !oka || !okb {
		return 0, false
	}
	for i := range pa {
		switch {
		case pa[i] < pb[i]:
			return -1, true
		case pa[i] > pb[i]:
			return 1, true
		}
	}
	return 0, true
}

func knativeParts(v string) ([3]int, bool) {
	var p [3]int
	v = strings.TrimPrefix(strings.TrimPrefix(v, "knative-"), "v")
	fields := strings.Split(v, ".")
	if len(fields) != 3 {
		return p, false
	}
	for i, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 {
			return p, false
		}
		p[i] = n
	}
	return p, true
}

// kubectl runs kubectl against this cluster's own kubeconfig.
func (c *Cluster) kubectl(ctx context.Context, args ...string) (string, error) {
	return c.runner.Run(ctx, "kubectl", append([]string{"--kubeconfig", c.opts.Kubeconfig}, args...)...)
}

// ReadStamp reads this cluster's stamp.
func (c *Cluster) ReadStamp(ctx context.Context) (Stamp, error) { return ReadStamp(ctx, c.kubectl) }

// observedNodeImage asks Docker which image the first node container runs,
// for a cluster created before the stamp existed. Empty when it cannot tell.
func (c *Cluster) observedNodeImage(ctx context.Context) string {
	nodes, err := c.nodes(ctx)
	if err != nil || len(nodes) == 0 {
		return ""
	}
	out, err := c.runner.Run(ctx, "docker", "inspect", "-f", "{{.Config.Image}}", nodes[0])
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// Reconcile checks a ready cluster against the pins and records the stamp.
//
// created says this `up` made the cluster, which is then stamped with the
// node image it was made with. An existing cluster is compared: a node
// image other than the pinned one is refused with ErrNodeImageDrift, since
// only `cloudburrow delete` and a new `up` can change it. A cluster with no
// recorded image, created before the stamp existed, is judged by the image
// its node container runs, and stamped with it; if that cannot be read
// either, the image is reported unknown and left unrecorded rather than
// guessed. Every `up` records its CLI version.
//
// It returns what the cluster is now stamped with, and a note for the
// operator when there is something to say.
func (c *Cluster) Reconcile(ctx context.Context, created bool) (Stamp, string, error) {
	write := Stamp{CLIVersion: c.opts.CLIVersion}
	var now Stamp
	var note string
	if created {
		write.NodeImage = c.opts.NodeImage
	} else {
		have, err := c.ReadStamp(ctx)
		if err != nil {
			return Stamp{}, "", err
		}
		now = have
		image, from := have.NodeImage, "is stamped with"
		if image == "" {
			image, from = c.observedNodeImage(ctx), "runs"
			write.NodeImage = image
		}
		switch {
		case image == "":
			note = "node image unknown: the cluster predates the version stamp and its node container could not be inspected"
		case !SameNodeImage(image, c.opts.NodeImage):
			return Stamp{}, "", fmt.Errorf("%w: cluster %s %s node image %s, but this cloudburrow pins %s. "+
				"A node image cannot be changed in place: run `cloudburrow delete` (this destroys the cluster and "+
				"the state in it) and then `cloudburrow up`; or, to keep this cluster, set cluster.nodeImage to %s",
				ErrNodeImageDrift, c.opts.Name, from, image, c.opts.NodeImage, image)
		}
	}
	if err := WriteStamp(ctx, c.kubectl, write); err != nil {
		return Stamp{}, "", err
	}
	now.Present = true
	if write.CLIVersion != "" {
		now.CLIVersion = write.CLIVersion
	}
	if write.NodeImage != "" {
		now.NodeImage = write.NodeImage
	}
	return now, note, nil
}
