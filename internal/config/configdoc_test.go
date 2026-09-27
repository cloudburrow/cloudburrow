package config

import (
	"encoding/json"
	"flag"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// configurationDoc is the reference every `up` flag and CLOUDBURROW_*
// variable must have a row in (#710).
const configurationDoc = "../../docs/configuration.md"

// refusedEnv are variables Load reads only to refuse them. They have no
// row in the Settings table; the doc names them as refused instead.
var refusedEnv = map[string]bool{
	EnvPrefix + "STORAGE_BACKEND": true, // #519
}

// settingsRow is one row of the Settings table in docs/configuration.md.
type settingsRow struct {
	flag, env, fileKey, def string
}

// settingsTable returns the Settings table's rows keyed by flag name
// (without the dashes), and the whole doc.
func settingsTable(t *testing.T) (map[string]settingsRow, string) {
	t.Helper()
	b, err := os.ReadFile(configurationDoc)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	start := strings.Index(doc, "\n## Settings\n")
	if start < 0 {
		t.Fatalf("%s: no \"## Settings\" section", configurationDoc)
	}
	section := doc[start+1:]
	if end := strings.Index(section, "\n#"); end >= 0 {
		section = section[:end]
	}
	rows := map[string]settingsRow{}
	for _, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "| `--") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) != 5 {
			t.Errorf("%s: row has %d cells, want 5 (flag, environment, file key, default, meaning): %s", configurationDoc, len(cells), line)
			continue
		}
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		name := strings.TrimSuffix(strings.TrimPrefix(cells[0], "`--"), "`")
		if _, dup := rows[name]; dup {
			t.Errorf("%s: --%s has two rows", configurationDoc, name)
		}
		rows[name] = settingsRow{flag: cells[0], env: cells[1], fileKey: cells[2], def: cells[3]}
	}
	return rows, doc
}

// envRead returns every CLOUDBURROW_* variable Load reads. Load reads each
// one unconditionally when it is unset, so recording the lookups of a run
// with an empty environment is the list, with no table to keep in step.
func envRead(t *testing.T) []string {
	t.Helper()
	var mu sync.Mutex
	seen := map[string]bool{}
	getenv := func(k string) string {
		mu.Lock()
		defer mu.Unlock()
		if strings.HasPrefix(k, EnvPrefix) {
			seen[k] = true
		}
		return ""
	}
	// The result does not matter: every lookup happens before validation.
	_, _ = Load(Options{WorkDir: t.TempDir(), Getenv: getenv})
	var out []string
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) == 0 {
		t.Fatal("Load read no CLOUDBURROW_* variable; the recorder is not seeing its lookups")
	}
	return out
}

// Every flag config.Load parses and every CLOUDBURROW_* variable it reads
// has a row in docs/configuration.md's Settings table giving its flag,
// environment variable, file key and default (#710). A flag or variable
// added without its row fails here.
func TestConfigurationDocCoversEveryFlagAndVariable(t *testing.T) {
	rows, doc := settingsTable(t)

	fs, _ := newFlagSet(nil)
	fs.VisitAll(func(f *flag.Flag) {
		row, ok := rows[f.Name]
		if !ok {
			t.Errorf("--%s has no row in the Settings table of %s", f.Name, configurationDoc)
			return
		}
		for _, c := range []struct{ name, v string }{{"environment", row.env}, {"file key", row.fileKey}, {"default", row.def}} {
			if c.v == "" {
				t.Errorf("--%s: the %s cell is empty; write — when there is none", f.Name, c.name)
			}
		}
	})

	documented := map[string]string{} // variable -> flag row naming it
	for name, row := range rows {
		if row.env == "—" {
			continue
		}
		v := strings.Trim(row.env, "`")
		if !strings.HasPrefix(v, EnvPrefix) || strings.ContainsAny(v, " `,") {
			t.Errorf("--%s: environment cell %q is not one `CLOUDBURROW_*` variable or —", name, row.env)
			continue
		}
		documented[v] = name
	}
	read := map[string]bool{}
	for _, v := range envRead(t) {
		read[v] = true
		if refusedEnv[v] {
			if !strings.Contains(doc, "`"+v+"`") {
				t.Errorf("%s is refused but %s does not name it", v, configurationDoc)
			}
			if _, ok := documented[v]; ok {
				t.Errorf("%s is refused but has a row as a setting", v)
			}
			continue
		}
		if _, ok := documented[v]; !ok {
			t.Errorf("%s is read by config.Load but no row of the Settings table in %s names it", v, configurationDoc)
		}
	}
	// A row naming a variable nothing reads sends a user to a setting
	// that is silently ignored.
	for v, name := range documented {
		if !read[v] {
			t.Errorf("--%s's row names %s, which config.Load does not read", name, v)
		}
	}
}

// Each documented file key is one the config file accepts, and each fixed
// port's documented default is Default()'s.
func TestConfigurationDocFileKeysAndPortDefaults(t *testing.T) {
	rows, _ := settingsTable(t)

	ports := map[string]int{}
	b, err := json.Marshal(Default().Endpoints)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &ports); err != nil {
		t.Fatal(err)
	}

	for name, row := range rows {
		if row.fileKey == "—" {
			continue
		}
		key := strings.Trim(row.fileKey, "`")
		// The value is null, which a field's own decoder may reject (a
		// Duration does); only an unknown key is the doc's mistake.
		parts := strings.Split(key, ".")
		doc := "null"
		for i := len(parts) - 1; i >= 0; i-- {
			doc = `{"` + parts[i] + `":` + doc + `}`
		}
		dec := json.NewDecoder(strings.NewReader(doc))
		dec.DisallowUnknownFields()
		cfg := Default()
		if err := dec.Decode(&cfg); err != nil && strings.Contains(err.Error(), "unknown field") {
			t.Errorf("--%s: file key %s is not accepted by the config file: %v", name, key, err)
		}

		if port, ok := strings.CutPrefix(key, "endpoints."); ok {
			want, known := ports[port]
			if !known {
				t.Errorf("--%s: %s is not an endpoint", name, key)
				continue
			}
			if want == 0 {
				continue // OS-assigned by default; the cell explains it
			}
			if got := strings.Trim(row.def, "`"); got != strconv.Itoa(want) {
				t.Errorf("--%s: documented default %s, Default() has %d", name, row.def, want)
			}
		}
	}
}
