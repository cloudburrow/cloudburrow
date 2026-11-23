package main

import (
	"context"
	"fmt"
	"io"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/images"
	"github.com/cloudburrow/cloudburrow/internal/spannerimage"
)

// spannerImageComponent loads CloudBurrow's patched Spanner emulator
// (#2029) into the cluster when Docker has it for the node's architecture
// (scripts/spanner-emulator-image.sh builds it), between the cluster and
// the components that deploy it. Without it the Spanner pod runs Google's
// image, pinned by digest, and says so.
type spannerImageComponent struct {
	kubeconfig string
	loader     *images.Loader
	runner     images.Runner
	comps      *components.LifecycleComponent
	out        io.Writer
}

func newSpannerImageComponent(kubeconfig, cluster string, comps *components.LifecycleComponent, out io.Writer) *spannerImageComponent {
	r := images.ExecRunner{}
	return &spannerImageComponent{kubeconfig: kubeconfig, loader: &images.Loader{ClusterName: cluster, Runner: r}, runner: r, comps: comps, out: out}
}

func (s *spannerImageComponent) Name() string { return "spanner-image" }

func (s *spannerImageComponent) Start(ctx context.Context) error {
	arch, err := s.loader.NodeArchitecture(ctx, s.kubeconfig)
	if err != nil {
		return fmt.Errorf("the cluster's node architecture: %w", err)
	}
	if !spannerimage.Available(ctx, s.runner, arch) {
		fmt.Fprintf(s.out, "  spanner: Google's emulator image; CloudBurrow's patched build (%s, linux/%s; scripts/spanner-emulator-image.sh) is not in Docker, so GQL's RETURN ... ORDER BY order, PROPERTY_EXISTS, COUNT of a group variable and ANY CHEAPEST are not served\n",
			spannerimage.Ref, arch)
		return nil
	}
	if err := s.loader.Load(ctx, spannerimage.Ref, s.kubeconfig); err != nil {
		return fmt.Errorf("load %s: %w", spannerimage.Ref, err)
	}
	fmt.Fprintf(s.out, "  spanner: emulator image %s (cloud-spanner-emulator %s with CloudBurrow's patches, linux/%s)\n", spannerimage.Ref, spannerimage.Upstream, arch)
	s.comps.SetSpannerImage(spannerimage.Ref)
	return nil
}

func (s *spannerImageComponent) Stop(context.Context) error { return nil }
