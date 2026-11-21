# bigquery.spark.procedure, the library BigQuery's runtime gives a stored
# procedure for Apache Spark written in Python
# (https://docs.cloud.google.com/bigquery/docs/spark-procedures):
# SparkProcParamContext.getOrCreate(spark) holds the procedure's arguments,
# read from the BIGQUERY_PROC_PARAM.<name> environment variables, whose
# values are JSON, as the page's Python types: BOOL bool, STRING str,
# FLOAT64 float, INT64 int, BYTES bytes, DATE datetime.date, TIMESTAMP and
# DATETIME datetime.datetime, TIME datetime.time, ARRAY a list, STRUCT a
# dict, JSON an object. An OUT or INOUT argument is set by assigning the
# attribute of its name; "TIME and DATETIME type values are converted to
# the UTC timezone" when they are returned. CloudBurrow's runner reads the
# values set from CB_SPARK_OUT when the application ends; CB_SPARK_TYPES
# names each argument's GoogleSQL type.
import atexit
import base64
import datetime
import json
import os

_PREFIX = "BIGQUERY_PROC_PARAM."


def _split_top(s):
    """The comma-separated parts of s outside <...> and (...)."""
    parts, depth, cur = [], 0, ""
    for ch in s:
        if ch in "<(":
            depth += 1
        elif ch in ">)":
            depth -= 1
        if ch == "," and depth == 0:
            parts.append(cur.strip())
            cur = ""
        else:
            cur += ch
    if cur.strip():
        parts.append(cur.strip())
    return parts


def _struct_fields(typ):
    inner = typ[typ.index("<") + 1:typ.rindex(">")]
    out = []
    for part in _split_top(inner):
        bits = part.split(None, 1)
        if len(bits) == 2 and not bits[0].upper().startswith(("ARRAY<", "STRUCT<")):
            out.append((bits[0], bits[1]))
        else:
            out.append((None, part))
    return out


def _parse_time(s):
    for fmt in ("%H:%M:%S.%f", "%H:%M:%S"):
        try:
            return datetime.datetime.strptime(s, fmt).time()
        except ValueError:
            pass
    return s


def _parse_datetime(s, aware):
    s = s.replace("T", " ")
    if aware:
        s = s.replace(" UTC", "+00:00")
        if s.endswith("Z"):
            s = s[:-1] + "+00:00"
    try:
        v = datetime.datetime.fromisoformat(s)
    except ValueError:
        return s
    if aware and v.tzinfo is None:
        v = v.replace(tzinfo=datetime.timezone.utc)
    return v


def _from_json(value, typ):
    """A JSON argument value as the Python value of GoogleSQL type typ."""
    if value is None or not typ:
        return value
    t = typ.strip()
    u = t.upper()
    if u.startswith("ARRAY<"):
        elem = t[t.index("<") + 1:t.rindex(">")]
        return [_from_json(v, elem) for v in value]
    if u.startswith("STRUCT<"):
        fields = _struct_fields(t)
        if isinstance(value, dict):
            return {n: _from_json(value.get(n), ft) for n, ft in fields if n is not None}
        return value
    if u == "INT64":
        return int(value)
    if u == "FLOAT64":
        return float(value)
    if u == "BYTES":
        return base64.b64decode(value)
    if u == "DATE":
        try:
            return datetime.date.fromisoformat(value)
        except ValueError:
            return value
    if u == "TIME":
        return _parse_time(value)
    if u == "DATETIME":
        return _parse_datetime(value, False)
    if u == "TIMESTAMP":
        return _parse_datetime(value, True)
    return value


def _to_json(value):
    """A Python value as JSON for the runner: bytes as base64, times in ISO
    8601, TIME and DATETIME in UTC."""
    if isinstance(value, (bytes, bytearray)):
        return base64.b64encode(bytes(value)).decode("ascii")
    if isinstance(value, datetime.datetime):
        if value.tzinfo is not None:
            value = value.astimezone(datetime.timezone.utc).replace(tzinfo=None)
        return value.isoformat()
    if isinstance(value, (datetime.date, datetime.time)):
        if isinstance(value, datetime.time) and value.tzinfo is not None:
            value = value.replace(tzinfo=None)
        return value.isoformat()
    if isinstance(value, dict):
        return {k: _to_json(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [_to_json(v) for v in value]
    if hasattr(value, "asDict"):  # a pyspark Row
        return _to_json(value.asDict(recursive=True))
    return value


def _types():
    path = os.environ.get("CB_SPARK_TYPES")
    if not path:
        return {}
    try:
        with open(path) as f:
            return json.load(f) or {}
    except (OSError, ValueError):
        return {}


class SparkProcParamContext(object):
    _instance = None

    def __init__(self):
        types = _types()
        values = {}
        for key, value in os.environ.items():
            if key.startswith(_PREFIX):
                name = key[len(_PREFIX):]
                values[name] = _from_json(json.loads(value), types.get(name))
        object.__setattr__(self, "_values", values)
        object.__setattr__(self, "_assigned", {})

    @classmethod
    def getOrCreate(cls, spark=None):
        if cls._instance is None:
            cls._instance = cls()
            atexit.register(cls._instance._write)
        return cls._instance

    def __getattr__(self, name):
        assigned = object.__getattribute__(self, "_assigned")
        if name in assigned:
            return assigned[name]
        values = object.__getattribute__(self, "_values")
        if name in values:
            return values[name]
        raise AttributeError("The procedure has no argument named " + name)

    def __setattr__(self, name, value):
        object.__getattribute__(self, "_assigned")[name] = value

    def _write(self):
        path = os.environ.get("CB_SPARK_OUT")
        if not path:
            return
        out = {}
        for name, value in object.__getattribute__(self, "_assigned").items():
            out[name] = json.dumps(_to_json(value), default=str)
        with open(path, "w") as f:
            json.dump(out, f)
