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
embeddings, together with latency.

## OpenAI-compatible (`openai`, `deepseek`, `custom_openai`)

The client request is forwarded as-is apart from `model` (replaced by the connection's
model name), `stream` (set by the gateway) and `stream_options` (set to
`{"include_usage": true}` on streams so token counts are recorded; removed on
non-streaming calls). Responses and SSE chunks are relayed in the OpenAI schema.

`custom_openai` covers vLLM, LM Studio, LiteLLM, text-generation-inference's OpenAI
route, Ollama's `/v1` shim and any other server speaking the Chat Completions format.

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
  and RAG context end up).
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
  `tool_use` → `tool_calls`. Usage (`input_tokens`, `output_tokens`) is mapped to
  `prompt_tokens`/`completion_tokens`, including on streams.

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
  `usageMetadata` is mapped to OpenAI usage.
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
  `prompt_eval_count` / `eval_count`.

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
