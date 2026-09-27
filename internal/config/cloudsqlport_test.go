package config

import "testing"

// Cloud SQL for PostgreSQL has a fixed default port, like MySQL's (#584), so
// `cloudburrow env` can export PGPORT without a running instance.
func TestCloudSQLPortDefault(t *testing.T) {
	t.Parallel()
	d := Default().Endpoints
	if d.CloudSQL != 9019 {
		t.Errorf("default CloudSQL port = %d, want 9019", d.CloudSQL)
	}
	if got := d.OptionalPort(ServiceCloudSQL); got != 9019 {
		t.Errorf("OptionalPort(cloudsql) = %d, want 9019", got)
	}
	cfg := Default()
	cfg.Services = []Service{ServiceCloudSQL, ServiceCloudSQLMySQL}
	if err := cfg.Validate(); err != nil {
		t.Errorf("the default ports with cloudsql do not validate: %v", err)
	}
}

// --port-base moves it with every other fixed default.
func TestPortBaseMovesTheCloudSQLPort(t *testing.T) {
	t.Parallel()
	cfg, err := Load(Options{Args: []string{"--port-base", "9100"}, Getenv: envMap(nil), WorkDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Endpoints.CloudSQL != 9119 {
		t.Errorf("CloudSQL at --port-base 9100 = %d, want 9119", cfg.Endpoints.CloudSQL)
	}
}

// The flag, the environment variable and the file key each set it, with the
// usual precedence, and 0 still means OS-assigned.
func TestCloudSQLPortSources(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := writeFile(t, dir, "cb.json", `{"endpoints":{"cloudsql":9801}}`)
	for _, c := range []struct {
		name string
		args []string
		env  map[string]string
		want int
	}{
		{"file", nil, map[string]string{EnvPrefix + "CONFIG": file}, 9801},
		{"env over file", nil, map[string]string{EnvPrefix + "CONFIG": file, EnvPrefix + "PORT_CLOUDSQL": "9802"}, 9802},
		{"flag over env", []string{"--port-cloudsql", "9803"}, map[string]string{EnvPrefix + "PORT_CLOUDSQL": "9802"}, 9803},
		{"flag 0", []string{"--port-cloudsql", "0"}, nil, 0},
		{"explicit, with a base", []string{"--port-base", "9300", "--port-cloudsql", "9804"}, nil, 9804},
	} {
		cfg, err := Load(Options{Args: c.args, Getenv: envMap(c.env), WorkDir: dir})
		if err != nil {
			t.Fatalf("%s: Load: %v", c.name, err)
		}
		if cfg.Endpoints.CloudSQL != c.want {
			t.Errorf("%s: CloudSQL = %d, want %d", c.name, cfg.Endpoints.CloudSQL, c.want)
		}
	}
}
