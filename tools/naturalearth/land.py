# Writes internal/console/assets/vendor/naturalearth/land-110m.json, the base
# map of BigQuery Studio's Map visualization (#2094), from Natural Earth's
# public-domain ne_110m_land.geojson (third_party/naturalearth/PROVENANCE.md):
# each polygon's rings as [longitude, latitude] pairs rounded to 0.1 degree.
#
#   python3 -I tools/naturalearth/land.py ne_110m_land.geojson internal/console/assets/vendor/naturalearth/land-110m.json
import json
import sys

src = json.load(open(sys.argv[1], encoding="utf-8"))
rings = []
for feature in src["features"]:
    geom = feature["geometry"]
    polys = geom["coordinates"] if geom["type"] == "MultiPolygon" else [geom["coordinates"]]
    for poly in polys:
        for ring in poly:
            pts = []
            for lon, lat in ring:
                p = [round(lon, 1), round(lat, 1)]
                if not pts or pts[-1] != p:
                    pts.append(p)
            if len(pts) >= 4:
                rings.append(pts)
with open(sys.argv[2], "w", encoding="utf-8") as out:
    json.dump({"source": "Natural Earth ne_110m_land v5.1.2 (public domain)", "rings": rings}, out, separators=(",", ":"))
    out.write("\n")
