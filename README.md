# rates: job archive

Working state of the rates job, not meant for consumers:

- `state/` — what the job remembers between runs (validators, pending
  jumps, last published values);
- `raw/` — gzipped upstream bodies of Frankfurter and fawazahmed0, kept to
  audit and rebuild history. ExchangeRate-API responses are never stored;
- `overrides.json` — manual overrides, optional, edited by hand.

Published files live on the `data` branch.
