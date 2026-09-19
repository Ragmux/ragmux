# Faz 02 — Deployment Recipes + Operations Runbook

- **Durum:** tamam (review BLOCKED → düzeltme → doğrulama turu → son iki düzeltme)
- **Sahip:** backend-engineer
- **Bağımlılık:** Faz 01 (sayfa adları ve sidebar sabitlendi)
- **İlgili karar:** ADR-001 (`/readyz` rolling update davranışı)

Bu brief kendi kendine yeter. Ortak kurallar için **önce** aynı dizindeki
`00-plan.md`'yi oku — bağlayıcıdır.

## Amaç

Kurulum şekillerini ve 2. gün işlerini, kopyalanıp çalıştırılabilir iki sayfada
toplamak. Referans (`docs/configuration.md`, `docs/backup-restore.md`,
`docs/scaling.md`, `docs/observability.md`) tekrarlanmaz, linklenir.

## Kapsam

### `Deployment-Recipes.md`

Beş reçete, her biri aynı kalıpta: **ne zaman bunu seçersin → komutlar →
gereken `.env` → doğrulama → sonraki adım**.

1. **Single container (`docker run`)** — `ragmux/ragmux:latest`, gateway +
   kendi PostgreSQL 17 + pgvector'ü tek konteynerde (`Dockerfile.aio`).
   Volume `ragmux-data:/data`; `/data/pg` cluster (`PGDATA`), `/data/ragmux`
   `secret.key` fallback (`DATA_DIR`). Gömülü PostgreSQL'in TCP listener'ı yok,
   tek port `8765`. Doğrulama: `curl localhost:8765/healthz`.
2. **Compose, default layout** — `git clone`, `cp .env.example .env`,
   `SECRET_KEY` üretimi, `docker compose up -d`. `docker-compose.yml`
   servisleri: `ragmux` (port `127.0.0.1:8765:8765`, volume'ler `ragmux-data:/data`
   ve `ragmux-pgsocket:/var/run/postgresql`) ve opsiyonel `backup` profili
   (`prodrigestivill/postgres-backup-local:17`, paylaşılan socket üzerinden).
3. **Split layout — kendi PostgreSQL'in** — `docker-compose.split.yml`:
   `ragmux` (`-app` imajı, distroless, `Dockerfile`) + `postgres`
   (`pgvector/pgvector:pg17`) + `backup`. `.env`'de zorunlu (hepsi `:?` ile):
   `SECRET_KEY`, `POSTGRES_PASSWORD`, `RAGMUX_DB_PASSWORD`. Uygulama, ilk
   açılışta `docker/postgres-init/01-ragmux.sh`/`.sql` tarafından oluşturulan
   en az yetkili `ragmux_app` rolüyle bağlanır. Gateway konteyneri stateless;
   state = `pgdata` volume + `SECRET_KEY`.
4. **ParadeDB (BM25 lexical retrieval)** — `docker-compose.paradedb.yml`,
   split ile aynı, yalnız `postgres` imajı digest-pinned `paradedb/paradedb:latest-pg17`.
   Store'ların `search_backend` değeri `pg_search` yapılabilir hâle gelir.
   **Varsayılan all-in-one imajdan geçiş reprocess istemez** — bunu açıkça yaz.
   Ayrıntı için RAG Cookbook linki.
5. **Several replicas** — `docker compose -f docker-compose.split.yml -f docker-compose.scale.yml up -d --scale ragmux=3`.
   Overlay ne yapıyor: `container_name` ve sabit portu `!override` ile düşürür,
   `proxy` servisi ekler (`nginx:1.27-alpine`, config
   `docker/nginx/ragmux.conf`, round-robin), `TRUST_PROXY_HEADERS=true`,
   `stop_grace_period: 90s`, ingestion lease değişkenleri (`INGEST_LEASE`,
   `INGEST_POLL_INTERVAL`, `INGEST_MAX_ATTEMPTS`, `MAX_PENDING_DOCUMENTS`).
   **All-in-one imaj yatay ölçeklenemez** (PostgreSQL'i içinde) — bir cümleyle
   söyle. Bütün replikalarda **aynı** `SECRET_KEY` zorunlu; yoksa açılış hatası
   (Troubleshooting'e link).

Ek bölümler:

- **Which image** — üç varyant (`:latest`, `:latest-app`, `:latest-paradedb`)
  ve hangisi ne zaman. `latest` denemek için; production'da sürüm ya da manifest
  digest pinlenir.
- **Verify the image** — `cosign verify` komutu (v0.3.1'den itibaren imzalı),
  `--certificate-identity-regexp` ve `--certificate-oidc-issuer` ile.
  Ayrıntı `ragmux.com/docs/security/`.
- **Behind a reverse proxy** — TLS sonlandırma, `TRUST_PROXY_HEADERS`,
  `SECURE_COOKIES`, `TRUSTED_PROXY_CIDRS`; streaming için buffering kapatma
  notu. Değişken açıklamaları Configuration'a linklenir.
- **Kubernetes notes** — kısa: readiness probe `/readyz`, liveness `/healthz`,
  rolling update davranışı (aşağıda), `SECRET_KEY` bir Secret'tan, all-in-one
  imaj burada kullanılmaz.
- **Observability overlay** — `docker-compose.otel.yml` overlay'i
  (`otel/opentelemetry-collector-contrib`, config `otel-collector.yaml`);
  ayrıntı Operations Runbook'ta.

### `Operations-Runbook.md`

1. **Back up.** `scripts/backup.sh` — `pg_dump` custom format, layout'u kendi
   saptar (direct / split / aio); env: `BACKUP_DIR`, `KEEP_DAYS` (14),
   `LAYOUT`, `INCLUDE_SECRET_KEY`, `SECRET_KEY_DIR`. `make backup` kısayolu.
   Zamanlanmış alternatif: Compose `backup` profili. **`SECRET_KEY` yedeğin
   parçası değilse yedek işe yaramaz** — sağlayıcı kimlik bilgileri onunla
   şifreli; bunu vurgula.
2. **Restore.** `scripts/restore.sh --yes` (onaysız çalışmaz): dump'ı doğrular,
   gateway'i durdurur, `pg_restore --clean --if-exists --no-owner --no-privileges`,
   sahipliği `APP_ROLE`'a (`ragmux_app`) devreder, yeniden başlatır,
   `/healthz`'i yoklar ve `/admin/api/system`'den `migrations_version` basar.
   `make restore FILE=… YES=1`; `YES=1` olmadan yalnız plan.
3. **Disaster-recovery drill.** Üç ayda bir koşulacak kısa adım listesi (`docs/backup-restore.md:391` çeyreklik diyor); ayrıntı
   `ragmux.com/docs/backup-restore/`.
4. **Rotate `SECRET_KEY`.** Gateway durdurulur, `ragmux rotate-key --new <64-hex>`
   **mevcut** `SECRET_KEY` ortamdayken koşturulur (saklı sağlayıcı kimlik
   bilgilerini yeniden şifreler), sonra yeni anahtarla başlatılır. Çok replikalı
   kurulumda hepsi aynı anda yeni anahtara geçer.
5. **Upgrade.** Yeni imaj sürümü, migration'lar açılışta advisory lock altında
   uygulanır. Rolling update sırasında `/readyz` davranışı (**ADR-001**):
   şeması binary'nin beklediğinden **geri** olan replika `503 migrating` döner;
   **ileri** olan `200` döner ve gövdesinde `degraded` girdisi taşır — filo
   deploy ortasında boşalmasın diye. Alarm `degraded`'in *görünmesine* değil,
   *kalıcı olmasına* kurulur. Bu, migration'ların ekleyici ve daraltmayan
   kalmasına bağlıdır; kolon düşüren/daraltan ya da mevcut bir kolona unique
   index ekleyen bir migration aşamalı rollout ister (`docs/scaling.md`).
6. **Retention.** `LOG_RETENTION_DAYS` (90), `AUDIT_RETENTION_DAYS` (365); saatlik
   bakım işi, çok replikalı kurulumda lider seçimiyle tek replikada koşar.
   `0` = sonsuza kadar sakla.
7. **Metrics and traces.** `METRICS_ENABLED=true` **`METRICS_TOKEN` olmadan
   açılışta reddedilir** (proje başına harcamayı kimliksiz yayınlar). Ayrı port
   isteniyorsa `METRICS_LISTEN`; loopback host token istemez. Trace'ler OTLP
   üzerinden; `docker-compose.otel.yml` overlay'i Collector'ı getirir.
8. **What to alert on.** Üç madde ve nedeni: `ragmux_metrics_series_dropped_total`
   herhangi bir artış (tavan dolduğunda tahliye yok, yeni seri kalıcı kaybolur —
   **ADR-002**); bütçe tükenmesi yaklaşımı; `/readyz` `degraded` girdisinin
   deploy penceresi dışında kalıcılaşması. Metrik listesi
   `ragmux.com/docs/observability/`'ye linklenir, tekrarlanmaz.
9. **PgBouncer.** Retention lider kilidi session mode ister; transaction mode'da
   kilit tutmaz (retention idempotent olduğu için bozulmaz, gereksiz tekrar
   koşar). pgx v5 statement cache'i transaction mode'da
   `default_query_exec_mode=exec&statement_cache_capacity=0` ister.

## Dokunulacak dosyalar

- `../wiki/Deployment-Recipes.md` (yeni)
- `../wiki/Operations-Runbook.md` (yeni)

## Dokunulmayacak

- `../wiki/` altındaki başka hiçbir dosya — `_Sidebar.md` dâhil (Faz 01 yazdı,
  sayfa adları zaten orada).
- `Ragmux/` deposundaki hiçbir dosya.
- `git commit` / `git push`.
- Env değişkeni tablosu: `docs/configuration.md`'yi kopyalama. Bir reçetenin
  ihtiyaç duyduğu değişken adı geçer, tam tablo geçmez.

## Kabul ölçütü

- İki dosya var, ikisi de İngilizce.
- Beş reçetenin hepsi gerçek dosya adlarını kullanıyor: `docker-compose.yml`,
  `docker-compose.split.yml`, `docker-compose.paradedb.yml`,
  `docker-compose.scale.yml`, `docker-compose.otel.yml`. Her biri depoda var
  (`ls` ile doğrula).
- Split ve ParadeDB reçetelerinde üç zorunlu `.env` değişkeni de yazılı
  (`SECRET_KEY`, `POSTGRES_PASSWORD`, `RAGMUX_DB_PASSWORD`) — Compose dosyaları
  bunları `:?` ile zorunlu kılıyor, eksik olan kurulumu ilk komutta düşürür.
- Operations Runbook'taki her komut depoda karşılığı olan bir şeye işaret
  ediyor: `scripts/backup.sh`, `scripts/restore.sh`, `make backup`,
  `make restore`, `ragmux rotate-key`. `Makefile` ve `scripts/` okunarak
  doğrulanır.
- `/readyz` bölümü ADR-001'i doğru anlatıyor: geri kalan 503, ileri giden 200 +
  `degraded`.
- Hiçbir sayfa `docs/configuration.md`'nin env tablosunu, `docs/observability.md`'nin
  metrik tablosunu ya da `docs/api.md`'nin endpoint listesini taşımıyor.
- Her iki sayfa da `_Sidebar.md`'deki adıyla birebir eşleşen dosya adında.

## Notlar

Kaynaklar: `Ragmux/docker-compose*.yml`, `Ragmux/Dockerfile*`,
`Ragmux/scripts/`, `Ragmux/Makefile`, `Ragmux/docker/`,
`Ragmux/docs/configuration.md` (deployment layouts, published images, reverse
proxy), `Ragmux/docs/backup-restore.md`, `Ragmux/docs/scaling.md`,
`Ragmux/docs/observability.md`, `Ragmux/SECURITY.md` (cosign), `.ssot/ADR.md`
(ADR-001, ADR-002).

Compose dosyalarının içindeki varsayılanları olduğu gibi aktar — `.env.example`
12 KB ve her değişkeni açıklıyor; wiki onu tekrar etmez, reçetenin çalışması
için gereken minimumu yazar.
