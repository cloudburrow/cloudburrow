# Console icon provenance

**No Google product or category icon ships with CloudBurrow.** Every mark the
console draws is CloudBurrow's own line drawing, authored in `console.js`
(`ICONS` for products, `CATEGORY_ICONS` for the navigation's categories).

## What was removed, and why

Removed on 2026-09-27 (issue #684).

Until then this directory held 20 of Google's published product icons and 9
of its product category icons, downloaded on 2026-09-22 from
<https://cloud.google.com/icons>. Neither archive held a licence or terms
file, and the icons page stated no redistribution terms. So the Apache-2.0
binary was redistributing Google artwork under no recorded terms, while
docs/console-parity.md §1 said no Google asset shipped and §2 said no Google
product logo was used.

The maintainer chose to remove the artwork rather than look for terms. The
navigation, the product catalogue and every other screen now draw the
CloudBurrow line marks that were already kept as the fallback. The category
*names* (Serverless computing, Containers, Databases and the rest) are still
Google's product taxonomy. A name describes the product being emulated; it is
not artwork.

## Third-party assets

Every file under `internal/console/assets` is CloudBurrow's own work unless
it is listed below. A third-party file may only be added with a row here that
names its source, its licence and the date that licence was checked.
`TestEveryEmbeddedAssetIsOwnOrLicensed` reads this table and fails on any
embedded file that is neither in its list of CloudBurrow's own files nor
recorded here.

| Path | Source | Licence | Checked |
|---|---|---|---|
| `vendor/xterm/xterm.js` | [xterm.js](https://github.com/xtermjs/xterm.js) `@xterm/xterm` 6.0.0, unmodified | MIT (`vendor/xterm/LICENSE`) | 2026-09-27 |
| `vendor/xterm/xterm.css` | [xterm.js](https://github.com/xtermjs/xterm.js) `@xterm/xterm` 6.0.0, unmodified | MIT (`vendor/xterm/LICENSE`) | 2026-09-27 |
| `vendor/xterm/LICENSE` | [xterm.js](https://github.com/xtermjs/xterm.js) `@xterm/xterm` 6.0.0, unmodified | MIT | 2026-09-27 |
| `vendor/xterm/addon-fit.js` | [xterm.js](https://github.com/xtermjs/xterm.js) `@xterm/addon-fit` 0.11.0, unmodified | MIT (`vendor/xterm/addon-fit.LICENSE`) | 2026-09-27 |
| `vendor/xterm/addon-fit.LICENSE` | [xterm.js](https://github.com/xtermjs/xterm.js) `@xterm/addon-fit` 0.11.0, unmodified | MIT | 2026-09-27 |

The table is empty: no third-party asset is embedded.
