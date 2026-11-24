package byoc

import (
	"bufio"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The wizard's region defaults to the closest one (#2209). Where the user
// is comes from this machine's time zone and the coordinates the tz
// database gives it (zone1970.tab), so nothing is asked of the network.

// LocalZone is this machine's IANA time zone name: $TZ, else the
// /etc/localtime link's target.
func LocalZone() string {
	if tz := strings.TrimPrefix(os.Getenv("TZ"), ":"); tz != "" && !strings.HasPrefix(tz, "/") {
		return tz
	}
	target, err := os.Readlink("/etc/localtime")
	if err != nil {
		return ""
	}
	if i := strings.Index(target, "zoneinfo/"); i >= 0 {
		return target[i+len("zoneinfo/"):]
	}
	return ""
}

// zoneTabs are where the tz database's zone table usually is.
var zoneTabs = []string{"/usr/share/zoneinfo/zone1970.tab", "/usr/share/zoneinfo/zone.tab", "/var/db/timezone/zoneinfo/zone1970.tab"}

// ZoneCoordinates is the latitude and longitude of zone's principal city.
func ZoneCoordinates(zone string) (lat, lon float64, ok bool) {
	if zone == "" {
		return 0, 0, false
	}
	for _, p := range zoneTabs {
		f, err := os.Open(filepath.Clean(p))
		if err != nil {
			continue
		}
		lat, lon, ok = findZone(bufio.NewScanner(f), zone)
		f.Close()
		if ok {
			return lat, lon, true
		}
	}
	return 0, 0, false
}

func findZone(sc *bufio.Scanner, zone string) (float64, float64, bool) {
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 3 || f[2] != zone {
			continue
		}
		return parseISO6709(f[1])
	}
	return 0, 0, false
}

// parseISO6709 reads zone.tab's ±DDMM±DDDMM or ±DDMMSS±DDDMMSS.
func parseISO6709(s string) (float64, float64, bool) {
	i := strings.IndexAny(s[1:], "+-")
	if i < 0 {
		return 0, 0, false
	}
	lat, ok1 := dms(s[:i+1], 2)
	lon, ok2 := dms(s[i+1:], 3)
	return lat, lon, ok1 && ok2
}

func dms(s string, degDigits int) (float64, bool) {
	if len(s) < 1+degDigits+2 {
		return 0, false
	}
	sign := 1.0
	if s[0] == '-' {
		sign = -1
	}
	d := s[1:]
	deg, err1 := strconv.Atoi(d[:degDigits])
	min, err2 := strconv.Atoi(d[degDigits : degDigits+2])
	sec := 0
	var err3 error
	if len(d) >= degDigits+4 {
		sec, err3 = strconv.Atoi(d[degDigits+2 : degDigits+4])
	}
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, false
	}
	return sign * (float64(deg) + float64(min)/60 + float64(sec)/3600), true
}

// ClosestRegion is the region nearest (lat, lon), or regions[0] when no
// region has coordinates. It is "" for no regions.
func ClosestRegion(regions []Region, lat, lon float64) string {
	best, bestD := "", math.Inf(1)
	for _, r := range regions {
		if r.Latitude == 0 && r.Longitude == 0 {
			continue
		}
		if d := haversine(lat, lon, r.Latitude, r.Longitude); d < bestD {
			best, bestD = r.Name, d
		}
	}
	if best == "" && len(regions) > 0 {
		return regions[0].Name
	}
	return best
}

func haversine(lat1, lon1, lat2, lon2 float64) float64 {
	const rad = math.Pi / 180
	dlat, dlon := (lat2-lat1)*rad, (lon2-lon1)*rad
	a := math.Sin(dlat/2)*math.Sin(dlat/2) + math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dlon/2)*math.Sin(dlon/2)
	return 2 * 6371 * math.Asin(math.Sqrt(a))
}
