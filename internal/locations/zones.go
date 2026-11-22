package locations

// zoneSuffixes are the zones of the regions whose zones are not -a, -b
// and -c, as Google's regions and zones page lists them.
var zoneSuffixes = map[string][]string{
	"us-central1":  {"a", "b", "c", "f"},
	"us-east1":     {"b", "c", "d"},
	"europe-west1": {"b", "c", "d"},
	"us-east4":     {"a", "b", "c"},
	"europe-west2": {"a", "b", "c"},
}

// Zones are a region's zones, as Compute Engine lists them: shared by
// Compute Engine and the services that place resources in zones without
// importing it (Cloud SQL).
func Zones(region string) []string {
	suf, ok := zoneSuffixes[region]
	if !ok {
		suf = []string{"a", "b", "c"}
	}
	out := make([]string, len(suf))
	for i, x := range suf {
		out[i] = region + "-" + x
	}
	return out
}
