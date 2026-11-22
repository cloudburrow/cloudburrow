package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/dockergc"
	"github.com/cloudburrow/cloudburrow/internal/images"
)

// dockerGCRunner runs docker for stop's and delete's cleanup and for gc; a
// variable so a test can stand in for Docker.
var dockerGCRunner dockergc.Runner = images.ExecRunner{}

// instanceNodeImages is what the instance's running nodes hold, read before
// stop or delete takes them down, so the cleanup after knows which shared
// images this instance used (#2152).
func instanceNodeImages(ctx context.Context, instance string) map[string]bool {
	running, err := dockergc.RunningClusters(ctx, dockerGCRunner)
	if err != nil {
		return nil
	}
	return dockergc.NodeImages(ctx, dockerGCRunner, running[dockergc.ClusterName(instance)])
}

// cleanInstanceDocker removes the images and volumes CloudBurrow made for
// the instance once its cluster is stopped or deleted (#2152). It never
// fails the stop or delete: what it cannot remove is reported, and
// `cloudburrow gc` lists it later.
func cleanInstanceDocker(instance string, used map[string]bool, stdout io.Writer) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := dockergc.CleanInstance(ctx, dockerGCRunner, instance, used)
	if err != nil {
		fmt.Fprintf(stdout, "warning: the instance's Docker images were not cleaned up: %v\n", err)
		return
	}
	var size int64
	for _, img := range res.Removed {
		size += img.Size
	}
	if len(res.Removed) > 0 || len(res.Volumes) > 0 {
		fmt.Fprintf(stdout, "removed %d Docker image(s) (%s) and %d volume(s) labelled %s\n",
			len(res.Removed), dockerSize(size), len(res.Volumes), dockergc.Label(instance))
	}
	for _, k := range res.Kept {
		fmt.Fprintf(stdout, "kept Docker image %s %v: %s\n", k.Image.Short(), k.Image.Tags, k.Reason)
	}
	for _, e := range res.Errors {
		fmt.Fprintf(stdout, "warning: %v\n", e)
	}
}

// dockerSize is n as docker images prints a size.
func dockerSize(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "kMGTPE"[exp])
}

const gcHelp = `List the Docker images and volumes CloudBurrow left behind, and with --yes
remove them (#2152): images under CloudBurrow's own repositories
(dev.local/cloudburrow-*, cloudburrow.local/*, cloudburrow-vm-*, and the
dev.local/<Artifact Registry or Container Registry host>/... images Cloud Run
source builds and Cloud Build make) that carry no io.cloudburrow.instance
label, as builds before the label left them; images with the cloudburrow.dev/owned
label; and images and volumes labelled for an instance whose cluster no longer
exists. An image a container uses, or a running instance's nodes hold, is
never listed, nor is any image or volume without CloudBurrow's labels or
repositories. stop and delete remove an instance's own labelled images
themselves. See "Docker images and volumes" in docs/configuration.md.

Flags of gc:
  -yes
    	remove what is listed (default: list only)

`

// runGC implements `cloudburrow gc`.
func runGC(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	fs.SetOutput(stderr)
	yes := fs.Bool("yes", false, "remove what is listed")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		return errUsage
	}
	imgs, err := dockergc.Collect(ctx, dockerGCRunner)
	if err != nil {
		return err
	}
	vols, err := dockergc.OrphanVolumes(ctx, dockerGCRunner)
	if err != nil {
		return err
	}
	if len(imgs) == 0 && len(vols) == 0 {
		fmt.Fprintln(stdout, "nothing to remove")
		return nil
	}
	var size int64
	for _, img := range imgs {
		size += img.Size
		tag := "<none>"
		if len(img.Tags) > 0 {
			tag = img.Tags[0]
			if len(img.Tags) > 1 {
				tag += fmt.Sprintf(" (+%d)", len(img.Tags)-1)
			}
		}
		fmt.Fprintf(stdout, "image  %s  %8s  %s  (%s)\n", img.Short(), dockerSize(img.Size), tag, img.Reason)
	}
	for _, v := range vols {
		fmt.Fprintf(stdout, "volume %s  (instance no longer exists)\n", v)
	}
	if !*yes {
		fmt.Fprintf(stdout, "%d image(s) (%s) and %d volume(s); run `cloudburrow gc --yes` to remove them\n",
			len(imgs), dockerSize(size), len(vols))
		return nil
	}
	removed, failed := 0, 0
	var freed int64
	for _, img := range imgs {
		if err := dockergc.RemoveImage(ctx, dockerGCRunner, img); err != nil {
			fmt.Fprintf(stderr, "warning: %v\n", err)
			failed++
			continue
		}
		removed++
		freed += img.Size
	}
	vremoved := 0
	for _, v := range vols {
		if out, err := dockerGCRunner.Run(ctx, "docker", "volume", "rm", v); err != nil {
			fmt.Fprintf(stderr, "warning: docker volume rm %s: %v %s\n", v, err, out)
			failed++
			continue
		}
		vremoved++
	}
	fmt.Fprintf(stdout, "removed %d image(s) (%s) and %d volume(s)\n", removed, dockerSize(freed), vremoved)
	if failed > 0 {
		return fmt.Errorf("%d removal(s) failed", failed)
	}
	return nil
}
