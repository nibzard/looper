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

Looper resolves every model ID in the pinned catalog. It uses exact IDs,
model matches, and date conditions from the feed. Cache reads, cache
writes, one-hour writes, and reported web searches have separate rates.
A model alias ending in `[1m]` keeps its display name and uses the canonical model
for price matching.

An explicit provider selects only that provider's catalog. Without a provider,
Claude names default to Anthropic. GPT, `o1`/`o3`/`o4`, and `codex-mini` names
default to OpenAI. GLM names default to Z.AI. These defaults apply only when
the provider has a matching rule. Otherwise, Looper searches all providers for
an exact ID, then a matching rule. Catalog order resolves provider ties.
Within a provider, exact IDs take precedence. The longest model ID wins among
matching rules. Supply the provider to select a specific rate for a shared ID.

An empty price object (`{}`) is an explicit free marker and produces a zero
estimate. Missing prices remain unknown. Resolving a catalog entry does not
guarantee an estimate: usage must include the units and context details that
its rates require.

Aggregated agent usage cannot identify each request's context length. If the
applicable rates require a context tier, Looper leaves the cost unknown.
Unknown models and nonstandard service tiers also remain unpriced. It does not
infer a model from a CLI default or measure tokens from transcript length.

Costs are API equivalents, not invoices or subscription charges. Agent-reported
costs remain separate, including reports whose upstream cost basis is unknown.
