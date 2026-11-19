# python-holidays: the calendar of BigQuery ML's HOLIDAY_REGION

BigQuery ML's ARIMA_PLUS and ARIMA_PLUS_XREG models take `HOLIDAY_REGION` (#1776). Google
publishes its holiday calendar only as the BigQuery public dataset
`bigquery-public-data.ml_datasets.holidays_and_events_for_forecasting`, which CloudBurrow must not
query, and publishes no openly licensed copy. CloudBurrow's built-in calendar is therefore
generated from the MIT-licensed `holidays` package (https://github.com/vacanza/holidays), and is
an UNVERIFIED stand-in for Google's: the holidays, their names and the continental groups are
not Google's.

| Input | Version | Source (official) | SHA-256 | Licence |
|---|---|---|---|---|
| `holidays-0.84-py3-none-any.whl` | 0.84 | https://pypi.org/project/holidays/0.84/ (`pip download --no-deps --only-binary=:all: holidays==0.84`) | `bca376e3becb36ea8e370d08268520934d8a9bd897ae5007ba51b3d7b4e34988` | MIT (`LICENSE`, copied here from the wheel's `holidays-0.84.dist-info/licenses/LICENSE`) |

Only data derived from the package is in the repository, no code of it:
`internal/bigqueryfront/holidaycal.tsv.gz`, the output of `tools/bqholidays/generate.py`
(CloudBurrow's own script) run on the wheel above with its dependency `python-dateutil`
(Apache-2.0/BSD-3-Clause, used only to run the generator):

    python3 -m venv <scratch>/venv
    <scratch>/venv/bin/pip install holidays-0.84-py3-none-any.whl
    <scratch>/venv/bin/python -I tools/bqholidays/generate.py internal/bigqueryfront/holidaycal.tsv.gz

The decompressed calendar (27,518 lines, `region<TAB>holiday_name<TAB>YYYY-MM-DD`, 2000 through
2035, the 63 documented countries plus GLOBAL, NA, JAPAC, EMEA and LAC) has SHA-256
`76dffb88d42ff68d6293f3712cbd0a0a36b80713a9562412d1fd996e3e2e9a57`, which
`TestHolidayCalendarDigest` checks. What the generator chooses (English names in CamelCase,
substitute "observed" days left out, multi-day holidays folded onto their first day, the
continental groups) is documented in the script and in `internal/bigqueryfront/modelholidayregion.go`.
