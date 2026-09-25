package netfwd

import "testing"

const testSvc = `{"spec":{"selector":{"app":"cloudsql-mysql","tier":"db"},"ports":[{"port":3306,"targetPort":3306},{"port":80,"targetPort":"http"}]}}`

func pod(name, created string, deleting, ready bool) string {
	del := "null"
	if deleting {
		del = `"2026-09-25T00:00:30Z"`
	}
	status := "False"
	if ready {
		status = "True"
	}
	return `{"metadata":{"name":"` + name + `","creationTimestamp":"` + created + `","deletionTimestamp":` + del + `},
		"spec":{"containers":[{"ports":[{"name":"http","containerPort":8080}]}]},
		"status":{"conditions":[{"type":"Ready","status":"` + status + `"}]}}`
}

func list(pods ...string) []byte {
	out := `{"items":[`
	for i, p := range pods {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return []byte(out + "]}")
}

// During a rollout the old pod is still Ready while it terminates; the
// tunnel must bind the new one (#381).
func TestChoosePodSkipsATerminatingPodThatIsStillReady(t *testing.T) {
	pods := list(
		pod("old", "2026-09-25T00:00:00Z", true, true),
		pod("new", "2026-09-25T00:00:25Z", false, true),
	)
	name, port, ok := choosePod([]byte(testSvc), pods, 3306)
	if !ok || name != "new" || port != 3306 {
		t.Errorf("choosePod = %q %d %v, want the new pod on 3306", name, port, ok)
	}
}

func TestChoosePodPrefersTheNewestReadyPodAndSkipsUnready(t *testing.T) {
	pods := list(
		pod("older", "2026-09-25T00:00:00Z", false, true),
		pod("newest-unready", "2026-09-25T00:00:40Z", false, false),
		pod("newer", "2026-09-25T00:00:20Z", false, true),
	)
	if name, _, ok := choosePod([]byte(testSvc), pods, 3306); !ok || name != "newer" {
		t.Errorf("choosePod = %q %v, want the newest Ready pod", name, ok)
	}
}

// A named targetPort resolves through the pod's container ports.
func TestChoosePodResolvesANamedTargetPort(t *testing.T) {
	pods := list(pod("p", "2026-09-25T00:00:00Z", false, true))
	if _, port, ok := choosePod([]byte(testSvc), pods, 80); !ok || port != 8080 {
		t.Errorf("named targetPort resolved to %d %v, want 8080", port, ok)
	}
}

// With nothing usable the caller falls back to the Service, as before.
func TestChoosePodFallsBackWhenNoPodQualifies(t *testing.T) {
	for name, pods := range map[string][]byte{
		"only terminating": list(pod("old", "2026-09-25T00:00:00Z", true, true)),
		"none ready":       list(pod("p", "2026-09-25T00:00:00Z", false, false)),
		"empty":            list(),
		"not json":         []byte("nope"),
	} {
		if _, _, ok := choosePod([]byte(testSvc), pods, 3306); ok {
			t.Errorf("%s: choosePod found a pod", name)
		}
	}
}

func TestSelectorIsSortedAndEmptyWithoutOne(t *testing.T) {
	if s, err := selector([]byte(testSvc)); err != nil || s != "app=cloudsql-mysql,tier=db" {
		t.Errorf("selector = %q, %v", s, err)
	}
	if s, err := selector([]byte(`{"spec":{}}`)); err != nil || s != "" {
		t.Errorf("selector without one = %q, %v", s, err)
	}
}
