# Ragmux v0.4 — uygulama planı

- **Durum:** tamam (kod), yayın bekliyor
- **Tarih:** 2026-09-19
- **Sonuç:** sekiz başlığın tamamı `v0.4` branch'inde; 59 commit, 139 dosya.
  Yayın öncesi review üç turda 5 blocker + 3 major buldu, hepsi kapatıldı.
  Kalan işler: `.ssot/plans/2026-09-19-v0.4-review-artiklari/`.

> Bu dosya tarihsel kayıttır: planlandığı hâliyle korunmuştur. Uygulama
> sırasında ondan sapılan yerler commit mesajlarında gerekçeleriyle yazılıdır.

## Context

Ragmux 0.3.1 çalışan bir self-hosted AI gateway: chi + pgx + pgvector üzerine kurulu ~19k satır Go, tek `<script>` etiketli vanilla-JS dashboard, tüm durum tek bir PostgreSQL'de. Bağımlılık listesi kasıtlı olarak yalın (chi, pgx, pgvector, `golang.org/x/*`).

v0.4, ürünü "çalışıyor" noktasından "ekip halinde ve çok replikayla işletilebilir" noktasına taşıyor. Sekiz başlık üç gerçek boşluğu kapatıyor:

1. **Kimlik** — bugün `/v1` tarafında kullanıcı kavramı hiç yok: proje başına tek, süresiz, scope'suz bir `sk-proj-` key'i var. Kim ne harcadı sorusunun cevabı yok, key iptal edilemiyor (sadece rotate ediliyor), otomasyon için 24 saatte sönen session token'ı kullanmak gerekiyor.
2. **Parite ve maliyet** — Gemini tool calling 400 ile reddediliyor, prompt caching hiç desteklenmiyor (Anthropic'in döndüğü cache sayaçları sessizce düşüyor), ve depoda hiçbir yerde fiyat/maliyet kavramı yok.
3. **İşletim** — `/metrics` yok, trace yok, ve çok replika ilk denemede bozuluyor: ingest kuyruğu süreç içinde ve cross-replica claim yok, janitor her replikada ayrı koşuyor, `SECRET_KEY` dosya fallback'i replikalar arasında ıraksıyor.

Hedef: sekizinin tamamı tek sürümde, fazlar sırayla, her faz kendi başına derlenip yeşil geçecek ve kendi başına yayınlanabilir olacak. Key oluşturmayan bir kurulum yükselttiğinde davranış bit düzeyinde aynı kalacak.

---

## Kapsam düzeltmeleri — iki madde talep edildiği gibi değil

**1. İlk kurulum ekranı zaten var.** Log'a rastgele şifre basan mekanizma bu depoda hiç bulunmuyor. `internal/admin/setup.go` (`setupStatus` L39, `setup` L58) kimliksiz iki endpoint sunuyor, `store.CreateFirstUser` (`internal/store/users.go:72`) advisory lock altında atomik olarak sadece `users` boşken yazıyor, `web/index.html:552` `renderSetup()` formu render ediyor. Kodun kendi yorumu: *"Instead of printing a generated password to the logs…"*. `ADMIN_PASSWORD` verilmezse `cmd/ragmux/main.go:278` sadece "open /admin/ to create the first administrator" diyor.

Kullanıcı kararı: madde 1 yerine **(a)** `ragmux reset-password` CLI'ı (asıl boşluk: tek admin şifresini unutursa elle SQL'den başka yol yok) ve **(b)** kurulum sihirbazının 3 adıma çıkarılması.

**2. Streaming zaten sticky değil.** Sunucu tarafında akış durumu tutulmuyor; SSE istek başına doğrudan geçiriliyor, bellekte oturum haritası yok. Çok replikayı bozan şeyler başka ve hepsi doğrulandı: süreç-içi ingest kuyruğu (`internal/rag/ingest.go:23`), cross-replica claim'in olmaması (`internal/store/documents.go:122-127` koşulsuz `UPDATE ... SET status='processing'`), her replikada koşan janitor (`cmd/ragmux/main.go:222-228`), varsayılan AIO compose'un Postgres'i içinde barındırması, ve `SECRET_KEY` dosya fallback'i (`internal/store/crypto.go:24`). Madde 8, "sticky olmayan streaming yap" yerine "bunları düzelt + kanıt testi + `docs/scaling.md`" olarak yeniden tanımlandı.

---

## Fazlar arası kesişen kararlar

Bunlar üç kümenin de aynı dosyalara dokunduğu yerler; uygulama sırasında bunlara uyulmazsa sessiz bozulma olur.

**Migration numaraları.** `0010_api_keys.sql` (Faz 1) · `0011_pricing_and_cache_tokens.sql` (Faz 3) · `0012_v04.sql` (Faz 4–6). **Doğrulandı:** `migrate()` (`internal/store/db.go:184-220`) dosya adından `fmt.Sscanf(name, "%d_", &version)` ile versiyonu okuyup `schema_migrations`'ta var mı diye bakıyor — aynı numaralı ikinci dosya **hata vermeden sessizce atlanır**. Faz 0'da migration numaralarının tekil ve ardışık olduğunu doğrulayan bir test eklenecek (`TestMigrationNumbersUniqueAndContiguous`).

**`request_logs` kolonları.** Faz 1 `api_key_id` + `user_id` ekler; Faz 3 `cached_prompt_tokens`, `cache_write_tokens`, `cost_micros`, `cost_source` ekler. İkisi de `ADD COLUMN IF NOT EXISTS`, sırasız güvenli. Faz 1 attribution için **index eklemez** — büyük bir `request_logs` üzerinde iki index build'i boot'u `migrate()`'in transaction'ı içinde durdurur ve v0.4'te o kolonlara filtreleyen sorgu yok.

**CSV şeması tam olarak bir kez değişir — Faz 3'te.** `csvHeader`/`csvRecord` (`internal/admin/admin.go:1656`) ve `TestCSVRecordEscapesFormulas` (tam 13 alan iddia ediyor) Faz 1'de **dokunulmaz**. Faz 3, altı kolonun hepsini (`api_key_id`, `user_id`, `cached_prompt_tokens`, `cache_write_tokens`, `cost_usd`, `cost_source`) sona ekler ve testi bir kez günceller.

**Tek capability mekanizması.** `provider.Capabilities` struct'ı + `provider.CapabilitiesFor(providerType)` free function'ı (Faz 2). Rerank provider tipleri buna `Rerank bool` alanı olarak katılır — ayrı bir string listesi **yapılmaz**. `SupportsEmbeddings` bu tablonun üzerine `return CapabilitiesFor(t).Embeddings` olur. `/admin/api/provider-types` mevcut düz alanları (`supports_tools` vb.) geriye uyumluluk için korur ve yanına `"capabilities": {…}` ekler.

**`cmd/ragmux/main.go` üç fazdan da değişiyor** (fetcher, pricing cache, metrics registry, tracer, ingester dispatcher, reranker factory). Fazlar sırayla gittiği için çakışma yok, ama her faz kendi wiring'ini eklerken mevcut sırayı bozmamalı.

**Dashboard CSP kısıtı.** `TestDashboardCSPMatchesEmbeddedScript` (`cmd/ragmux/main_test.go:81`) tam olarak bir `<script>` etiketi ve sıfır inline handler (`onclick=`, `onsubmit=`, …) olmasını zorunlu kılıyor — **doğrulandı**. Tüm yeni UI bağlamaları `on(root, sel, ev, fn)` / `.onclick =` üzerinden gider. CSP hash'i boot'ta yeniden hesaplandığı için elle adım yok.

---

## Faz 0 — Ortak zemin

Kullanıcıya görünen değişiklik yok; sonraki her şey buna dayanıyor.

- `internal/store/db.go:30-33` lock sabitleri genişletilir: `LockJanitor int64 = 0x7261676d75780002` (exported — `internal/maintenance` alacak), `lockBM25Index int32 = 0x72616702`.
- `Store.TryAdvisoryLock(ctx, key) (release func(), ok bool, err error)` — **connection pin edilmeli**: `pool.Exec` üzerinden `pg_try_advisory_lock` rastgele bir havuz bağlantısına düşer ve o bağlantı başka bir iş için yeniden kullanıldığı anda lock düşer. `pool.Acquire` → lock → `release` içinde unlock + `conn.Release()`.
- `Store.Caps() Capabilities` — `Open`'da `migrate()`'ten hemen sonra bir kez doldurulur (`pg_extension`'da `pg_search` var mı) ve ardından bir **smoke query** ile doğrulanır: `SELECT 1 FROM chunks WHERE id @@@ paradedb.match('content','x') LIMIT 0`. Smoke hata verirse yetenek `false`'a düşürülür ve sebep loglanır — sorgu API'si uyuşmazlığını ilk aramada 500 yerine loglanmış bir downgrade'e çevirir.
- `TestMigrationNumbersUniqueAndContiguous`.

---

## Faz 1 — Kimlik, kurulum ve API key'ler

### Migration `0010_api_keys.sql`

`projects.api_key_hash` **olduğu gibi kalır**, `api_keys`'e aynalanmaz: sahibi yok, iptal edilemiyor, süresi dolmuyor — yeni tablodaki her constraint'i tek bir legacy satır için gevşetmek gerekirdi. Dashboard'da "projenin varsayılan key'i" olarak etiketlenir. Gateway prefix'e göre dallandığı için birlikte yaşama maliyeti tek bir `strings.HasPrefix`.

```sql
CREATE TABLE IF NOT EXISTS api_keys (
    id BIGSERIAL PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('gateway','management')),
    name TEXT NOT NULL,
    key_hash TEXT NOT NULL UNIQUE,
    key_prefix TEXT NOT NULL,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_by BIGINT REFERENCES users(id) ON DELETE SET NULL,
    scopes TEXT[] NOT NULL DEFAULT '{}',
    default_project_id BIGINT REFERENCES projects(id) ON DELETE SET NULL,
    rate_limit_rpm INT NOT NULL DEFAULT 0 CHECK (rate_limit_rpm >= 0),
    rate_limit_tpm INT NOT NULL DEFAULT 0 CHECK (rate_limit_tpm >= 0),
    budget_daily_tokens BIGINT NOT NULL DEFAULT 0 CHECK (budget_daily_tokens >= 0),
    budget_monthly_tokens BIGINT NOT NULL DEFAULT 0 CHECK (budget_monthly_tokens >= 0),
    expires_at TIMESTAMPTZ, revoked_at TIMESTAMPTZ, last_used_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT api_keys_name_per_user UNIQUE (user_id, name),
    CONSTRAINT api_keys_mgmt_has_no_project CHECK (kind <> 'management' OR default_project_id IS NULL),
    CONSTRAINT api_keys_mgmt_has_no_limits CHECK (kind <> 'management'
        OR (rate_limit_rpm=0 AND rate_limit_tpm=0 AND budget_daily_tokens=0 AND budget_monthly_tokens=0))
);
CREATE TABLE IF NOT EXISTS api_key_projects (
    api_key_id BIGINT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    project_id BIGINT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    PRIMARY KEY (api_key_id, project_id)
);
-- key_usage: project_usage ile birebir aynı şekil, aynı retention kodu geçerli olsun diye
CREATE TABLE IF NOT EXISTS key_usage ( … PRIMARY KEY (api_key_id, period, period_start) );
ALTER TABLE request_logs
    ADD COLUMN IF NOT EXISTS api_key_id BIGINT NULL REFERENCES api_keys(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS user_id BIGINT NULL REFERENCES users(id) ON DELETE SET NULL;
```

`ON DELETE SET NULL` + denormalize `user_id`: key silinince faturalama geçmişi yok olmaz.

### Key formatı

`internal/store/keys.go`: `ProjectKeyPrefix = "sk-proj-"`, `GatewayKeyPrefix = "sk-user-"`, `ManagementKeyPrefix = "sk-mgmt-"`. Mevcut `generateToken` + 62 karakterlik alfabe + 43 karakter (≈256 bit), `store.HashToken` (SHA-256 — bcrypt değil, sıcak yolda indexli eşitlik araması gerekiyor), görüntü prefix'i mevcut `keyPrefix()` ile 15 karakter. Üçü de `sk-` ile başlar, böylece mevcut secret-scanner desenleri çalışmaya devam eder.

### Scope sözlüğü

| kind | scope | kapsam |
|---|---|---|
| `gateway` | `chat` | `POST /v1/chat/completions` |
| `gateway` | `models` | `GET /v1/models`, `/v1/models/{id}` |
| `management` | `read` | `admin.go:107-174`'te düz `r`'ye bağlı rotalar |
| `management` | `write` | `editor` grubuna bağlı rotalar |
| `management` | `admin` | `adminOnly` grubuna bağlı rotalar |
| `management` | `keys` | `/api/keys*` — sızan bir `write` key'i yeni key basamasın diye ayrı |

**Sert kural: key hiçbir zaman sahibinin rolünü aşamaz.** Etkin yetki = `scopes ∩ rol matrisi`. `RequireRole` her istekte sahibin *canlı* rolüne bakmaya devam eder; scope yalnızca daraltır. Henüz var olmayan endpoint'ler için scope tanımlanmaz.

### CSRF — hiçbir şey zayıflamıyor

`auth.Middleware` (`internal/auth/auth.go:144`) mevcut `IsBearer || SameOriginOK` kontrolünü **değiştirmeden** korur, altına bir dal ekler: `sk-mgmt-` yalnızca `Authorization` header'ında kabul edilir; `ragmux_session` cookie'sine yapıştırılmış bir management key **sorgulanmadan** 401 döner. Gerekçe (koda yorum ve `docs/api.md`'ye yazılacak): tarayıcı cross-origin `Authorization` header'ını CORS preflight olmadan ekleyemez, `cors()` de yalnızca `CORS_ORIGINS`'teki origin'lere preflight cevaplar (varsayılan boş) — yani mevcut bearer muafiyetinin dayandığı önerme birebir geçerli.

Ayrıca `sessionOnly(w, r) bool` guard'ı: `changePassword`, `resetPassword` ve `kind=management` key oluşturma yalnızca interaktif session ile yapılabilir — sızan bir key hesabı ele geçiremesin.

### Gateway'in genişletilmesi (`gateway.go:77`)

`ResolveGatewayKey` prefix'e göre dallanır; `sk-user-` tek bir sorguda her şeyi doğrular, böylece iptal anında etkili olur ve tek bir uygulama noktası olur:

```sql
… FROM api_keys k JOIN users u ON u.id = k.user_id
WHERE k.key_hash=$1 AND k.kind='gateway' AND k.revoked_at IS NULL
  AND (k.expires_at IS NULL OR k.expires_at > now()) AND u.is_active
```

`principal` tipi ek kimliği taşır, `projectFrom(ctx)` imzası **değişmez** — hiçbir downstream handler'a dokunulmaz.

**Çok projeli key'de seçim sırası:** `X-Ragmux-Project: <ad>` header'ı → key'in `default_project_id`'si → tek grant varsa o → aksi halde 400 (`project_required`, geçerli değerler `X-Ragmux-Projects` yanıt header'ında). `model` alanına `"proje/model"` gömme yöntemi **reddedildi**: `model_name` meşru olarak `/` içeriyor (`admin_test.go` `org/model-1.5:latest@v2_x`'i geçerli sayıyor), bu yüzden ayrıştırma belirsiz ve mevcut istemcileri sessizce başka projeye yönlendirir. Bu gerekçe dokümana yazılacak ki tekrar önerilmesin.

Grant edilmemiş bir projeyi isteyen header 403 `project_not_granted` alır ve mesaj "yok" ile "yetkin yok"u **ayırt etmez** — key sahibi proje adı taraması yapamasın. Son grant'ı silinmiş key **kapalı fail eder** (ayrı test).

`project_members` `/v1` tarafında yetki kaynağı **değildir** (üyelik dashboard görünürlüğü); grant'lar yetkilidir, üyelik yalnızca key *oluşturma* anında `auth.CanAccessProject` ile doğrulanır. Aksi halde admin'in üye düzenlemesi üretimdeki bir key'i sessizce kırardı.

CORS düzeltmesi: `cmd/ragmux/main.go:439`'a `X-Ragmux-Project` eklenecek ve yanına `Access-Control-Expose-Headers` (`x-ratelimit-*`, `x-ragmux-*`, `Retry-After`) — bunlar bugün tarayıcı JS'inden okunamıyor, mevcut bir eksik.

### `internal/limits` — eklemeli, yeniden yazım değil

`Check`/`Record` sarmalayıcı olarak kalır, mevcut çağrı yerleri ve testler derlenmeye devam eder.

```go
type Caps struct { RPM, TPM int; DailyTokens, MonthlyTokens int64 }
type Subject struct { Project *store.Project; KeyID *int64; Key Caps }
func (l *Limiter) CheckSubject(ctx, s Subject, estPromptTokens int) (Decision, error)
func (l *Limiter) RecordSubject(ctx, s Subject, prompt, completion int, countRequest bool) error
```

İki katman **`min()` ile birleştirilemez** — sayaçlar ayrı. Her katman kendi sayacına karşı bağımsız değerlendirilir (aylık → günlük → TPM sırası korunur), ilk ihlal kazanır, `Decision.Scope` hangi katmanın bağladığını söyler. RPM'de önce proje slotu sonra key slotu rezerve edilir; key slotu taşarsa **ikisi birden** geri alınır (açık rollback helper'ı — rezervasyon sızıntısının tek olası yeri burası). Header'lar `remaining`'i küçük olan katmandan raporlar, `setLimitHeaders` şekli değişmez. 429 gövdesine `"scope"` eklenir, `code` değerleri aynı kalır.

`PurgeUsage` mevcut döngüsüne `DeleteKeyUsageBefore`'u ekler.

### Süre dolumu ve iptal

Tek uygulama noktası yukarıdaki `WHERE`; cache yok, iptal bir sonraki istekte etkili. Miss yolunda **tek ucuz ikinci sorgu** (`GetAPIKeyState`) 401'i `key_expired` / `key_revoked` / `key_owner_inactive` olarak detaylandırır — **bilinmeyen key'ler bugünkü gövdeyi birebir korur**, yani ek bilgi yalnızca key byte'larını zaten elinde tutana gösterilir, hiçbir şey sayılamaz.

Temizlik janitor'da: `DELETE FROM api_keys WHERE (revoked_at < $1 OR expires_at < $1) AND NOT EXISTS (SELECT 1 FROM request_logs WHERE api_key_id = api_keys.id)`. `NOT EXISTS` guard'ı `ON DELETE SET NULL`'ı güvenli kılar. Admin API'de **iptal tercih edilir, silme değil**: `DELETE` admin-only ve request log referansı varken 409.

### Admin API

`GET/POST /api/keys`, `GET/PUT /api/keys/{id}`, `POST /api/keys/{id}/revoke`, `GET /api/keys/{id}/usage`, `DELETE /api/keys/{id}` (adminOnly). Sahibi olmayan + admin olmayan id'ler **404** (proje konvansiyonu). `loadKey(w, r)` helper'ı `loadProject` (`admin.go:1274`) kalıbını izler. Her mutasyon `a.audit(...)` ile `apikey.create|update|revoke|delete` yazar; detaylarda key asla yer almaz.

`setupStatus` iki ucuz sayım daha döner: `has_connections`, `has_projects` — sihirbaz reload'dan sonra doğru adımdan devam etsin diye.

### Dashboard

Yeni üst seviye **Keys** sekmesi (`projects` ile `playground` arası), **her role görünür** — `management` key'in projesi yok (projects altına sığmaz), viewer da kendi gateway key'ini üretebilmeli (users admin-only). Her kullanıcı kendi key'lerini görür; admin'de "tüm kullanıcılar" filtresi.

`renderKeys()` `renderUsers` (L984-1024) kalıbında; form `drawer({…})` + mevcut `field/input/select/toggle/checklist` helper'ları; limit alanları `projectForm`'dan kopyalanır, `limitSummary()` alan adları zaten uyuştuğu için aynen kullanılır. `showKey(p, key)` (L862) `showSecret({title, key, kind, projects})` olarak genelleştirilir ve `showKey` iki satırlık sarmalayıcıya iner — projects sekmesi hiç değişmez. Gateway snippet'i birden fazla grant varsa `-H "X-Ragmux-Project: <ad>"` içerir.

### 3 adımlı kurulum sihirbazı

Kritik tasarım kararı: **2. ve 3. adım yeni kimliksiz yüzey açmaz.** Adım 1 bugünkü `/setup`'ı aynen POST eder ve session cookie'sini kurar; adım 2 ve 3 sıradan kimlikli çağrılardır (`POST /admin/api/models`, `POST /admin/api/projects`) — zaten rol korumalı ve zaten audit'li. Sihirbaz mevcut rotaların üzerinde rehberli bir formdan ibaret.

`renderSetup()` bir dispatcher'a dönüşür (`renderSetupAdmin/Model/Project`), aynı `.auth-form` kartı ve `heroHtml()` korunur, eyebrow `First run · step N of 3` olur. Adım 2 mevcut `POST /api/models/test` ile "Test connection" sunar. İkisi de atlanabilir; bağlantı yokken adım 3 otomatik atlanır (proje bir bağlantı gerektiriyor). Adım 3 sonunda `showKey` reveal-once modalı.

### `ragmux reset-password` CLI

`cmd/ragmux/resetpw.go`, `rotate.go` kalıbında (`flag.NewFlagSet` + `ContinueOnError`, `config.Load()`, `store.Open` `MaxConns: 2`, `io.Discard` logger, int dönüş kodu), `main.go:42-48`'e pozisyonel dispatch.

```
ragmux reset-password <username> [--generate] [--stdin] [--force] [--no-revoke] [--revoke-keys]
```

`--password` bayrağı ve interaktif TTY prompt'u **bilerek yok**: bayrak değeri `ps` ve shell geçmişine sızar, gizli prompt ise `golang.org/x/term` gerektirir ve tek bir acil komut için bağımlılık kuralını bozar. `--generate` (konsoldayım) ve `--stdin` (script'teyim) iki senaryoyu da kapatır.

Minimum uzunluk **12** (dashboard'ın 8'i değil — `setupMinPasswordLen` ile hizalı). Varsayılan olarak kullanıcının tüm session'larını iptal eder (`--no-revoke` kapatır); `--revoke-keys` ayrıca tüm `api_keys` satırlarını iptal eder. Yalnızca DB erişimi gerekir, gateway koşmasına gerek yok — kilitlenme çıkışının bütün amacı bu; `DATABASE_URL` + `SECRET_KEY` elinde olan zaten her şeyi yazabilir, yeni yetki vermiyor.

Audit: `audit_logs.actor_user_id` zaten nullable, migration gerekmiyor — `actor_username="cli"`, `action="user.reset_password"`, `details={"via":"cli","host":…,"os_user":…,"revoked_sessions":n,"revoked_keys":m}`. Konsoldan yapılan sıfırlama dashboard'ın Audit sekmesinde görünür.

### Dosyalar

`internal/store/{keys,apikeys,keyusage}.go` (yeni) · `internal/store/migrations/0010_api_keys.sql` · `internal/gateway/gateway.go` (L69-93, 183-201, 500, 519) · `internal/auth/{auth.go,policy.go}` · `internal/admin/{admin.go,keys.go,setup.go}` · `internal/limits/limits.go` · `internal/maintenance/janitor.go` · `web/index.html` · `cmd/ragmux/{main.go,resetpw.go}`

### Sıra

migration + store CRUD → limits (`Subject`) → gateway → auth (3'ten bağımsız, paralel olabilir) → admin `/api/keys` → janitor → dashboard Keys sekmesi → kurulum sihirbazı → `reset-password` (1'den sonra her şeyden bağımsız) → docs.

---

## Faz 2 — Provider paritesi

### Paylaşılan normalizatör (önce bu)

`internal/provider/toolcalls.go`: Anthropic'in `toolIndex map[int]int` kalıbı (`anthropic.go:328-329, 367-374, 397-405`) yeniden kullanılabilir bir tipe çıkarılır.

```go
type toolCallStream struct { index map[int]int; next int }
func (t *toolCallStream) Open(block int, id, name string) json.RawMessage
func (t *toolCallStream) Args(block int, partial string) json.RawMessage
func (t *toolCallStream) Whole(id, name, args string) json.RawMessage
type toolCall struct { ID, Name, Arguments string }
func toolCallsJSON(calls []toolCall) json.RawMessage
```

Anthropic çıktısı byte düzeyinde aynı kalmalı — bunu koruyan bir regresyon testi yazılır.

**Ollama kararı: tek parça kalır, yapay incremental delta üretilmez.** Ollama'nın NDJSON'u argümanları gerçekten atomik teslim ediyor; bölmek bilgi taşımayan bir çerçeveleme uydurmak olur ve OpenAI şeklindeki her istemci `arguments`'ı zaten string birleştirmeyle topluyor. `docs/providers.md`'de protokol özelliği olarak belgelenir. Ama `toolCallStream`'e taşımak **mevcut bir hatayı bedava düzeltiyor**: `ollama.go:330` her NDJSON satırında index'i `0`'dan başlatıyor; yeni Ollama sürümleri tool call'ları birden çok satıra yayabildiği için iki çağrı da `index: 0` alıp istemcide tek bozuk çağrıya birleşiyor. Stream kapsamlı sayaç bunu `0` ve `1` yapar — regresyon testi eklenir.

### `internal/provider/images.go` — birleşik içerik ayrıştırma

Üç yerde kopyalanmış part parser'ı (`anthropic.go:211-217`, `gemini.go:145-151`, `ollama.go:181-219`) tek `parseContent(raw) ([]contentPart, error)`'a iner. `contentPart` `CacheControl json.RawMessage` de taşır (Faz 3'te kullanılacak). Üç `openAIPartsTo*` fonksiyonu adlarını ve dönüş tiplerini korur, yalnızca ayrıştırma gövdeleri kaybolur.

### Capability modeli

`internal/provider/capabilities.go` — provider tipine göre anahtarlanan **free function**, `Provider` üzerinde metot değil: bağlantı henüz yokken de sorulabilmeli (`testUnsavedConnection`, `admin.go:126`) ve mevcut `SupportsEmbeddings` şekliyle uyumlu.

```go
type Capabilities struct {
    Streaming, Embeddings, Tools, ToolStreaming, Vision bool
    RemoteImages, PromptCaching, CachedTokenUsage, Rerank bool
}
func CapabilitiesFor(providerType string) Capabilities
```

**Gateway'de hiçbir şeyi kapıda tutmaz** — adapter'lar zaten tipli `*provider.Error` döndürüyor, `chatCompletions`'ta ön kontrol tekrarı ıraksamaya davetiye. Tablo bir *tanım*: dashboard, bağlantı testi yanıtı ve docs tablosu için tek doğruluk kaynağı. Faz 4'ün `cohere_rerank`/`voyage_rerank` tipleri buraya `Rerank: true` ile katılır ve `web/index.html`'deki **her picker** bu alana göre filtrelenecek şekilde denetlenir — bu kümenin sessiz bozulma riski tam olarak burada.

### Gemini tool calling

`gemini.go:76-79`'daki sert red silinir. `geminiPart` `FunctionCall`/`FunctionResponse` varyantları kazanır; `geminiRequest` `Tools []geminiTool` + `ToolConfig` alır.

`tool_choice` eşlemesi: yok → `toolConfig` yok · `auto` → `AUTO` · `required` → `ANY` · `none` → `NONE` (deklarasyonlar korunur — Gemini'nin birebir karşılığı var) · belirli fonksiyon → `ANY` + `allowedFunctionNames`.

**Gemini'de tool call ID'si yok, isimle eşleştiriyor.** OpenAI `role:"tool"` mesajının hangi çağrıya ait olduğu, önceki assistant `tool_calls`'larında `id` aranarak geri kazanılır (`geminiToolName`); bulunamazsa part düşürülür ve debug loglanır (başıboş bir tool mesajı 400'e değmez). `functionResponse` **`role:"user"`** ile gönderilir — Gemini yalnızca `user`/`model` kabul ediyor; `appendGemini` ardışık aynı rolleri zaten birleştiriyor, paralel tool sonuçları için tam istenen davranış.

**JSON-Schema: sanitize edilir, geçirilmez.** `gemini_schema.go` → `sanitizeGeminiSchema(raw) (out, dropped)`. Gerekçe: her OpenAI strict-mode tool şeması `"additionalProperties": false` taşıyor ve çoğu `$defs`/`$ref` içeriyor; geçirmek pratikte her gerçek tool tanımında 400 üretir ve dönen Gemini hatası (`Unknown name "additionalProperties"`) SDK yazarına hiçbir şey anlatmaz. Kurallar: bilinmeyen anahtar kelimeler düşürülür; `const`→`enum`, `oneOf`/`allOf`→`anyOf`, `type:["string","null"]`→`nullable`, `exclusiveMinimum`→`minimum`; `$ref` derinlik 8 sınırıyla inline edilir, döngü `{"type":"string","description":"(recursive schema elided)"}` olur. Asla hata vermez. Düşürülen anahtarlar istek başına bir kez debug loglanır ve tam liste dokümana yazılır (kayıplı olduğu açıkça söylenir). Sanitize sonrası Gemini yine reddederse `upstreamError` zaten aynen relay ediyor.

Streaming: Gemini kısmi argüman akıtmıyor, `functionCall` tek SSE chunk'ında tam geliyor → çağrı başına tek `toolCallStream.Whole(...)` deltası. Bir çağrı görüldüyse finish reason `tool_calls`'a çevrilir (Gemini'nin ayrı bir karşılığı yok).

### Uzak görsel indirme

`ImageFetcher` `provider.Config.Images` alanına oturur ve **adapter'ların kullandığı aynı `httpClient`'ı** alır — `netguard.SafeDialContext` (DNS-rebind güvenli, özel aralıklar kapalı) ve `netguard.CheckRedirect(3)` (cross-host yönlendirme yok) sıfır yeni SSRF yüzeyiyle geçerli olur.

Sınırlar: yalnız `GET`, yalnız `http`/`https`, `Content-Type` `image/png|jpeg|gif|webp` (URL uzantısına asla güvenilmez), `Content-Length > MaxBytes` dial öncesi red + `io.LimitReader(MaxBytes+1)` ile yalan/eksik `Content-Length`'e karşı koruma, görsel başına timeout, istek başına adet tavanı, **sıralı** indirme (tek istek fan-out yapamasın). İstemci kaynaklı hatalar 400, ana bilgisayar adı dışında URL loglanmaz; taşıma hataları mevcut `transportError` ile aynı sabit metinlere eşlenir; görsel sunucusundan gelen non-2xx **400**'dür (502 değil — URL'yi istemci seçti).

LRU cache (`container/list` + map, 64 kayıt / 64 MiB / 10 dk, `Cache-Control: no-store`'da atlanır): çok turlu konuşmalar aynı görseli her turda yeniden gönderiyor. URL ile anahtarlandığı için TTL içinde değişen içerik bayat servis edilir — dokümana yazılır.

Adapter politikası: **Ollama** her zaman inline (başka yolu yok) · **Gemini** her zaman inline — bu aynı zamanda mevcut bir hatayı düzeltiyor: `gemini.go:169` (**doğrulandı**) `data:` olmayan her URL'yi `fileData.fileUri` olarak gönderiyor, oysa Gemini orada yalnızca Files-API ve `gs://` URI'lerini kabul ediyor, yani sıradan bir genel görsel URL'i bugün upstream'de reddediliyor · **Anthropic** doğal kalır (`source.type=url` destekliyor, kendi indiriyor; daha az byte). Politika adapter başına tek sabit, değiştirmek tek satır.

Config: `IMAGE_FETCH` (varsayılan `true`, `false` eski davranışı geri getirir), `IMAGE_FETCH_MAX_MB=8`, `IMAGE_FETCH_TIMEOUT=10s`, `IMAGE_FETCH_MAX_PER_REQUEST=8`, `IMAGE_CACHE_ENTRIES=64`, `IMAGE_CACHE_TTL=10m`.

### Sıra

normalizatör + `parseContent` (saf refactor) → capability modeli → Gemini tool calling → görsel indirme.

---

## Faz 3 — Prompt caching ve maliyet

### `Usage` genişletmesi

```go
type Usage struct {
    PromptTokens, CompletionTokens, TotalTokens int
    PromptTokensDetails     *PromptTokensDetails
    CompletionTokensDetails *CompletionTokensDetails
    PromptCacheHitTokens, PromptCacheMissTokens int // DeepSeek üst seviyede raporluyor
}
type PromptTokensDetails struct { CachedTokens, CacheWriteTokens, AudioTokens int }
func (u *Usage) normalize()
func (u *Usage) CachedTokens() int
func (u *Usage) CacheWriteTokens() int
```

Compat adapter upstream gövdesini doğrudan `ChatResponse`/`StreamChunk`'a açtığı için **OpenAI'ın `prompt_tokens_details.cached_tokens`'ı alan var olur olmaz akmaya başlar**.

**Anthropic eşlemesi ince nokta:** Anthropic'in `input_tokens`'ı iki cache sayacını **dışlar**, OpenAI'ın `prompt_tokens`'ı `cached_tokens`'ı **içerir**. Bu yüzden `PromptTokens = InputTokens + CacheCreation + CacheRead`, `CachedTokens = CacheRead`, `CacheWriteTokens = CacheCreation`. Böylece `prompt_tokens` provider'lar arası karşılaştırılabilir kalır ve `prompt + completion == total` korunur. **Bu, cache'li Anthropic isteklerinde raporlanan `prompt_tokens`'ı 0.3.x'e göre değiştirir** (eski sürüm eksik raporluyordu) — CHANGELOG "Changed", geriye dönük doldurma mümkün değil.

Streaming'de `message_start` ve `message_delta` yapılarına iki alan eklenir, yalnız sıfır olmayan değerler üzerine yazılır.

### İstek tarafı

**OpenAI/DeepSeek: gönderilecek hiçbir şey yok** — caching otomatik ve sunucu tarafında, `cache_control` diye bir alan yok. İstemci content part'ına koyarsa `json.RawMessage` olduğu için dokunulmadan gider ve OpenAI yok sayar. Yalnız yanıt tarafı anlamlı — dokümana açıkça yazılır ki bug olarak açılmasın.

**Anthropic: üç yerleşim.** Content part'ları (`parseContent` zaten `CacheControl`'ü kaldırıyor) · tool'lar (üst seviyede veya `function` içinde kabul edilir) · **system**. Sonuncusu tek gerçek wire değişikliği: `anthropicRequest.System` `string` → `json.RawMessage` olur; hiçbir `cache_control` yoksa bugünkü gibi düz JSON string marshal edilir (wire birebir aynı), varsa blok dizisi. `TestTranslateAnthropic` (`provider_test.go:52`) buna göre güncellenir.

**Gemini: yalnız yanıt tarafı** — explicit caching durumlu bir `cachedContents` kaynağı gerektiriyor, stateless passthrough'a oturmuyor; implicit caching zaten otomatik. **Ollama:** kavram yok.

**Ertelenen ve bilerek yapılmayan:** gateway'in kendi enjekte ettiği proje system prompt'u ve RAG bağlam bloğu (`gateway.go:204-238`) aslında cache breakpoint'i için en değerli sabit önek. Proje seviyesinde `cache_prompt` bayrağı kolon + dashboard + kendi migration'ı demek; kanca noktası not edilip 0.5'e bırakılıyor.

### Fiyat tablosu

`internal/pricing/prices.json`, `go:embed` (YAML değil — `encoding/json` bedava, bağımlılık kuralı korunur). `cache_write`/`cache_read` **mutlak fiyat, çarpan değil**: çarpan Anthropic'in 1.25×/0.1× konvansiyonunu şemaya gömerdi ve OpenAI (okuma 0.5×, yazma ücretsiz) ile Gemini'de (depolama fiyatlı) kırılırdı. Konvansiyon yalnız veri dosyasında, önceden hesaplanmış sayılar olarak yaşar.

Eşleşme anahtarı `(provider_type, model_name)`, glob ile: tam eşleşme → en uzun literal önek → en düşük id. `path.Match` **kullanılmaz** (`/`'ı özel sayıyor, model adları `/` içeriyor: `deepseek-ai/DeepSeek-V3`). `ollama` ve `custom_openai` `*` deseniyle 0 fiyatla gelir ki yerel modeller hayalî harcama göstermesin.

### Migration `0011_pricing_and_cache_tokens.sql`

`model_prices` tablosu (`source TEXT CHECK (source IN ('builtin','user'))`, `builtin_version INT`, `UNIQUE (provider_type, model_pattern)`) + `request_logs`'a `cached_prompt_tokens`, `cache_write_tokens`, `cost_micros BIGINT`, `cost_source`.

**`BIGINT` micros, `NUMERIC` değil:** 50 000 satırlık export'ta `SUM()` tam kalır, Go tarafı tamsayı kalır, $0.000001 tabanı her istek başı fiyattan ince.

**Kullanıcı düzenlemesini ezmeme kuralı** (dokümana bu haliyle): dashboard'dan düzenlenen satır `source='user'` olur ve **yükseltmeler bir daha asla dokunmaz**. Dokunulmamış `builtin` satır yalnızca `prices.json`'un `version`'ı **arttığında** tazelenir (`ON CONFLICT … WHERE source='builtin' AND builtin_version < EXCLUDED.builtin_version`). `builtin` satırlar **silinemez**, yalnız düzenlenir veya reset edilir — "silinen satır yükseltmede dirilir" sorunu tombstone kolonu olmadan tamamen ortadan kalkar. `TestPricesVersionPinned` gömülü byte'ların SHA-256'sını sabitler: versiyon bumplamadan fiyat değiştirmek build'i kırar.

### Maliyetin hesaplandığı yer

**`fillUsage`'ın içinde değil** — o saf bir fonksiyon, DB bağımlılığı almamalı. Maliyet, `gateway.go:242-254`'teki **mevcut `defer` bloğunda** ikinci saf adım olarak hesaplanır: o blok başarı, hata, akış-ortası kopma ve 499 yollarının hepsinde koşuyor ve o noktada `rec.PromptTokens`/`rec.CompletionTokens` kesinleşmiş oluyor. `conn` zaten kapsamda (`gateway.go:161`). Lookup `context.Background()` ile yapılır (istemci koptuysa istek context'i iptal olmuş olabilir; cache zaten çoğu zaman DB'ye gitmez).

```
uncached = max(prompt - cached - cacheWrite, 0)
micros   = round(uncached*Input + cacheWrite*CacheWrite + cached*CacheRead + completion*Output)
```

`pricing.Cache` 60 sn TTL + `RWMutex`; admin yazmaları `Invalidate()` çağırır.

### $ nereye çıkar

`MetricsSummary`, `DailyBucket`, `ProjectMetrics` `CostMicros` + `CostUSD` kazanır (`CostUSD` Go tarafında `micros/1e6` — tam tamsayı korunur, dashboard hazır sayı alır). **CSV tam bu fazda, tek seferde** altı kolonu sona ekler (Faz 1'in `api_key_id`/`user_id`'si dahil) ve `TestCSVRecordEscapesFormulas` bir kez güncellenir; mevcut formül-enjeksiyon koruması yeni hücreleri zaten kapsıyor.

Dashboard: Overview'da "Spend (est.)" kutucuğu, proje ve son-istek tablolarında `$` kolonu, günlük grafikte maliyet çizgisi, editor-only **Prices** sekmesi (inline düzenle / reset / ekle). **Her yerde "tahmini" etiketi** — bu bir fatura değil.

Admin endpoint'leri `/api/prices` (GET viewer; POST/PUT editor; DELETE `builtin`'de 409; `POST /{id}/reset`), hepsi `price.*` olarak audit'li.

### Sıra

`Usage` + caching (migration'ın cache-token yarısı) **önce**, sonra pricing — maliyet cache kolonlarına ihtiyaç duyuyor.

---

## Faz 4 — Retrieval backend'leri

### Migration `0012_v04.sql`

`rag_stores`'a `search_backend` (`pgvector|pg_search`), `rerank_backend` (`llm|cohere|voyage`), `rerank_connection_id`; `documents`'a `claimed_by`, `claimed_at`, `lease_until`, `attempts` + kısmi index; `instance_settings` tablosu (Faz 6 canary'si için). Tek dosya — sürüm çapında tek DDL adımı gözden geçirmesi ve geri alması daha kolay.

`CHECK (rerank_backend='llm' OR rerank_connection_id IS NOT NULL)` **bilerek yok**: `ON DELETE SET NULL` operatör Cohere bağlantısını silince onu ihlal ederdi. Değişmez `validateRAG`'da yazma anında, okuma anında ise NULL bağlantı "reranker yok → atla ve logla" olarak ele alınır — rerank hatasıyla aynı sözleşme.

### Pluggable search backend

`Store.Search` (`vector.go:204-311`) tek genel chokepoint olarak kalır ve backend'den bağımsız her şeyi (option varsayılanları, `GetRAGStore`, boyut kontrolü, transaction, `SET LOCAL hnsw.ef_search`, scan döngüsü, `MaxDistance`) yapmaya devam eder. Yalnız **hybrid SQL** devredilir; vector-only mod iki backend'de de birebir aynı olduğu için backend yalnızca `Mode == SearchHybrid`'de sorulur.

Backend'ler satır değil **SQL döndürür** — tek scan döngüsü ve tek `SearchHit` kolon sözleşmesi kalsın diye:

```go
type SearchBackend interface {
    Name() string
    Available(caps Capabilities) bool
    Prepare(ctx context.Context, s *Store) error
    HybridQuery(p SearchParams) (sql string, args []any)
}
```

`SearchHit` `LexScore float64` kazanır (pg_search'te gerçek BM25 skoru, pgvector'de 0 — `ts_rank_cd` karşılaştırılabilir değil, dışa verilmiyor). `FTSRank` JSON adını korur.

**BM25 index'i global, store başına değil** — store başına kısmi index, index sayısını store sayısıyla çarpar ve her biri tam segment ağacı taşır. Lazy oluşturulur (`ensureBM25Index`, advisory lock + süreç içi cache): migration'da olsaydı sonradan pg_search kazanan sunucu alamazdı ve pg_search'süz sunucu her chunk insert'ünde bedel öderdi. Tokenizer `PG_SEARCH_TOKENIZER`, varsayılan `default` (unicode word split + lowercase, stemming yok) — tsvector yolu `'simple'` ile indeksliyor, iki backend kutudan karşılaştırılabilir çıksın diye. **`fts_config` pg_search'te yok sayılır** (index global, ayar store başına); `docs/rag.md`'ye yazılır ve admin arama yanıtında görünür kılınır ki sürpriz olmasın.

pg_search sorgusunda `$5` ham sorgu metnidir; `paradedb.match` kendi analizörüyle tokenize edip terimleri varsayılan olarak OR'lar — `ftsQuery`'nin `websearch_to_tsquery` için taklit etmek zorunda kaldığı şey. Bu yüzden **`ftsQuery` pg_search yolunda uygulanmaz** ve `search_pgvector.go`'ya taşınır. Kullanıcıdan gelen her şey bind parametresi; tek `fmt.Sprintf` doğrulanmış tamsayıdan türeyen vektör tablo adı — bugünkü gibi.

**Füzyon RRF kalır** (`rrfK = 60` sabite çıkarılır), pg_search gerçek BM25 skoru verse bile:
1. BM25 skorları sınırsız ve korpusa bağlı, kosinüs mesafesi `[0,2]` — ağırlıklı toplam sorgu başına min/max normalizasyon ister ve bu tam da bir bacak 2, öbürü 50 aday döndürdüğünde (kısa sorgu, küçük store) kararsızlaşır.
2. `SearchHit.Score` genel bir sözleşme: `x-ragmux-rag-sources`, yanıt gövdesindeki `ragmux.sources[].score`, admin arama ekranı ve mevcut testler. `score`'un anlamının store ayarına göre değişmesi, config bayrağı kılığında bir API değişikliğidir.
3. BM25'in asıl kazancı **daha iyi sıralanmış ve daha kapsayıcı leksik aday listesi** (terim frekansı, doküman uzunluğu normalizasyonu, gerçek analizör, phrase desteği) — füzyon skaleri değil. RRF bu iyileşmeyi doğrudan tüketir.

Ham BM25 `lex_score` olarak eklemeli şekilde açılır. Ağırlıklı füzyon modu ayrı bir knob, v0.4 kapsamı dışı.

**Backend değiştirmek hiçbir zaman dokümanları yeniden işlemeyi gerektirmez** — leksik taraf `chunks.content`'ten türüyor. `updateRAGStore`'un `reprocess_recommended` ipucu backend değişiminde **tetiklenmemeli** (`admin.go:976` denetlenecek). `pg_search → pgvector` dönüşünde `chunks.tsv` ve GIN index'i hiç kaldırılmadığı için çalışmaya devam eder; `idx_chunks_bm25`'in elle `DROP INDEX` edilmesi manuel temizlik olarak belgelenir.

**Okuma anında backend yoksa** (pgvector-only sunucuya restore edilmiş dump, ya da smoke query downgrade'i): `pgvector`'a düşer, store başına süreç başına bir kez `Warn` loglar, asla hata vermez. Etkin backend `rag.Result.Backend` ve admin arama JSON'unda `"backend"` olarak döner — fallback sessiz değil, gözlemlenebilir. **Yazma anında** `validateRAG` 400 ile reddeder.

`GET /admin/api/search-backends` yeni endpoint (`providerTypes` yanında) `{"id","available","reason"}` döner; dashboard kullanılamayan seçeneği pasifleştirir.

### Pluggable reranker

`rag.Reranker` arayüze çıkar, mevcut somut struct `LLMReranker` olur (aynı `rerankInstruction` metni korunur — `internal/e2e` mock upstream'leri ona göre eşleşiyor). `Retriever.Reranker *Reranker` → `Retriever.Rerankers RerankerFactory`; `NewRetriever` varsayılanı `LLMReranker` döndüren bir factory yapar, mevcut test wiring'i kırılmaz.

**Cohere/Voyage istemcileri `internal/provider/rerank.go`'ya konur, `internal/rag`'a değil.** Bir rerank çağrısı **doküman içeriğini üçüncü tarafa gönderiyor**; tek sertleştirilmiş çıkış yolundan geçmek zorunda: `netguard.SafeDialContext`, `CheckRedirect(3)`, `transportError`'ın sabit mesaj eşlemesi, `RedactWith(err, cfg.APIKey)`, `upstreamError`'ın 5xx→502 normalizasyonu ve `doJSON`'un `io.LimitReader(32<<20)` gövde tavanı. `internal/rag`'da ikinci bir ad-hoc `http.Client` bunların hepsinden sessizce çıkardı.

Her ikisi de `parseRankArray` duruşuyla savunmacı doğrular: aralık dışı index'ler ve tekrarlar atılır, filtrelemeden sonra liste boşsa 502. **Uzak bir index'e asla slice offset olarak güvenilmez.** API rerank'lerinde passage kesme 4000 karakter (LLM yolundaki 800 değil — prompt bütçesi yok ama gövde yine sınırlanmalı; `rerank_candidates` 100 tavanıyla ≤400 KB).

**Kimlik bilgileri `model_connections` satırında yaşar**, `ValidProviderTypes` `cohere_rerank` ve `voyage_rerank` kazanır. Gerekçe: o satır zaten AES-256-GCM (bağlantı id'si AAD olarak bağlı), `key_version` migrasyonu, **`ragmux rotate-key` kapsamı** (`ReencryptConnections`), JSON çıktısında `MaskKey`, kaydetme anında netguard kontrolü, test endpoint'i, audit ve proxy/self-hosted için `base_url` veriyor. Ayrı bir kimlik yolu bunların hepsini yeniden yazmak zorunda kalırdı — ve `rotate-key` onu sessizce atlardı, ki bu operasyonel bir araçta veri kaybı hatasıdır. Env değişkeni (`COHERE_API_KEY`) tek satır ama projenin merkezî özelliğini kırar ve store başına key'i imkânsızlaştırır.

Bunun bedeli — asıl iş kalemi: **sohbet edemeyen ve embed edemeyen iki provider tipi.** `provider.New` ve `NewEmbedder` net mesajla reddeder; `Capabilities` tablosunda yalnız `Rerank: true`; ve `web/index.html`'deki proje model picker'ı ile RAG store embedding picker'ı bu alana göre filtrelenecek şekilde **tek tek denetlenir**.

Başarısızlık sözleşmesi bugünküyle birebir korunur: factory `ErrRerankUnavailable` dönerse veya `Rerank` hata verirse → logla, birleştirilmiş sırayı kullan, `Reranked=false`, `RerankFallback=true`. `RerankLatencyMS` `rs.Rerank` açıkken hep non-nil kalır (panelde boş değil "0 ms, atlandı" görünür). LLM timeout'u 10 sn, API backend'leri 5 sn; `RERANK_TIMEOUT` ikisini de ezer.

`searchRAGStore`'un override bloğu `backend`, `rerank_backend` ve `rerank_connection_id` kazanır — kaydetmeden önce iki backend'i karşılaştırabilmek bu ayarı kullanılabilir kılan özellik.

---

## Faz 5 — Gözlemlenebilirlik (sıfır bağımlılık)

### `internal/metrics`

Paket doc'unda açıkça yazılacak: bu genel amaçlı bir client kütüphanesi değil, gateway'in dışa verdiği şeyin tam olarak karşılığı. Counter, gauge, histogram, sabit label seti, scrape anında okunan gauge'lar. Summary yok, exemplar yok, push yok.

Sıcak yolda **yalnız atomik**; `RWMutex` sadece map'i korur ve yalnız bir label kombinasyonunun ilk dokunuşu yazma kilidi alır. Kayıt sırası = çıktı sırası, `WriteTo` sıralama yapmaz. `FloatCounter` `math.Float64bits`'i `atomic.Uint64`'te CAS döngüsüyle tutar (`cost_usd_total` için).

**Tahsis kuralı** (paket yorumuna): `With(vals...)` label değerlerini birleştirir — çağrı başına bir tahsis. Label seti istek boyunca sabit olan her çağrı yeri seriyi dışarı almalı; streaming chunk sayaçları seriyi döngüden önce çözer.

Çıktı `text/plain; version=0.0.4; charset=utf-8`; isimler kayıt anında `^[a-zA-Z_][a-zA-Z0-9_]*$` ile doğrulanır, çift kayıt panikler (test edilen bir başlangıç hatası); label değerlerinde `\`, `"` ve newline kaçışlanır; histogram kümülatif `_bucket{le=…}` + `_sum` + `_count`. Çıktı `bytes.Buffer`'a kurulup tek seferde yazılır — yavaş bir scraper registry'nin okuma kilidini tutamasın.

### Metrik seti

`ragmux_` öneki. HTTP: `requests_total{route,method,status}`, `request_duration_seconds{route,method}` (`.005…60` — bir chat completion meşru olarak 30 sn sürebiliyor), `requests_in_flight`, `response_bytes_total{route}`.

**`route` chi pattern'idir** (`RouteContext(r.Context()).RoutePattern()`, `next.ServeHTTP`'den **sonra** okunur), asla `r.URL.Path` değil — bu tek karar başlıca kardinalite koruması: doküman id'si başına seri değil, `/admin/api/rag-stores/{id}/documents`.

Gateway: `requests_total{project,model,status,streamed}`, `request_duration_seconds`, `upstream_duration_seconds{provider,model}`, `time_to_first_token_seconds`, `stream_chunks_total`, `streams_active`, `tokens_total{project,model,kind}`, `tokens_estimated_total`, `cost_usd_total{project,model}`, `errors_total{project,provider,type}`, `client_disconnects_total`.

**`model` her zaman `conn.ModelName`** (sunucu tarafı, bağlantı sayısıyla sınırlı), asla istemcinin gönderdiği `req.Model` — saldırgan kontrollü bir string label'da sınırsız bellek hatasıdır. `tokens_estimated_total` ayrı duruyor çünkü `rec.Estimated` değerlerini sessizce karıştıran bir token serisi maliyet hesabı yapan herkes için tuzak.

Ayrıca: `limits_denied_total{project,reason}` (beş sabit `limits.Reason*`), RAG (`retrieval_duration_seconds{store,backend,mode}`, `embed_duration_seconds`, `rerank_duration_seconds{backend}`, `rerank_failures_total{backend,reason}` — `reason` **sabit küme**, asla `err.Error()`), ingest (`jobs_total{outcome}`, `duration_seconds`, `claims_total{outcome}`, `lease_lost_total`, `documents{status}` — 30 sn cache'li gauge, çünkü backlog artık bir veritabanı özelliği, `len(chan)` değil), DB havuzu (`pgxpool.Stat()` üzerinden `GaugeFunc`, sorgu maliyeti yok), runtime (`runtime/metrics` üzerinden — `ReadMemStats` **değil**, o her scrape'te stop-the-world yapar), ve öz-metrikler (`metrics_series_dropped_total`, `tracing_spans_dropped_total`).

Kardinalite korumaları: `MaxSeries` **varsayılan 5000** (aşan kombinasyonlar düşürülür ve sayılır, bir kez loglanır) + dokümandaki checklist: label asla header'dan, hata mesajından, dosya adından, query string'den, user agent'tan veya IP'den gelmez.

### Endpoint yetkisi — varsayılan kapalı

Metrik seti **proje envanterini, kullanılan model adlarını, istek hacimlerini, token sayılarını ve harcamayı** sızdırıyor. Bu, `/admin/api/metrics/summary`'nin bugün kimlik istediği verinin aynısı; varsayılan açık olması ürünün geri kalanıyla tutarsız olurdu.

`METRICS_ENABLED=false` (varsayılan) · `METRICS_TOKEN`/`METRICS_TOKEN_FILE` (`subtle.ConstantTimeCompare`, mevcut `envOrFile` konvansiyonu) · `METRICS_LISTEN` (ör. `127.0.0.1:9090` — ayrılmış ikinci bir `http.Server`, bu durumda ana router'a **hiç bağlanmaz**, yani hiçbir proxy kuralı onu açamaz).

`config.Load`'da fail-fast: `METRICS_ENABLED=true` ama ne `METRICS_TOKEN` ne de loopback `METRICS_LISTEN` varsa başlatma hatası — *"METRICS_ENABLED without METRICS_TOKEN would publish per-project usage and spend unauthenticated…"*.

Endpoint `requestLogger`'dan hariç tutulur (15 sn'lik scrape JSON logunu boğar), `Cache-Control: no-store`.

**`/readyz`**: havuz ping'i + `schema_migrations`'ın `MAX(version)`'ı gömülü baş ile eşit → 200. `/healthz` liveness olarak değişmeden kalır (container HEALTHCHECK aynı). **Store'un search backend'i eksik olması readiness'ı düşürmez** — fallback zaten sorunsuz çalışıyor ve replikayı rotasyondan çıkarmak bozulmuş-ama-çalışan bir durumu kesintiye çevirirdi; bilgi amaçlı `"degraded"` alanı olarak raporlanır.

### `internal/tracing`

Span modeli kasıtlı olarak asgari: `Start/SetAttributes/RecordError/End`, `Attr` değerleri yalnız string/int64/float64/bool. Event yok, link yok, baggage yok, span processor yok.

**Kapalıyken sıfır maliyet:** paket seviyesinde `atomic.Bool` + paylaşılan `noSpan` singleton'ı; `noSpan.tr == nil` olduğu için tüm metotlar erken döner. Kapalı yolda **span noktası başına bir atomik okuma ve bir dal, sıfır tahsis** — `BenchmarkDisabledSpan` ve `testing.AllocsPerRun == 0` ile iddia edilir.

Örnekleme kökte bir kez: geçerli gelen `traceparent` varsa onun sampled bayrağı, yoksa trace-ID oran kuralı (`OTEL_TRACES_SAMPLER_ARG`, varsayılan `0.05`) — aynı trace her serviste aynı örneklenir.

**Güven kapısı:** gelen `traceparent` yalnızca `TRACING_TRUST_INCOMING=true` ile onurlandırılır (varsayılan `false`). Aksi halde bir istemci her isteği tek trace id'ye çivileyip trafiğin %100'ünde `sampled=1` zorlayabilir — collector'a ucuz bir amplifikasyon saldırısı.

Yedi span noktası ve başka hiçbiri: `http.server` · `gateway.chat_completion` · `rag.retrieve` · `rag.embed_query` · `rag.rerank` · `provider.chat|embed|rerank` (`provider/http.go` `doRequest`/`doStream` — çıkışa `traceparent` yazan tek yer de burası, her chat/embed/stream/rerank oradan geçiyor) · `ingest.document` (kendi trace kökü). Streaming'de `provider.chat` span'i `streamBody.Close()`'da biter, tüm akışı kapsar.

**Tek kural, bir kez söylenip test edilen:** hiçbir attribute değeri mesaj içeriği, sorgu metni, passage metni, dosya adı, system prompt veya kimlik bilgisi taşımaz — bilinen bir secret'ı yığından geçirip yakalanan OTLP yükünde arayan bir e2e testiyle zorlanır.

Export: OTLP/HTTP + JSON. İki kodlama tuzağı ilk seferde doğru yapılmalı: **(1)** OTLP/JSON'da trace/span id'leri küçük harf hex string'dir, base64 değil (JSON eşlemesinde açık istisna); **(2)** 64-bit tamsayılar (`startTimeUnixNano`, `endTimeUnixNano`, `intValue`) JSON **string**'dir, sayı değil — sayı olarak yazmak hassasiyet kaybettirir ve bazı alıcılar reddeder.

Batch: 2048'lik kanal, 512 span veya 5 sn'de flush; dolu tampon düşürür ve sayar — tracing asla bir isteği bloklamaz. 429/5xx/taşıma hatasında 1 sn sonra tek deneme, sonra düşür. Disk kuyruğu yok.

Exporter **ayrı, düz dialer'lı bir `http.Client` kullanır** — netguard'lı olan değil. Collector normalde `http://otel-collector:4318`'de, yani SSRF guard'ının tam da engellemek için var olduğu özel bir adreste. Bu `docs/observability.md`'ye yazılır: `OTEL_EXPORTER_OTLP_ENDPOINT` `DATABASE_URL` gibi operatör tarafından yapılandırılır ve guard'ı bilerek atlar; guard yalnızca kullanıcının etkileyebildiği, kimlik bilgisi taşıyan provider çağrılarını yönetir.

`tracer.Shutdown(ctx)` 5 sn bütçeyle, `srv.Shutdown` ve `ingester.StopWithTimeout`'tan **sonra**, `stopBackground()`'dan önce.

**Elle yazılmış exporter'ın vermediği şeyler — dokümana açıkça:** auto-instrumentation yok (yalnız yukarıdaki yedi nokta; pgx sorguları, chi routing, DNS, TLS handshake ve PDF worker alt süreci span üretmez — yavaş bir sorgu `gateway.chat_completion` içinde açıklanamayan süre olarak görünür) · metrics/logs sinyali yok · OTLP/gRPC yok · sıkıştırma yok · span link/event/baggage yok · resource auto-detection yok. **Protobuf/JSON:** OTLP/HTTP + `application/json` spesifikasyonun normatif parçası ve OpenTelemetry Collector'ın `otlp` alıcısı bunu 4318'de ek yapılandırma olmadan kabul ediyor; vendor'a doğrudan giden OTLP uçları değişkenlik gösteriyor, bir kısmı yalnız protobuf. **Desteklenen yapılandırma: Ragmux'ı bir Collector'a bağlayın, yeniden kodlamayı Collector yapsın.** `docker-compose.otel.yml` + asgari `otel-collector.yaml` ile kopyala-yapıştır hale getirilir.

---

## Faz 6 — Yatay ölçek

### Ingest: claim + lease

Süreç içi `chan int64` bir **uyandırma ipucuna** iner; sahiplik veritabanına taşınır.

```go
func (s *Store) ClaimDocument(ctx, owner string, lease time.Duration, maxAttempts int) (*Document, error)
```
```sql
WITH next AS (
    SELECT id FROM documents
     WHERE (status='pending' OR (status='processing' AND (lease_until IS NULL OR lease_until < now())))
       AND attempts < $3
     ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 1)
UPDATE documents d SET status='processing', claimed_by=$1, claimed_at=now(),
       lease_until=now()+$2::interval, attempts=d.attempts+1, error='', progress_percent=0, …
  FROM next WHERE d.id=next.id RETURNING …
```

`StartDocumentProcessing` (`documents.go:122-127`) **silinir** — claim o yazmayı zaten yapıyor ve koşulsuz bir `UPDATE ... SET status='processing'`'i etrafta tutmak tam da değiştirilen cross-replica tehlikesi. Yanına `ClaimDocumentByID` (yükleme bu replikada işi az önce yarattıysa), `ExtendDocumentLease` (`RowsAffected()==0` → satır artık bizim değil) ve `ReleaseDocuments` (temiz kapanışta kendi satırlarını `pending`'e bırakır).

Topoloji *N worker kanaldan çekiyor* yerine **bir dispatcher + N worker** olur: dispatcher `notify` tekmesi veya `INGEST_POLL_INTERVAL` (5 sn) ile `ClaimDocument`'ı boşalana kadar döner ve `jobs chan *store.Document`'a yollar. Veritabanı **replika başına bir poll** görür, worker başına değil.

`Enqueue` imzasını korur (yükleme API'sinin 503 sözleşmesinin parçası) ama anlamı değişir: küme backlog derinliğini `MAX_PENDING_DOCUMENTS` (1024) ile karşılaştırır. **Belgelenecek davranış değişikliği:** yüklemede 503 artık "bu replikanın kanalı dolu" değil "kümenin backlog'u dolu" demek — kesinlikle daha iyi ve gerçek cross-replica backpressure.

Heartbeat `lease/3`'te `ExtendDocumentLease` çağırır; `ErrNotFound`'da iş context'i iptal edilir, worker "lost ingestion lease" loglar ve **başarısızlık statüsü yazmaz** (satır artık yeni sahibinin). `INGEST_LEASE` 2 dk (`processTimeout` 15 dk — lease yenilenir, işe göre boyutlanmaz).

**`Resume(ctx)` silinir.** Lease tabanlı claim ile resume edilecek bir şey yok: dispatcher'ın ilk poll'ü her `pending` satırı ve lease'i dolmuş her `processing` satırı bulur, her replikada, sürekli. `main.go:218-220` `Resume` yerine `ingester.Notify()` çağırır. `Stop` kapanış payı içinde `ReleaseDocuments` çağırır ki temiz tek-instance yeniden başlatma bugünkü anında devralmayı korusun — mevcut geliştirici deneyimi birebir korunur.

`attempts` tavanı (`INGEST_MAX_ATTEMPTS=5`) olmadan, süreci güvenilir şekilde öldüren bir doküman replikalar sırayla claim ettikçe küme çapında bir çökme döngüsüne dönüşürdü. Tavanı aşan satırı claim sorgusu atlar, janitor `failed` işaretler.

**Dokümana ve risk kaydına yazılacak dürüst çerçeveleme:** dolan bir lease ile yavaş-ama-canlı bir worker yarışırsa doküman iki kez işlenebilir. Bu **embedding parası maliyeti, veri bütünlüğü değil** — `ReplaceDocumentChunks` tek transaction'da `DELETE`+`INSERT`+`UPDATE status`, son yazan temiz kazanır. Heartbeat'in context iptali bunu nadir tutan şey.

### Janitor leader lock

`runLogged` başında `TryAdvisoryLock(ctx, store.LockJanitor)`; alamayan replika debug loglayıp döner. İlk geçişe jitter eklenir (`StartDelay + rand.N(StartDelay)`) — tamamen kozmetik, logları ve DB yükünü okunur tutar.

Reddedilen alternatif: lease satırlı `leader_election` tablosu. `pg_try_advisory_lock` şema istemiyor, bağlantı ölünce kendiliğinden bırakılıyor (bayat lease penceresi hiç yok) ve mevcut `lockMigrations`/`lockVecTables` konvansiyonuna uyuyor.

**Riski düşüren gerçek, dokümana:** her janitor adımı idempotent bir `DELETE ... WHERE created_at < cutoff`. Lock, tekrarlanan işe karşı bir optimizasyon, doğruluk gereği değil — transaction modundaki bir connection pooler session lock semantiğini bozsa bile maliyeti yalnız tekrarlanmış bir geçiş.

### SECRET_KEY canary'si

`loadCipher` (`crypto.go:24`) `SECRET_KEY` yokken `DATA_DIR/secret.key` üretiyor. N replikada her biri kendi anahtarını yazar, ikinci replika birincinin yazdığı her kimlik bilgisini çözemez — kafa karıştırıcı, istek başına, çalışma zamanı hatası.

**Replika sayısı değil, anahtar ıraksaması tespit edilir ve boot'ta fail edilir.** `store.Open`'da `loadCipher`'dan sonra, `UpgradeConnectionKeys`'ten **önce** (aksi halde o ham bir decrypt hatasıyla ölür): `instance_settings`'e `ON CONFLICT DO NOTHING` ile sabit bir plaintext mühürlenir, sonra okunup açılır. Açılamazsa ölümcül ve açıklayıcı bir mesaj (anahtarın kaynağını da söyleyerek, `docs/scaling.md#secret-key`'e yönlendirerek). Bu aynı zamanda "dump'ı yeni sunucuya restore ettim", "anahtarı tek node'da rotate ettim" ve "data volume'ü sildim" vakalarını da yakalar. Tek instance'ta maliyeti sıfır.

⚠️ **Sert bağımlılık:** `ReencryptConnections` (ve dolayısıyla `ragmux rotate-key`) canary'yi **aynı transaction'da yeniden mühürlemek zorunda**, yoksa yeni anahtarla ilk boot fail eder. `TestRotateKeyReSealsCanary` regresyon testi ile birlikte uygulanır — bu kümede yanlış yapılması en kolay şey, "olsa iyi olur" değil bloke edici bir checklist maddesi.

### Kanıt testi: streaming sticky değil

`internal/e2e/streaming_replica_test.go`: tek `testdb.Config(t)` üzerine **iki** tam router yığını (mevcut `newEnvWith` bunu zaten destekliyor). Yavaş SSE akıtan bir mock upstream; A isteği replika 1'e, B isteği eşzamanlı olarak **aynı proje key'iyle** replika 2'ye; ikisi de tamamlanmalı, ikisi de `request_logs` satırı yazmalı ve `rpm=2` ile üçüncü istek hangi replikadan gelirse gelsin 429 almalı — limitlerin süreç değil veritabanı özelliği olduğunun kanıtı. Sonra replika 1'in sunucusu akış ortasında kapatılır: replika 2 etkilenmemeli ve istemcinin oraya yaptığı retry başarılı olmalı — sunucu tarafı akış durumu olmadığının, dolayısıyla session affinity gerekmediğinin kanıtı. Replika 1'in kesilen akış satırı `499`, replika 2'ninki 200 olmalı.

Yanında: `ingest_cluster_test.go` (iki ingester, 10 doküman, **tam olarak 10 embed geçişi**, hepsi `ready`, `attempts <= 1`), lease-expiry testi (A 1 sn lease ile claim edip heartbeat atmaz; B süre dolunca `attempts=2` ile devralır), `TestRetentionPassRunsOnLeaderOnly`.

### Compose ve paketleme

`docker-compose.split.yml` yazıldığı haliyle ölçeklenemiyor: `container_name: ragmux` ve sabit `127.0.0.1:8765:8765` engel. Yeni overlay `docker-compose.scale.yml` (`-f docker-compose.split.yml -f docker-compose.scale.yml up -d --scale ragmux=3`): `container_name` düşer, `ports` efemeral olur, `docs/configuration.md#behind-a-reverse-proxy`'deki nginx örneği + `proxy_buffering off` + `proxy_read_timeout >= STREAM_MAX_DURATION` eklenir.

**Varsayılan AIO compose ölçeklenemez** (Postgres'i içinde barındırıyor) — dosyanın başına yorum ve `docs/scaling.md`'ye yazılır.

**ParadeDB varyantı:** `docker-compose.paradedb.yml` (split'in `postgres.image`'ı `paradedb/paradedb:latest-pg17@sha256:…` — digest pinlenir, `Dockerfile.aio`'nun pgvector'ü pinlemesiyle aynı; imaj hem `pg_search` hem `vector` taşıdığı için başka hiçbir şey değişmez) ve `Dockerfile.aio.paradedb` (`ghcr.io/ragmux/ragmux:<version>-paradedb`). AIO varyantında doğrulanacak iki şey: `entrypoint.sh`'ın değişmeden çalışması için `gosu` ve `pg_ctl` imajda olmalı; ve `shared_preload_libraries = pg_search` ayarlı olmalı.

`docker/postgres-init/01-ragmux.sql` üç layout'a da hizmet etmeye devam eder; `pg_available_extensions`'a bakan idempotent bir `DO` bloğu eklenir (`CREATE EXTENSION IF NOT EXISTS pg_search` + `GRANT USAGE ON SCHEMA paradedb TO ragmux_app`). SQL'imiz hep şema nitelikli (`paradedb.score`, `paradedb.match`) olduğu için `search_path` `public` kalır ve yalnız `USAGE` verilir. Aynı blok yeni `02-extensions.sql` olarak **her açılışta** koşar (yalnız boş PGDATA'da değil) — AIO yükseltmesini düz bir imaj değişimine indiren şey bu.

⚠️ **CI'daki tek bilinmeyen:** pg_search `shared_preload_libraries` gerektiriyor. `paradedb/paradedb` imajı bunu kendi entrypoint'inde ayarlıyorsa düz bir `services:` bloğu yeter; `-c shared_preload_libraries=pg_search` geçirmek gerekiyorsa `services:` bunu yapamaz ve job'un açık bir `docker run -d … postgres -c …` + `pg_isready` bekleme adımıyla başlaması gerekir. Önce doğrulanacak; iki şekil de birkaç satır.

**Yükseltme yolu (dump/restore yok):** split'te imajı ParadeDB digest'ine çevir (**aynı PG major, 17** — farklı major `pg_upgrade` ister, bu kural açıkça yazılacak), `01-ragmux.sql`'i bir kez elle koştur, gateway'i yeniden başlat (`Open` yetenekleri yeniden okusun), store başına `search_backend: "pg_search"` yap. AIO'da imajı `-paradedb`'ye çevirip `up` — `/data/pg` aynı PG 17 cluster'ı, `02-extensions.sql` extension'ı açılışta yaratır. **Geri dönüşte önce store'ları `pgvector`'a al, sonra imajı değiştir**; tersini yaparlarsa çalışma zamanı fallback'i aramayı ayakta tutar ve loglar — o fallback tam olarak bunun için var.

⚠️ **`docs/backup-restore.md`'ye girmesi gereken yedek tuzağı:** ParadeDB veritabanından alınan bir `pg_dump`, pgvector-only bir sunucuya **restore edilemeyecek** bir `CREATE INDEX ... USING bm25` ifadesi içerir. `pg_dump`'ın `--exclude-index`'i yok. İki çalışan reçete tam komutlarıyla yazılacak: dump'tan önce `DROP INDEX idx_chunks_bm25`, ya da `pg_restore -l`/`-L` ile o TOC girdisini filtrelemek.

---

## Faz 7 — Dokümantasyon, landing ve sürüm

### Ragmux deposu

| Dosya | Değişiklik |
|---|---|
| `docs/observability.md` **(yeni)** | `/metrics` tam metrik tablosu (ad \| tip \| label \| anlam), yetki kararı ve **gerekçesi**, env tablosu, Prometheus scrape örneği, kardinalite checklist'i, `/readyz` vs `/healthz`; tracing (env, yedi span, sampling, **neyin enstrümante edilmediği**, Collector gerekliliği ve JSON kodlama uyarısı), `docker-compose.otel.yml` |
| `docs/scaling.md` **(yeni)** | Zaten paylaşılan olanlar (tablo) · ayarlamanız gerekenler (SECRET_KEY + canary, harici Postgres, connection pooling ve PgBouncer session mode notu) · replikalar arası ingest (claim/lease/heartbeat, 503'ün değişen anlamı) · retention leader lock'u · **streaming ve affinity gerekmemesi, nedeni** · rolling restart ve shutdown · compose örneği · Kubernetes notları (`/readyz` readiness, `/healthz` liveness, preStop sleep, PDB, `requests_in_flight` üzerinde HPA) · replike edilmeyenler |
| `docs/users-and-limits.md` | Yeni `## API keys` bölümü (kind'lar, prefix'ler, scope tabloları, "sahibinin rolünü aşamaz" kuralı, süre/iptal, reveal-once, `X-Ragmux-Project`); `## Rate limits and budgets`'a key alt limitleri; `## Audit log`'a `apikey.*` ve `cli` aktörü; `## Metrics and retention`'a yeni kolonlar |
| `docs/api.md` | `sk-mgmt-` bearer alternatifi + **CSRF'in neden etkilenmediği paragrafı**; `sk-user-` ve `X-Ragmux-Project` (ve `proje/model`'in neden desteklenmediği); `### API keys` blok; yeni `code` değerleri; `### Model prices`; `/metrics`, `/readyz`, `/admin/api/search-backends`; metrik JSON'una `cost_*`; CSV kolon listesi; kurulum durumunun iki yeni alanı; yüklemede 503'ün değişen anlamı |
| `docs/providers.md` | Başa **Capabilities** tablosu; Gemini (tool'lar, **hangi şema anahtar kelimelerinin düşürüldüğü/dönüştürüldüğü**, görsel inline'lama, `fileData`'nın artık Files-API/`gs://` ile sınırlı olması, cached token); Ollama (uzak görseller ve sınırları, tek-parça tool call çerçevelemesi protokol özelliği olarak); Anthropic (`cache_control` üç yerleşimi, yeni `prompt_tokens` muhasebesi, inline'a geçirmenin tek satır olması); OpenAI ("caching otomatik — hiçbir şey gönderilmez, `cached_tokens` yüzeye çıkar"); `cohere_rerank`/`voyage_rerank`'in yalnız rerank olduğu |
| `docs/rag.md` | Store ayarları tablosuna üç yeni alan; `### Search backends` (tsvector vs BM25, pg_search'te `fts_config`'in yok sayılması, **yeniden işleme gerekmemesi**, lazy index, fallback); `## Reranking`'e üç backend, gecikme/maliyet profilleri ve fallback sözleşmesi; arama yanıtına `backend`, `rerank_backend`, `rerank_fallback`, `lex_score` |
| `docs/configuration.md` | `IMAGE_*` (6), `METRICS_*` (3), `TRACING_*` + `OTEL_*`, `INGEST_LEASE`/`INGEST_POLL_INTERVAL`/`INGEST_MAX_ATTEMPTS`/`MAX_PENDING_DOCUMENTS`, `RERANK_TIMEOUT`, `PG_SEARCH_TOKENIZER`; `reset-password` bloğu `rotate-key` yanına; deployment layout'larına ParadeDB varyantı + `scaling.md` çapraz bağlantısı |
| `docs/backup-restore.md` | BM25-index-in-a-dump tuzağı, iki reçete |
| `.env.example` | Yeni anahtarlar mevcut üslupla: her birinin üstünde ne işe yaradığı **ve neden**, opsiyoneller `#` ile kapalı. Faz 1 **yeni env değişkeni eklemez** (her şey key başına ve veritabanında) — bu karar görünür olsun diye yazılıyor |
| `README.md` | `## Features` (L26-42): BM25 backend, üç reranker, Gemini tool'ları, prompt caching passthrough, maliyet takibi, per-user key'ler, management key'ler/scope'lar; depo düzeni bloğuna `internal/metrics/` ve `internal/tracing/`; `## Documentation` tablosuna iki yeni sayfa; `## Dashboard` listesine **Keys** ve **Prices**; `## Quick start`'a 3 adımlı sihirbaz; **`## Roadmap` (L235) üç maddeyi düşürür** — Gemini tool calling, Prometheus endpoint'i, prompt caching passthrough. Kalanlar: 0.1 import aracı, OCR, SSO/OIDC |
| `CHANGELOG.md` | `## [Unreleased]` altına `## [0.4.0] — YYYY-MM-DD` (em dash). `### Changed` şunları **açıkça** söylemeli: Anthropic `prompt_tokens` artık cache-read ve cache-write token'larını içeriyor (eski satırlar eksik raporluyordu, doldurulamaz) · `provider-types`'ta `gemini.supports_tools` `true`'ya döndü · Gemini uzak görselleri artık inline ediliyor · CSV altı kolon kazandı · yüklemede 503 artık küme backlog'u demek · `Ingester.Resume` kaldırıldı · farklı anahtarla şifrelenmiş bir veritabanı artık istek başına hata vermek yerine **açılışta reddediyor** · `rag.Reranker` artık bir arayüz |

### ragmux.com

1. `scripts/sync-docs.mjs` `PAGES` (L46-71) — `SECURITY.md` girdisinden sonra iki satır: `docs/observability.md` → slug `observability`, `operations`, order 32; `docs/scaling.md` → `scaling`, `operations`, order 33. Sonra `npm run sync-docs` ve üretilen `src/content/docs/en/{observability,scaling}.md` **commit edilir** (script uzlaştırıcı: eşlenmemiş dosyaları siliyor, elle temizlik yok). `ragmux.com/docs/RECIPES.md`'deki mevcut reçete izlenir.
2. `src/site.config.ts:76` — `PROJECT.version: '0.3.1'` → `'0.4.0'` (wordmark yanındaki sürüm çipi).
3. `src/data/landing.ts` — **EN ve TR birlikte** (tip zaten yarım çeviriyi derleme hatası yapıyor):
   - `docs.roadmapLabel` (EN L357, TR L620) → `'Not in 0.4.0'` / `'0.4.0'da yok'`; `roadmap` listesinden **"a Prometheus endpoint" / "Prometheus ucu"**, **"Gemini tool calling"** ve **"prompt caching passthrough"** çıkarılır. Kalan: OCR, SSO/OIDC, 0.1 import aracı.
   - `retrieval.cards` (EN L222-235, TR L485-498) — **üçlü grid, dördüncü kart eklemeden önce layout doğrulanacak**. `fts_config` kartı "BM25 or tsvector" ile değiştirilir (ParadeDB `pg_search`'e yönlendirme, dokümanları Postgres'ten çıkarmadan, iki yönde de yeniden işleme yok), rerank kartı üç backend'i ve fallback sözleşmesini anlatacak şekilde güncellenir, `max_distance` kartı kalır.
   - `docs` bölümünün kart dizisine (EN ~L340-356, TR ~L603-619) **Observability** ve **Scaling** kartları, doğru `slug`'larla.
   - `control.cards`, `proof.facts`, `dropin.bullets` yeni özelliklere karşı denetlenir (per-user key'ler ve maliyet takibi muhtemelen `control` tarafına ait).
4. Landing'in kendi kuralı (`landing.ts:1-8`): *"bir olgu kaynakta yoksa burada da yeri yok"* — yazılan her cümlenin karşılığı kodda olmalı.

---

## Doğrulama

Her fazın sonunda, sırayla:

```bash
cd /Users/muhammetsafak/Documents/Projects/Tunedness/Tools/Ragmux/Ragmux
make dev-db            # pgvector'lü test Postgres'i
go build ./... && go vet ./...
golangci-lint run      # .golangci.yml mevcut
go test -race -count=1 ./...
make docker-build      # AIO imajı hâlâ kuruluyor mu
```

Faz 4 sonrası ek olarak ParadeDB yolunda:

```bash
make dev-db-paradedb
make test-paradedb     # store + rag + e2e, pg_search'lü sunucuya karşı
```

**pg_search'ü extension olmadan test etmenin üç katmanı** — regresyon değerinin çoğu ilk katmanda:
1. **Sunucusuz SQL-şekli birim testleri** (`search_pgsearch_test.go`): `HybridQuery` saf bir builder; SQL golden-file'lanır, arg sayısı/sırası, store id'sinin `$2` olup asla interpolate edilmediği, `fts_config`'in pg_search SQL'inde hiç geçmediği ve `ftsQuery`'nin o yolda uygulanmadığı iddia edilir. **Her CI job'ında koşar.**
2. **Yetenek kapılı entegrasyon testleri** (`testdb.RequirePgSearch(t, s)` → `t.Skipf`, mevcut konvansiyon), yalnız `test-paradedb` job'ında. `testdb` şema ile izole ediyor (`search_path=<schema>,public`); extension `public`'te, fonksiyonları `paradedb`'de — yani tüm SQL'imizin şema nitelikli kalması gerekiyor ve bu testler aynı zamanda o kontrolü yapıyor.
3. **pgvector job'ında fallback testleri** (ParadeDB gerektirmeyen, en değerli olanlar): `validateRAG` extension'sız sunucuda 400 veriyor; DB'ye doğrudan yazılmış bir satır (restore edilmiş dump simülasyonu) `pgvector`'a düşüyor, sonuç döndürüyor, `Result.Backend == "pgvector"` ve bir kez logluyor; smoke-query downgrade yolu.

CI'a eklenecekler: `test-paradedb` **ikinci bir job olarak** (mevcut pgvector job'ı değiştirilmez — fallback yolunun extension'sız bir sunucuda test edilmeye devam etmesi gereken şey tam olarak o), `docker` job'ına `Dockerfile.aio.paradedb` build'i (push yok), `release.yml`'ye `-paradedb` tag'i.

Uçtan uca elle doğrulama (Faz 7'den önce): temiz volume ile `docker compose up` → 3 adımlı sihirbaz → bir `sk-user-` key'i üret, iki projeye grant ver, `X-Ragmux-Project` ile çağır → `/metrics`'i token'la scrape et → Collector'lı `docker-compose.otel.yml` ile trace'leri gör → `--scale ragmux=3` ile doküman yükle ve tam olarak bir kez işlendiğini doğrula.

---

## Riskler

| Risk | Azaltma |
|---|---|
| **Aynı numaralı migration sessizce atlanır** (doğrulandı: `migrate()` hata vermiyor) | Faz 0'da tekillik/ardışıklık testi; 0010/0011/0012 tahsisi bu planda sabit |
| `auth.Middleware` her `/admin/api` rotasına dokunuyor — buradaki hata tam yetki atlatması | Yeni dal açık bir allowlist (`sk-mgmt-` **ve** bearer **ve** cookie-değil); `RequireRole` sahibin canlı rolüne bakmaya devam ediyor; `csrf_test.go` cookie'ye yapıştırma vakasını kazanıyor |
| Gemini şema sanitize'ı **tasarım gereği kayıplı** — `oneOf` ayrımına dayanan bir tool Gemini'de farklı davranır | Tool başına düşürülen anahtarların debug log'u, dokümanda tam beyaz liste, asla istek reddedilmez |
| Anthropic `prompt_tokens` muhasebesi değişimi geçmiş dashboard karşılaştırmalarını kaydırır | CHANGELOG "Changed"; eski satırlar doldurulamaz, bu açıkça söylenir |
| **Görsel indirme, gateway'in ilk kez istemci-kontrollü bir URL'i takip etmesi** | netguard SSRF'i kapatıyor; istek başına adet + görsel başına boyut + timeout amplifikasyonu kapatıyor; `IMAGE_FETCH=false` kill switch |
| `/metrics` proje envanterini ve harcamayı sızdırıyor | Varsayılan kapalı, token kapılı, ayrı listener seçeneği, ve kimliksiz-public yapılandırmanın başlatmayı reddetmesi. Kalan risk (aynı public proxy'nin arkasına koymak) dokümanda uyarı kutusu |
| Metrik kardinalitesi | `route` chi pattern'inden, `model` `conn.ModelName`'den; `MaxSeries` 5000 + düşürülen seri sayacı + docs checklist'i |
| **Canary vs `rotate-key`**: `ReencryptConnections` canary'yi yeniden mühürlemezse rotasyondan sonraki ilk boot fail eder | Birlikte uygulanır + `TestRotateKeyReSealsCanary`. Bloke edici checklist maddesi |
| Ingest çift işleme (lease dolması yavaş-ama-canlı worker ile yarışırsa) | Heartbeat context iptali; ve dürüst çerçeveleme: `ReplaceDocumentChunks` transactional, **embedding parası maliyeti, bozulma değil**. `attempts` tavanı küme çapında çökme döngüsünü engelliyor |
| **Sohbet/embed edemeyen iki provider tipi mevcut dashboard picker'larında görünür** | `Capabilities` alanı + `web/index.html`'deki **her picker'ın tek tek denetlenmesi**. Bu kümenin sessiz bozulma riski |
| pg_search sorgu API'si sürümler arası değişiyor | Digest pinleme; tüm pg_search SQL'i tek dosyada builder arkasında; golden test; ve açılıştaki smoke query downgrade'i |
| BM25 index build süresi büyük korpusta chunk yazmalarını dakikalarca bloklar | Advisory lock, satır sayısı + süreyle loglama, kendi histogramı, ve büyük kurulumlar için belgelenmiş manuel `CREATE INDEX CONCURRENTLY` alternatifi |
| PgBouncer transaction mode session advisory lock'u bozuyor | Session mode dokümante edilir; ayrıca janitor adımları idempotent, lock doğruluk gereği değil — düşük kalan risk |
| OTLP/JSON'u her backend kabul etmiyor | Collector zorunlu kılınır ve `docker-compose.otel.yml` ile kopyala-yapıştır hale getirilir; exporter her export hatasını düşür-ve-say olarak ele alır, asla istek hatası yapmaz |
| Dashboard CSP testi yeni UI bağlamalarında kırılır | Tüm yeni handler'lar `on()` / `.onclick =` üzerinden; tek-`<script>` ve inline-handler-yok iddiaları sert kısıt |
