package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/cloudburrow/cloudburrow/internal/bigqueryimage"
	"github.com/cloudburrow/cloudburrow/internal/components"
	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/doctor"
	"github.com/cloudburrow/cloudburrow/internal/images"
)

// bigQueryImageComponent builds the BigQuery emulator's image from the
// patched build this CLI embeds and loads it into the cluster (#1061),
// between the cluster and the components that deploy it, as the storage
// image is built. Nothing is pulled but the digest-pinned base; an image
// already built from this binary is reused.
type bigQueryImageComponent struct {
	kubeconfig string
	loader     *images.Loader
	build      func(ctx context.Context, arch string) (string, error)
	comps      *components.LifecycleComponent
	out        io.Writer
}

func newBigQueryImageComponent(kubeconfig, cluster string, comps *components.LifecycleComponent, out io.Writer) *bigQueryImageComponent {
	r := images.ExecRunner{}
	return &bigQueryImageComponent{
		kubeconfig: kubeconfig,
		loader:     &images.Loader{ClusterName: cluster, Runner: r},
		build:      func(ctx context.Context, arch string) (string, error) { return bigqueryimage.Build(ctx, r, arch) },
		comps:      comps,
		out:        out,
	}
}

func (b *bigQueryImageComponent) Name() string { return "bigquery-image" }

func (b *bigQueryImageComponent) Start(ctx context.Context) error {
	arch, err := b.loader.NodeArchitecture(ctx, b.kubeconfig)
	if err != nil {
		return fmt.Errorf("the cluster's node architecture: %w", err)
	}
	ref, err := b.build(ctx, arch)
	if err != nil {
		return fmt.Errorf("the BigQuery emulator image: %w", err)
	}
	if err := b.loader.Load(ctx, ref, b.kubeconfig); err != nil {
		return fmt.Errorf("load %s: %w", ref, err)
	}
	fmt.Fprintf(b.out, "  bigquery: emulator image %s (goccy/bigquery-emulator %s, linux/%s)\n", ref, bigqueryimage.Version, arch)
	b.comps.SetBigQueryImage(ref)
	return nil
}

func (b *bigQueryImageComponent) Stop(context.Context) error { return nil }

// bigQueryEmbedResult is doctor's row for the BigQuery emulator this CLI
// embeds (#1061): which Linux builds it has, and whether one is for
// nodeArch.
func bigQueryEmbedResult(cfg config.Config, nodeArch string) doctor.Result {
	missing := map[string]string{}
	for arch, err := range bigqueryimage.Check() {
		var ne *bigqueryimage.NotEmbeddedError
		switch {
		case err == nil:
			missing[arch] = ""
		case errors.As(err, &ne):
			missing[arch] = ne.Reason
		default:
			missing[arch] = err.Error()
		}
	}
	return doctor.EmbeddedBigQuery(serviceEnabled(cfg, config.ServiceBigQuery), nodeArch, missing)
}

// preflightBigQuery refuses `up` with BigQuery enabled from a CLI that
// embeds no emulator for the node's architecture, before anything is
// created, as preflightStorage does for the storage server.
func preflightBigQuery(cfg config.Config, nodeArch string, stderr io.Writer) error {
	res := bigQueryEmbedResult(cfg, nodeArch)
	if res.Level != doctor.LevelFail {
		return nil
	}
	fmt.Fprintf(stderr, "cloudburrow up: this CLI cannot start BigQuery; nothing was created\n\n")
	doctor.Report{Results: []doctor.Result{res}}.Write(stderr)
	err := bigqueryimage.Check()[nodeArch]
	if err == nil {
		err = bigqueryimage.ErrNotEmbedded
	}
	return fmt.Errorf("BigQuery is enabled, but %w; or pass --services without bigquery. Nothing was created", err)
}
