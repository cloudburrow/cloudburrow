package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestCarryImagesVerifiesTheArchive (main run 45770556272,
// TestGKERollbackNodePoolUpgrade: an import read a short archive): the
// loaded images are exported and imported with --local, not through the
// transfer API's streams, which end as a clean EOF when they break; the
// export is read back in full and renamed from a .part name; the archive is
// named for its target node; and the copy is imported only with the
// export's checksum.
func TestCarryImagesVerifiesTheArchive(t *testing.T) {
	const sum = "fd90b142b93effa63ddd214f93c33a01447d052b364623ef29e4ef0974d83fa4"
	kind := OwnerPrefix + "c"
	from, to := kind+"-default-pool-2", kind+"-default-pool-4"
	path := "/kind/carry-" + to + ".tar"
	run := func(exportOut string, importErr error) ([]string, error) {
		var calls []string
		r := &funcRunner{fn: func(cmd string) (string, error) {
			calls = append(calls, cmd)
			switch {
			case strings.HasPrefix(cmd, "docker exec "+from+" ctr -n k8s.io images ls"):
				return "dev.local/probe:1\nregistry.k8s.io/pause:3.10\n", nil
			case strings.HasPrefix(cmd, "docker exec "+to+" ctr -n k8s.io images ls"):
				return "registry.k8s.io/pause:3.10\n", nil
			case strings.HasPrefix(cmd, "docker exec "+from+" sh -c"):
				return exportOut, nil
			case strings.HasPrefix(cmd, "docker exec "+to+" sh -c"):
				return "", importErr
			}
			return "", nil
		}}
		g := &GKE{Dir: t.TempDir(), Runner: r}
		return calls, g.carryImages(context.Background(), kind, from, to)
	}

	calls, err := run("amd64 "+sum+"\n", nil)
	if err != nil {
		t.Fatal(err)
	}
	var export, imp, cpOut, cpIn string
	for _, c := range calls {
		switch {
		case strings.HasPrefix(c, "docker exec "+from+" sh -c"):
			export = c
		case strings.HasPrefix(c, "docker exec "+to+" sh -c"):
			imp = c
		case strings.HasPrefix(c, "docker cp "+from+":"):
			cpOut = c
		case strings.HasPrefix(c, "docker cp ") && strings.HasSuffix(c, " "+to+":"+path):
			cpIn = c
		}
	}
	for _, want := range []string{"images export --local ", `"$out.part"`, `tar -tf "$out.part"`, `mv "$out.part" "$out"`, "sha256sum", " sh " + path + " dev.local/probe:1"} {
		if !strings.Contains(export, want) {
			t.Errorf("the export %q lacks %q", export, want)
		}
	}
	if strings.Contains(export, "registry.k8s.io/pause") {
		t.Errorf("the export carries the node image's own image: %q", export)
	}
	if cpOut != "docker cp "+from+":"+path+" "+strings.Fields(cpOut)[3] || cpIn == "" {
		t.Errorf("the copies = %q, %q, want %s out of %s and into %s", cpOut, cpIn, path, from, to)
	}
	for _, want := range []string{"sha256sum -c", "images import --local ", " sh " + path + " " + sum + " amd64"} {
		if !strings.Contains(imp, want) {
			t.Errorf("the import %q lacks %q", imp, want)
		}
	}

	// A copy that is not the export is refused, and the archives removed.
	calls, err = run("amd64 "+sum+"\n", errors.New("exit status 1: the copied archive is not the exported one"))
	if err == nil || !strings.Contains(err.Error(), "import the loaded images into node "+to) {
		t.Errorf("a refused import = %v", err)
	}
	for _, want := range []string{"docker exec " + to + " rm -f " + path, "docker exec " + from + " rm -f " + path + " " + path + ".part"} {
		found := false
		for _, c := range calls {
			found = found || c == want
		}
		if !found {
			t.Errorf("no %q after a refused import: %q", want, calls)
		}
	}

	// An export that printed no checksum is never copied.
	calls, err = run("amd64\n", nil)
	if err == nil {
		t.Error("an export with no checksum was carried")
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "docker cp ") {
			t.Errorf("copied after an export with no checksum: %q", c)
		}
	}
}

// TestUpgradeControlPlaneVerifiesTheArchive (Landing 170 run 46073901317,
// TestGKEServedUpgradeThroughTheBrowser: the control plane's import read a
// short archive): the new version's control-plane images are exported and
// imported with --local, the export read back in full and renamed from a
// .part name, and the copy imported only with the export's checksum; an
// export that printed none is never copied in.
func TestUpgradeControlPlaneVerifiesTheArchive(t *testing.T) {
	const sum = "fd90b142b93effa63ddd214f93c33a01447d052b364623ef29e4ef0974d83fa4"
	kind := OwnerPrefix + "c"
	cp, helper := kind+"-control-plane", kind+"-upgrade"
	run := func(exportOut string) ([]string, error) {
		var calls []string
		r := &funcRunner{fn: func(cmd string) (string, error) {
			calls = append(calls, cmd)
			switch {
			case strings.HasPrefix(cmd, "docker exec "+helper+" sh -c"):
				return exportOut, nil
			case strings.HasPrefix(cmd, "docker exec "+cp+" sh -c") && !strings.Contains(cmd, "images import"):
				// The step after the import ends the run here.
				return "", errStopAfterImport
			}
			return "", nil
		}}
		g := &GKE{Dir: t.TempDir(), Runner: r}
		return calls, g.UpgradeControlPlane(context.Background(), kind, "kindest/node:v1.36.4")
	}

	calls, err := run("amd64 " + sum + "\n")
	if !errors.Is(err, errStopAfterImport) {
		t.Fatalf("the upgrade stopped at %v, not after the import", err)
	}
	var export, imp string
	for _, c := range calls {
		switch {
		case strings.HasPrefix(c, "docker exec "+helper+" sh -c"):
			export = c
		case strings.HasPrefix(c, "docker exec "+cp+" sh -c") && strings.Contains(c, "images import"):
			imp = c
		}
	}
	for _, want := range []string{"images export --local ", "/upgrade-images.tar.part", "tar -tf /upgrade-images.tar.part", "mv /upgrade-images.tar.part /upgrade-images.tar", "sha256sum"} {
		if !strings.Contains(export, want) {
			t.Errorf("the export %q lacks %q", export, want)
		}
	}
	for _, want := range []string{"sha256sum -c", "images import --local ", " sh /kind/upgrade-images.tar " + sum + " amd64"} {
		if !strings.Contains(imp, want) {
			t.Errorf("the import %q lacks %q", imp, want)
		}
	}

	calls, err = run("amd64\n")
	if err == nil {
		t.Error("an export with no checksum was imported")
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "docker cp ") {
			t.Errorf("copied after an export with no checksum: %q", c)
		}
	}
}

var errStopAfterImport = errors.New("stop after the import")
