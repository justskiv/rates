# rates

Hourly reconciled exchange rates for 153 currencies, published as static
JSON files. A Go job fetches three upstream feeds, cross-checks them,
applies hard pegs and jump protection, and commits the result to the
`data` branch, which GitHub Pages and jsDelivr serve. There is no server:
a client request never reaches our code.

The files feed a currency converter app, but they are plain public data:
use them if they help.

## Files

| URL | Content | Changes |
|---|---|---|
| `https://rates.tuzov.dev/v1/latest.json` | current snapshot | only when the data changes (stable `ETag`, honest `304`) |
| `https://cdn.jsdelivr.net/gh/justskiv/rates@data/v1/latest.json` | mirror of the same file | after a purge, at most a few hours behind |
| `https://rates.tuzov.dev/v1/history/YYYY/YYYY-MM-DD.json` | snapshot in force at the end of that UTC day | never: written once when the day closes |
| `https://rates.tuzov.dev/v1/history/index.json` | first and last day, missing days | daily |
| `https://rates.tuzov.dev/v1/health.json` | outcome of the last run, source status | every run |

The `rates.tuzov.dev` URLs go live when Pages is switched on; until then
the files are on the `data` branch only.

```json
{"schemaVersion":1,"base":"USD","asOf":"2026-11-01T00:00:00Z",
 "generatedAt":"2026-11-01T22:23:41Z","source":"frankfurter-v2-blend",
 "rates":{"AED":3.6725,"EUR":0.89478,"JPY":157.66,"USD":1},
 "flags":{"AED":["pegged"],"BWP":["disputed"]}}
```

- `rates[C]` is units of `C` per 1 USD; a cross rate A→B is
  `rates[B] / rates[A]`.
- `asOf` is when the rates were observed (a day, 00:00:00Z), not when the
  file was written. It never goes back.
- `flags`: `pegged` (from the peg table), `disputed` (sources disagree),
  `secondary` (primary source had no value), `held` (a suspicious jump was
  rejected, previous value kept), `stale` (no fresh value), `multiRate`
  (official and street rates differ a lot), `override` (set by hand).
  Ignore flags you do not know.
- The format is [`contracts/rates.schema.json`](contracts/rates.schema.json)
  (JSON Schema 2020-12). Additive changes keep `schemaVersion` 1; a breaking
  change would be published under `/v2/`.

## Sources

| Source | Role | Terms |
|---|---|---|
| [Frankfurter](https://frankfurter.dev) v2, blended rate | published value | "free to use", no attribution required |
| [fawazahmed0/exchange-api](https://github.com/fawazahmed0/exchange-api) | second vote, fallback value | CC0 |
| [ExchangeRate-API open access](https://www.exchangerate-api.com) | third vote only | values are never published, stored, or logged: their terms forbid redistribution. Rates By Exchange Rate API |

A value is published from Frankfurter when one of the two other feeds
agrees within 2%; when both others agree against it, the fawazahmed0 value
is published as `disputed`. Pegs (AED, SAR, XOF via EUR and others) come
from a fixed table. A move of more than 15% (50% for high-inflation
currencies) is held until it repeats over several runs with both
publishable sources agreeing. A run that would publish fewer than 140
fresh currencies, or more than 10 held ones, publishes nothing and the
previous file stays.

The data is provided as is, without warranty. Check `asOf` and `flags`
before relying on a value.

## Repository

| Ref | Content |
|---|---|
| `main` | Go code, workflows, `contracts/` |
| `rates-v*` tags | job releases; the publish workflow builds the tag in the `RATES_JOB_REF` variable |
| `data` | published files (`v1/**`), plus `CNAME`, `.nojekyll`, `README.md` that the job never touches |
| `archive` | `state/`, raw upstream bodies of Frankfurter and fawazahmed0 in `raw/`, `overrides.json` |

`contracts/` is a generated, read-only export of the app's catalog, peg
table and schema; edits belong in the source repository. CI rejects files
that do not match `contracts/SHA256SUMS`.

## Run locally

Go 1.27.3 or newer; the binary uses the standard library only.

```sh
go test -race ./...
go run ./cmd/ratesjob run --public out/public --state out/state \
  --archive out/raw --overrides out/overrides.json --precompress=false
go run ./cmd/ratesjob verify --dir out/public
```

With [Task](https://taskfile.dev): `task check`, `task fuzz`,
`task run:local`, `task verify:local`.

## License

Code: [MIT](LICENSE). Published values come from Frankfurter (free to use)
and fawazahmed0 (CC0).
