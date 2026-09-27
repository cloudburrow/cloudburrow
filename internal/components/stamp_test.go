package components

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/cloudburrow/cloudburrow/internal/cluster"
	"github.com/cloudburrow/cloudburrow/internal/config"
)

// stampedWith answers as a cluster with Knative installed whose stamp
// records knative; "" is a cluster with no stamp at all.
func stampedWith(knative string) func(string) (string, error) {
	return func(call string) (string, error) {
		if strings.Contains(call, "get configmap "+cluster.StampName) {
			if knative == "" {
				return "", nil
			}
			if strings.Contains(call, "-o name") {
				return "configmap/" + cluster.StampName, nil
			}
			return fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, cluster.AnnotationKnativeVersion, knative), nil
		}
		return "", nil
	}
}

// applied reports whether a manifest was applied: since #597 its bytes go
// on stdin, so it is found there.
func (r *recordingRunner) applied(name string) bool {
	for i, c := range r.calls {
		if strings.Contains(c, "apply -f -") && strings.Contains(r.stdins[i], name) {
			return true
		}
	}
	return false
}

func runComponent(t *testing.T, r *recordingRunner) (string, error) {
	t.Helper()
	var out strings.Builder
	c := &LifecycleComponent{installer: newTestInstaller(r), services: []config.Service{config.ServiceRun},
		timeout: time.Minute, out: &out}
	err := c.Start(context.Background())
	return out.String(), err
}

// A cluster whose Knative is older than the pin, or whose release was never
// recorded, gets the pinned manifests applied in place and the stamp
// updated (#601). It used to be left as it was, so a Knative bump never
// reached an existing instance.
func TestAnOlderOrUnrecordedKnativeIsUpgradedInPlace(t *testing.T) {
	t.Parallel()
	for _, stamped := range []string{"knative-v1.22.0", ""} {
		t.Run("stamped "+stamped, func(t *testing.T) {
			t.Parallel()
			r := &recordingRunner{respond: stampedWith(stamped)}
			out, err := runComponent(t, r)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range fakeManifests {
				if !r.applied(m.Name) {
					t.Errorf("%s was not applied:\n%s", m.Name, strings.Join(r.calls, "\n"))
				}
			}
			want := "annotate --overwrite configmap/" + cluster.StampName + " " + cluster.AnnotationKnativeVersion + "=" + KnativeVersion
			if r.find(want) == "" {
				t.Errorf("the stamp was not updated to %s:\n%s", KnativeVersion, strings.Join(r.calls, "\n"))
			}
			if !strings.Contains(out, "applying "+KnativeVersion+" in place") {
				t.Errorf("the upgrade was not reported:\n%s", out)
			}
			// Knative's webhooks validate serving-core's ConfigMaps, so it is
			// waited for before the apply, as after stop and up.
			wait, apply := -1, -1
			for i, c := range r.calls {
				if wait < 0 && strings.Contains(c, "-n knative-serving wait") {
					wait = i
				}
				if apply < 0 && strings.Contains(c, "apply -f -") && strings.Contains(r.stdins[i], "serving-crds.yaml") {
					apply = i
				}
			}
			if wait < 0 || wait > apply {
				t.Errorf("wait at %d, first apply at %d; the wait must come first", wait, apply)
			}
		})
	}
}

// A newer Knative than the pin is not downgraded: Knative does not support
// it, and the error says what to do instead.
func TestANewerKnativeIsRefused(t *testing.T) {
	t.Parallel()
	r := &recordingRunner{respond: stampedWith("knative-v9.0.0")}
	_, err := runComponent(t, r)
	if !errors.Is(err, cluster.ErrKnativeDowngrade) || !strings.Contains(err.Error(), "cloudburrow delete") {
		t.Fatalf("err = %v; want ErrKnativeDowngrade naming `cloudburrow delete`", err)
	}
	if r.applied("serving-core.yaml") {
		t.Error("a newer Knative was overwritten with the pinned one")
	}
}

// A fresh install stamps the release it installed.
func TestAFreshKnativeInstallIsStamped(t *testing.T) {
	t.Parallel()
	r := &recordingRunner{respond: func(call string) (string, error) {
		if strings.Contains(call, "get namespace knative-serving") {
			return "", errors.New(`namespaces "knative-serving" not found`)
		}
		return "", nil
	}}
	if _, err := runComponent(t, r); err != nil {
		t.Fatal(err)
	}
	if !r.applied("serving-core.yaml") {
		t.Error("Knative was not installed")
	}
	for _, want := range []string{
		"-n kube-system create configmap " + cluster.StampName,
		"label --overwrite configmap/" + cluster.StampName + " cloudburrow.dev/owned=true",
		cluster.AnnotationKnativeVersion + "=" + KnativeVersion,
	} {
		if r.find(want) == "" {
			t.Errorf("no call contains %q:\n%s", want, strings.Join(r.calls, "\n"))
		}
	}
}

// An instance without Cloud Run neither reads nor writes the Knative stamp.
func TestNoKnativeNoStamp(t *testing.T) {
	t.Parallel()
	r := &recordingRunner{}
	c := &LifecycleComponent{installer: newTestInstaller(r), services: []config.Service{config.ServicePubSub},
		timeout: time.Minute, out: io.Discard}
	if err := c.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := r.find(cluster.StampName); got != "" {
		t.Errorf("the stamp was touched without Knative: %s", got)
	}
}
