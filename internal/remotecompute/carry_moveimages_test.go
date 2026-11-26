package remotecompute

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/dockergc"
)

// The move's image is labelled for the instance, so stop, delete and the
// next move find it again (#2259).
func TestMoveCommitLabelsTheImageForTheInstance(t *testing.T) {
	got := moveCommitArgs("w1", "abc", "cloudburrow.local/move-w1-cloudburrow-w1-control-plane:1")
	want := []string{"commit", "--pause=false", "--change", `LABEL io.cloudburrow.move-instance="w1"`, "abc", "cloudburrow.local/move-w1-cloudburrow-w1-control-plane:1"}
	if !slices.Equal(got, want) {
		t.Errorf("moveCommitArgs = %q, want %q", got, want)
	}
	if dockergc.MoveLabel == dockergc.InstanceLabel {
		t.Error("the move label is the instance label, which stop and delete remove containers by")
	}
}

// fakeMoveDocker is a docker CLI for both ends of a carry: each end
// (DOCKER_HOST, "local" for none) has the move images listed in its
// directory, one file per image ID holding its tag, and runs from those in
// its "used" file. It logs each call as "<end> <args>".
func fakeMoveDocker(t *testing.T) (bin, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake docker is a shell script")
	}
	dir = t.TempDir()
	bin = filepath.Join(dir, "docker")
	script := `#!/bin/sh
end=${DOCKER_HOST:-local}; end=$(printf '%s' "$end" | tr -c 'a-zA-Z0-9\n' _)
d="` + dir + `/$end"; mkdir -p "$d/images"; touch "$d/used"
echo "$end $*" >> "` + dir + `/calls"
case "$*" in
  "image ls -q --no-trunc --filter label=io.cloudburrow.move-instance=w1") ls "$d/images" ;;
  "image inspect --format {{json .Id}}"*)
    shift 4
    for id in "$@"; do
      [ -f "$d/images/$id" ] || { echo "no such image $id" >&2; exit 1; }
      printf '"%s"\t["%s"]\t{"Labels":{"io.cloudburrow.move-instance":"w1"}}\t1300000000\n' "$id" "$(cat "$d/images/$id")"
    done ;;
  "image inspect --format {{.Id}} "*) [ -f "$d/images/$5" ] || exit 1; echo "$5" ;;
  "ps -a -q --no-trunc") [ -s "$d/used" ] && echo node ;;
  "container inspect --format "*" node") cat "$d/used" ;;
  "image rm "*)
    for id in $(ls "$d/images"); do
      if [ "$id" = "$3" ] || [ "$(cat "$d/images/$id")" = "$3" ]; then
        grep -q "$id" "$d/used" && { echo "image is being used by a container" >&2; exit 1; }
        rm "$d/images/$id"; exit 0
      fi
    done
    echo "no such image $3" >&2; exit 1 ;;
esac
exit 0
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, dir
}

func moveImage(t *testing.T, dir, end, id, tag string, used bool) {
	t.Helper()
	d := filepath.Join(dir, end)
	if err := os.MkdirAll(filepath.Join(d, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "images", id), []byte(tag), 0o644); err != nil {
		t.Fatal(err)
	}
	if used {
		f, err := os.OpenFile(filepath.Join(d, "used"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.WriteString(id + "\t" + tag + "\n")
		f.Close()
	}
}

func moveImagesLeft(t *testing.T, dir, end string) []string {
	t.Helper()
	ents, _ := os.ReadDir(filepath.Join(dir, end, "images"))
	var ids []string
	for _, e := range ents {
		ids = append(ids, e.Name())
	}
	return ids
}

// A move back to this machine left its cloudburrow.local/move-* image
// behind each time (#2259). When a move completes, every move image of the
// instance that nothing runs from goes, on both ends; the one the moved
// node runs from stays (delete removes it, TestCleanInstanceRemovesMoveImages).
func TestRemoveSourceRemovesTheMovesImages(t *testing.T) {
	bin, dir := fakeMoveDocker(t)
	// From is the VM, To this machine.
	moveImage(t, dir, "ssh___cb_vm", "sha256:committed", "cloudburrow.local/move-w1-cloudburrow-w1-control-plane:2", false)
	moveImage(t, dir, "ssh___cb_vm", "sha256:old", "cloudburrow.local/move-w1-cloudburrow-w1-control-plane:1", false)
	moveImage(t, dir, "local", "sha256:running", "cloudburrow.local/move-w1-cloudburrow-w1-control-plane:2", true)
	moveImage(t, dir, "local", "sha256:earlier", "cloudburrow.local/move-w1-cloudburrow-w1-control-plane:0", false)
	c := &Carry{o: CarryOptions{Instance: "w1", Docker: bin, From: DockerEndpoint{Host: "ssh://cb@vm"}},
		committed: []string{"cloudburrow.local/move-w1-cloudburrow-w1-control-plane:2"}}
	c.init()
	if err := c.RemoveSource(context.Background()); err != nil {
		t.Fatal(err)
	}
	if left := moveImagesLeft(t, dir, "ssh___cb_vm"); len(left) != 0 {
		t.Errorf("the VM keeps move images %v", left)
	}
	if left := moveImagesLeft(t, dir, "local"); !slices.Equal(left, []string{"sha256:running"}) {
		t.Errorf("this machine keeps move images %v; want only the one its node runs from", left)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "calls"))
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, " rm ") && strings.Contains(l, " -f") && strings.Contains(l, "image") {
			t.Errorf("forced image removal: %s", l)
		}
	}
}

// A rolled-back move removes what it loaded and committed, and any other
// move image of the instance nothing runs from, on both ends.
func TestRollbackRemovesTheMovesImages(t *testing.T) {
	bin, dir := fakeMoveDocker(t)
	// From is this machine, its node running from an earlier move's image;
	// To is the VM, with this move's loaded image and an older one.
	moveImage(t, dir, "local", "sha256:running", "cloudburrow.local/move-w1-cloudburrow-w1-control-plane:1", true)
	moveImage(t, dir, "local", "sha256:committed", "cloudburrow.local/move-w1-cloudburrow-w1-control-plane:2", false)
	moveImage(t, dir, "ssh___cb_vm", "sha256:loaded", "cloudburrow.local/move-w1-cloudburrow-w1-control-plane:2", false)
	moveImage(t, dir, "ssh___cb_vm", "sha256:old", "cloudburrow.local/move-w1-cloudburrow-w1-control-plane:0", false)
	c := &Carry{o: CarryOptions{Instance: "w1", Docker: bin, To: DockerEndpoint{Host: "ssh://cb@vm"}},
		committed: []string{"cloudburrow.local/move-w1-cloudburrow-w1-control-plane:2"}}
	c.init()
	c.Rollback(context.Background())
	if left := moveImagesLeft(t, dir, "ssh___cb_vm"); len(left) != 0 {
		t.Errorf("the VM keeps move images %v", left)
	}
	if left := moveImagesLeft(t, dir, "local"); !slices.Equal(left, []string{"sha256:running"}) {
		t.Errorf("this machine keeps move images %v; want only the one its node runs from", left)
	}
}

// While the instance was away, another kind cluster made on the target's
// Docker was given its node's address (#2259): addressHolder names it, by
// IPv4 or IPv6, and never the carry's own container.
func TestAddressHolderFindsWhoHoldsTheAddress(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake docker is a shell script")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "docker")
	script := `#!/bin/sh
[ "$1 $2 $3" = "network inspect kind" ] || exit 1
echo '[{"Name":"kind","Containers":{"ours":{"Name":"cloudburrow-w1-control-plane","IPv4Address":"172.26.0.4/16"},` +
		`"theirs":{"Name":"cloudburrow-w9-control-plane","IPv4Address":"172.26.0.5/16","IPv6Address":"fc00:f853:ccd:e793::6/64"}}}]'
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	c := &Carry{o: CarryOptions{Instance: "w1", Docker: bin}, madeContainers: []string{"ours"}}
	node := func(ip4, ip6 string) containerInfo {
		var ct containerInfo
		ct.Name = "/cloudburrow-w1-control-plane"
		ct.Config.Labels = map[string]string{"io.x-k8s.kind.cluster": "cloudburrow-w1"}
		ct.NetworkSettings.Networks = map[string]struct {
			IPAMConfig *struct {
				IPv4Address string `json:"IPv4Address"`
				IPv6Address string `json:"IPv6Address"`
			} `json:"IPAMConfig"`
			IPAddress         string   `json:"IPAddress"`
			GlobalIPv6Address string   `json:"GlobalIPv6Address"`
			Aliases           []string `json:"Aliases"`
		}{"kind": {IPAddress: ip4, GlobalIPv6Address: ip6}}
		return ct
	}
	ctx := context.Background()
	if h, ip := c.addressHolder(ctx, node("172.26.0.5", ""), "kind"); h != "cloudburrow-w9-control-plane" || ip != "172.26.0.5" {
		t.Errorf("IPv4 holder = %q %q", h, ip)
	}
	if h, ip := c.addressHolder(ctx, node("172.26.0.9", "fc00:f853:ccd:e793::6"), "kind"); h != "cloudburrow-w9-control-plane" || ip != "fc00:f853:ccd:e793::6" {
		t.Errorf("IPv6 holder = %q %q", h, ip)
	}
	if h, _ := c.addressHolder(ctx, node("172.26.0.4", ""), "kind"); h != "" {
		t.Errorf("the carry's own container holds its address: %q", h)
	}
	if h, _ := c.addressHolder(ctx, node("172.26.0.9", ""), "kind"); h != "" {
		t.Errorf("a free address is held by %q", h)
	}
	if h, _ := c.addressHolder(ctx, node("172.26.0.5", ""), "elsewhere"); h != "" {
		t.Errorf("a network the target lacks: held by %q", h)
	}
	if !node("", "").kindNode() {
		t.Error("a kind node is not recognised")
	}
}
