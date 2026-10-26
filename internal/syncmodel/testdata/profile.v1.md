# Synthetic canonical JSON profile v1

These vectors freeze the LC-S5B harness normalization only. Any change requires
a new protocol version and reviewed vectors. This is not deployment ratification.

Each typed node is a JSON object with integer `v:1` and `t` from null, bool,
string, decimal, binary, list, object, domain. The exact additional fields are:
null: none; bool/string/binary: `value`; decimal: `coefficient`,`exponent`;
list: `items`; object: `fields`; domain: `tag`,`value`. Unknown, missing,
duplicate decoded names and invalid Unicode are errors. No bare payload number.
Object `fields` maps distinct Unicode names to nodes. Omission is a construction
state, never a wire node: omitted fields disappear, explicit null nodes remain.
Lists preserve order. Domain tags are the twelve reviewed catalog family names;
family-specific envelope basis validation narrows the tagged content further.

Decimal source grammar is JSON number grammar, parsed exactly. Represent value
as signed integer coefficient times ten to the signed exponent. Strip leading
coefficient zeros and move trailing zeros into the exponent. All zero forms,
including negative zero, normalize to coefficient `0`, exponent `0`. No plus,
leading zeros or negative zero in either normalized integer string. Coefficient
has at most 4096 digits; exponent is within -1000000..1000000. Precision is never
binary floating point. Decimal lexemes are at most 8192 bytes.

Binary is RFC 4648 URL alphabet, unpadded, with zero unused tail bits. IDs are
16 bytes with respectively op_, transfer_, restore_, revision_, generation_
prefixes followed by this encoding. Commitments use commitment_ plus 32 bytes.
Typed counter wire values are canonical unsigned decimal strings: lifecycle
0..9007199254740991; capture and graph epochs 1..9223372036854775807; graph revision
and ordinal 0..9223372036854775807. Limits follow synthetic bigint storage.

Canonical JSON has no insignificant whitespace, keys sorted by UTF-8 bytes,
and no Unicode normalization. Strings use literal valid Unicode except quote,
backslash, ASCII controls, `<`, `>`, `&`, U+2028 and U+2029. Escape quote/backslash
with a backslash, backspace/form-feed/newline/carriage-return/tab with b/f/n/r/t,
and remaining controls and the five excluded characters with lowercase four-digit
Unicode escapes. Slash is literal. Surrogate pairs decode to Unicode scalars;
unpaired surrogates and invalid UTF-8 refuse. Schema versions are JSON integers;
no floating point value is accepted by the canonical API. A semantic decoder may
accept whitespace/key order/ordinary escape alternatives, then emits canonical
bytes. Normalized typed decimals, identifiers, counters and binary must already
have canonical spellings on input. Maximum complete encoding is 65536 bytes,
maximum typed nesting depth is 64. Duplicate keys compare after JSON unescaping.

The reference encoder consumes independent source descriptors, uses Python
Decimal tuples, and never invokes the Go implementation or its helpers. The Go
vector test requires Python; absence fails rather than silently skipping.

An envelope uses exact snake_case schema fields, every field present; pointer
absence is explicit JSON null. Expected revision sets sort by decoded 16-byte
identity; dependencies sort by complete canonical dependency bytes; required
fragment sets sort by UTF-8 names. Duplicate members refuse. Ordered compound
members and payload lists preserve order. The complete envelope vectors freeze
these rules as well as nested identity and decimal wire forms.
