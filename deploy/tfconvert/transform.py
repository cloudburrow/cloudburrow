"""The preprocessing SavedModel of a BigQuery ML TRANSFORM clause (#2210).

export(spec, fail) writes /out/model, a TensorFlow SavedModel whose
serving_default signature takes each column the TRANSFORM reads and answers
each column it outputs, as "Export models" documents ("an additional
preprocessing model performs the same logic in the TRANSFORM clause and is
saved in the TensorFlow SavedModel format under the subdirectory
transform"). spec, which the BigQuery front writes
(internal/bigqueryfront/modelexporttransform.go):

  inputs:  [{"name", "dtype", "type"}]: dtype the tensor's (int64, float64,
           bool, string), type the column's GoogleSQL type; a DATE is a
           "%F" string, a DATETIME "%F %H:%M:%E6S", a TIME "%H:%M:%E6S", a
           TIMESTAMP "%F %H:%M:%E1S %z";
  outputs: [{"name", "type", "graph"}]: graph the column's expression;
  nulls:   the inputs' NULL values (float64, int64, string).

Each node of a graph is {"op", "t", "args", "a"}: t its dtype (i int64, f
float64, s string, b bool; DATE values are int64 days since 1970-01-01,
DATETIME, TIME and TIMESTAMP int64 microseconds). Every value is a pair, a
[batch] tensor and its NULL mask, computed as GoogleSQL does (NULL in, NULL
out; AND, OR and NOT three-valued). Where GoogleSQL raises an error (a
division by zero, a bad cast), the exported graph gives NULL.
"""

US_DAY = 86400000000
DT = {"i": "int64", "f": "float64", "s": "string", "b": "bool"}
DAY_NAMES = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"]
MONTH_NAMES = ["January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November",
               "December"]
WEEKDAYS = {"SUNDAY": 0, "MONDAY": 1, "TUESDAY": 2, "WEDNESDAY": 3, "THURSDAY": 4, "FRIDAY": 5, "SATURDAY": 6}
UNIT_US = {"MICROSECOND": 1, "MILLISECOND": 1000, "SECOND": 1000000, "MINUTE": 60000000, "HOUR": 3600000000, "DAY": US_DAY}


def export(spec, fail):
    import tensorflow as tf

    module = tf.Module()
    module.tables = []
    nulls = spec["nulls"]
    dtypes = {"int64": tf.int64, "float64": tf.float64, "string": tf.string, "bool": tf.bool}

    def tfd(t):
        return dtypes[DT[t]]

    # Civil calendar arithmetic (Howard Hinnant's days_from_civil and
    # civil_from_days), in int64.
    def fdiv(a, b):
        return tf.math.floordiv(a, b)

    def fmod(a, b):
        return tf.math.floormod(a, b)

    def days_from_civil(y, m, d):
        y = y - tf.cast(m <= 2, tf.int64)
        era = fdiv(y, 400)
        yoe = y - era * 400
        mp = fmod(m + 9, 12)
        doy = fdiv(153 * mp + 2, 5) + d - 1
        doe = yoe * 365 + fdiv(yoe, 4) - fdiv(yoe, 100) + doy
        return era * 146097 + doe - 719468

    def civil_from_days(z):
        z = z + 719468
        era = fdiv(z, 146097)
        doe = z - era * 146097
        yoe = fdiv(doe - fdiv(doe, 1460) + fdiv(doe, 36524) - fdiv(doe, 146096), 365)
        y = yoe + era * 400
        doy = doe - (365 * yoe + fdiv(yoe, 4) - fdiv(yoe, 100))
        mp = fdiv(5 * doy + 2, 153)
        d = doy - fdiv(153 * mp + 2, 5) + 1
        m = tf.where(mp < 10, mp + 3, mp - 9)
        return y + tf.cast(m <= 2, tf.int64), m, d

    def is_leap(y):
        return (fmod(y, 4) == 0) & ((fmod(y, 100) != 0) | (fmod(y, 400) == 0))

    def days_in_month(y, m):
        base = tf.gather(tf.constant([31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31], tf.int64), m - 1)
        return base + tf.cast((m == 2) & is_leap(y), tf.int64)

    def valid_date(y, m, d):
        ok = (y >= 1) & (y <= 9999) & (m >= 1) & (m <= 12) & (d >= 1)
        mm = tf.clip_by_value(m, 1, 12)
        return ok & (d <= days_in_month(y, mm))

    def dow(days):  # 0 Sunday .. 6 Saturday; 1970-01-01 was a Thursday
        return fmod(days + 4, 7)

    def day_of_year(days):
        y, _, _ = civil_from_days(days)
        return days - days_from_civil(y, tf.ones_like(y), tf.ones_like(y)) + 1

    def iso_year_week(days):
        # The ISO week's Thursday decides its year.
        wd = fmod(days + 3, 7)  # 0 Monday
        thu = days - wd + 3
        y, _, _ = civil_from_days(thu)
        jan1 = days_from_civil(y, tf.ones_like(y), tf.ones_like(y))
        return y, fdiv(thu - jan1, 7) + 1

    def week_of_year(days, start):
        # Weeks begin on weekday start; days before the year's first one
        # are week 0.
        y, _, _ = civil_from_days(days)
        jan1 = days_from_civil(y, tf.ones_like(y), tf.ones_like(y))
        first = jan1 + fmod(start - dow(jan1), 7)
        return tf.where(days < first, tf.zeros_like(days), fdiv(days - first, 7) + 1)

    def split_us(t, typ):
        # (days, microseconds of the day) of a DATETIME, TIMESTAMP or TIME.
        if typ == "TIME":
            return tf.zeros_like(t), t
        return fdiv(t, US_DAY), fmod(t, US_DAY)

    def add_months(days, n):
        y, m, d = civil_from_days(days)
        tot = y * 12 + (m - 1) + n
        ny, nm = fdiv(tot, 12), fmod(tot, 12) + 1
        nd = tf.minimum(d, days_in_month(ny, nm))
        return days_from_civil(ny, nm, nd)

    def week_start(unit):
        if unit == "WEEK":
            return 0
        if unit.startswith("WEEK("):
            return WEEKDAYS[unit[5:-1]]
        return None

    def trunc_days(days, unit):
        y, m, d = civil_from_days(days)
        one = tf.ones_like(y)
        if unit == "DAY":
            return days
        s = week_start(unit)
        if s is not None:
            return days - fmod(dow(days) - s, 7)
        if unit == "ISOWEEK":
            return days - fmod(days + 3, 7)
        if unit == "MONTH":
            return days_from_civil(y, m, one)
        if unit == "QUARTER":
            return days_from_civil(y, fdiv(m - 1, 3) * 3 + 1, one)
        if unit == "YEAR":
            return days_from_civil(y, one, one)
        if unit == "ISOYEAR":
            iy, _ = iso_year_week(days)
            jan4 = days_from_civil(iy, one, one * 4)
            return jan4 - fmod(jan4 + 3, 7)
        raise ValueError("unit " + unit)

    def pad(x, width, fill="0"):
        return tf.strings.as_string(x, width=width, fill=fill)

    def fraction(us, digits):
        # The microseconds as digits (6) or as few as they need (None).
        s = pad(us, 6)
        if digits is None:
            s = tf.strings.regex_replace(s, "0+$", "")
            return tf.where(tf.strings.length(s) > 0, tf.strings.join([".", s]), "")
        if digits == 0:
            return tf.fill(tf.shape(us), "")
        if digits <= 6:
            return tf.strings.join([".", tf.strings.substr(s, 0, digits)])
        return tf.strings.join([".", s, "0" * (digits - 6)])

    def fmt(t, typ, parts):
        if typ == "DATE":
            days, us = t, tf.zeros_like(t)
        else:
            days, us = split_us(t, typ)
        y, m, d = civil_from_days(days)
        hh, rem = fdiv(us, 3600000000), fmod(us, 3600000000)
        mi, rem = fdiv(rem, 60000000), fmod(rem, 60000000)
        ss, micro = fdiv(rem, 1000000), fmod(rem, 1000000)
        out = []
        for p in parts:
            if p.startswith("="):
                out.append(tf.fill(tf.shape(t), p[1:]))
                continue
            k = p[1:]
            if k == "Y":
                out.append(pad(y, 4))
            elif k == "C":
                out.append(pad(fdiv(y, 100), 2))
            elif k == "y":
                out.append(pad(fmod(y, 100), 2))
            elif k == "m":
                out.append(pad(m, 2))
            elif k == "d":
                out.append(pad(d, 2))
            elif k == "e":
                out.append(pad(d, 2, " "))
            elif k == "j":
                out.append(pad(day_of_year(days), 3))
            elif k == "H":
                out.append(pad(hh, 2))
            elif k == "k":
                out.append(pad(hh, 2, " "))
            elif k in ("I", "l"):
                h12 = fmod(hh + 11, 12) + 1
                out.append(pad(h12, 2, "0" if k == "I" else " "))
            elif k == "p":
                out.append(tf.where(hh < 12, "AM", "PM"))
            elif k == "M":
                out.append(pad(mi, 2))
            elif k == "S":
                out.append(pad(ss, 2))
            elif k.startswith("E") and k.endswith("S"):
                n = k[1:-1]
                out.append(tf.strings.join([pad(ss, 2), fraction(micro, None if n == "*" else int(n))]))
            elif k in ("a", "A"):
                names = [n[:3] for n in DAY_NAMES] if k == "a" else DAY_NAMES
                out.append(tf.gather(tf.constant(names), dow(days)))
            elif k in ("b", "B", "h"):
                names = [n[:3] for n in MONTH_NAMES] if k != "B" else MONTH_NAMES
                out.append(tf.gather(tf.constant(names), m - 1))
            elif k == "u":
                out.append(tf.strings.as_string(fmod(dow(days) + 6, 7) + 1))
            elif k == "w":
                out.append(tf.strings.as_string(dow(days)))
            elif k == "Q":
                out.append(tf.strings.as_string(fdiv(m - 1, 3) + 1))
            elif k == "s":
                out.append(tf.strings.as_string(fdiv(t, 1000000) if typ != "DATE" else t * 86400))
            elif k == "z":
                out.append(tf.fill(tf.shape(t), "+0000"))
            elif k == "Ez":
                out.append(tf.fill(tf.shape(t), "+00:00"))
            elif k == "Z":
                out.append(tf.fill(tf.shape(t), "UTC"))
            else:
                raise ValueError("format element %" + k)
        if not out:
            return tf.fill(tf.shape(t), "")
        return tf.strings.join(out)

    import re as _re

    FRAG = {"Y": r"[0-9]{1,4}", "m": r"[0-9]{1,2}", "d": r"[0-9]{1,2}", "e": r" ?[0-9]{1,2}", "H": r"[0-9]{1,2}",
            "k": r" ?[0-9]{1,2}", "I": r"[0-9]{1,2}", "l": r" ?[0-9]{1,2}", "M": r"[0-9]{1,2}", "S": r"[0-9]{1,2}",
            "y": r"[0-9]{1,2}", "j": r"[0-9]{1,3}", "p": r"(?i:AM|PM)", "s": r"-?[0-9]+",
            "b": r"(?i:jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec)[a-zA-Z]*", "z": r"[+-][0-9]{2}:?[0-9]{2}|Z|UTC",
            "Ez": r"[+-][0-9]{2}:?[0-9]{2}|Z|UTC"}
    FRAG["B"] = FRAG["h"] = FRAG["b"]

    def frag_of(p):
        if p.startswith("="):
            lit = p[1:]
            # A space in the format matches any run of whitespace.
            return "".join(r"\s*" if ch.isspace() else _re.escape(ch) for ch in lit)
        k = p[1:]
        if k.startswith("E") and k.endswith("S"):
            return r"[0-9]{1,2}(?:\.[0-9]*)?"
        return FRAG[k]

    def grab(s, frags, k):
        # The text of fragment k of s, a full match of the fragments.
        pat = "^" + "".join("(" + f + ")" if i == k else "(?:" + f + ")" for i, f in enumerate(frags)) + "$"
        return tf.strings.regex_replace(s, pat, r"\1")

    def num(s):
        s = tf.strings.regex_replace(s, r"^\s+", "")
        ok = tf.strings.regex_full_match(s, r"-?[0-9]+")
        return tf.strings.to_number(tf.where(ok, s, "0"), tf.int64)

    def parse(s, typ, parts):
        frags = [frag_of(p) for p in parts]
        full = "^" + "".join("(?:" + f + ")" for f in frags) + "$"
        ok = tf.strings.regex_full_match(s, full)
        s = tf.where(ok, s, "")
        z = tf.zeros(tf.shape(s), tf.int64)
        y, m, d, hh, mi, ss, us, off, doy, epoch = z + 1970, z + 1, z + 1, z, z, z, z, z, None, None
        pm = None
        for k, p in enumerate(parts):
            if p.startswith("="):
                continue
            e = p[1:]
            g = grab(s, frags, k)
            if e == "Y":
                y = num(g)
            elif e == "y":
                v = num(g)
                y = tf.where(v < 69, v + 2000, v + 1900)
            elif e == "m":
                m = num(g)
            elif e in ("d", "e"):
                d = num(g)
            elif e in ("H", "k", "I", "l"):
                hh = num(g)
            elif e == "p":
                pm = tf.strings.lower(g) == "pm"
            elif e == "M":
                mi = num(g)
            elif e == "S":
                ss = num(g)
            elif e.startswith("E") and e.endswith("S"):
                ss = num(tf.strings.regex_replace(g, r"\..*$", ""))
                fr = tf.strings.regex_replace(g, r"^[0-9]*\.?", "")
                us = num(tf.strings.substr(tf.strings.join([fr, "000000"]), 0, 6))
            elif e == "j":
                doy = num(g)
            elif e == "s":
                epoch = num(g)
            elif e in ("b", "B", "h"):
                low = tf.strings.substr(tf.strings.lower(g), 0, 3)
                table = tf.lookup.StaticHashTable(tf.lookup.KeyValueTensorInitializer(
                    tf.constant([n[:3].lower() for n in MONTH_NAMES]), tf.range(1, 13, dtype=tf.int64)), 0)
                module.tables.append(table)
                m = table.lookup(low)
            elif e in ("z", "Ez"):
                sign = tf.where(tf.strings.substr(g, 0, 1) == "-", -1, 1)
                digits = tf.strings.regex_replace(g, r"[^0-9]", "")
                has = tf.strings.length(digits) == 4
                digits = tf.where(has, digits, "0000")
                oh = num(tf.strings.substr(digits, 0, 2))
                om = num(tf.strings.substr(digits, 2, 2))
                off = tf.cast(sign, tf.int64) * (oh * 60 + om) * 60000000
        if pm is not None:
            hh = fmod(hh, 12) + tf.where(pm, 12, 0)
        valid = ok & valid_date(y, m, d) & (hh < 24) & (mi < 60) & (ss < 60)
        days = days_from_civil(y, m, tf.clip_by_value(d, 1, 31))
        if doy is not None:
            days = days_from_civil(y, tf.ones_like(y), tf.ones_like(y)) + doy - 1
        tod = hh * 3600000000 + mi * 60000000 + ss * 1000000 + us
        if typ == "DATE":
            v = days
        elif typ == "TIME":
            v = tod
        else:
            v = days * US_DAY + tod - off
        if epoch is not None:
            v = epoch * 1000000 if typ != "DATE" else fdiv(epoch, 86400)
        return v, ~valid

    INPUT_FORMATS = {"DATE": ["%Y", "=-", "%m", "=-", "%d"],
                     "DATETIME": ["%Y", "=-", "%m", "=-", "%d", "=T", "%H", "=:", "%M", "=:", "%E6S"],
                     "TIME": ["%H", "=:", "%M", "=:", "%E6S"],
                     "TIMESTAMP": ["%Y", "=-", "%m", "=-", "%d", "=T", "%H", "=:", "%M", "=:", "%E6S", "= ", "%Ez"]}

    def parse_input(s, typ):
        # A DATE, DATETIME, TIME or TIMESTAMP in its input format; a space
        # or T between the date and the time, the TIMESTAMP's offset
        # optional.
        parts = INPUT_FORMATS[typ]
        if typ in ("DATETIME", "TIMESTAMP"):
            s = tf.strings.regex_replace(s, r"^([0-9]+-[0-9]+-[0-9]+)[ T]", r"\1T")
        if typ == "TIMESTAMP":
            # The offset: Z or UTC is +0000, none is UTC, +HH is +HH00.
            s = tf.strings.regex_replace(s, r"^(\S+T[0-9:.]+)\s*(?:Z|UTC)$", r"\1 +0000")
            has = tf.strings.regex_full_match(s, r"^\S+T[0-9:.]+\s*[+-][0-9]{2}(?::?[0-9]{2})?$")
            s = tf.where(has, s, tf.strings.join([s, " +0000"]))
            s = tf.strings.regex_replace(s, r"^(\S+T[0-9:.]+)\s*([+-][0-9]{2}(?::?[0-9]{2})?)$", r"\1 \2")
            s = tf.strings.regex_replace(s, r"^(.* [+-][0-9]{2})$", r"\100")
        if typ == "DATE":
            s = tf.strings.regex_replace(s, r"^([0-9]+-[0-9]+-[0-9]+)[ T].*$", r"\1")
        return parse(s, typ, parts)

    def output_string(v, typ):
        if typ == "DATE":
            return fmt(v, "DATE", ["%Y", "=-", "%m", "=-", "%d"])
        if typ == "DATETIME":
            return fmt(v, "DATETIME", ["%Y", "=-", "%m", "=-", "%d", "= ", "%H", "=:", "%M", "=:", "%E6S"])
        return fmt(v, "TIME", ["%H", "=:", "%M", "=:", "%E6S"])

    def float_string(x):
        # GoogleSQL's CAST(FLOAT64 AS STRING): the shortest form that reads
        # back the same.
        best = tf.strings.as_string(x, precision=17, shortest=True)
        done = tf.zeros(tf.shape(x), tf.bool)
        for p in range(1, 18):
            s = tf.strings.as_string(x, precision=p, shortest=True)
            back = tf.strings.to_number(s, tf.float64)
            hit = (~done) & (back == x)
            best = tf.where(hit, s, best)
            done = done | hit
        best = tf.where(tf.math.is_nan(x), "nan", best)
        best = tf.where(tf.math.is_inf(x) & (x > 0), "inf", best)
        return tf.where(tf.math.is_inf(x) & (x < 0), "-inf", best)

    def round_away(x):
        return tf.sign(x) * tf.floor(tf.abs(x) + 0.5)

    def cast(n, x):
        v, null = x
        a = n.get("a") or {}
        frm, to, safe = a.get("from"), a.get("to"), a.get("safe")
        t = n["t"]
        src = n["args"][0]["t"]
        if frm is None:
            if src == "f" and t == "i":
                bad = tf.math.is_nan(v) | (tf.abs(v) >= 9.223372036854775e18)
                return tf.cast(round_away(tf.where(bad, tf.zeros_like(v), v)), tf.int64), null | bad
            return tf.cast(v, tfd(t)), null
        dated = ("DATE", "DATETIME", "TIME", "TIMESTAMP")
        if frm in ("STRING", "BYTES"):
            s = tf.strings.strip(v) if to not in ("STRING", "BYTES") else v
            if to == "INT64":
                ok = tf.strings.regex_full_match(s, r"[+-]?[0-9]+|[+-]?0[xX][0-9a-fA-F]+")
                dec = tf.strings.regex_full_match(s, r"[+-]?[0-9]+")
                val = tf.strings.to_number(tf.where(dec, s, "0"), tf.int64)
                return val, null | ~(ok & dec)
            if to in ("FLOAT64", "NUMERIC", "BIGNUMERIC"):
                ok = tf.strings.regex_full_match(s, r"[+-]?([0-9]+\.?[0-9]*|\.[0-9]+)([eE][+-]?[0-9]+)?|(?i:[+-]?(inf|infinity|nan))")
                val = tf.strings.to_number(tf.where(ok, s, "0"), tf.float64)
                return val, null | ~ok
            if to == "BOOL":
                low = tf.strings.lower(s)
                return low == "true", null | ~((low == "true") | (low == "false"))
            if to in dated:
                pv, pn = parse_input(s, to)
                return pv, null | pn
            return v, null
        if to in ("STRING", "BYTES"):
            if frm == "INT64":
                return tf.strings.as_string(v), null
            if frm in ("FLOAT64", "NUMERIC", "BIGNUMERIC"):
                return float_string(v), null
            if frm == "BOOL":
                return tf.where(v, "true", "false"), null
            if frm in ("DATE", "DATETIME", "TIME"):
                return output_string(v, frm), null
            if frm == "TIMESTAMP":
                return fmt(v, "TIMESTAMP", ["%Y", "=-", "%m", "=-", "%d", "= ", "%H", "=:", "%M", "=:", "%E*S", "=+00"]), null
        if to == "BOOL":
            return v != tf.zeros_like(v), null
        if frm == "BOOL":
            return tf.cast(v, tfd(t)), null
        if frm in ("FLOAT64", "NUMERIC", "BIGNUMERIC") and to == "INT64":
            bad = tf.math.is_nan(v) | (tf.abs(v) >= 9.223372036854775e18)
            return tf.cast(round_away(tf.where(bad, tf.zeros_like(v), v)), tf.int64), null | bad
        if frm in dated and to in dated:
            if frm == "DATE":
                return v * US_DAY, null
            if to == "DATE":
                return fdiv(v, US_DAY), null
            if to == "TIME":
                return fmod(v, US_DAY), null
            return v, null
        return tf.cast(v, tfd(t)), null

    memo = {}

    def ev(n, inputs):
        key = id(n)
        if key in memo:
            return memo[key]
        out = ev1(n, inputs)
        memo[key] = out
        return out

    def ev1(n, inputs):
        op, t, a = n["op"], n["t"], n.get("a") or {}
        shape = tf.shape(inputs["__n"])
        no = tf.zeros(shape, tf.bool)
        if op == "input":
            return inputs[a["name"]]
        if op == "lit":
            val = a["v"]
            if t == "b":
                val = bool(val)
            return tf.fill(shape, tf.constant(val, tfd(t))), no
        if op == "null":
            zero = {"i": 0, "f": 0.0, "s": "", "b": False}[t]
            return tf.fill(shape, tf.constant(zero, tfd(t))), tf.ones(shape, tf.bool)
        if op in ("split_get", "split_len"):
            # Their first argument is SPLIT's ARRAY, which they read.
            args = [(None, no)] + [ev(x, inputs) for x in n["args"][1:]]
        else:
            args = [ev(x, inputs) for x in n.get("args") or []]
        vs = [x[0] for x in args]
        anynull = no
        for x in args:
            anynull = anynull | x[1]
        if op == "cast":
            return cast(n, args[0])
        if op in ("add", "sub", "mul"):
            f = {"add": tf.add, "sub": tf.subtract, "mul": tf.multiply}[op]
            return f(vs[0], vs[1]), anynull
        if op == "div":
            zero = vs[1] == 0
            return tf.math.divide_no_nan(vs[0], tf.where(zero, tf.ones_like(vs[1]), vs[1])), anynull | zero
        if op == "safe_div":
            zero = vs[1] == 0
            return tf.math.divide_no_nan(vs[0], tf.where(zero, tf.ones_like(vs[1]), vs[1])), anynull | zero
        if op == "ieee_div":
            return vs[0] / vs[1], anynull
        if op == "intdiv":
            zero = vs[1] == 0
            return tf.truncatediv(vs[0], tf.where(zero, tf.ones_like(vs[1]), vs[1])), anynull | zero
        if op == "mod":
            zero = vs[1] == 0
            b = tf.where(zero, tf.ones_like(vs[1]), vs[1])
            if t == "i":
                return tf.truncatemod(vs[0], b), anynull | zero
            return vs[0] - b * tf.where(vs[0] / b < 0, tf.math.ceil(vs[0] / b), tf.floor(vs[0] / b)), anynull | zero
        if op == "neg":
            return -vs[0], anynull
        if op == "bitnot":
            return tf.bitwise.invert(vs[0]), anynull
        if op in ("bitand", "bitor", "bitxor"):
            f = {"bitand": tf.bitwise.bitwise_and, "bitor": tf.bitwise.bitwise_or, "bitxor": tf.bitwise.bitwise_xor}[op]
            return f(vs[0], vs[1]), anynull
        if op in ("shl", "shr"):
            big = (vs[1] >= 64) | (vs[1] < 0)
            sh = tf.where(big, tf.zeros_like(vs[1]), vs[1])
            u = tf.bitcast(vs[0], tf.uint64)
            r = tf.bitwise.left_shift(u, tf.cast(sh, tf.uint64)) if op == "shl" else tf.bitwise.right_shift(u, tf.cast(sh, tf.uint64))
            r = tf.bitcast(r, tf.int64)
            return tf.where(big, tf.zeros_like(r), r), anynull
        if op == "cmp":
            f = {"eq": tf.equal, "ne": tf.not_equal, "lt": tf.less, "le": tf.less_equal, "gt": tf.greater,
                 "ge": tf.greater_equal}[a["op"]]
            if n["args"][0]["t"] == "s" and a["op"] not in ("eq", "ne"):
                # Strings order by their characters' code points.
                lt = _string_less(tf, vs[0], vs[1])
                gt = _string_less(tf, vs[1], vs[0])
                eq = tf.equal(vs[0], vs[1])
                return {"lt": lt, "le": lt | eq, "gt": gt, "ge": gt | eq}[a["op"]], anynull
            return f(vs[0], vs[1]), anynull
        if op == "and":
            (x, xn), (y, yn) = args
            false = (~x & ~xn) | (~y & ~yn)
            return x & y & ~xn & ~yn, ~false & (xn | yn)
        if op == "or":
            (x, xn), (y, yn) = args
            true = (x & ~xn) | (y & ~yn)
            return true, ~true & (xn | yn)
        if op == "not":
            return ~vs[0], args[0][1]
        if op == "isnull":
            return args[0][1], no
        if op == "istrue":
            return vs[0] & ~args[0][1], no
        if op == "isfalse":
            return ~vs[0] & ~args[0][1], no
        if op == "if":
            (cv, cn), (av, an), (bv, bn) = args
            take = cv & ~cn
            return tf.where(take, av, bv), tf.where(take, an, bn)
        if op == "coalesce":
            v, nl = args[-1]
            for (x, xn) in reversed(args[:-1]):
                v, nl = tf.where(xn, v, x), xn & nl
            return v, nl
        if op in ("greatest", "least"):
            f = tf.maximum if op == "greatest" else tf.minimum
            v = vs[0]
            for x in vs[1:]:
                if t == "s":
                    pick = _string_less(tf, v, x) if op == "greatest" else _string_less(tf, x, v)
                    v = tf.where(pick, x, v)
                else:
                    v = f(v, x)
            return v, anynull
        if op == "math":
            x = vs[0]
            fn = a["fn"]
            if t == "i":
                return (tf.abs(x) if fn == "abs" else tf.sign(x)), anynull
            g = {"abs": tf.abs, "sign": tf.sign, "sqrt": tf.sqrt, "exp": tf.exp, "ln": tf.math.log,
                 "log10": lambda z: tf.math.log(z) / tf.math.log(tf.constant(10.0, tf.float64)),
                 "floor": tf.floor, "ceil": tf.math.ceil, "sin": tf.sin, "cos": tf.cos, "tan": tf.tan, "asin": tf.asin,
                 "acos": tf.acos, "atan": tf.atan, "sinh": tf.sinh, "cosh": tf.cosh, "tanh": tf.tanh, "asinh": tf.asinh,
                 "acosh": tf.acosh, "atanh": tf.atanh, "cbrt": lambda z: tf.sign(z) * tf.pow(tf.abs(z), 1.0 / 3.0),
                 "cot": lambda z: 1 / tf.tan(z), "coth": lambda z: 1 / tf.tanh(z), "csc": lambda z: 1 / tf.sin(z),
                 "csch": lambda z: 1 / tf.sinh(z), "sec": lambda z: 1 / tf.cos(z), "sech": lambda z: 1 / tf.cosh(z)}[fn]
            r = g(x)
            if fn in ("sqrt",):
                return r, anynull | (x < 0)
            if fn in ("ln", "log10"):
                return r, anynull | (x <= 0)
            return r, anynull
        if op == "math2":
            x, y = vs
            fn = a["fn"]
            if fn == "pow":
                return tf.pow(x, y), anynull
            if fn == "atan2":
                return tf.atan2(x, y), anynull
            return tf.math.log(x) / tf.math.log(y), anynull | (x <= 0) | (y <= 0) | (y == 1)
        if op in ("round", "trunc"):
            s = 10.0 ** a.get("digits", 0)
            x = vs[0] * s
            r = round_away(x) if op == "round" else tf.sign(x) * tf.floor(tf.abs(x))
            return r / s, anynull
        if op == "is_inf":
            return tf.math.is_inf(vs[0]), anynull
        if op == "is_nan":
            return tf.math.is_nan(vs[0]), anynull
        if op == "farm_fingerprint":
            fp = tf.fingerprint(vs[0], method="farmhash64")
            return tf.reshape(tf.bitcast(fp, tf.int64), shape), anynull
        if op == "concat":
            return tf.strings.join(vs), anynull
        if op in ("lower", "upper"):
            f = tf.strings.lower if op == "lower" else tf.strings.upper
            return f(vs[0], encoding="utf-8"), anynull
        if op == "length":
            return tf.cast(tf.strings.length(vs[0], unit="UTF8_CHAR" if a["unit"] == "char" else "BYTE"), tf.int64), anynull
        if op in ("substr", "left", "right"):
            unit = "UTF8_CHAR" if a["unit"] == "char" else "BYTE"
            s = vs[0]
            ln = tf.cast(tf.strings.length(s, unit=unit), tf.int64)
            if op == "substr":
                pos = vs[1]
                p = tf.where(pos > 0, pos - 1, tf.where(pos < 0, tf.maximum(ln + pos, 0), tf.zeros_like(pos)))
                p = tf.minimum(p, ln)
                want = vs[2] if len(vs) > 2 else ln
                bad = want < 0
                cnt = tf.clip_by_value(tf.minimum(want, ln - p), 0, ln)
            elif op == "left":
                bad = vs[1] < 0
                p, cnt = tf.zeros_like(ln), tf.clip_by_value(tf.minimum(vs[1], ln), 0, ln)
            else:
                bad = vs[1] < 0
                cnt = tf.clip_by_value(tf.minimum(vs[1], ln), 0, ln)
                p = ln - cnt
            return tf.strings.substr(s, tf.cast(p, tf.int32), tf.cast(cnt, tf.int32), unit=unit), anynull | bad
        if op == "regex_replace":
            return tf.strings.regex_replace(vs[0], a["pattern"], _rewrite(a["rewrite"])), anynull
        if op == "regex_full_match":
            return tf.strings.regex_full_match(vs[0], a["pattern"]), anynull
        if op in ("split", "split_get", "split_len"):
            src = n if op == "split" else n["args"][0]
            s, sn = ev(src["args"][0], inputs)
            sep = src["a"]["sep"]
            if sep == "":
                rt = tf.strings.unicode_split(s, "UTF-8")
            else:
                rt = tf.strings.split(s, sep)
            rows = tf.cast(rt.row_lengths(), tf.int64)
            rows = tf.where(tf.strings.length(s) == 0, tf.zeros_like(rows), rows)
            if op == "split_len":
                return rows, sn
            if op == "split":
                raise ValueError("an ARRAY output")
            idx = vs[1] - a.get("base", 0)
            ok = (idx >= 0) & (idx < rows)
            dense = rt.to_tensor(default_value="")
            width = tf.shape(dense, out_type=tf.int64)[1]
            safe = tf.clip_by_value(idx, 0, tf.maximum(width - 1, 0))
            dense = tf.concat([dense, tf.fill([tf.shape(dense)[0], 1], "")], 1)
            got = tf.gather(dense, safe, batch_dims=1)
            return got, sn | args[1][1] | ~ok
        if op == "lookup":
            keys = a["keys"]
            kt = n["args"][0]["t"]
            if not keys:
                return tf.fill(shape, tf.constant(-1, tf.int64)), tf.ones(shape, tf.bool)
            table = tf.lookup.StaticHashTable(tf.lookup.KeyValueTensorInitializer(
                tf.constant(keys, tfd(kt)), tf.range(len(keys), dtype=tf.int64)), -1)
            module.tables.append(table)
            idx = table.lookup(vs[0])
            return idx, anynull | (idx < 0)
        if op == "floordiv_const":
            return fdiv(vs[0], a["d"]), anynull
        if op == "floormod_const":
            return fmod(vs[0], a["d"]), anynull
        if op == "make_date":
            y, m, d = vs
            return days_from_civil(y, tf.clip_by_value(m, 1, 12), tf.clip_by_value(d, 1, 31)), anynull | ~valid_date(y, m, d)
        if op == "make_time":
            h, m, s = vs
            ok = (h >= 0) & (h < 24) & (m >= 0) & (m < 60) & (s >= 0) & (s < 60)
            return h * 3600000000 + m * 60000000 + s * 1000000, anynull | ~ok
        if op == "make_datetime":
            y, mo, d, h, mi, s = vs
            ok = valid_date(y, mo, d) & (h >= 0) & (h < 24) & (mi >= 0) & (mi < 60) & (s >= 0) & (s < 60)
            days = days_from_civil(y, tf.clip_by_value(mo, 1, 12), tf.clip_by_value(d, 1, 31))
            return days * US_DAY + h * 3600000000 + mi * 60000000 + s * 1000000, anynull | ~ok
        if op == "date_add":
            x, k = vs
            typ, unit = a["type"], a["unit"]
            if unit in ("MONTH", "QUARTER", "YEAR"):
                months = k * {"MONTH": 1, "QUARTER": 3, "YEAR": 12}[unit]
                if typ == "DATE":
                    return add_months(x, months), anynull
                days, us = split_us(x, typ)
                return add_months(days, months) * US_DAY + us, anynull
            if typ == "DATE":
                return x + k * (7 if unit == "WEEK" else 1), anynull
            step = UNIT_US.get(unit, US_DAY * 7)
            r = x + k * step
            if typ == "TIME":
                r = fmod(r, US_DAY)
            return r, anynull
        if op == "date_trunc":
            x = vs[0]
            typ, unit = a["type"], a["unit"]
            if typ == "DATE":
                return trunc_days(x, unit), anynull
            if unit in UNIT_US:
                u = UNIT_US[unit]
                return x - fmod(x, u), anynull
            days, _ = split_us(x, typ)
            return trunc_days(days, unit) * US_DAY, anynull
        if op == "date_diff":
            x, y = vs
            typ, unit = a["type"], a["unit"]
            if unit in UNIT_US and typ != "DATE":
                u = UNIT_US[unit]
                if typ == "TIMESTAMP":
                    return tf.truncatediv(x - y, u), anynull
                return fdiv(x, u) - fdiv(y, u), anynull
            dx = x if typ == "DATE" else fdiv(x, US_DAY)
            dy = y if typ == "DATE" else fdiv(y, US_DAY)
            if unit == "DAY":
                return dx - dy, anynull
            if unit in ("MONTH", "QUARTER", "YEAR"):
                yx, mx, _ = civil_from_days(dx)
                yy, my, _ = civil_from_days(dy)
                if unit == "MONTH":
                    return (yx * 12 + mx) - (yy * 12 + my), anynull
                if unit == "QUARTER":
                    return (yx * 4 + fdiv(mx - 1, 3)) - (yy * 4 + fdiv(my - 1, 3)), anynull
                return yx - yy, anynull
            if unit == "ISOYEAR":
                ix, _ = iso_year_week(dx)
                iy, _ = iso_year_week(dy)
                return ix - iy, anynull
            return fdiv(trunc_days(dx, unit) - trunc_days(dy, unit), 7), anynull
        if op == "extract":
            x = vs[0]
            typ, part = a["type"], a["part"]
            if typ == "DATE":
                days, us = x, tf.zeros_like(x)
            else:
                days, us = split_us(x, typ)
            y, m, d = civil_from_days(days)
            if part in ("MICROSECOND", "MILLISECOND", "SECOND", "MINUTE", "HOUR"):
                if part == "HOUR":
                    return fdiv(us, 3600000000), anynull
                if part == "MINUTE":
                    return fmod(fdiv(us, 60000000), 60), anynull
                if part == "SECOND":
                    return fmod(fdiv(us, 1000000), 60), anynull
                if part == "MILLISECOND":
                    return fdiv(fmod(us, 1000000), 1000), anynull
                return fmod(us, 1000000), anynull
            if part == "YEAR":
                return y, anynull
            if part == "MONTH":
                return m, anynull
            if part == "DAY":
                return d, anynull
            if part == "QUARTER":
                return fdiv(m - 1, 3) + 1, anynull
            if part == "DAYOFWEEK":
                return dow(days) + 1, anynull
            if part == "DAYOFYEAR":
                return day_of_year(days), anynull
            if part == "ISOYEAR":
                return iso_year_week(days)[0], anynull
            if part == "ISOWEEK":
                return iso_year_week(days)[1], anynull
            s = week_start(part)
            if s is not None:
                return week_of_year(days, s), anynull
            raise ValueError("EXTRACT " + part)
        if op == "format_date":
            return fmt(vs[0], a["type"], a["parts"]), anynull
        if op == "parse_date":
            v, bad = parse(vs[0], a["type"], a["parts"])
            return v, anynull | bad
        raise ValueError("op " + op)

    specs = [tf.TensorSpec([None], dtypes[s["dtype"]], name=s["name"]) for s in spec["inputs"]]

    def serve(*args):
        inputs = {}
        memo.clear()
        for s, x in zip(spec["inputs"], args):
            typ = s["type"]
            if s["dtype"] == "float64":
                null = tf.equal(x, nulls["float64"])
            elif s["dtype"] == "int64":
                null = tf.equal(x, tf.constant(nulls["int64"], tf.int64))
            elif s["dtype"] == "string":
                null = tf.equal(tf.strings.strip(x), nulls["string"])
            else:
                null = tf.zeros(tf.shape(x), tf.bool)
            v = x
            if typ in ("DATE", "DATETIME", "TIME", "TIMESTAMP"):
                v, bad = parse_input(tf.strings.strip(x), typ)
                null = null | bad
            inputs[s["name"]] = (v, null)
        if args:
            inputs["__n"] = args[0]
        else:
            inputs["__n"] = tf.zeros([1], tf.int64)
        out = {}
        for o in spec["outputs"]:
            v, null = ev(o["graph"], inputs)
            typ = o["type"]
            if typ in ("DATE", "DATETIME", "TIME"):
                v = output_string(v, typ)
            if v.dtype == tf.string:
                v = tf.where(null, nulls["string"], v)
            elif v.dtype == tf.int64:
                v = tf.where(null, tf.constant(nulls["int64"], tf.int64), v)
            elif v.dtype == tf.float64:
                v = tf.where(null, tf.constant(nulls["float64"], tf.float64), v)
            else:
                v = tf.where(null, tf.zeros_like(v), v)
            out[o["name"]] = v
        return out

    try:
        fn = tf.function(serve, input_signature=specs)
        module.serve = fn
        tf.saved_model.save(module, "/out/model", signatures={"serving_default": fn.get_concrete_function()})
    except ValueError as e:
        fail("convert", "the TRANSFORM could not be built in TensorFlow: " + str(e).strip()[:2000])
    except Exception as e:  # noqa: BLE001
        fail("convert", "TensorFlow could not save the preprocessing SavedModel: " + str(e).strip()[:2000])


def _rewrite(r):
    # GoogleSQL's REGEXP_REPLACE replacement: \1..\9 the groups, \0 the
    # match, \\ a backslash; RE2's rewrite (TensorFlow's) reads the same.
    return r


def _string_less(tf, a, b):
    # a < b by their bytes, element by element.
    ca = tf.strings.unicode_decode(a, "UTF-8").to_tensor(default_value=-1, shape=[None, None])
    cb = tf.strings.unicode_decode(b, "UTF-8").to_tensor(default_value=-1, shape=[None, None])
    w = tf.maximum(tf.shape(ca)[1], tf.shape(cb)[1])
    ca = tf.pad(ca, [[0, 0], [0, w - tf.shape(ca)[1]]], constant_values=-1)
    cb = tf.pad(cb, [[0, 0], [0, w - tf.shape(cb)[1]]], constant_values=-1)
    diff = tf.not_equal(ca, cb)
    first = tf.argmax(tf.cast(diff, tf.int32), axis=1, output_type=tf.int32)
    anyd = tf.reduce_any(diff, axis=1)
    pa = tf.gather(ca, first, batch_dims=1)
    pb = tf.gather(cb, first, batch_dims=1)
    return anyd & (pa < pb)

