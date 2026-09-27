package config

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestPortBaseMovesEveryFixedDefault is the point of --port-base (#584): a
// second instance needs every fixed port moved, not the three the old
// recipe zeroed. Every Endpoints field moves by the same distance, except
// LocalAI, which is OS-assigned by default and stays so.
func TestPortBaseMovesEveryFixedDefault(t *testing.T) {
	t.Parallel()
	cfg, err := Load(Options{Args: []string{"--port-base", "9100"}, Getenv: envMap(nil), WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PortBase != 9100 {
		t.Errorf("PortBase = %d, want 9100", cfg.PortBase)
	}
	defaults := Default().Endpoints.Named()
	got := cfg.Endpoints.Named()
	for i, d := range defaults {
		want := d.Port + 100
		if d.Port == 0 {
			want = 0
		}
		if got[i].Port != want {
			t.Errorf("endpoints.%s = %d, want %d (default %d moved by 100)", d.Name, got[i].Port, want, d.Port)
		}
	}
	if cfg.Endpoints.Ingress != 9180 || cfg.Endpoints.Console != 9190 {
		t.Errorf("ingress, console = %d, %d; want 9180, 9190", cfg.Endpoints.Ingress, cfg.Endpoints.Console)
	}
	if cfg.Endpoints.LocalAI != 0 {
		t.Errorf("localAI = %d, want 0: an OS-assigned port stays OS-assigned", cfg.Endpoints.LocalAI)
	}
}

// An explicit port is where the user put it, whichever source set it; the
// base moves only what nobody chose.
func TestPortBaseKeepsExplicitPorts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := writeFile(t, dir, "cb.json", `{"portBase":9200,"endpoints":{"control":9700,"ingress":0}}`)

	cfg, err := Load(Options{
		Args: []string{"--port-storage", "9701"},
		Getenv: envMap(map[string]string{
			EnvPrefix + "CONFIG":   file,
			EnvPrefix + "PORT_RUN": "9702",
		}),
		WorkDir: dir,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, c := range []struct {
		name      string
		got, want int
	}{
		{"control (file)", cfg.Endpoints.Control, 9700},
		{"storage (flag)", cfg.Endpoints.Storage, 9701},
		{"run (env)", cfg.Endpoints.Run, 9702},
		{"ingress (file, 0)", cfg.Endpoints.Ingress, 0},
		{"pubsub (moved)", cfg.Endpoints.PubSub, 9202},
		{"console (moved)", cfg.Endpoints.Console, 9290},
	} {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

// The base follows the one precedence every setting does: flag, then
// environment, then file.
func TestPortBasePrecedence(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := writeFile(t, dir, "cb.json", `{"portBase":9200}`)
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want int
	}{
		{"unset", nil, nil, 0},
		{"file", nil, map[string]string{EnvPrefix + "CONFIG": file}, 9200},
		{"env beats file", nil, map[string]string{EnvPrefix + "CONFIG": file, EnvPrefix + "PORT_BASE": "9300"}, 9300},
		{"flag beats env", []string{"--port-base", "9400"}, map[string]string{EnvPrefix + "CONFIG": file, EnvPrefix + "PORT_BASE": "9300"}, 9400},
		{"flag 0 restores the defaults", []string{"--port-base", "0"}, map[string]string{EnvPrefix + "PORT_BASE": "9300"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := Load(Options{Args: tt.args, Getenv: envMap(tt.env), WorkDir: t.TempDir()})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.PortBase != tt.want {
				t.Errorf("PortBase = %d, want %d", cfg.PortBase, tt.want)
			}
			wantControl := tt.want
			if wantControl == 0 {
				wantControl = DefaultPortBase
			}
			if cfg.Endpoints.Control != wantControl {
				t.Errorf("control = %d, want %d", cfg.Endpoints.Control, wantControl)
			}
		})
	}
}

// The default base is the layout the defaults already have.
func TestPortBaseAtTheDefaultChangesNothing(t *testing.T) {
	t.Parallel()
	cfg, err := Load(Options{Args: []string{"--port-base", fmt.Sprint(DefaultPortBase)}, Getenv: envMap(nil), WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Endpoints != Default().Endpoints {
		t.Errorf("endpoints = %+v, want the defaults", cfg.Endpoints)
	}
}

// A base that would push a port past 65535, or into the privileged range, is
// refused once, by name, instead of as a list of endpoints nobody set.
func TestPortBaseValidation(t *testing.T) {
	t.Parallel()
	maxBase := MaxPortBase()
	if maxBase != 65535-90 {
		t.Errorf("MaxPortBase() = %d, want %d: the console, base+90, is the highest default", maxBase, 65535-90)
	}
	tests := []struct {
		name    string
		base    string
		wantErr bool
	}{
		{"lowest", fmt.Sprint(MinPortBase), false},
		{"highest", fmt.Sprint(maxBase), false},
		{"console would overflow", fmt.Sprint(maxBase + 1), true},
		{"far past the range", "70000", true},
		{"privileged", "80", true},
		{"negative", "-100", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := Load(Options{Args: []string{"--port-base", tt.base}, Getenv: envMap(nil), WorkDir: t.TempDir()})
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("Load: %v", err)
				}
				if cfg.Endpoints.Console > 65535 {
					t.Errorf("console = %d, past the port range", cfg.Endpoints.Console)
				}
				return
			}
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("Load error = %v, want a *ValidationError", err)
			}
			if len(verr.Problems) != 1 || verr.Problems[0].Field != "portBase" {
				t.Errorf("problems = %v, want exactly one, naming portBase", verr.Problems)
			}
			if !strings.Contains(err.Error(), "65536") {
				t.Errorf("error = %v, want it to say why", err)
			}
		})
	}
}

func TestPortBaseEnvMustBeAnInteger(t *testing.T) {
	t.Parallel()
	_, err := Load(Options{Getenv: envMap(map[string]string{EnvPrefix + "PORT_BASE": "high"}), WorkDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "PORT_BASE") {
		t.Errorf("Load error = %v, want one naming %sPORT_BASE", err, EnvPrefix)
	}
}

// An explicit port that lands on a moved default is a collision, and the
// error names both endpoints and says the base is why.
func TestPortBaseCollisionWithAnExplicitPortIsNamed(t *testing.T) {
	t.Parallel()
	_, err := Load(Options{
		Args:    []string{"--port-base", "9100", "--port-storage", "9102"},
		Getenv:  envMap(nil),
		WorkDir: t.TempDir(),
	})
	if err == nil {
		t.Fatal("Load = nil, want a duplicate-port error")
	}
	msg := err.Error()
	for _, want := range []string{"endpoints.storage", "endpoints.pubsub", "9102", "portBase 9100"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error = %q, want it to contain %q", msg, want)
		}
	}
}
