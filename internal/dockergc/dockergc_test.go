package dockergc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// fakeDocker is the part of Docker the cleanup reads and changes: images,
// containers, kind clusters' nodes and the images they hold, and volumes.
type fakeDocker struct {
	images     map[string]*fakeImage
	containers []fakeContainer
	volumes    map[string]map[string]string // name -> labels
	mounted    map[string]bool              // volumes a container mounts
	calls      []string
}

type fakeImage struct {
	tags   []string
	labels map[string]string
	size   int64
}

type fakeContainer struct {
	id, image, ref string
	cluster        string // kind cluster label, if a node
	running        bool
	nodeImages     []string // tags the node's runtime holds
}

func newFake() *fakeDocker {
	return &fakeDocker{images: map[string]*fakeImage{}, volumes: map[string]map[string]string{}, mounted: map[string]bool{}}
}

func (f *fakeDocker) image(id string, labels map[string]string, tags ...string) {
	f.images["sha256:"+id] = &fakeImage{tags: tags, labels: labels, size: 113_000_000}
}

func (f *fakeDocker) node(cluster string, running bool, holds ...string) {
	f.containers = append(f.containers, fakeContainer{id: "c-" + cluster + fmt.Sprint(len(f.containers)), image: "sha256:node",
		ref: "kindest/node", cluster: cluster, running: running, nodeImages: holds})
}

func (f *fakeDocker) Run(_ context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	if name != "docker" {
		return "", errors.New("not docker")
	}
	j := strings.Join(args, " ")
	switch {
	case strings.HasPrefix(j, "image ls -q --no-trunc"):
		var out []string
		filter := ""
		if i := strings.Index(j, "label="); i >= 0 {
			filter = j[i+len("label="):]
		}
		for id, img := range f.images {
			if filter != "" {
				if _, ok := img.labels[filter]; !ok {
					continue
				}
			}
			for range max(1, len(img.tags)) {
				out = append(out, id)
			}
		}
		sort.Strings(out)
		return strings.Join(out, "\n"), nil
	case strings.HasPrefix(j, "image inspect --format {{.Id}} "):
		if f.lookup(args[len(args)-1]) == "" {
			return "", errors.New("no such image")
		}
		return f.lookup(args[len(args)-1]), nil
	case strings.HasPrefix(j, "image inspect --format"):
		var out []string
		for _, ref := range args[4:] {
			id := f.lookup(ref)
			if id == "" {
				return "", errors.New("no such image " + ref)
			}
			img := f.images[id]
			idj, _ := json.Marshal(id)
			tj, _ := json.Marshal(img.tags)
			lj, _ := json.Marshal(map[string]any{"Labels": img.labels})
			out = append(out, fmt.Sprintf("%s\t%s\t%s\t%d", idj, tj, lj, img.size))
		}
		return strings.Join(out, "\n"), nil
	case strings.HasPrefix(j, "image rm "):
		ref := args[2]
		id := f.lookup(ref)
		if id == "" {
			return "", errors.New("no such image")
		}
		for _, c := range f.containers {
			if c.image == id {
				return "", errors.New("image is being used by a container")
			}
		}
		img := f.images[id]
		if ref != id {
			var rest []string
			for _, t := range img.tags {
				if t != ref {
					rest = append(rest, t)
				}
			}
			img.tags = rest
			if len(rest) > 0 {
				return "Untagged: " + ref, nil
			}
		} else if len(img.tags) > 1 {
			return "", errors.New("image is referenced in multiple repositories")
		}
		delete(f.images, id)
		return "Deleted: " + id, nil
	case j == "ps -a -q --no-trunc":
		var out []string
		for _, c := range f.containers {
			out = append(out, c.id)
		}
		return strings.Join(out, "\n"), nil
	case strings.HasPrefix(j, "container inspect"):
		var out []string
		for _, id := range args[4:] {
			for _, c := range f.containers {
				if c.id == id {
					out = append(out, c.image+"\t"+c.ref)
				}
			}
		}
		return strings.Join(out, "\n"), nil
	case strings.HasPrefix(j, "ps --filter label="+kindClusterLabel), strings.HasPrefix(j, "ps -a --filter label="+kindClusterLabel):
		all := args[1] == "-a"
		var out []string
		for _, c := range f.containers {
			if c.cluster == "" || (!all && !c.running) {
				continue
			}
			if all {
				out = append(out, c.cluster)
			} else {
				out = append(out, c.cluster+"\t"+c.id)
			}
		}
		return strings.Join(out, "\n"), nil
	case strings.HasPrefix(j, "exec "):
		for _, c := range f.containers {
			if c.id == args[1] && c.running {
				type img struct {
					ID       string   `json:"id"`
					RepoTags []string `json:"repoTags"`
				}
				var doc struct {
					Images []img `json:"images"`
				}
				for _, t := range c.nodeImages {
					doc.Images = append(doc.Images, img{ID: "sha256:node-" + t, RepoTags: []string{t}})
				}
				b, _ := json.Marshal(doc)
				return string(b), nil
			}
		}
		return "", errors.New("container is not running")
	case strings.HasPrefix(j, "volume ls -q --filter label="):
		key, val, _ := strings.Cut(strings.TrimPrefix(args[4], "label="), "=")
		var out []string
		for n, l := range f.volumes {
			if v, ok := l[key]; ok && v == val {
				out = append(out, n)
			}
		}
		sort.Strings(out)
		return strings.Join(out, "\n"), nil
	case strings.HasPrefix(j, "volume ls --filter label="):
		key := strings.TrimPrefix(args[3], "label=")
		var out []string
		for n, l := range f.volumes {
			if v, ok := l[key]; ok {
				out = append(out, n+"\t"+v)
			}
		}
		sort.Strings(out)
		return strings.Join(out, "\n"), nil
	case strings.HasPrefix(j, "volume rm "):
		if f.mounted[args[2]] {
			return "", errors.New("volume is in use")
		}
		delete(f.volumes, args[2])
		return args[2], nil
	}
	return "", fmt.Errorf("unexpected docker %s", j)
}

// lookup resolves an ID or a tag to an ID.
func (f *fakeDocker) lookup(ref string) string {
	if _, ok := f.images[ref]; ok {
		return ref
	}
	for id, img := range f.images {
		for _, t := range img.tags {
			if t == ref {
				return id
			}
		}
	}
	return ""
}

func (f *fakeDocker) has(id string) bool { _, ok := f.images["sha256:"+id]; return ok }

func inst(name string) map[string]string { return map[string]string{InstanceLabel: name} }

func TestLabels(t *testing.T) {
	if got := LabelArgs("w1"); strings.Join(got, " ") != "--label io.cloudburrow.instance=w1" {
		t.Errorf("LabelArgs = %v", got)
	}
	if LabelArgs("") != nil {
		t.Error("LabelArgs of no instance labels")
	}
	if got := LabelDockerfile("127.0.0.1:5000/build-1:latest", "w1"); got != "FROM 127.0.0.1:5000/build-1:latest\nLABEL io.cloudburrow.instance=\"w1\"\n" {
		t.Errorf("LabelDockerfile = %q", got)
	}
	if ClusterName("cloudburrow") != "cloudburrow" || ClusterName("w1") != "cloudburrow-w1" {
		t.Error("ClusterName differs from config.ClusterName")
	}
}

func TestKnownRepository(t *testing.T) {
	for ref, want := range map[string]bool{
		"dev.local/cloudburrow-storage:0123456789abcdef":             true,
		"dev.local/cloudburrow-bigquery:0123456789abcdef":            true,
		"dev.local/us-central1-docker.pkg.dev/p/r/app:latest":        true,
		"dev.local/us-docker.pkg.dev/p/r/app:v1":                     true,
		"dev.local/gcr.io/p/app:latest":                              true,
		"dev.local/eu.gcr.io/p/app:latest":                           true,
		"cloudburrow.local/spark-container:0123":                     true,
		"cloudburrow-vm-debian-12:1ab64d203ce3":                      true,
		"dev.local/myapp:1":                                          false,
		"dev.local/docker.io/library/nginx:1":                        false,
		"us-central1-docker.pkg.dev/p/r/app:latest":                  false,
		"gcr.io/distroless/static-debian12:latest":                   false,
		"kindest/node:v1.36.4":                                       false,
		"dev.local/gcr.io":                                           false,
		"example.com/dev.local/cloudburrow-storage:0123456789abcdef": false,
	} {
		if got := KnownRepository(ref); got != want {
			t.Errorf("KnownRepository(%s) = %v, want %v", ref, got, want)
		}
	}
}

// Stop removes what was labelled for the instance and nothing else: not
// another instance's, not an unlabelled one, not one a container uses,
// not one another running instance's nodes hold.
func TestCleanInstance(t *testing.T) {
	f := newFake()
	f.image("mine", inst("w1"), "dev.local/us-central1-docker.pkg.dev/p/r/app:latest")
	f.image("mine2", inst("w1"), "dev.local/gcr.io/p/a:v1", "dev.local/gcr.io/p/a:v2")
	f.image("dangling", inst("w1"))
	f.image("shared", inst("w1"), "dev.local/cloudburrow-storage:aaaa")
	f.image("inuse", inst("w1"), "dev.local/gcr.io/p/b:v1")
	f.image("other", inst("w2"), "dev.local/gcr.io/p/c:v1")
	f.image("user", nil, "dev.local/myapp:1")
	f.image("unlabelled", nil, "dev.local/cloudburrow-storage:bbbb")
	f.node("cloudburrow-w1", false) // stopped
	f.node("cloudburrow-w2", true, "dev.local/cloudburrow-storage:aaaa")
	f.containers = append(f.containers, fakeContainer{id: "app", image: "sha256:inuse", ref: "dev.local/gcr.io/p/b:v1"})
	f.volumes["cb-build-ws"] = map[string]string{legacyVolumeLabel: "w1"}
	f.volumes["anon1"] = map[string]string{InstanceLabel: "w1"}
	f.volumes["busy"] = map[string]string{InstanceLabel: "w1"}
	f.volumes["theirs"] = map[string]string{InstanceLabel: "w2"}
	f.volumes["users"] = map[string]string{}
	f.mounted["busy"] = true

	res, err := CleanInstance(context.Background(), f, "w1", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"mine", "mine2", "dangling"} {
		if f.has(id) {
			t.Errorf("image %s was not removed", id)
		}
	}
	for _, id := range []string{"shared", "inuse", "other", "user", "unlabelled"} {
		if !f.has(id) {
			t.Errorf("image %s was removed", id)
		}
	}
	if len(res.Removed) != 3 || len(res.Kept) != 2 {
		t.Errorf("removed %v, kept %v", res.Removed, res.Kept)
	}
	for _, k := range res.Kept {
		if k.Image.ID == "sha256:shared" && k.Reason != "cluster cloudburrow-w2 uses it" ||
			k.Image.ID == "sha256:inuse" && k.Reason != "a container uses it" {
			t.Errorf("kept %s: %s", k.Image.ID, k.Reason)
		}
	}
	sort.Strings(res.Volumes)
	if strings.Join(res.Volumes, ",") != "anon1,cb-build-ws" || len(res.Errors) != 1 {
		t.Errorf("volumes removed %v, errors %v", res.Volumes, res.Errors)
	}
	for _, v := range []string{"busy", "theirs", "users"} {
		if _, ok := f.volumes[v]; !ok {
			t.Errorf("volume %s was removed", v)
		}
	}
	for _, c := range f.calls {
		if strings.Contains(c, " -f") && strings.HasPrefix(c, "image rm") {
			t.Errorf("forced removal: %s", c)
		}
	}
}

// A shared image another instance built and left, because this one still
// used it, goes with this one's stop once its builder no longer runs.
func TestCleanInstanceAdoptsSharedImage(t *testing.T) {
	f := newFake()
	f.image("shared", inst("w2"), "dev.local/cloudburrow-storage:aaaa")
	f.image("w2only", inst("w2"), "dev.local/gcr.io/p/c:v1")
	f.image("w3", inst("w3"), "dev.local/cloudburrow-bigquery:cccc")
	f.node("cloudburrow-w1", false)
	f.node("cloudburrow-w2", false)
	f.node("cloudburrow-w3", true)
	used := map[string]bool{"dev.local/cloudburrow-storage:aaaa": true, "dev.local/cloudburrow-bigquery:cccc": true}
	res, err := CleanInstance(context.Background(), f, "w1", used)
	if err != nil {
		t.Fatal(err)
	}
	if f.has("shared") || !f.has("w2only") || !f.has("w3") || len(res.Removed) != 1 {
		t.Errorf("removed %v; left %v", res.Removed, f.images)
	}
}

// Without an instance name there is no label to clean by.
func TestCleanInstanceNoInstance(t *testing.T) {
	if _, err := CleanInstance(context.Background(), newFake(), "", nil); err == nil {
		t.Error("CleanInstance with no instance ran")
	}
}

// gc lists unlabelled CloudBurrow builds and orphans, never the user's own
// images, never one a container uses or a running node holds.
func TestCollect(t *testing.T) {
	f := newFake()
	f.image("old-storage", nil, "dev.local/cloudburrow-storage:1111")
	f.image("old-run", nil, "dev.local/us-central1-docker.pkg.dev/p/r/app:latest")
	f.image("vm", map[string]string{ownedLabel: "true"}, "cloudburrow-vm-debian-12:1ab64d203ce3")
	f.image("orphan", inst("gone"), "dev.local/gcr.io/p/a:v1")
	f.image("stopped", inst("w1"), "dev.local/gcr.io/p/b:v1")
	f.image("user", nil, "dev.local/myapp:1")
	f.image("mixed", nil, "dev.local/cloudburrow-storage:2222", "myrepo/storage:copy")
	f.image("dangling", nil)
	f.image("running-node", nil, "dev.local/cloudburrow-storage:3333")
	f.image("container", nil, "dev.local/gcr.io/p/c:v1")
	f.image("base", nil, "gcr.io/distroless/static-debian12:latest")
	f.node("cloudburrow-w1", false)
	f.node("cloudburrow-demo", true, "dev.local/cloudburrow-storage:3333")
	f.containers = append(f.containers, fakeContainer{id: "app", image: "sha256:container", ref: "dev.local/gcr.io/p/c:v1"})
	f.volumes["orphan-vol"] = map[string]string{InstanceLabel: "gone"}
	f.volumes["kept-vol"] = map[string]string{InstanceLabel: "w1"}
	f.volumes["legacy-orphan"] = map[string]string{legacyVolumeLabel: "gone2"}
	f.volumes["users"] = map[string]string{"com.example": "x"}

	imgs, err := Collect(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, i := range imgs {
		got = append(got, strings.TrimPrefix(i.ID, "sha256:"))
	}
	sort.Strings(got)
	if strings.Join(got, ",") != "old-run,old-storage,orphan,vm" {
		t.Errorf("Collect = %v", got)
	}
	vols, err := OrphanVolumes(context.Background(), f)
	sort.Strings(vols)
	if err != nil || strings.Join(vols, ",") != "legacy-orphan,orphan-vol" {
		t.Errorf("OrphanVolumes = %v, %v", vols, err)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "image rm") || strings.HasPrefix(c, "volume rm") {
			t.Errorf("Collect removed something: %s", c)
		}
	}
}

func TestRemoveImageUntagsEachTag(t *testing.T) {
	f := newFake()
	f.image("x", inst("w1"), "dev.local/gcr.io/p/a:v1", "dev.local/gcr.io/p/a:v2")
	if err := RemoveImage(context.Background(), f, Image{ID: "sha256:x", Tags: []string{"dev.local/gcr.io/p/a:v1", "dev.local/gcr.io/p/a:v2"}}); err != nil || f.has("x") {
		t.Errorf("RemoveImage = %v; left %v", err, f.images)
	}
	f.image("y", inst("w1"))
	f.containers = append(f.containers, fakeContainer{id: "c", image: "sha256:y"})
	if err := RemoveImage(context.Background(), f, Image{ID: "sha256:y"}); err == nil || !f.has("y") {
		t.Errorf("RemoveImage of a used image = %v", err)
	}
}
