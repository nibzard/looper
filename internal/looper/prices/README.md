# Model price snapshot

The data comes from [Pydantic GenAI Prices](https://github.com/pydantic/genai-prices).
It uses the current v2 feed. The old flat v1 feed is frozen.

`source.json` records the upstream commit, retrieval date, URL, and checksum.
`LICENSE` contains the upstream MIT license. The Go binary embeds `data.json`.
The terminal interface never fetches prices during a run.

Refresh the snapshot and rebuild:

```sh
python3 scripts/update-prices.py
make build
```

Use `--revision <commit>` to select an upstream revision. Existing usage records
retain their original estimate and pricing revision. Updates affect new records.

Looper estimates standard public API text usage for OpenAI, Anthropic, and Z.AI.
It uses model matches and date conditions from the feed. Cache reads, cache
writes, one-hour writes, and reported web searches have separate rates.
A model alias ending in `[1m]` keeps its display name and uses the canonical model
for price matching.

Aggregated agent usage cannot identify each request's context length. If the
applicable rates require a context tier, Looper leaves the cost unknown.
Unknown models and nonstandard service tiers also remain unpriced. It does not
infer a model from a CLI default or measure tokens from transcript length.

Costs are API equivalents, not invoices or subscription charges. Agent-reported
costs remain separate, including reports whose upstream cost basis is unknown.
