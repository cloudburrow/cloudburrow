# Natural Earth: the base map of BigQuery Studio's Map visualization

BigQuery Studio draws a GEOGRAPHY result's Map over a Google Maps base map (#2094), which a local
instance does not reach. CloudBurrow's Map draws its features over Natural Earth's 1:110m land
instead, which is in the public domain (`LICENSE.md`, copied from the tag below).

| Input | Version | Source (official) | SHA-256 | Licence |
|---|---|---|---|---|
| `geojson/ne_110m_land.geojson` | v5.1.2 | https://github.com/nvkelso/natural-earth-vector/blob/v5.1.2/geojson/ne_110m_land.geojson | `9e0729ee253ca7d7a5c4ae9395fb1902264c5377c52e224d13dd85010e2835d9` | Public domain (`LICENSE.md`) |

Only data derived from it is in the repository: `internal/console/assets/vendor/naturalearth/land-110m.json`,
each polygon's rings with coordinates rounded to 0.1 degree, written by CloudBurrow's own
`tools/naturalearth/land.py`:

    python3 -I tools/naturalearth/land.py ne_110m_land.geojson internal/console/assets/vendor/naturalearth/land-110m.json

The output's SHA-256 is `dcb8f027bfd0c5ea3b1f68c079c6b5fd45da2e1022050edfa514c1e5d89df7a7`.
