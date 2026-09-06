# Binance fixtures

Recorded, redacted exchange payloads. Development runs against these files, never against
the live API (`CLAUDE.md` §2).

Every file carries a `_source` key saying where its bytes came from:

- `recorded` — captured from a real account by `plimsollctl record`, redacted in the write
  path. The API key header, account ids and address fields are replaced; every numeric
  field is kept exactly as sent, as a string.
- `documented` — transcribed from the field list in the official documentation, because no
  real key was available when the code was written. The shape is the vendor's; the values
  are chosen to exercise a case. **Replace with a recorded payload when a key exists** —
  a documented example proves the parser handles the shape Binance publishes, not the
  shape Binance sends.
- `derived` — a hand-edited variant of one of the above, to reach a case a real account
  will not produce on demand (an over-permissioned key, a malformed frame).

A payload that is a JSON **array** -- `myTrades`, the history endpoints -- has nowhere to
put a key, so `plimsollctl record` wraps it: the metadata sits at the top level and the
exchange's bytes sit under `payload`. Object payloads keep the metadata inline, as below.
Wrapping one shape rather than annotating none is deliberate: the alternative is a file
committed with no provenance at all.

`_source` and `_why` are ours. Nothing reads them but a human; the parser ignores unknown
fields by design, which is the same property that makes them safe to add.
