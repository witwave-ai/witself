"""Independent encoder derived solely from profile.v1.md; no Go imports/calls."""
import base64
import decimal
import json
import sys


def encode_source(src):
    kind = src["kind"]
    result = {"v": 1, "t": kind}
    if kind == "null":
        pass
    elif kind in ("bool", "string"):
        result["value"] = src["value"]
    elif kind == "binary":
        result["value"] = base64.urlsafe_b64encode(bytes(src["bytes"])).decode("ascii").rstrip("=")
    elif kind == "decimal":
        number = decimal.Decimal(src["value"] + "0" * src.get("trailing_zeros", 0))
        sign, digits, exponent = number.as_tuple()
        coefficient = "".join(str(d) for d in digits).lstrip("0")
        if not coefficient:
            coefficient, exponent = "0", 0
        else:
            while coefficient.endswith("0"):
                coefficient, exponent = coefficient[:-1], exponent + 1
            if sign:
                coefficient = "-" + coefficient
        result.update(coefficient=coefficient, exponent=str(exponent))
    elif kind == "list":
        result["items"] = [encode_source(v) for v in src["items"]]
    elif kind == "object":
        result["fields"] = {f["name"]: encode_source(f["value"]) for f in src["fields"] if f["value"]["kind"] != "omitted"}
    elif kind == "domain":
        result.update(tag=src["tag"], value=encode_source(src["value"]))
    else:
        raise ValueError("unknown profile kind")
    return result


def canonical(value):
    # Python's independent sorted dictionary encoder follows scalar/UTF-8 order.
    raw = json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))
    for char in ("<", ">", "&", "\u2028", "\u2029"):
        raw = raw.replace(char, "\\u%04x" % ord(char))
    return raw


def encode_envelope(source):
    # Deep copy only standard JSON data, then normalize declared semantic sets.
    out = json.loads(json.dumps(source))
    out["basis"]["expected"].sort(key=lambda value: base64.urlsafe_b64decode(value.split("_", 1)[1] + "=="))
    out["basis"]["dependencies"].sort(key=canonical)
    if out["compound"] is not None:
        out["compound"]["required_fragments"].sort()
    return canonical(out)


if __name__ == "__main__":
    vectors = json.load(sys.stdin)
    if len(sys.argv) > 1 and sys.argv[1] == "--envelope":
        encoded = [encode_envelope(v["source"]) for v in vectors["valid"]]
    else:
        encoded = [canonical(encode_source(v["source"])) for v in vectors["valid"]]
    json.dump(encoded, sys.stdout, ensure_ascii=True)
