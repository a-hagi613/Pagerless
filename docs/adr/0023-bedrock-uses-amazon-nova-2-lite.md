# ADR-0023: Bedrock uses Amazon Nova 2 Lite through Converse

- **Status:** Accepted
- **Date:** 2026-11-17
- **Relates to:** [ADR-0010](0010-cost-model.md)

## Context

ADR-0010 named Claude Haiku 5.5 as the agent's model and left the provider as an implementation choice. The Anthropic API serves Haiku 5.5, but Claude on Amazon Bedrock is not available to this project's AWS account, so the Bedrock path could not run. Amazon Nova 2 Lite (`amazon.nova-2-lite-v1:0`) is generally available in the account through the geo cross-Region inference profile `us.amazon.nova-2-lite-v1:0`, verified ACTIVE in us-east-1 on 2026-11-17. Nova 2 Lite has no in-Region endpoint in us-east-1, and the profile routes a request to us-east-1, us-east-2 or us-west-2.

Nova does not accept the Messages protocol that the Anthropic SDK speaks. It is served through the Bedrock Converse API, whose request and response shapes differ from Messages: content blocks are `text`, `toolUse` and `toolResult`; stop reasons include `content_filtered` and `malformed_tool_use`; and token usage reports cache reads and writes in separate fields.

## Decision

The Bedrock provider uses Amazon Nova 2 Lite through Converse and the `us.amazon.nova-2-lite-v1:0` inference profile. The Anthropic provider keeps Claude Haiku 5.5 unchanged for the Anthropic API, local Replay capture and the evaluations.

- **Tool use.** Tool specifications keep their JSON schema, nested under `toolSpec.inputSchema.json`. Tool results become `toolResult` content blocks in a user message, and consecutive results share one message, as the graph's protocol requires.
- **Structured output.** Nova 2 Lite does not support Bedrock structured outputs (`outputConfig.textFormat`), so a stage that needs a Diagnosis or Proposal sends one tool with the graph's schema, forces it with `toolChoice: {tool: {name}}`, and treats its arguments as the structured value. Converse documents `toolChoice.tool` as supported by Amazon Nova.
- **Prompt caching.** Nova 2 Lite supports explicit prompt caching: at most four checkpoints, a 1K-token minimum per checkpoint, a five-minute TTL, and checkpoints in `system` and `messages` only, not in `tools`. The provider therefore places one `cachePoint` after the system prompt and sets none on the tools. Converse evaluates the prefix in the order tools, system, messages, so that checkpoint caches the tool definitions and the system prompt together. All input tokens, cached or not, count against the graph's budget.
- **Cost estimate.** Bedrock calls are priced at Nova 2 Lite's published Standard-tier US geo cross-Region rates, $0.33 per million input tokens and $2.75 per million output tokens; Anthropic and Replay calls keep Haiku 5.5's rates. The estimate prices every input token at the base rate, so it overstates cached reads and can understate cache writes; it is an estimate for the Triage record, not an invoice.

No paid Bedrock call is part of this change. A live investigation, a genuine Bedrock recording and any evaluation figures remain Phase 3 work.

## Consequences

- The two live providers no longer share a request adapter. A change to the graph's message protocol must be made in both the Messages and the Converse adapters.
- A Bedrock Job needs `AWS_REGION` set to a US region; the profile, not the account, selects the destination Region. Data stays in the US geography but not necessarily in us-east-1.
- Prompt caching only takes effect once the cumulative prefix reaches 1K tokens. A shorter prompt still succeeds, uncached, so the cache point is safe but not always a hit.
- Replay recordings and the published evaluation remain Claude Haiku 5.5 captures through the Anthropic API, so ADR-0016's provenance and normalization rules are unchanged. Any future figure produced on Nova must name the model that produced it.
- The Bedrock path now has a direct runtime dependency on `boto3`, pinned to the version that the Anthropic SDK's Bedrock extra already resolves.

## Alternatives considered

- **Claude Haiku 5.5 on Bedrock.** Keeps one model and one cost table, but Claude on Bedrock is not available to the account, so this path cannot run.
- **Nova 2 Lite structured outputs through `outputConfig.textFormat`.** Would remove the tool shim, but the Nova 2 Lite model card lists structured outputs as unsupported, and the request would be rejected.
- **Cache the tool definitions with a tool cache point.** The same model card restricts checkpoints to `system` and `messages`, so a tool checkpoint would be rejected.
- **Remove prompt caching on Bedrock.** Unnecessary: Nova 2 Lite supports explicit caching, and the cache point costs nothing when the prefix is below the minimum.
- **`InvokeModel` instead of Converse.** Converse is the API that carries tools, structured output and cache points in one provider-neutral shape, and it avoids model-specific request bodies.
