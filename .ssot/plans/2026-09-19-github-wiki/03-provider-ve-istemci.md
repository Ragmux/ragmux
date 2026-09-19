# Faz 03 — Provider Setup + Client Integrations

- **Durum:** tamam (review CHANGES REQUESTED → 9 düzeltme uygulandı)
- **Sahip:** backend-engineer
- **Bağımlılık:** Faz 01
- **İlgili karar:** ADR-005 (görsel indirme doygunluğu 429)

Bu brief kendi kendine yeter. Ortak kurallar için **önce** aynı dizindeki
`00-plan.md`'yi oku — bağlayıcıdır.

## Amaç

İki yönü de reçeteye dökmek: Ragmux'ı bir sağlayıcıya bağlamak, ve bir
uygulamayı Ragmux'a bağlamak.

## Kapsam

### `Provider-Setup.md`

Sekiz sağlayıcı tipi. Her biri aynı kalıpta: **ne zaman → connection oluşturma
çağrısı → doğrulama → tipe özgü tuzak**.

Ortak açılış: bağlantılar `POST /admin/api/models` ile oluşturulur;
sağlayıcı kimlik bilgileri sunucuda AES-256-GCM ile şifreli durur ve bir daha
okunmaz. Kaydetmeden önce `POST /admin/api/models/test`, kayıtlıyı sınamak için
`POST /admin/api/models/{id}/test` (`{"mode":"chat"|"embedding"}`).

| tip | `base_url` varsayılanı | auth | not |
|---|---|---|---|
| `openai` | `https://api.openai.com/v1` | `Authorization: Bearer` | chat + embeddings |
| `anthropic` | `https://api.anthropic.com` | `x-api-key` + `anthropic-version: 2023-06-01` | embedding yok |
| `gemini` | `…/v1beta` | `x-goog-api-key` | chat + embeddings |
| `deepseek` | `https://api.deepseek.com/v1` | `Authorization: Bearer` | embedding yok |
| `ollama` | `http://localhost:11434` | yok (proxy için opsiyonel bearer) | native `/api/chat` |
| `custom_openai` | **yok, zorunlu** | opsiyonel bearer | URL aynen kullanılır |
| `cohere_rerank` | `https://api.cohere.com` | bearer | yalnız rerank |
| `voyage_rerank` | `https://api.voyageai.com` | bearer | yalnız rerank |

Tipe özgü, mutlaka yazılacak tuzaklar:

- **Ollama ve her özel ağ adresi:** `base_url` özel/loopback bir adrese
  çözülüyorsa SSRF koruması reddeder. Hata metni hangi ayarı istediğini söyler.
  Çözüm: `PRIVATE_UPSTREAM_ALLOWLIST=host.docker.internal,ollama` (hostname
  bazlı, tercih edilen) ya da güvenilen bir ağda `ALLOW_PRIVATE_UPSTREAMS=true`
  (filtreyi tümden kapatır **ve** sağlayıcı çağrıları için
  `HTTP_PROXY`/`HTTPS_PROXY`'yi yeniden açar). Bağlantıyı eklemeden **önce**
  ayarlanmalı.
- **`custom_openai`:** `base_url` neyse o kullanılır — `/v1`'i sen yazarsın.
  vLLM, LM Studio, LiteLLM örnekleri.
- **`gemini`:** tool şeması budanıyor. Gemini'nin kabul etmediği JSON Schema
  anahtarları yeniden yazılıyor ya da düşürülüyor; tam tablo
  `ragmux.com/docs/providers/#tool-schema-sanitising`. Wiki yalnız "şeman
  aynen gitmeyebilir, tabloya bak" der.
- **`anthropic`:** prompt caching istek tarafı yalnız burada geçerli
  (`cache_control` passthrough). 0.4'te cache'li isteklerde `prompt_tokens`
  artık **büyük** sayıyı raporluyor (önceki sürümler eksik sayıyordu) —
  yükseltmede bütçe ve metrik rakamları sıçrar.
- **rerank tipleri:** chat ya da embedding olarak kullanılamaz; registry
  bunları açıkça reddeder. Yalnız bir RAG store'un `rerank_connection_id`'si
  olurlar.
- **Embedding desteği:** `anthropic` ve `deepseek` embedding vermez; RAG store
  için ayrı bir embedding bağlantısı gerekir.

Kapanış: capability matrisi (streaming / embeddings / tools / tool streaming /
vision / remote images / prompt caching / cached token usage / rerank)
**linklenir**, kopyalanmaz — `ragmux.com/docs/providers/#capabilities`. Canlı
hâli `GET /admin/api/provider-types`.

Bir de kısa **Images** bölümü: hangi sağlayıcı uzak URL'yi kendi çekiyor,
hangisi için gateway indirip inline gönderiyor (`gemini`, `ollama`);
`IMAGE_FETCH=false` böyle bir isteği `400` ile reddeder; doygunlukta `429`
`image_fetch_saturated` + `Retry-After` döner (**ADR-005**) — bu bir kaynak
sınırı, arıza değil. Kabul edilen medya tipleri sağlayıcıya göre değişir,
tablo docs'ta.

### `Client-Integrations.md`

1. **The idea in one paragraph.** `base_url` ve `api_key` değiştir, gerisi
   aynı. `model` alanı yankılanır ama gerçek modeli proje seçer.
2. **OpenAI Python SDK** — non-streaming ve streaming. Streaming örneğinde
   `if chunk.choices:` koruması ve nedeni (upstream kendi frame'ini
   gönderebilir; `stream_options.include_usage` istendiğinde son chunk yalnız
   token sayısı taşır).
3. **OpenAI Node SDK** — aynı iki biçim.
4. **curl** — `-N` ile SSE; `data: {...}` + `data: [DONE]` akışı; akış ortası
   hata `data: {"error":…}` olarak `[DONE]`'dan önce gelir.
5. **LangChain / LlamaIndex** — `ChatOpenAI(base_url=…, api_key=…)` ve
   LlamaIndex `OpenAILike` karşılığı; iki kısa blok.
6. **Ragmux-specific surface** — asıl katma değer:
   - `X-Ragmux-Project` başlığı: birden çok projeye grant verilmiş bir
     `sk-user-…` key'i hangi projede konuşacağını bununla söyler. Söylemezse
     `400 project_required` ve `X-Ragmux-Projects` başlığında adlar döner.
   - Yanıt başlıkları: `x-ragmux-rag-hits`, `x-ragmux-rag-sources` (JSON, ≤2 KB),
     `x-ratelimit-limit-requests` / `-remaining-requests` / `-reset-requests`,
     `x-ragmux-budget-daily-remaining`, `x-ragmux-budget-monthly-remaining`.
     Yalnız ilgili limit tanımlıysa gönderilir. Tarayıcıdan okunabilmeleri için
     `CORS_ORIGINS` gerekir (`Access-Control-Expose-Headers` o zaman kurulur).
   - `{"ragmux":{"include_context":true}}` istek alanı — upstream'e gitmeden
     ayıklanır; non-streaming yanıta üst düzey `ragmux: {sources, context}`
     ekler. Alıntı göstermek isteyen uygulamalar için.
   - `GET /v1/models`: `{"id","object":"model","created","owned_by":<provider_type>}`.
     Birden çok projeye grant verilmiş ve başlık göndermeyen bir key, proje
     başına birer girdi görür (model adına göre tekilleştirilmiş).
   - Tool calls: her sağlayıcıda iki yönlü çevriliyor. Gemini ve Ollama tool
     call delta'sını **bütün** gönderir (argüman parçalanmaz) — parça birleştiren
     istemci kodu bunu hesaba katmalı.
   - Vision: OpenAI içerik parçası `image_url`, inline `data:` ya da uzak URL.
7. **Handling 429 and budgets** — kısa: `Retry-After` her zaman var; `code`
   alanı `rate_limit_rpm` / `rate_limit_tpm` / `budget_daily` / `budget_monthly`,
   `scope` alanı `project` ya da `key`. Ayrıntı ve nasıl düzeltileceği
   Troubleshooting sayfasında.

## Dokunulacak dosyalar

- `../wiki/Provider-Setup.md` (yeni)
- `../wiki/Client-Integrations.md` (yeni)

## Dokunulmayacak

- `../wiki/` altındaki başka hiçbir dosya.
- `Ragmux/` deposundaki hiçbir dosya.
- `git commit` / `git push`.
- Capability matrisi ve tool-şeması dönüşüm tablosu: kopyalanmaz, linklenir.
- Hata kodlarının tam listesi: Faz 05'in işi. Burada yalnız istemcinin
  doğrudan karşılaşacağı birkaçı geçer.

## Kabul ölçütü

- İki dosya var, ikisi de İngilizce.
- Sekiz sağlayıcı tipinin sekizi de var ve `base_url` varsayılanları
  `internal/provider/registry.go` ile uyuşuyor (okuyarak doğrula).
- Ollama bölümü private upstream allowlist'i bağlantı eklemeden **önce**
  ayarlamayı söylüyor.
- `custom_openai` bölümü `/v1`'in elle yazılacağını söylüyor.
- Python streaming örneği `if chunk.choices:` korumasını ve nedenini taşıyor.
- `X-Ragmux-Project`, `x-ragmux-rag-hits`, `x-ragmux-rag-sources` ve
  `ragmux.include_context` dördü de Client Integrations'ta açıklanmış.
- Hiçbir sayfa provider capability matrisini tekrarlamıyor.
- Kod örneklerinin hepsi sözdizimi olarak geçerli (gözle okunur; koşturma yok).

## Notlar

Kaynaklar: `Ragmux/docs/providers.md`, `Ragmux/docs/api.md` (`/v1` bölümü,
satır 670+), `Ragmux/internal/provider/registry.go`,
`Ragmux/internal/gateway/gateway.go` (başlıklar `setLimitHeaders` ~864,
`x-ragmux-rag-*` ~526, `ragmux` alanı ~626-699, `/v1/models` ~371-396),
`.ssot/ADR.md` (ADR-005).

Port örneklerinde `localhost:8765` kullan — Quickstart ile tutarlı olsun.
