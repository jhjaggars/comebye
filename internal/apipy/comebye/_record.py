"""Strict, read-only records over the hub's JSON answers.

A record exposes exactly the fields its Go type has. Reading any other
attribute raises AttributeError with the closest field names, so a typo
(todo.title for todo.question) fails loudly instead of reading as empty.
"""

import datetime as _dt
import difflib
import re

_RECORDS = {}


def register(records):
    _RECORDS.update(records)


_TIME_RE = re.compile(
    r"^(\d{4})-(\d\d)-(\d\d)[T ](\d\d):(\d\d):(\d\d)(?:\.(\d+))?(Z|[+-]\d\d:?\d\d)?$"
)


def parse_time(s):
    """An RFC 3339 time (as the hub writes them) as an aware UTC datetime.

    None, "" and Go's zero time ("0001-01-01T00:00:00Z") are None.
    """
    if s is None or s == "":
        return None
    if isinstance(s, _dt.datetime):
        return s if s.tzinfo else s.replace(tzinfo=_dt.timezone.utc)
    m = _TIME_RE.match(s)
    if not m:
        raise ValueError("not an RFC 3339 time: %r" % (s,))
    y, mo, d, h, mi, sec, frac, tz = m.groups()
    if y == "0001" and mo == "01" and d == "01":
        return None
    us = int((frac or "0")[:6].ljust(6, "0"))
    t = _dt.datetime(int(y), int(mo), int(d), int(h), int(mi), int(sec), us, tzinfo=_dt.timezone.utc)
    if tz and tz != "Z":
        sign = 1 if tz[0] == "+" else -1
        hh, mm = int(tz[1:3]), int(tz[-2:])
        t -= sign * _dt.timedelta(hours=hh, minutes=mm)
    return t


def _zero(typ):
    if typ.endswith("?") or typ == "any" or typ == "time":
        return None
    if typ == "str":
        return ""
    if typ == "int":
        return 0
    if typ == "float":
        return 0.0
    if typ == "bool":
        return False
    if typ.startswith("["):
        return []
    if typ.startswith("{"):
        return {}
    return _convert(typ, {})  # a record that is never null


def _convert(typ, v):
    if typ.endswith("?"):
        return None if v is None else _convert(typ[:-1], v)
    if v is None:
        return _zero(typ)
    if typ == "time":
        return parse_time(v)
    if typ in ("str", "int", "float", "bool", "any"):
        return v
    if typ.startswith("["):
        inner = typ[1:-1]
        return [_convert(inner, x) for x in v]
    if typ.startswith("{"):
        inner = typ[1:-1]
        return {k: _convert(inner, x) for k, x in v.items()}
    cls = _RECORDS.get(typ)
    if cls is None:
        return v
    return cls(v)


class Record:
    """A hub record. Fields are attributes; see `comebye api types NAME`."""

    __slots__ = ("_raw", "_cache")
    _go = ""
    _fields = {}

    def __init__(self, raw):
        if not isinstance(raw, dict):
            raise TypeError("%s: want a JSON object, got %s" % (type(self).__name__, type(raw).__name__))
        object.__setattr__(self, "_raw", raw)
        object.__setattr__(self, "_cache", {})

    def __getattr__(self, name):
        spec = type(self)._fields.get(name)
        if spec is None:
            raise AttributeError(_no_field(type(self), name))
        cache = object.__getattribute__(self, "_cache")
        if name not in cache:
            key, typ = spec
            raw = object.__getattribute__(self, "_raw")
            cache[name] = _convert(typ, raw.get(key)) if key in raw else _zero(typ)
        return cache[name]

    def __setattr__(self, name, value):
        raise AttributeError("%s is read-only" % type(self).__name__)

    def __delattr__(self, name):
        raise AttributeError("%s is read-only" % type(self).__name__)

    def __dir__(self):
        return sorted(type(self)._fields)

    def __eq__(self, other):
        return type(self) is type(other) and self._raw == other._raw

    __hash__ = None  # compared by content; use an id field as a key

    def __getitem__(self, key):
        raise TypeError(
            "%s is a record, not a dict: use attributes (rec.%s); comebye.to_dict(rec) gives the JSON"
            % (type(self).__name__, key if isinstance(key, str) else "field")
        )

    def __repr__(self):
        parts = []
        raw = self._raw
        for attr, (key, typ) in type(self)._fields.items():
            v = raw.get(key)
            if v in (None, "", [], {}, False, 0) or typ.lstrip("[{").rstrip("]}?") in _RECORDS:
                continue
            s = repr(v)
            if len(s) > 60:
                s = s[:57] + "..."
            parts.append("%s=%s" % (attr, s))
            if len(parts) >= 6:
                parts.append("...")
                break
        return "%s(%s)" % (type(self).__name__, ", ".join(parts))


# Names scripts reach for that the hub spells differently.
_SYNONYMS = {
    "title": ("question", "name", "agent_name"),
    "text": ("question", "final_report", "body"),
    "status": ("state", "agent_status"),
    "state": ("status", "agent_status"),
    "name": ("agent_name",),
    "agent": ("agent_name", "agent_id"),
    "report": ("final_report", "last_pass_report"),
    "created": ("created_at",),
    "updated": ("updated_at",),
    "pr": ("branch",),
}


def _no_field(cls, name):
    names = list(cls._fields)
    close = [n for n in _SYNONYMS.get(name, ()) if n in cls._fields]
    for n in difflib.get_close_matches(name, names, n=3, cutoff=0.5):
        if n not in close:
            close.append(n)
    close = close[:3]
    msg = "%s has no field %r" % (cls.__name__, name)
    if close:
        msg += "; did you mean %s?" % " or ".join(repr(c) for c in close)
    msg += " (fields: %s; `comebye api types %s` describes them)" % (", ".join(names), cls.__name__)
    return msg


def to_dict(rec):
    """The JSON object behind a record (a plain dict; lists of records too)."""
    if isinstance(rec, Record):
        return rec._raw
    if isinstance(rec, list):
        return [to_dict(x) for x in rec]
    if isinstance(rec, dict):
        return {k: to_dict(v) for k, v in rec.items()}
    return rec


def fields(rec_or_class):
    """The field names of a record (or record class)."""
    cls = rec_or_class if isinstance(rec_or_class, type) else type(rec_or_class)
    return list(cls._fields)
