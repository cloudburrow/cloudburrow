# Writes the Parquet fixtures in this directory with Apache Arrow's own
# Parquet writer (pyarrow, which wraps Arrow's C++ Parquet implementation),
# so the front's footer reader (parquetfooter.go) is tested against files
# it did not write. Run by hand, never in CI:
#
#   uv venv .venv && uv pip install --python .venv/bin/python pyarrow==25.0.1
#   .venv/bin/python gen.py
#
# The files are committed; rerunning this with the same pyarrow version
# rewrites them.
import datetime
import decimal

import pyarrow as pa
import pyarrow.parquet as pq


def write(name, table, **kw):
    pq.write_table(table, name, compression="NONE", **kw)


# ab: a INT64 and b STRING, both nullable (OPTIONAL), three rows.
write("ab.parquet", pa.table({"a": pa.array([1, 2, 3], pa.int64()), "b": pa.array(["x", "y", "z"])}))

# ab_required: the same columns, REQUIRED.
write("ab_required.parquet", pa.Table.from_arrays(
    [pa.array([1, 2, 3], pa.int64()), pa.array(["x", "y", "z"])],
    schema=pa.schema([pa.field("a", pa.int64(), nullable=False), pa.field("b", pa.string(), nullable=False)])))

# AB: columns A and B, which BigQuery's names match a and b.
write("upper.parquet", pa.table({"A": pa.array([1, 2, 3], pa.int64()), "B": pa.array(["x", "y", "z"])}))

# types: one column of each flat Parquet type BigQuery's conversion table
# lists, with the type it lists (the column's name says which).
ts = datetime.datetime(2024, 1, 2, 3, 4, 5, 123456, tzinfo=datetime.timezone.utc)
write("types.parquet", pa.table({
    "bool": pa.array([True, False]),
    "int32": pa.array([7, -7], pa.int32()),
    "int8": pa.array([8, -8], pa.int8()),
    "uint16": pa.array([16, 17], pa.uint16()),
    "int64": pa.array([64, -64], pa.int64()),
    "float": pa.array([1.5, -1.5], pa.float32()),
    "double": pa.array([2.25, -2.25], pa.float64()),
    "string": pa.array(["s", "t"]),
    "bytes": pa.array([b"\x00\x01", b"\xff"], pa.binary()),
    "fixed": pa.array([b"ab", b"cd"], pa.binary(2)),
    "date": pa.array([datetime.date(2024, 1, 2), datetime.date(1969, 12, 31)], pa.date32()),
    "time_ms": pa.array([datetime.time(1, 2, 3, 4000), datetime.time(23, 59, 59)], pa.time32("ms")),
    "time_us": pa.array([datetime.time(1, 2, 3, 4567), datetime.time(0, 0)], pa.time64("us")),
    "ts_ms": pa.array([ts.replace(microsecond=123000), ts], pa.timestamp("ms", tz="UTC")),
    "ts_us": pa.array([ts, ts], pa.timestamp("us", tz="UTC")),
}))

# loadable: the columns of types that the front loads, with a row of
# NULLs and a string that is not ASCII.
write("loadable.parquet", pa.table({
    "bool": pa.array([True, False, None]),
    "int32": pa.array([7, -7, None], pa.int32()),
    "int8": pa.array([8, -8, None], pa.int8()),
    "uint16": pa.array([16, 65535, None], pa.uint16()),
    "uint32": pa.array([32, 4294967295, None], pa.uint32()),
    "int64": pa.array([64, -9223372036854775808, None], pa.int64()),
    "float": pa.array([1.5, -1.5, None], pa.float32()),
    "double": pa.array([2.25, -2.25, None], pa.float64()),
    "string": pa.array(["s", "\u00e9\u2603", None]),
    "date": pa.array([datetime.date(2024, 1, 2), datetime.date(1969, 12, 31), None], pa.date32()),
    "ts_us": pa.array([ts, datetime.datetime(1969, 12, 31, 23, 59, 59, 999999, tzinfo=datetime.timezone.utc), None],
                      pa.timestamp("us", tz="UTC")),
}))

# The types BigQuery's conversion table lists that the front does not
# load (501), or that the table does not list, one per file.
write("int96.parquet", pa.table({"t": pa.array([ts, ts], pa.timestamp("ns", tz="UTC"))}),
      use_deprecated_int96_timestamps=True)
write("ts_ns.parquet", pa.table({"t": pa.array([ts, ts], pa.timestamp("ns", tz="UTC"))}), version="2.6")
write("decimal.parquet", pa.table({"d": pa.array([decimal.Decimal("1.25"), decimal.Decimal("-2.50")], pa.decimal128(9, 2))}))
write("uint64.parquet", pa.table({"u": pa.array([1, 2], pa.uint64())}))
write("list.parquet", pa.table({"l": pa.array([[1, 2], [3]], pa.list_(pa.int64()))}))
write("struct.parquet", pa.table({"s": pa.array([{"x": 1}, {"x": 2}], pa.struct([("x", pa.int64())]))}))
write("json.parquet", pa.table({"j": pa.array(['{"k":1}', "[]"], pa.json_())}))
