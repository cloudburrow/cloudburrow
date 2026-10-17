package main

import (
	"strconv"
	"strings"
	"testing"

	"github.com/cloudburrow/cloudburrow/internal/config"
	"github.com/cloudburrow/cloudburrow/internal/doctor"
)

// Every tunnel of every service, and every other fixed port the instance
// binds, gets a host port of its own at any port base, and each tunnel's
// port is one the preflight checks (#1526). Two forwards on one port would
// fail the second's kubectl port-forward after the preflight passed.
func TestEveryForwardGetsADistinctHostPort(t *testing.T) {
	var all []string
	for _, s := range config.KnownServices() {
		all = append(all, string(s))
	}
	services := strings.Join(all, ",")
	for _, base := range []int{0, config.DefaultPortBase + 100, 21000, 31900, 40200, config.MaxPortBase()} {
		args := []string{"--services", services}
		if base != 0 {
			args = append(args, "--port-base", strconv.Itoa(base))
		}
		cfg := loadForTest(t, args...)
		ports := hostPorts(cfg)

		owner := map[int]string{}
		for name, port := range ports {
			if other, dup := owner[port]; dup {
				t.Errorf("port base %d: %s and %s share host port %d", base, name, other, port)
			}
			owner[port] = name
		}

		forwards := map[int]string{}
		n := 0
		for _, s := range cfg.EnabledServices() {
			for _, f := range forwardTargets(cfg, s) {
				n++
				label := f.Name
				if f.Label != "" {
					label = f.Label
				}
				if f.HostPort <= 0 {
					t.Errorf("port base %d: forward %s has no fixed host port (%d)", base, label, f.HostPort)
					continue
				}
				if other, dup := forwards[f.HostPort]; dup {
					t.Errorf("port base %d: forwards %s and %s share host port %d", base, label, other, f.HostPort)
				}
				forwards[f.HostPort] = label
				if _, checked := owner[f.HostPort]; !checked {
					t.Errorf("port base %d: forward %s's host port %d is not in the preflight's set", base, label, f.HostPort)
				}
				if base != 0 && (f.HostPort < base || f.HostPort > base+65535-config.MaxPortBase()) {
					t.Errorf("port base %d: forward %s's host port %d is outside the base's block", base, label, f.HostPort)
				}
			}
		}
		// Storage, Pub/Sub, and every opt-in service with a backend in the
		// cluster, Spanner and BigQuery with a second tunnel each.
		if n < 10 {
			t.Errorf("port base %d: %d forwards, want every in-cluster service's", base, n)
		}
	}
}

func TestParsePortRange(t *testing.T) {
	for in, want := range map[string]portRange{
		"32768\t60999\n": {32768, 60999},
		"49152\n65535\n": {49152, 65535},
	} {
		if got, ok := parsePortRange(in); !ok || got != want {
			t.Errorf("parsePortRange(%q) = %v, %v; want %v", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "32768", "a b", "60999 32768", "0 100", "1 70000"} {
		if _, ok := parsePortRange(in); ok {
			t.Errorf("parsePortRange(%q) ok, want refused", in)
		}
	}
}

// At --port-base 40200, the base #1526's run drew, every fixed port is in
// Linux's ephemeral range, and doctor says so with a base that is not; at
// 21000 nothing is.
func TestEphemeralPortsResult(t *testing.T) {
	saved := ephemeralPortRange
	t.Cleanup(func() { ephemeralPortRange = saved })
	ephemeralPortRange = func() (portRange, bool) { return portRange{32768, 60999}, true }

	r := ephemeralPortsResult(loadForTest(t, "--port-base", "40200", "--services", "storage,pubsub,bigtable"))
	if r.Level != doctor.LevelWarn {
		t.Fatalf("level = %v, want warn: %+v", r.Level, r)
	}
	for _, want := range []string{"bigtable 40212", "32768–60999"} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("detail = %q, want it to contain %q", r.Detail, want)
		}
	}
	if !strings.Contains(r.Remedy, "below 32768") {
		t.Errorf("remedy = %q, want a base below the range", r.Remedy)
	}

	if r := ephemeralPortsResult(loadForTest(t, "--port-base", "21000", "--services", "storage,pubsub,bigtable")); r.Level != doctor.LevelOK {
		t.Errorf("port base 21000: %+v, want ok", r)
	}
	// macOS's range starts at 49152, so the defaults are clear of both.
	if r := ephemeralPortsResult(loadForTest(t)); r.Level != doctor.LevelOK {
		t.Errorf("defaults: %+v, want ok", r)
	}

	ephemeralPortRange = func() (portRange, bool) { return portRange{}, false }
	if r := ephemeralPortsResult(loadForTest(t, "--port-base", "40200")); r.Level != doctor.LevelOK {
		t.Errorf("unreadable range: %+v, want ok (not checked)", r)
	}
}
