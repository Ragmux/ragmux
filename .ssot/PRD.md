# Ragmux — PRD

> Bu dosya projenin **otoritesidir**. Kodun, yorumun ya da eski bir konuşmanın
> aksini söylemesi burayı değiştirmez. Değişiklik yalnız kullanıcı kararıyla
> yazılır.
>
> Aşağıda `> [ ] doldurulacak` ile işaretli bölümler niyet bildirir ve
> kullanıcıya aittir. İşaretsiz bölümler depodan okunarak yazılmıştır ve
> kaynakları verilmiştir; yanlışsa düzeltilmeli.

## Problem

> [ ] doldurulacak

## Kullanıcı ve kullanım senaryoları

> [ ] doldurulacak

## Kapsam

Depoda hâlihazırda karşılanan yüzeyler (`README.md`, `docs/`):

- **OpenAI uyumlu geçit.** `/v1/chat/completions` (JSON + SSE) ve `/v1/models`;
  resmî SDK'lar yalnız `base_url` ve `api_key` değiştirerek çalışır.
- **Sağlayıcılar.** `openai`, `anthropic`, `gemini`, `deepseek`, `ollama`
  (yerel `/api/chat`), `custom_openai`; ayrıca yalnız rerank yapan
  `cohere_rerank` ve `voyage_rerank`. İstek, akış ve tool call çevirisi
  gateway'de yapılır (`internal/provider`).
- **Retrieval.** PDF / DOCX / HTML / TXT / Markdown yükleme, bölüm farkındalıklı
  chunk'lama, pgvector ile vektör araması ve sözcüksel yarısı Postgres tam metin
  ya da ParadeDB BM25 olan hibrit arama; rerank kendi modelinizle veya Cohere /
  Voyage ile (`internal/rag`, `internal/store/search*.go`).
- **Projeler ve API key'ler.** Proje başına bir `sk-proj-…` key'i; ayrıca
  kullanıcıya ait `sk-user-…` (gateway) ve `sk-mgmt-…` (yönetim) key'leri,
  scope / süre / iptal ve opsiyonel alt limitlerle (`internal/store/apikeys.go`).
- **Kullanıcılar ve roller.** `admin` > `editor` > `viewer`, proje üyeliği,
  giriş hız sınırı ve kilitleme, her yönetim eyleminin denetim kaydı.
- **Limitler ve bütçeler.** Proje başına dakikalık istek/token ve günlük/aylık
  token bütçeleri; sayaçlar veritabanında, replikalar arası paylaşılır.
- **Maliyet.** Prompt caching passthrough, cache farkındalıklı token muhasebesi
  ve düzenlenebilir bir fiyat tablosuyla istek başına **tahmini** maliyet.
- **Gözlemlenebilirlik.** İstek kayıtları, özetler, günlük seriler, kimlik
  isteyen Prometheus `/metrics`, OTLP üzerinden OpenTelemetry trace'leri,
  `/healthz` ve `/readyz`.
- **Dağıtım.** Tek binary; gömülü PostgreSQL'li tek konteyner, ayrık layout,
  ParadeDB varyantı ve çok replikalı overlay (`docker-compose*.yml`).
- **Durum.** Kullanıcılar, bağlantılar, projeler, belgeler, chunk'lar, vektörler
  ve metrikler tek bir PostgreSQL + pgvector veritabanında. Redis yok, ayrı
  vektör veritabanı yok.
- **Lisans.** AGPL-3.0-or-later (`LICENSE`).

## Kapsam dışı

Depoda **açıkça** kapsam dışı bırakılmış olanlar:

- 0.1 (PostgreSQL öncesi) veritabanları için içe aktarma aracı — `README.md`
  `## Roadmap`.
- Taranmış PDF'ler için OCR — aynı yer.
- SSO / OIDC girişi — aynı yer.
- **Para cinsinden bütçe uygulaması.** Maliyet 0.4'te bilgilendirmedir; bütçeler
  token üzerinden işler (`CHANGELOG.md`, `docs/users-and-limits.md`).
- **Gateway'in kendi system prompt'u ve RAG bloğu için cache breakpoint'i.**
  Kanca noktası `internal/gateway/gateway.go` içinde yorumla işaretli, bilinçli
  ertelendi.
- **Gemini explicit caching** (`cachedContents`) — durumlu bir kaynak gerektirdiği
  için stateless passthrough'a oturmuyor (`docs/providers.md`).
- **Tek konteyner (all-in-one) layout'unda yatay ölçekleme** — PostgreSQL'i
  içinde barındırdığı için tasarımı gereği tek instance (`docs/scaling.md`).

## Davranış kuralları

Kodda ve dokümanda yazılı, teste bağlanmış değişmezler:

1. **Bir API key hiçbir zaman sahibinin rolünü aşamaz.** Etkin yetki =
   scope'lar ∩ sahibin *canlı* rolü; kullanıcı düşürülür ya da pasifleştirilirse
   elindeki her key anında daralır.
2. **Yönetim key'i yalnız `Authorization` header'ından kabul edilir.** Session
   cookie'sine yapıştırılmış bir key, aranmadan reddedilir; bearer istekler için
   var olan CSRF muafiyetini ayakta tutan şey budur.
3. **Hesabı devralmaya yarayan eylemler interaktif session ister** — şifre
   değiştirme, başkasının şifresini sıfırlama, yönetim key'i basma ve düzenleme.
4. **Bilinmeyen bir key hiçbir şey sızdırmaz.** 401 gövdesi değişmez; yalnız
   bilinen-ama-kullanılamaz key'ler `code` alır.
5. **Kullanım geçmişi sessizce kaybolmaz.** Kendisine istek kaydı atfedilmiş bir
   key ya da kullanıcı silinemez; iptal ya da pasifleştirme kullanılır.
6. **İstemcinin seçtiği URL'ler operatör muafiyetlerini miras almaz.**
   `ALLOW_PRIVATE_UPSTREAMS` ve `PRIVATE_UPSTREAM_ALLOWLIST` bir `base_url` için
   geçerlidir, bir `image_url` için asla.
7. **Maliyet bir tahmindir, fatura değildir.** Her yüzeyde böyle etiketlenir.
8. **Retrieval hatası isteği düşürmez.** Rerank ya da arama backend'i
   kullanılamıyorsa birleştirilmiş sıraya / pgvector'a düşülür ve loglanır.
9. **Yanlış `SECRET_KEY` açılışta yakalanır**, istek başına şifre çözme hatası
   olarak değil; ve mevcut credential'ları açamayan bir anahtar veritabanına
   kaydedilmez.
10. **Metrik ve span etiketleri kurulumun büyüklüğüyle sınırlıdır, trafikle
    değil.** Etiket asla header, hata mesajı, dosya adı, query string, user-agent
    ya da IP'den alınmaz; span'ler mesaj içeriği, sorgu, passage, system prompt
    ya da kimlik bilgisi taşımaz.

> [ ] gözden geçirilecek — bu liste depodan çıkarıldı; eksik ya da fazlası varsa
> kullanıcı düzeltmeli.

## Başarı ölçütü

> [ ] doldurulacak

## Açık sorular

> [ ] doldurulacak
