package images

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The terminal image, as internal/terminal pins it.
const terminalRef = "gcr.io/google.com/cloudsdktool/google-cloud-cli@sha256:2b156513775d9ffdba9be27f47ec57567573d10ca77df1eed9f711b9fbf3c9ce"

// recorded reads ctr output recorded on a kind v1.36.4 node (containerd
// 2.3.4, linux/arm64) pulling the terminal image (#826): its index and
// arm64 manifest, and `content active` and `content ls -q` 2.5 and 10.5
// seconds into the pull.
func recorded(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "ctr", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const meterNode = "cloudburrow-x-control-plane"

func meter(t *testing.T, active, ls string) (*PullMeter, *fakeRunner) {
	t.Helper()
	r := &fakeRunner{out: map[string]string{
		"kind get nodes --name cloudburrow-x": meterNode + "\n",
		"uname -m":                            "aarch64\n",
		"content get sha256:2b156513":         recorded(t, "index.json"),
		"content get sha256:304059ff":         recorded(t, "manifest-arm64.json"),
		"content active":                      recorded(t, active),
		"content ls -q":                       recorded(t, ls),
	}}
	return &PullMeter{Loader: &Loader{ClusterName: "cloudburrow-x", Runner: r}, Ref: terminalRef}, r
}

// The arm64 manifest's five layers: 1032608788 bytes, dependencies.json's
// 1033 MB (consoleTerminal.cloudSdkImage.measured).
const terminalLayers = 30189691 + 3227 + 1002412895 + 2778 + 197

// Recorded ctr output gives the bytes fetched of the image's compressed
// size, both read from the node.
func TestPullMeterReadsRecordedCtrOutput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		active, ls string
		extra      string
		fetched    int64
	}{
		// Two layers in flight (9.437MB and 2.097MB), the three small ones
		// committed.
		{"start", "active-start.txt", "ls-start.txt", "", 9437000 + 2097000 + 3227 + 2778 + 197},
		// The 30 MB layer committed too, and the 1 GB one at 243.3MB.
		{"mid", "active-mid.txt", "ls-mid.txt", "", 243300000 + 30189691 + 3227 + 2778 + 197},
		// Nothing in flight and every layer committed: fetched, and being
		// unpacked.
		{"fetched", "active-none.txt", "ls-mid.txt", "sha256:56dac265a020eadb9a7659402eba4db4f579e071545863d910ef74733f45fb4d\n", terminalLayers},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m, r := meter(t, tt.active, tt.ls)
			r.out["content ls -q"] += tt.extra
			p, err := m.Read(context.Background(), meterNode)
			if err != nil {
				t.Fatal(err)
			}
			if p.Total != terminalLayers || p.Fetched != tt.fetched {
				t.Errorf("Read = %+v; want %d of %d", p, tt.fetched, terminalLayers)
			}
			for _, c := range r.calls {
				for _, bad := range []string{" import", " pull", " rm", " delete", "--privileged"} {
					if strings.Contains(c, bad) {
						t.Errorf("the meter ran %q; it only reads", c)
					}
				}
			}
		})
	}
}

// The manifest is read once per node; each Read after that is the two
// listings.
func TestPullMeterReadsTheManifestOnce(t *testing.T) {
	t.Parallel()
	m, r := meter(t, "active-start.txt", "ls-start.txt")
	for i := 0; i < 3; i++ {
		if _, err := m.Read(context.Background(), meterNode); err != nil {
			t.Fatal(err)
		}
	}
	gets := 0
	for _, c := range r.calls {
		if strings.Contains(c, "content get") {
			gets++
		}
	}
	if gets != 2 {
		t.Errorf("read the index and manifest %d times over three reads; want once each:\n%s", gets, strings.Join(r.calls, "\n"))
	}
}

// What the meter cannot read is an error, for the caller to show the
// elapsed time instead: a pull whose manifest is not on the node yet, a
// node that cannot be reached, output it does not recognise, and a
// container that is not one of the cluster's nodes, which is never run in.
func TestPullMeterFailsWhenItCannotRead(t *testing.T) {
	t.Parallel()
	t.Run("no manifest yet", func(t *testing.T) {
		m, r := meter(t, "active-none.txt", "ls-start.txt")
		r.err = map[string]error{"content get": errors.New("ctr: content digest sha256:2b156513...: not found")}
		if _, err := m.Read(context.Background(), meterNode); err == nil {
			t.Error("Read succeeded with no manifest on the node")
		}
		// Once the manifest arrives the next read succeeds.
		r.err = nil
		if _, err := m.Read(context.Background(), meterNode); err != nil {
			t.Errorf("Read after the manifest arrived: %v", err)
		}
	})
	t.Run("node unreachable", func(t *testing.T) {
		m, r := meter(t, "active-start.txt", "ls-start.txt")
		if _, err := m.Read(context.Background(), meterNode); err != nil {
			t.Fatal(err)
		}
		r.err = map[string]error{"content active": errors.New("Error response from daemon: container is not running")}
		if _, err := m.Read(context.Background(), meterNode); err == nil {
			t.Error("Read succeeded with the node unreachable")
		}
	})
	t.Run("unrecognised output", func(t *testing.T) {
		m, r := meter(t, "active-start.txt", "ls-start.txt")
		r.out["content active"] = "REF\tSIZE\tAGE\nlayer-sha256:ab\t12 parsecs\t1 second\n"
		if _, err := m.Read(context.Background(), meterNode); err == nil {
			t.Error("Read succeeded on output it cannot parse")
		}
	})
	t.Run("not a node", func(t *testing.T) {
		m, r := meter(t, "active-start.txt", "ls-start.txt")
		if _, err := m.Read(context.Background(), "someone-elses-container"); err == nil {
			t.Error("Read succeeded on a container that is not a node")
		}
		if r.ran("docker exec") {
			t.Errorf("ran in a container that is not a node: %q", r.calls)
		}
	})
}

func TestParseHumanSize(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]int64{
		"0B": 0, "197B": 197, "3.227kB": 3227, "1.049MB": 1049000, "243.3MB": 243300000, "1.002GB": 1002000000,
		"9.0MiB": 9 << 20,
	} {
		if got, err := parseHumanSize(in); err != nil || got != want {
			t.Errorf("parseHumanSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "MB", "12 parsecs", "1.2.3MB"} {
		if _, err := parseHumanSize(in); err == nil {
			t.Errorf("parseHumanSize(%q) succeeded", in)
		}
	}
}
