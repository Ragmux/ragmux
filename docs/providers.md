# Providers

A **model connection** is one provider type plus a base URL, an API key and a model
name. Projects route chat requests through one connection; RAG stores use one for
embeddings. Credentials are AES-256-GCM encrypted with `SECRET_KEY` and never leave the
gateway. Connections are managed with `POST /admin/api/models` (see the
[API reference](api.md#model-connections)) or in the dashboard's **Models** tab;
`GET /admin/api/provider-types` lists the types with their defaults.

## Provider types

| `provider_type` | Default `base_url` | Chat endpoint used | Embeddings | Credentials |
|---|---|---|---|---|
| `openai` | `https://api.openai.com/v1` | `POST {base}/chat/completions` | yes — `POST {base}/embeddings` | `Authorization: Bearer <api_key>` |
| `anthropic` | `https://api.anthropic.com` | `POST {base}/v1/messages` | no | `x-api-key: <api_key>`, `anthropic-version: 2023-06-01` |
| `gemini` | `https://generativelanguage.googleapis.com/v1beta` | `POST {base}/models/{model}:generateContent` (`:streamGenerateContent?alt=sse` when streaming) | yes — `POST {base}/models/{model}:batchEmbedContents` | `x-goog-api-key: <api_key>` |
| `deepseek` | `https://api.deepseek.com/v1` | `POST {base}/chat/completions` | reported as unsupported (`supports_embeddings: false`) | `Authorization: Bearer <api_key>` |
| `ollama` | `http://localhost:11434` | `POST {base}/api/chat` (native API) | yes — `POST {base}/api/embed` | none; an `api_key` is sent as `Authorization: Bearer` for a proxy in front of Ollama |
| `custom_openai` | *(required; dashboard suggests `http://localhost:8000/v1`)* | `POST {base}/chat/completions` | yes — `POST {base}/embeddings` | `Authorization: Bearer <api_key>` (optional) |

Base URL handling: a trailing `/` is trimmed. For `openai` and `deepseek` a `/v1` suffix
is appended when the URL does not already end in one. For `custom_openai` the URL is used
exactly as given (so include `/v1` yourself, e.g. `http://vllm:8000/v1`). For `ollama` a
trailing `/v1` is removed because the native endpoints live at the root. When the gateway
runs in Docker, local servers on the host are reachable as `http://host.docker.internal:<port>`.

A `base_url` must be a plain `http://` or `https://` URL with a host and without
credentials, query string or fragment. Hosts on private or local networks (the Docker
host, a Compose service, `localhost`) are refused unless they are listed in
`PRIVATE_UPSTREAM_ALLOWLIST` or `ALLOW_PRIVATE_UPSTREAMS=true` is set — see
[Private upstreams](configuration.md#private-upstreams). `model_name` may contain letters,
digits and `. _ : / @ -` (up to 128 characters, no leading `/`, no `..`); for `gemini` it
is sent as a single escaped path segment.

`POST /admin/api/models/{id}/test` with `{"mode":"chat"}` or `{"mode":"embedding"}`
sends a ping through the connection and reports the reply, or the dimensions for
embeddings, together with latency; the outcome is kept on the connection
(`last_test_at`, `last_test_ok`, `last_test_latency_ms`, `last_test_error`). The same
test runs on a connection that is not saved yet through `POST /admin/api/models/test`,
which takes the create body plus `mode` and, with an empty `api_key`, a `connection_id`
whose stored key is reused. Each connection also reports `private_upstream`: whether its
`base_url` pointed at a loopback, private or link-local address when it was saved. See
[REST API reference](api.md#model-connections).

`GET /admin/api/provider-types` lists the types with their capabilities:
`supports_embeddings`, `requires_api_key`, `supports_streaming` (every adapter) and
`supports_tools` (every adapter except `gemini`, see below).

## OpenAI-compatible (`openai`, `deepseek`, `custom_openai`)

The client request is forwarded as-is apart from `model` (replaced by the connection's
model name), `stream` (set by the gateway) and `stream_options` (set to
`{"include_usage": true}` on streams so token counts are recorded; removed on
non-streaming calls). Responses and SSE chunks are relayed in the OpenAI schema.

`custom_openai` covers vLLM, LM Studio, LiteLLM, text-generation-inference's OpenAI
route, Ollama's `/v1` shim and any other server speaking the Chat Completions format.

Prompt caching on these providers is automatic and server-side: nothing is sent for it,
and `prompt_tokens_details.cached_tokens` (DeepSeek's `prompt_cache_hit_tokens`) is
surfaced back to the client. See [Prompt caching](#prompt-caching).

### Request field passthrough (`Extra`)

`/v1/chat/completions` parses the known fields (`model`, `messages`, `stream`,
`temperature`, `top_p`, `max_tokens`, `max_completion_tokens`, `stop`, `n`, `tools`,
`tool_choice`, `response_format`, `stream_options`, `user`) and keeps every other
top-level field verbatim. For OpenAI-compatible providers those extra fields are sent to
the upstream unchanged, so provider-specific options (`seed`, `logprobs`, `reasoning_effort`,
vLLM's `guided_json`, …) work without gateway support. For `ollama` a defined subset is
mapped (below); `anthropic` and `gemini` ignore unknown fields.

## Anthropic

Requests are translated to the Messages API and responses back to the OpenAI schema:

- `system` and `developer` messages are removed from the message list, joined with blank
  lines and sent as the top-level `system` field (this is where the project system prompt
  and RAG context end up). With a `cache_control` on one of them the field becomes an
  array of blocks instead — see [Prompt caching](#prompt-caching).
- `max_tokens` is required by Anthropic; when the client sends neither `max_tokens` nor
  `max_completion_tokens` the gateway uses **4096**.
- `temperature`, `top_p` and `stop` are mapped; `n`, `response_format`, `user` and unknown
  fields are dropped.
- Consecutive messages with the same role are merged, as the Messages API requires.
- Tools: OpenAI `tools[].function` definitions become Anthropic `tools` (`input_schema`
  from `parameters`, `{"type":"object","properties":{}}` when absent); `tool_choice`
  `"auto"` → `{"type":"auto"}`, `"required"` → `{"type":"any"}`, a named function →
  `{"type":"tool","name":…}`, `"none"` removes the tools. Assistant `tool_calls` become
  `tool_use` blocks, `tool` messages become `tool_result` blocks, and `tool_use` in the
  reply becomes OpenAI `tool_calls` (also in streams, via `input_json_delta`).
- Image parts: `image_url` with a `data:` URL is sent as a base64 image block, any other
  URL as a URL image block.
- Finish reasons: `end_turn`/`stop_sequence` → `stop`, `max_tokens` → `length`,
  `tool_use` → `tool_calls`. Usage is mapped to `prompt_tokens`/`completion_tokens`,
  including on streams; see [Prompt caching](#prompt-caching) for how the cache
  counters are folded in.

Anthropic has no embeddings API; choose another connection for RAG stores.

## Gemini

Requests are translated to `generateContent` / `streamGenerateContent`:

- `system`/`developer` messages become `systemInstruction`; `assistant` turns are sent
  with role `model`; `tool` messages are sent as user text.
- `temperature`, `top_p`, `max_tokens`/`max_completion_tokens` and `stop` map to
  `generationConfig` (`topP`, `maxOutputTokens`, `stopSequences`); a `response_format`
  whose `type` starts with `json` sets `responseMimeType: application/json`.
- Image parts: `image_url` with a `data:` URL becomes `inlineData` (MIME type taken from
  the data URL), any other URL becomes `fileData.fileUri`.
- **Tool calling is not supported yet**: a request with `tools` answers
  `400 {"error":{"type":"invalid_request_error","message":"tool calling is not supported for gemini connections in this gateway version"}}`.
- Finish reasons: `STOP` → `stop`, `MAX_TOKENS` → `length`, safety-related reasons
  (`SAFETY`, `RECITATION`, `BLOCKLIST`, `PROHIBITED_CONTENT`, `SPII`) → `content_filter`.
  `usageMetadata` is mapped to OpenAI usage, including `cachedContentTokenCount` — see
  [Prompt caching](#prompt-caching). Explicit caching (`cachedContents`) is not supported.
- Embeddings use `batchEmbedContents`; the model name gets a `models/` prefix if missing.

## Ollama (native API)

The `ollama` type talks to Ollama's own `/api/chat` (one JSON object, or newline-delimited
JSON objects when streaming) and `/api/embed` endpoints instead of the OpenAI shim, which
gives access to Ollama-only options:

- `temperature`, `top_p`, `max_tokens` (→ `num_predict`) and `stop` are placed in
  `options`.
- Extra request fields are mapped: `keep_alive` is passed through as-is, `num_ctx` is
  added to `options`, and an `options` object is merged into `options` (any Ollama model
  option, e.g. `{"options":{"num_gpu":1,"repeat_penalty":1.1}}`).
- `response_format` `json_object` sets `format: "json"`; `json_schema` sends the schema as
  `format`.
- Tools use OpenAI's `{type:"function", function:{…}}` shape natively, so `tools` are
  forwarded; assistant `tool_calls` and `tool` messages (with `tool_name`) are translated
  both ways.
- Images must be inline base64 `data:` URLs; other `image_url` values are rejected with
  `400`.
- Finish reasons: `stop` → `stop`, `length` → `length`; usage is built from
  `prompt_eval_count` / `eval_count`. Ollama has no prompt cache, so no cache fields are
  reported and the shipped price table charges `ollama` models nothing.

Ollama itself is unauthenticated; if you set an `api_key` it is sent as
`Authorization: Bearer …`, which is useful behind an authenticating reverse proxy.

Ollama usually runs on a private address, which the gateway refuses by default. Add its
hostname to the allowlist in `.env` before creating the connection:

```bash
PRIVATE_UPSTREAM_ALLOWLIST=host.docker.internal,ollama   # Docker host, or a Compose service named "ollama"
```

Then use `base_url` `http://host.docker.internal:11434` (Ollama on the host) or
`http://ollama:11434` (Ollama as a Compose service). `ALLOW_PRIVATE_UPSTREAMS=true`
allows every private host instead; see
[Private upstreams](configuration.md#private-upstreams).

To use Ollama's OpenAI-compatible endpoint instead, create a `custom_openai` connection
with `base_url` ending in `/v1` (for example `http://host.docker.internal:11434/v1`).
The native type is preferable for `keep_alive`, `num_ctx` and batch embeddings.

## Prompt caching

Providers cache a repeated prompt prefix and charge less for the cached part. Ragmux
passes a client's caching intent through where the provider has a field for it, and
always reports back what the provider said it cached. Both sides feed the
[cost estimate](api.md#model-prices) shown on request logs.

### Response side: one accounting for every provider

`usage` gains OpenAI's breakdown objects, so the same fields mean the same thing on
every connection:

```json
"usage": {"prompt_tokens": 250, "completion_tokens": 500, "total_tokens": 750,
          "prompt_tokens_details": {"cached_tokens": 200, "cache_creation_tokens": 40},
          "completion_tokens_details": {"reasoning_tokens": 30}}
```

`prompt_tokens` **always includes** the cached and freshly written parts, and
`prompt_tokens + completion_tokens == total_tokens` holds everywhere.
`cache_creation_tokens` has no OpenAI equivalent — OpenAI does not bill cache writes,
Anthropic does.

| Provider | What it reports | How it is mapped |
|---|---|---|
| `openai` | `prompt_tokens_details.cached_tokens` (already inside `prompt_tokens`) | passed through unchanged |
| `deepseek` | `prompt_cache_hit_tokens` / `prompt_cache_miss_tokens` at the top level (their sum is `prompt_tokens`) | the hit count is folded into `cached_tokens`; the top-level fields are still relayed |
| `anthropic` | `input_tokens` **excluding** `cache_creation_input_tokens` and `cache_read_input_tokens` | all three are added up into `prompt_tokens`; reads become `cached_tokens`, writes `cache_creation_tokens` |
| `gemini` | `usageMetadata.cachedContentTokenCount` (already inside `promptTokenCount`) | becomes `cached_tokens`; `thoughtsTokenCount` keeps counting as completion tokens and is also reported as `reasoning_tokens` |
| `ollama` | nothing; Ollama has no prompt cache | no cache fields |

> **Change from 0.3.x:** for a cached Anthropic request, `prompt_tokens` is now larger.
> Earlier releases reported Anthropic's `input_tokens` verbatim and so under-reported a
> cached prompt by the size of its cached prefix. Recorded token budgets and metrics for
> such requests move up accordingly.

### Request side: `cache_control`

**Anthropic** takes a `cache_control` marker in three places, and Ragmux relays each of
them verbatim (the gateway never inspects or invents one):

| Where the client puts it | What is sent upstream |
|---|---|
| on a content part: `{"type":"text","text":"…","cache_control":{"type":"ephemeral"}}` | copied onto the matching Anthropic content block |
| on a tool, at the top level *or* inside `function` | copied onto the Anthropic tool object |
| on a `system`/`developer` message's content part | the `system` field becomes an array of blocks, one per system message, carrying the marker |

With no `cache_control` anywhere, `system` stays the plain joined string it has always
been — a request that does not ask for caching is byte-identical to a 0.3.x one. When a
message has several marked parts the last marker wins, since Anthropic caches the prefix
up to and including the marked block.

**OpenAI, DeepSeek and `custom_openai`: nothing is sent.** Their caching is automatic and
server-side; there is no request field to emit, so Ragmux emits none. A `cache_control` a
client embeds rides along untouched (message content is relayed raw) and OpenAI ignores
it. Only the response side matters here — this is not a gap.

**Gemini and Ollama: response side only.** Gemini's explicit caching needs a stateful
`cachedContents` resource created and referenced across requests, which does not fit a
stateless passthrough; its implicit caching happens automatically and is reported in
`cachedContentTokenCount`. Ollama has no prompt cache at all.

The project system prompt and the RAG context block the gateway injects are the stable
prefix most worth a breakpoint, but the gateway does not mark one: a project-level
`cache_prompt` switch needs a column and dashboard work of its own and is not in 0.4.

## Model field echo

The `model` value in a client request never selects the upstream model — the project's
connection does. The gateway replaces it with the connection's model name before calling
the provider and echoes the client's original value back in the response (and in every
stream chunk) so SDKs that compare the field stay happy. When the client sends an empty
`model`, the connection's model name is returned instead. `GET /v1/models` lists that
model name with `owned_by` set to the provider type.

## Error relay and redaction

Provider errors are relayed with the upstream status and message (cut to 512
characters) in the OpenAI error envelope. Transport failures are never relayed verbatim:
they become `502 upstream_error` with one of `upstream unreachable`, `upstream TLS
handshake failed`, `upstream returned a non-HTTP response`, `upstream redirect
rejected: …`, the private-address message from
[Private upstreams](configuration.md#private-upstreams), or `upstream request failed`;
timeouts become `504 timeout` / `upstream request timed out`. The raw cause is logged at
warn level with the upstream host. Streaming responses end with an error after
`STREAM_MAX_DURATION` or `STREAM_MAX_BYTES_MB`.

Before an upstream message reaches a client, the request log or the gateway's own log,
the connection's own API key and anything matching `Bearer …`, `Basic …`, a JWT
(`eyJ…`), `sk-…`, `sk-ant-…`, `AIza…`, `ya29.…`, `gsk_…`, `hf_…`, `xai-…` or credentials
embedded in a URL (`https://user:pass@host`) is replaced by `[redacted]`. This covers
Anthropic stream `error` events and Ollama error bodies as well.
