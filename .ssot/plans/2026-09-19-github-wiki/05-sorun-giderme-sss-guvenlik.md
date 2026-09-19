# Faz 05 — Troubleshooting + FAQ + Security Hardening

- **Durum:** tamam (review CHANGES REQUESTED → 10 düzeltme uygulandı)
- **Sahip:** backend-engineer
- **Bağımlılık:** Faz 02, 03, 04 (kodlar ve reçeteler oralarda yerleşti)
- **İlgili karar:** ADR-002, ADR-003, ADR-005

Bu brief kendi kendine yeter. Ortak kurallar için **önce** aynı dizindeki
`00-plan.md`'yi oku — bağlayıcıdır.

## Amaç

Bir şey ters gittiğinde açılacak sayfayı, kısa soruların sayfasını ve public
bir kurulumu sertleştirme kontrol listesini yazmak.

## Kapsam

### `Troubleshooting.md`

Belirti → sebep → çözüm. Aşağıdaki malzeme depodan doğrulanarak çıkarıldı;
**her kodu yazmadan önce `grep` ile `internal/` içinde teyit et**, bulunmayan
satırı sil.

**1. Gateway `/v1` errors.** Zarf: `{"error":{"message","type","code"[,"scope"]}}`.

- `401 invalid_api_key`, kodsuz — bilinmeyen key. Gövde bilinçli olarak
  belirsiz; bilinmeyen bir key hiçbir şey sızdırmaz.
- `401` + `key_revoked` / `key_expired` / `key_owner_inactive` — yalnız
  `sk-user-…` gateway key'leri için çıkar, `sk-proj-…` için çıkmaz. Sırasıyla:
  yeni key bas; süresi geçmiş, yenile; key sahibi kullanıcı pasifleştirilmiş,
  ya kullanıcıyı aktifleştir ya başka hesapla key bas.
- `403 project_not_granted` — `X-Ragmux-Project` grant verilmemiş bir projeyi
  adlandırıyor, ya da key'in hiç grant'ı yok. "Böyle proje yok" ile "grant
  yok" aynı cevabı verir, id sızmasın diye.
- `400 project_required` — key birden çok projeye grant'lı ve hiçbiri
  adlandırılmamış; `X-Ragmux-Projects` yanıt başlığı adları verir.
- `403 insufficient_scope` — key'in scope'u yetmiyor (gateway key'leri:
  `chat`, `models`; yönetim key'leri: `read`, `write`, `admin`, `keys`).
- `429` + `rate_limit_rpm` / `rate_limit_tpm` (`type: rate_limit_exceeded`) ya
  da `budget_daily` / `budget_monthly` (`type: insufficient_quota`); `scope`
  alanı `project` ya da `key` — hangi katman daha sıkıysa o reddeder.
  `Retry-After` her zaman var.
- `429 image_fetch_saturated` — süreç geneli görsel indirme kuyruğu dolu.
  Kaynak sınırı, arıza değil (**ADR-005**); `Retry-After` kadar bekle,
  `IMAGE_FETCH_MAX_CONCURRENT`'ı yükselt ya da isteklerdeki görsel sayısını
  düşür.
- `500` "model connection unavailable" — projenin bağlantısı yok ya da sağlıklı
  değil; Models ekranından bağlantıyı test et.
- `502 upstream_error` — sağlayıcı hatası ayrıştırılamadı ya da taşıma
  başarısız. Ayrıştırılabilen sağlayıcı hataları kendi `type`/`code`'uyla
  aynen iletilir.

**2. Admin API errors.**

- `403 session_required` — parola değiştirme, başkasının parolasını sıfırlama
  ve yönetim key'i basma interaktif session ister; API key'le yapılmaz.
- `409 key_in_use` — istek kaydı atfedilmiş bir key silinemez; revoke et.
- `409 user_has_attributed_usage` — aynısı kullanıcı için; deaktive et.
- `422 store_quota` — yükleme kotayı aşıyor; hiçbir şey yazılmaz.
- `409 builtin_price` / `no_builtin_price` — builtin fiyat satırı silinemez;
  düzenle ya da sıfırla. `reset` yalnız shipped karşılığı olan satırda çalışır.
- Kodsuz `409` — son aktif admin deaktive/düşürülemez; kodsuz `400` — kendi
  hesabını deaktive edemezsin.
- `401 unauthorized` / `403 forbidden "cross-site request rejected"` (CSRF) /
  `403 "insufficient role"`; üye olunmayan proje **404** döner, 403 değil.

**3. Startup failures.** Süreç ayağa kalkmıyorsa:

- `DATABASE_URL (or DATABASE_URL_FILE) is required`.
- `SECRET_KEY must be 64 hex characters (32 bytes)` — `openssl rand -hex 32`.
- **`SECRET_KEY does not match the one this database was written with`** — en
  sık görülen ciddi hata. İlk replika `instance_settings` içine bir canary
  mühürler, sonraki her açılış onu doğrular. Sebepleri: `SECRET_KEY` hiç
  verilmemiş (her replika kendi `DATA_DIR/secret.key`'ini üretir), dump başka
  anahtarlı bir makineye geri yüklenmiş, rotation yalnız bir node'a
  uygulanmış, data volume silinmiş. Çözüm: özgün anahtarla başlat, ya da
  özgün anahtar ortamdayken `ragmux rotate-key` ile veritabanını yeni anahtara
  taşı.
- `upgrade stored provider keys (is SECRET_KEY the right one?)` — 0.2.3 öncesi
  kimlik bilgisi yeniden mühürleme başarısız; neredeyse her zaman yanlış
  `SECRET_KEY`.
- `METRICS_ENABLED=true` ama `METRICS_TOKEN` yok → açılış reddedilir.
- Geçersiz herhangi bir env değeri `config: <sebep>` basıp `2` ile çıkar.

**4. `/readyz` and `/healthz`.**

- `/healthz`: liveness, DB ping. `200 {"status":"ok","version":…}` ya da
  `503 db unavailable`.
- `/readyz` `503 "migrating"` — şema binary'nin beklediğinden geri. Migration
  sürüyor olabilir ya da eski bir replika yeni binary'den önce ayağa kalktı.
- `/readyz` `200` + `degraded` — şema binary'den **ileri**; rolling upgrade
  sırasında beklenen durum (**ADR-001**). Alarm `degraded`'in görünmesine
  değil, deploy penceresi dışında **kalıcılaşmasına** kurulur.
- `/readyz` `200` + `degraded: ["pg_search is not installed …"]` — store
  pgvector'a düşmüş; arama çalışıyor, bilgilendirme.

**5. Ingestion problems.**

- Belge `failed`: chunk sayısı `MAX_CHUNKS_PER_DOCUMENT`'i (20000) aştı — hata
  mesajı ayarı adlandırır; PDF sınırları (60 s, 2000 sayfa) aşıldı; taranmış
  PDF'de metin katmanı yok (OCR yok).
- Yüklemede `503 "the ingestion backlog is full (MAX_PENDING_DOCUMENTS)"` —
  küme geneli `pending` birikimi tavanda; belge aynı anda `failed` işaretlenir.
- Belge `pending`'de asılı: bir replika claim etti ve öldü — `INGEST_LEASE`
  dolunca yeniden claim edilebilir hâle gelir. `INGEST_MAX_ATTEMPTS` kez claim
  edilmiş belgeyi bakım işi `failed` yapar; başarılı bir ingest, yeni yükleme
  ya da reprocess sayacı sıfırlar.
- Log'da `lost ingestion lease; another replica took over` — beklenen,
  zararsız.

**6. Private upstream refused.** `upstream host "…" resolves to a private or
local address; set ALLOW_PRIVATE_UPSTREAMS=true or add it to
PRIVATE_UPSTREAM_ALLOWLIST`. Hostname bazlı allowlist tercih edilir. **Bu
muafiyet `image_url` için geçerli değildir** — görsel indirme yolu ayrı ve
daha sıkı bir politikayla koşar, çünkü URL'yi bir key sahibi seçer, bir
operatör değil.

**7. Locked out.** Eşikler: `LOGIN_USER_LIMIT_PER_MIN` (5, kullanıcı adı
başına), `LOGIN_RATE_LIMIT_PER_MIN` (10, IP başına),
`LOGIN_LOCKOUT_FAILURES` (20, **kullanıcı adı + IP çifti** başına)
`LOGIN_LOCKOUT_MINUTES` (15) penceresinde. Çift üzerinden kilitlenir ki bir
yabancı senin kullanıcı adını uzaktan kilitleyemesin. Başarılı giriş sayacı
sıfırlamaz, pencereler dolar. `429` gövdesindeki `locked: true` kilit,
`false` dakikalık limit demek. `401` gövdesi `attempts_remaining` taşır.
Adminler `GET /admin/api/security/logins` ile canlı sayaçları görür. Her admin
kilitliyse yol: `ragmux reset-password <username> --generate` (ya da `--stdin`);
`DATABASE_URL` ister, gateway'in çalışması gerekmez; varsayılan olarak
kullanıcının session'larını iptal eder, `--revoke-keys` ile API key'lerini de.

**8. Cost shows `none` / metrics froze.**

- Maliyet `none`: modele uyan fiyat satırı yok. Prices ekranından ekle.
  Maliyet her yüzeyde **tahmindir**, fatura değil.
- `ragmux_metrics_series_dropped_total` artıyor: `METRICS_MAX_SERIES` (5000)
  doldu. Tahliye **yok** — tavan dolduktan sonra her yeni seri kalıcı kaybolur
  (**ADR-002**). Tavanı yükselt ya da kardinaliteyi düşür; herhangi bir artışı
  olay say.

### `FAQ.md`

Her cevap 2-4 cümle + link. En az şunlar:

- **Why no Redis, no separate vector database?**
- **Why is my `model` field ignored?** Proje modeli seçer; alan yankılanır.
- **Is the cost figure a bill?** Hayır, tahmin.
- **I set a limit to `0` and nothing is limited.** `0` = **sınırsız**, bu kod
  tabanının her yerinde (proje limitleri, key sub-limit'leri, login eşikleri).
  En sık düşülen tuzak.
- **I removed a user from a project but their key still works.** Grant'lar
  key oluşturulurken donar; membership'ten bağımsız yaşar. Durdurmak için
  key'i revoke et, sub-limit ver ya da hesabı deaktive et.
- **A `viewer` created an API key and spent money.** Rol **admin yüzeyini**
  sınırlar, `/v1` harcamasını değil (**ADR-003**). `/v1` rolü hiç okumaz,
  yalnız hesabın aktif olup olmadığına bakar.
- **Does switching to ParadeDB/BM25 need reprocessing?** Hayır.
- **Can I run several replicas of the all-in-one image?** Hayır; `-app` imajı
  ve dış PostgreSQL gerekir.
- **Do you support OCR / SSO / cost-denominated budgets?** Hayır, bilinçli
  kapsam dışı.
- **My prompt token counts jumped after upgrading to 0.4.** Anthropic cache'li
  isteklerde artık büyük sayı raporlanıyor; önceki sürümler eksik sayıyordu.
- **Why is `/metrics` authenticated?** Proje başına kullanım ve harcama
  yayınlar.
- **Where do I report a security problem?** `SECURITY.md`, public issue değil.

### `Security-Hardening.md`

`SECURITY.md`'yi tekrarlamaz; işletme adımlarına çevirir. Kontrol listesi
biçiminde, her madde "neden" cümlesiyle:

1. **Terminate TLS in front** — `TRUST_PROXY_HEADERS`, `TRUSTED_PROXY_CIDRS`,
   `SECURE_COOKIES`. Proxy arkasında değilken `TRUST_PROXY_HEADERS` açmak
   istemci adresini sahte hâle getirir.
2. **Keep `SECRET_KEY` out of the image and with your backups.** Dosya
   fallback'i yalnız geliştirme içindir. Rotation yolu Operations Runbook'ta.
3. **Lock down `/metrics`** — `METRICS_TOKEN` zorunlu; `METRICS_LISTEN` ile
   ayrı bir loopback porta alınırsa ana router'a hiç bağlanmaz, yani hiçbir
   reverse proxy kuralı onu açamaz.
4. **Leave the SSRF guard on.** `ALLOW_PRIVATE_UPSTREAMS=true` filtreyi tümden
   kapatır ve proxy değişkenlerini yeniden açar; tek bir host için
   `PRIVATE_UPSTREAM_ALLOWLIST` yeter. Görsel indirme yolu bu muafiyeti
   **miras almaz** ve alamaz.
5. **Key hygiene** — scope'u dar tut, `expires_at` ver, sub-limit koy,
   kullanılmayanı revoke et. Kullanım geçmişi olan key silinemez, revoke
   edilir.
6. **Understand what a role does and does not bound** — ADR-003 özeti;
   harcamayı durduran şey rol değil, key ve proje limitleridir.
7. **Pin and verify the image** — sürüm ya da digest pinle, `cosign verify`
   ile doğrula (v0.3.1+).
8. **Budgets and rate limits as a blast radius** — her projeye bir tavan;
   `0`'ın sınırsız demek olduğunu bir kez daha söyle.
9. **Retention and the audit log** — ne kadar saklanıyor, kim ne yaptı.
10. **AGPL network clause** — değiştirilmiş bir sürümü ağ üzerinden servis
    ediyorsan kullanıcılarına kaynağı sunmalısın.

## Dokunulacak dosyalar

- `../wiki/Troubleshooting.md` (yeni)
- `../wiki/FAQ.md` (yeni)
- `../wiki/Security-Hardening.md` (yeni)

## Dokunulmayacak

- `../wiki/` altındaki başka hiçbir dosya.
- `Ragmux/` deposundaki hiçbir dosya.
- `git commit` / `git push`.
- `SECURITY.md`'nin içeriği: kopyalanmaz, linklenir.

## Kabul ölçütü

- Üç dosya var, hepsi İngilizce.
- **Troubleshooting'deki her hata kodu `internal/` içinde `grep` ile
  bulunabiliyor.** Bulunamayan satır silinmiş olmalı. Bu ölçüt kanıtla
  raporlanır: hangi kodlar arandı, hepsi bulundu mu.
- `0` = sınırsız kuralı FAQ'de **ve** Security Hardening'de geçiyor.
- ADR-002, ADR-003 ve ADR-005 doğru anlatılmış (`.ssot/ADR.md` okunarak).
- `ragmux reset-password` kullanımı gerçek bayraklarla yazılı
  (`--generate` / `--stdin` biri zorunlu; `--password` bayrağı **yoktur**).
- SECRET_KEY uyuşmazlığı hata metni birebir depodakiyle uyumlu.
- Security Hardening `SECURITY.md`'deki paragrafları tekrarlamıyor.

## Notlar

Kaynaklar: `Ragmux/internal/gateway/gateway.go`, `Ragmux/internal/admin/`,
`Ragmux/internal/auth/` (`auth.go`, `policy.go`, `ratelimit.go`),
`Ragmux/internal/config/config.go`, `Ragmux/internal/netguard/netguard.go`,
`Ragmux/internal/rag/ingest.go`, `Ragmux/internal/limits/limits.go`,
`Ragmux/cmd/ragmux/main.go` (readyz ~555-616), `Ragmux/cmd/ragmux/resetpw.go`,
`Ragmux/docs/users-and-limits.md`, `Ragmux/docs/scaling.md`,
`Ragmux/docs/observability.md`, `Ragmux/SECURITY.md`, `.ssot/ADR.md`.

Hata mesajlarını **tercüme etme ve yeniden ifade etme** — kullanıcı onları
aratacak. Depodaki metni birebir al.
