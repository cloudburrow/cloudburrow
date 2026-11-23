package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/images"
	"github.com/cloudburrow/cloudburrow/internal/spannerimage"
)

// spannerImageComponent builds the Spanner emulator's image from
// CloudBurrow's patched build this CLI embeds (#2029) and loads it into the
// cluster, between the cluster and the components that deploy it, as the
// BigQuery emulator's image is built. A CLI that embeds no build for the
// node's architecture runs Google's prebuilt image, pinned by digest, and
// warns what that image gets wrong.
type spannerImageComponent struct {
	kubeconfig string
	loader     *images.Loader
	build      func(ctx context.Context, arch string) (string, error)
	comps      *components.LifecycleComponent
	out        io.Writer
}

func newSpannerImageComponent(kubeconfig, cluster, instance string, comps *components.LifecycleComponent, out io.Writer) *spannerImageComponent {
	r := images.ExecRunner{}
	return &spannerImageComponent{
		kubeconfig: kubeconfig,
		loader:     &images.Loader{ClusterName: cluster, Runner: r},
		build: func(ctx context.Context, arch string) (string, error) {
			return spannerimage.Build(ctx, r, arch, instance)
		},
		comps: comps,
		out:   out,
	}
}

func (s *spannerImageComponent) Name() string { return "spanner-image" }

// spannerFallbackWarning is what `up` says when the Spanner pod runs
// Google's image.
func spannerFallbackWarning(arch string, why error) string {
	return fmt.Sprintf("  warning: spanner: running Google's emulator image (%s), not CloudBurrow's patched build: %v.\n"+
		"    Google's image returns a GQL query's RETURN ... ORDER BY rows out of order (its first two rows swapped)\n"+
		"    and refuses PROPERTY_EXISTS, COUNT of a group variable and ANY CHEAPEST (#2029). A release CLI embeds the\n"+
		"    patched build for linux/%s; from source, run make spanner-binaries before building the CLI.\n",
		components.SpannerImage, why, arch)
}

func (s *spannerImageComponent) Start(ctx context.Context) error {
	arch, err := s.loader.NodeArchitecture(ctx, s.kubeconfig)
	if err != nil {
		return fmt.Errorf("the cluster's node architecture: %w", err)
	}
	ref, err := s.build(ctx, arch)
	if errors.Is(err, spannerimage.ErrNotEmbedded) {
		fmt.Fprint(s.out, spannerFallbackWarning(arch, err))
		return nil
	}
	if err != nil {
		return fmt.Errorf("the Spanner emulator image: %w", err)
	}
	if err := s.loader.Load(ctx, ref, s.kubeconfig); err != nil {
		return fmt.Errorf("load %s: %w", ref, err)
	}
	fmt.Fprintf(s.out, "  spanner: emulator image %s (cloud-spanner-emulator %s with CloudBurrow's patches, linux/%s)\n", ref, spannerimage.Upstream, arch)
	s.comps.SetSpannerImage(ref)
	return nil
}

func (s *spannerImageComponent) Stop(context.Context) error { return nil }
