"""Generate BigQuery ML's holiday_region calendar (#1776).

Google publishes its ARIMA_PLUS holiday calendar only as a BigQuery public
dataset, which CloudBurrow must not query, so the built-in holidays come
from the MIT-licensed python-holidays package, pinned by SHA-256 in
dependencies.json (components.bigqueryml.pythonHolidays). UNVERIFIED (docs re-read 2026-11-18): the
holidays, their English names and the continental groups are not Google's.

Run with the pinned package installed (see tools/bqholidays/README in the
dependencies.json entry):

    python -I tools/bqholidays/generate.py internal/bigqueryfront/holidaycal.tsv.gz

Output: gzip of lines "region<TAB>holiday_name<TAB>YYYY-MM-DD", sorted.
"""

import collections
import datetime
import gzip
import re
import sys
import unicodedata

import holidays

FIRST, LAST = 2000, 2035

# The countries BigQuery ML documents for HOLIDAY_REGION.
COUNTRIES = (
    "AE AR AT AU BE BR CA CH CL CN CO CZ DE DK DZ EC EE EG ES FI FR GB GR HK "
    "HU ID IE IL IN IR IT JP KR LV MA MX MY NG NL NO NZ PE PH PK PL PT RO RS "
    "RU SA SE SG SI SK TH TR TW UA US VE VN ZA"
).split()

# UNVERIFIED (docs re-read 2026-11-18): Google does not document which countries each
# continental region covers, nor how its holidays are chosen. Here a
# continental region (and GLOBAL, over every country) has a holiday on a
# date when at least half of its countries do, named by the most common
# name among them.
GROUPS = {
    "NA": "US CA".split(),
    "LAC": "AR BR CL CO EC MX PE VE".split(),
    "JAPAC": "AU CN HK ID IN JP KR MY NZ PH PK SG TH TW VN".split(),
}
GROUPS["EMEA"] = [c for c in COUNTRIES if not any(c in g for g in GROUPS.values())]
GROUPS["GLOBAL"] = list(COUNTRIES)


def column_name(name):
    """A holiday's English name as BigQuery ML's CamelCase holiday_name."""
    name = re.sub(r"\(.*?\)", " ", name)
    name = name.replace("'s", "s").replace("'", "")
    name = unicodedata.normalize("NFKD", name).encode("ascii", "ignore").decode()
    words = re.findall(r"[A-Za-z0-9]+", name)
    out = "".join(w[:1].upper() + w[1:] for w in words)
    if out and out[0].isdigit():
        out = "H" + out
    return out[:100]


def country(code):
    years = range(FIRST, LAST + 1)
    try:
        cal = holidays.country_holidays(code, years=years, language="en_US")
    except (NotImplementedError, KeyError):
        cal = holidays.country_holidays(code, years=years)
    out = set()
    for day, names in cal.items():
        for name in names.split("; "):
            if "observed" in name.lower():
                continue  # a substitute day off; the holiday is on its date
            n = column_name(name)
            if n and n != "Sunday":
                out.add((n, day.isoformat()))
    return out


def merge(rows):
    """One occurrence per run of a holiday's days: a holiday's dates within
    2 days of its previous kept date (a multi-day festival, an observed
    bridge day) fold into that date, as BigQuery ML's holiday windows
    (1 day on each side here) must not overlap. UNVERIFIED (docs re-read 2026-11-18)."""
    out = set()
    by = collections.defaultdict(list)
    for r, n, d in rows:
        by[(r, n)].append(datetime.date.fromisoformat(d))
    for (r, n), days in by.items():
        last = None
        for d in sorted(days):
            if last is None or (d - last).days > 2:
                out.add((r, n, d.isoformat()))
                last = d
    return out


def main(path):
    rows = set()
    per = {}
    for code in COUNTRIES:
        per[code] = country(code)
        rows |= {(code, n, d) for n, d in per[code]}
    for group, members in GROUPS.items():
        by_day = collections.defaultdict(lambda: collections.defaultdict(set))
        for code in members:
            for n, d in per[code]:
                by_day[d][n].add(code)
        for d, names in by_day.items():
            observing = set().union(*names.values())
            if 2 * len(observing) >= len(members):
                n = sorted(names, key=lambda k: (-len(names[k]), k))[0]
                rows.add((group, n, d))
    rows = merge(rows)
    with gzip.GzipFile(path, "wb", mtime=0) as f:
        for r in sorted(rows):
            f.write(("\t".join(r) + "\n").encode())


if __name__ == "__main__":
    main(sys.argv[1])
