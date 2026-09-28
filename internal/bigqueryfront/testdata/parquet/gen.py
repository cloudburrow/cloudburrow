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

# The front reads these files' rows itself (#1004, #1005, #1006), so they
# also try the reader's page encodings and compressions: SNAPPY and ZSTD,
# dictionary pages, data pages of version 2, several row groups.

# nested: groups, LIST and MAP, with NULLs at each level.
nested_struct = pa.struct([("x", pa.int64()), ("y", pa.string()), ("z", pa.struct([("q", pa.bool_())]))])
pq.write_table(pa.table({
    "id": pa.array([1, 2, 3, 4], pa.int64()),
    "s": pa.array([{"x": 1, "y": "a", "z": {"q": True}}, {"x": None, "y": "b", "z": None}, None,
                   {"x": 4, "y": None, "z": {"q": None}}], nested_struct),
    "l": pa.array([[1, 2], [], None, [3]], pa.list_(pa.int64())),
    "ls": pa.array([[{"x": 1, "y": "a"}, {"x": 2, "y": None}], [], None, [{"x": 3, "y": "c"}]],
                   pa.list_(pa.struct([("x", pa.int64()), ("y", pa.string())]))),
    "m": pa.array([[("a", 1), ("b", None)], [], None, [("c", 3)]], pa.map_(pa.string(), pa.int64())),
    "sl": pa.array([{"l": [1, 2]}, {"l": None}, None, {"l": []}], pa.struct([("l", pa.list_(pa.int64()))])),
}), "nested.parquet", compression="SNAPPY", row_group_size=3)

# A LIST with a NULL element, and a LIST of LISTs.
write("list_null.parquet", pa.table({"l": pa.array([[1, None]], pa.list_(pa.int64()))}))
write("list_list.parquet", pa.table({"l": pa.array([[[1], [2, 3]]], pa.list_(pa.list_(pa.int64())))}))

# converted: the types the emulator loads wrongly, which the front
# converts, with a row of NULLs.
ts_ms = datetime.datetime(2024, 1, 2, 3, 4, 5, 123000, tzinfo=datetime.timezone.utc)
pq.write_table(pa.table({
    "id": pa.array([1, 2, 3], pa.int64()),
    "time_ms": pa.array([datetime.time(1, 2, 3, 4000), datetime.time(23, 59, 59, 999000), None], pa.time32("ms")),
    "time_us": pa.array([datetime.time(1, 2, 3, 4567), datetime.time(0, 0), None], pa.time64("us")),
    "ts_ms": pa.array([ts_ms, datetime.datetime(1969, 12, 31, 23, 59, 59, 999000, tzinfo=datetime.timezone.utc), None],
                      pa.timestamp("ms", tz="UTC")),
    "bytes": pa.array([b"\x00\x01", b"\xff", None], pa.binary()),
    "fixed": pa.array([b"ab", b"\xff\x00", None], pa.binary(2)),
    "uint64": pa.array([1, 9223372036854775807, None], pa.uint64()),
    "float": pa.array([1.5, float("inf"), None], pa.float32()),
    "double": pa.array([0.1, float("-inf"), None], pa.float64()),
}), "converted.parquet", compression="ZSTD", data_page_version="2.0", row_group_size=2)

# DECIMAL as INT32 and INT64 (store_decimal_as_integer), wide ones and a
# 256-bit one; unsigned INT64 and INT96 values BigQuery refuses or the
# front does not load; a NaN.
write("decimal_int.parquet", pa.table({
    "d32": pa.array([decimal.Decimal("1.25"), decimal.Decimal("-2.50"), None], pa.decimal128(9, 2)),
    "d64": pa.array([decimal.Decimal("123456789012.345678"), decimal.Decimal("-0.000001"), None], pa.decimal128(18, 6)),
}), store_decimal_as_integer=True)
write("decimal_wide.parquet", pa.table({
    "d": pa.array([decimal.Decimal("1.5"), decimal.Decimal("-12345678901234567890.1234567890")], pa.decimal128(38, 10)),
}))
write("decimal_frac.parquet", pa.table({"d": pa.array([decimal.Decimal("0.0000000001")], pa.decimal128(38, 10))}))
write("decimal256.parquet", pa.table({
    "d": pa.array([decimal.Decimal("12345678901234567890123456789012345678.12"), decimal.Decimal("-1")], pa.decimal256(40, 2)),
}))
write("uint64_big.parquet", pa.table({"u": pa.array([1, 9223372036854775808], pa.uint64())}))
write("int96_ns.parquet", pa.table({"t": pa.array([1704164645123456789], pa.timestamp("ns", tz="UTC"))}),
      use_deprecated_int96_timestamps=True)
write("nan.parquet", pa.table({"f": pa.array([1.0, float("nan")], pa.float64())}))

# Schema changes: a column the table (a, b) lacks, and A, B, C in
# another case.
write("abc.parquet", pa.table({"a": pa.array([4], pa.int64()), "b": pa.array(["w"]), "c": pa.array([True])}))
