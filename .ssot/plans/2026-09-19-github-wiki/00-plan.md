# GitHub wiki — genel plan

- **Durum:** 01–06 tamam; commit atıldı, `git push` kullanıcı onayı bekliyor
- **Tarih:** 2026-09-19
- **Hedef depo:** `../wiki` (`Ragmux/ragmux.wiki.git`, branch `master`)

## Neden

`wiki/` bugün tek satırlık bir placeholder `Home.md` taşıyor. Depoda ise
`README.md`, `docs/` altında 8 sayfa (~3.900 satır), `SECURITY.md` ve
`CHANGELOG.md` var; aynı içerik `ragmux.com/scripts/sync-docs.mjs` ile siteye
aktarılıp `https://ragmux.com/docs/<slug>/` adreslerinde yayınlanıyor.

Referans dokümantasyon zaten iki yerde yayınlı. Wiki üçüncü kopya olursa ilk
doküman değişikliğinde yalan söylemeye başlar. Kullanıcı kararı (2026-09-19):
wiki **görev odaklı** olacak — reçeteler, turlar, sorun giderme, mimari genel
bakış — referans içerik `docs/`'ta kalıp linklenecek.

## Faz tablosu

| Faz | Başlık | Sahip | Durum | Bağımlılık |
|---|---|---|---|---|
| 01 | İskelet, Home, Sidebar, Footer, Quickstart | frontend-engineer | **tamam** | — |
| 02 | Deployment Recipes + Operations Runbook | backend-engineer | **tamam** | 01 |
| 03 | Provider Setup + Client Integrations | backend-engineer | **tamam** | 01 |
| 04 | RAG Cookbook + Architecture Overview | backend-engineer | **tamam** | 01 |
| 05 | Troubleshooting + FAQ + Security Hardening | backend-engineer | **tamam** | 02, 03, 04 |
| 06 | Contributing + link geçişi + commit | backend-engineer | **tamam** | 01–05 |

02, 03, 04 birbirinden bağımsız; paralel verilir. Hiçbiri aynı wiki dosyasına
dokunmaz.

## Ortak kurallar — her faz bunlara uyar

**Dil.** Wiki sayfalarının tamamı **İngilizce** yazılır. Depodaki diğer public
yüzeyler (README, docs, CHANGELOG) İngilizce; wiki de public bir yüzey.

**Kapsam kuralı — wiki ne yazar, ne yazmaz.**

- *Yazılır:* kullanıcının yapmak istediği iş ("connect Ollama", "restore a
  backup", "why am I getting 429"), adım adım, çalışır komutlarla.
- *Yazılmaz:* env değişkeni tabloları, endpoint referansı, metrik listesi, rol
  matrisi, provider capability matrisi. Bunlar `docs/`'ta yaşar.
- *Kopyalama testi:* bir düzyazı cümlesi `docs/` içindeki bir cümlenin yeniden
  ifadesiyse silinir, yerine link konur. Kod bloğu tekrarı serbesttir (reçete
  çalıştırılabilir olmalı), düzyazı tekrarı değil.

**Link hedefleri.**

- Dokümantasyon: `https://ragmux.com/docs/<slug>/`. Geçerli slug'lar:
  `getting-started`, `providers`, `rag`, `users-and-limits`, `configuration`,
  `api`, `changelog`, `backup-restore`, `security`, `observability`, `scaling`.
  Başlık derinliğine inmek gerekirse `#<heading-slug>` eklenir.
- Depo dosyası (Compose dosyaları, `scripts/`, `Makefile`, migration'lar):
  `https://github.com/Ragmux/ragmux/blob/main/<path>`.
- Wiki içi: `[Text](Page-Name)` — dosya adı, `.md` uzantısız, tireli.

**GitHub wiki biçimi.** Bütün sayfalar `wiki/` kökünde düz dosya. Dosya adı =
sayfa başlığı (tire boşluğa döner). `_Sidebar.md` ve `_Footer.md` özel adlardır.
Tablo ve kod bloğu dışında HTML kullanılmaz.

**Sürüm.** Wiki `v0.4.1` davranışını anlatır ve sürümlenmez — `main`'i anlatır.
Sürüme bağlı bir cümle varsa sürümü açıkça söyler.

**Dokunulmayacak — bütün fazlar için.**

- `Ragmux/` deposundaki hiçbir dosya. `docs/`, `README.md`, `SECURITY.md`,
  `CHANGELOG.md`, kod — hiçbiri. Tek istisna: bu plan dizini.
- `ragmux.com/` sitesi.
- `.ssot/PRD.md` ve `.ssot/ADR.md` — kullanıcı kararı olmadan yazılmaz.
- `git push` — commit Faz 06'da atılır, push kullanıcı onayına bırakılır.

**Doğrulama.** Hiçbir faz canlı Ragmux kurulumu koşturmaz. Komutlar depodaki
gerçek dosya adlarına, env değişkenlerine ve endpoint'lere karşı okunarak
doğrulanır.

**Review.** Her faz bitince diff `reviewer`'a verilir; bulgular kullanıcıya
özetlenir, onay çıkmadan sonraki faz kapanmış sayılmaz.
