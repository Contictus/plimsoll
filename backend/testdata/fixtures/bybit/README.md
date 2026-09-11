# Bybit fixtures

The same rule as `../binance/README.md`: every file says where its bytes came from, and a
`documented` example is never mistaken for a `recorded` one.

- `recorded` — captured from a real account by `plimsollctl record`, redacted in the write
  path. No file here is `recorded` yet: no Bybit key exists (see `docs/PROJECT.md`, M8).
- `documented` — transcribed from the field list and enum pages in the official
  documentation (`docs/BYBIT-API-NOTES.md`, B1–B5). The shape is the vendor's; the values are
  chosen to exercise a case. **Replace with a recorded payload when a key exists** — a
  documented example proves the parser handles the shape Bybit publishes, not the shape Bybit
  sends.
- `derived` — a hand-edited variant of one of the above, to reach a case a real account will
  not produce on demand (an over-permissioned key, a status the enum lists but an account
  rarely reaches).

Bybit wraps every response in `{retCode, retMsg, result}`. The fixtures here hold the
**`result` object only**, because that is what the client hands the normalizer — the envelope
is the client's business and is tested there.
