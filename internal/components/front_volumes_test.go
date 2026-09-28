package components

import (
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
)

// podMounts reads a rendered Deployment's pod spec: the volume mounts of
// each container, as "volume:mountPath" in order, and the kind of each pod
// volume ("persistentVolumeClaim:<claim>" or "emptyDir"). It knows only the
// layout Backend.Manifest writes, which is what these tests pin.
func podMounts(t *testing.T, m string) (mounts map[string][]string, volumes map[string]string) {
	t.Helper()
	i := strings.Index(m, "kind: Deployment\n")
	if i < 0 {
		t.Fatalf("no Deployment:\n%s", m)
	}
	deploy, _, _ := strings.Cut(m[i:], "\n---\n")
	mounts = map[string][]string{}
	volumes = map[string]string{}
	const (
		inNone = iota
		inContainers
		inMounts
		inVolumes
	)
	state, container, volume := inNone, "", ""
	lines := strings.Split(deploy, "\n")
	for i, l := range lines {
		switch {
		case l == "      containers:":
			state = inContainers
		case l == "      volumes:":
			state = inVolumes
		case state != inNone && strings.HasPrefix(l, "        - name: "):
			name := strings.TrimPrefix(l, "        - name: ")
			if state == inVolumes {
				volume = name
				volumes[volume] = ""
			} else {
				container, state = name, inContainers
				mounts[container] = []string{}
			}
		case state != inNone && l == "          volumeMounts:":
			state = inMounts
		case state == inMounts && strings.HasPrefix(l, "            - name: "):
			v := strings.TrimPrefix(l, "            - name: ")
			p := ""
			if i+1 < len(lines) {
				p = strings.TrimPrefix(lines[i+1], "              mountPath: ")
			}
			mounts[container] = append(mounts[container], v+":"+p)
		case state == inMounts && strings.HasPrefix(l, "          ") && !strings.HasPrefix(l, "            "):
			state = inContainers
		case state == inVolumes && strings.HasPrefix(l, "          persistentVolumeClaim:"):
			volumes[volume] = "persistentVolumeClaim:" + strings.TrimSpace(strings.TrimPrefix(lines[i+1], "            claimName:"))
		case state == inVolumes && l == "          emptyDir: {}":
			volumes[volume] = "emptyDir"
		}
	}
	return mounts, volumes
}

// A persistent backend with a front (#915): its data volume is mounted in
// its own container only, and the front mounts only its own state, whether
// or not it has any. Nothing that ships is shaped like this yet; the
// renderer must not depend on that.
func TestPersistentBackendWithFrontMountsEachVolumeInItsOwnContainer(t *testing.T) {
	for _, tc := range []struct {
		name        string
		stateDir    string
		wantMounts  map[string][]string
		wantVolumes map[string]string
	}{
		{
			name:     "front with state",
			stateDir: "/var/lib/front",
			wantMounts: map[string][]string{
				"db":    {"data:/var/lib/db"},
				"front": {"front-state:/var/lib/front"},
			},
			wantVolumes: map[string]string{
				"data":        "persistentVolumeClaim:db-data",
				"front-state": "emptyDir",
			},
		},
		{
			name: "front without state",
			wantMounts: map[string][]string{
				"db":    {"data:/var/lib/db"},
				"front": {},
			},
			wantVolumes: map[string]string{"data": "persistentVolumeClaim:db-data"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := Backend{
				Name:       "db",
				Image:      "db:1",
				Port:       7000,
				Persistent: true,
				MountPath:  "/var/lib/db",
				Front: &Front{
					Name:         "front",
					Image:        "front:1",
					Args:         []string{"front"},
					UpstreamPort: 7001,
					StateDir:     tc.stateDir,
				},
			}
			m := b.Manifest("ns", "i")
			mounts, volumes := podMounts(t, m)
			if !reflect.DeepEqual(mounts, tc.wantMounts) {
				t.Errorf("mounts = %v, want %v:\n%s", mounts, tc.wantMounts, m)
			}
			if !reflect.DeepEqual(volumes, tc.wantVolumes) {
				t.Errorf("volumes = %v, want %v:\n%s", volumes, tc.wantVolumes, m)
			}
			if !strings.Contains(m, "kind: PersistentVolumeClaim\nmetadata:\n  name: db-data\n") {
				t.Errorf("no claim for the persistent backend:\n%s", m)
			}
		})
	}
}

// The Pub/Sub and BigQuery pods, which ship with a front (#873, #902), in
// both modes of an instance that also runs Cloud Storage: neither emulator
// is given a volume (neither is durable), the Pub/Sub front mounts only its
// emptyDir (#898), and the BigQuery front mounts nothing (#915); the
// BigQuery emulator's container mounts only the emptyDir its supervisor is
// put in (#1091).
func TestFrontPodsMountOnlyTheirOwnVolumes(t *testing.T) {
	want := map[string]struct {
		mounts  map[string][]string
		volumes map[string]string
	}{
		"pubsub": {
			mounts: map[string][]string{
				"pubsub": {},
				"front":  {"front-state:/var/lib/pubsub-front"},
			},
			volumes: map[string]string{"front-state": "emptyDir"},
		},
		"bigquery": {
			mounts:  map[string][]string{"bigquery": {"supervisor:/cloudburrow-supervisor"}, "front": {}},
			volumes: map[string]string{"supervisor": "emptyDir"},
		},
	}
	for _, mode := range []config.Mode{config.ModePersistent, config.ModeEphemeral} {
		var cfg config.Config
		cfg.Services = []config.Service{config.ServiceStorage, config.ServicePubSub, config.ServiceBigQuery}
		cfg.Mode = mode
		cfg.Cluster.Namespace = "cloudburrow"
		c := NewLifecycleComponent("kc", cfg, io.Discard)
		c.SetBuiltinStorageImage("dev.local/cloudburrow-storage:abc")
		seen := map[string]bool{}
		for _, b := range c.Backends() {
			w, ok := want[b.Name]
			if !ok {
				continue
			}
			seen[b.Name] = true
			m := b.Manifest("cloudburrow", "i")
			mounts, volumes := podMounts(t, m)
			if !reflect.DeepEqual(mounts, w.mounts) {
				t.Errorf("%s (%s): mounts = %v, want %v:\n%s", b.Name, mode, mounts, w.mounts, m)
			}
			if !reflect.DeepEqual(volumes, w.volumes) {
				t.Errorf("%s (%s): volumes = %v, want %v:\n%s", b.Name, mode, volumes, w.volumes, m)
			}
			if strings.Contains(m, "PersistentVolumeClaim") || strings.Contains(m, "persistentVolumeClaim") {
				t.Errorf("%s (%s): renders a claim:\n%s", b.Name, mode, m)
			}
		}
		for name := range want {
			if !seen[name] {
				t.Errorf("%s: no %s backend", mode, name)
			}
		}
	}
}

// The persistent storage server beside those pods keeps its data volume
// in its only container, so podMounts reads a backend without a front too.
func TestPodMountsReadsABackendWithoutAFront(t *testing.T) {
	b := BuiltinStorageBackend("cloudburrow", "img", true, true, nil, nil)
	mounts, volumes := podMounts(t, b.Manifest("cloudburrow", "i"))
	if want := map[string][]string{"storage": {"data:/data"}}; !reflect.DeepEqual(mounts, want) {
		t.Errorf("mounts = %v, want %v", mounts, want)
	}
	if want := map[string]string{"data": "persistentVolumeClaim:storage-data"}; !reflect.DeepEqual(volumes, want) {
		t.Errorf("volumes = %v, want %v", volumes, want)
	}
}
