# Upstream consensus vectors

`unified_sighash.json` is copied without alteration from Bitcoin Knots
`v29.4.1.knots20260508`, `src/test/data/unified_sighash.json`.
Source: https://github.com/bitcoinknots/bitcoin/blob/v29.4.1.knots20260508/src/test/data/unified_sighash.json

Copyright Bitcoin Knots / Bitcoin Core developers. Distributed under the MIT
license: https://github.com/bitcoinknots/bitcoin/blob/v29.4.1.knots20260508/COPYING

Our signer intentionally implements only SIGHASH_ALL|UNIFIED with segwit v0.
The unit test runs every applicable upstream vector, and the integration tests
ask the actual fork node to validate signatures and mine their spends.
