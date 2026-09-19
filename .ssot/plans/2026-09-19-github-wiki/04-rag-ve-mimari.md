# Faz 04 — RAG Cookbook + Architecture Overview

- **Durum:** tamam (review CHANGES REQUESTED → 12 düzeltme uygulandı)
- **Sahip:** backend-engineer
- **Bağımlılık:** Faz 01
- **İlgili karar:** ADR-001…005 (Architecture Overview bunları özetler)

Bu brief kendi kendine yeter. Ortak kurallar için **önce** aynı dizindeki
`00-plan.md`'yi oku — bağlayıcıdır.

## Amaç

İki soruyu cevaplamak: "retrieval'ı nasıl iyileştiririm" ve "bu şey içeride
nasıl çalışıyor".

## Kapsam

### `RAG-Cookbook.md`

Ayar listesi değil, **ayar yöntemi**. Bir ölç-değiştir-ölç döngüsü etrafında
kurulur.

1. **Get a baseline.** Store'u kur, birkaç belge yükle, `POST /admin/api/rag-stores/{id}/search`
   ile gerçek sorularını çalıştır (dashboard'daki retrieval tester aynı şeyi
   yapar ve aşama başına gecikme gösterir). Neyin kötü olduğunu adlandır:
   yanlış passage mı geliyor, doğru passage geliyor ama sırada altta mı, hiç
   bir şey mi gelmiyor.
2. **Documents in.** Desteklenen formatlar (PDF / DOCX / HTML / TXT / Markdown)
   ve bölüm farkındalıklı chunk'lama. Taranmış PDF'de metin katmanı yoksa
   reddedilir — OCR yok. PDF ayrıştırma ayrı bir `ragmux pdf-extract` alt
   sürecinde izole koşar; sınırlar (60 s, 2000 sayfa) aşılırsa belge `failed`
   olur ve nedeni yazılır.
3. **Chunking.** `chunk_size` (varsayılan 1000, 200-20000), `chunk_overlap`
   (200, `chunk_size`'dan küçük), `contextual_chunks` (açık). Ne zaman
   büyütülür/küçültülür — uzun teknik doküman mı, kısa SSS mi. **Bu üçünden
   biri değişirse mevcut belgeler yeniden işlenmeli** (`reprocess_recommended`
   bayrağı bunu söyler); arama backend'i değişirse gerekmez.
4. **Search mode.** `vector` vs `hybrid`. Hibrit, vektör sırasıyla sözcüksel
   sırayı reciprocal rank fusion ile birleştirir (`1/(60+rank)`), aday havuzu
   `3 × top_k`. Terim eşleşmesi önemliyse (ürün kodu, hata adı, kişi adı)
   hibrit; anlamsal yakınlık yetiyorsa vektör.
5. **Search backend.** `pgvector` (Postgres tam metin sıralaması) vs
   `pg_search` (ParadeDB BM25). Geçiş **reprocess istemez**. BM25 indeksi
   tembel kurulur: ilk hibrit sorgu indeksin yokluğunu fark eder, o sorguyu
   pgvector'dan cevaplar ve indeksi kurar — `chunks` taranırken ingestion
   yazımlarını bloklar, o yüzden trafik düşükken yap. `fts_config` ayarı
   `pg_search` altında **sessizce yok sayılır**. Sunucuda eklenti yoksa store
   `pgvector`'a düşer ve bu bir hata değil, `degraded` bir durumdur.
   Analizör/stemmer seçimi `PG_SEARCH_TOKENIZER` ile instance geneli.
6. **Reranking.** `rerank` açıldığında `rerank_candidates` (15, 1-100) aday
   yeniden sıralanır. Üç backend: `llm` (kendi modelinle, tam bir chat
   completion — pahalı ama ek servis yok), `cohere`, `voyage` (tek skorlama
   çağrısı, hızlı). `RERANK_TIMEOUT` boşsa `llm` 10 s, API'ler 5 s.
   **Zaman aşımı hata değildir** — birleştirilmiş sıraya düşülür ve loglanır.
   Ne zaman değer: doğru passage geliyor ama ilk sıralarda değilse.
7. **Distance cut-off.** `max_distance` (0 = kapalı, 0-2 kosinüs). Alakasız
   passage enjekte edilmesini kesmenin yolu. Nasıl ayarlanır: search
   endpoint'inin döndürdüğü mesafelere bak, iyi ve kötü sonuçların arasına
   koy. Çok sıkı bir eşik "hiç bağlam gelmiyor"a döner.
8. **top_k and the context budget.** `top_k` (5, ≤50) ile enjekte edilen
   bağlamın büyüklüğü arasındaki ilişki; her passage prompt token'ı demek ve
   bütçeye sayılır.
9. **Quotas and capacity.** `max_documents` / `max_bytes` (0 = sınırsız),
   instance tavanları `MAX_DOCUMENTS_PER_STORE` / `MAX_BYTES_PER_STORE_MB` ile
   birlikte küçük olan geçerli. Aşım `422 store_quota`, hiçbir şey yazılmadan.
   Reprocess kotaya sayılmaz.
10. **Checklist** — kapanışta 6-8 maddelik "retrieval kötüyse sırayla şuna bak"
    listesi.

Store ayarlarının tam tablosu **linklenir** (`ragmux.com/docs/rag/`),
kopyalanmaz.

### `Architecture-Overview.md`

Kod okumadan "neden böyle" sorusunu cevaplar.

1. **The shape.** Tek binary, tek PostgreSQL + pgvector. Redis yok, ayrı vektör
   veritabanı yok, kuyruk yok — kuyruk `documents` tablosunun kendisi. Bunun
   bedeli ve kazancı bir paragraf.
2. **Life of a request** — `POST /v1/chat/completions` adım adım:
   auth (bearer key → principal) → project seçimi (`X-Ragmux-Project` başlığı,
   yoksa key'in varsayılan projesi, yoksa tek grant) → connection ve provider
   adaptörü → **limit kontrolü** → sistem prompt'u ve RAG enjeksiyonu →
   sağlayıcı çağrısı (JSON ya da SSE) → yanıt normalizasyonu → token muhasebesi,
   fiyatlama, istek kaydı, metrik ve span. Limit kontrolünün RAG'den **önce**
   olduğunu ve nedenini (reddedilecek bir istek embedding parası harcamasın)
   söyle.
3. **Package map** — `internal/*` tablosu: `gateway`, `provider`, `rag`,
   `limits`, `store`, `admin`, `auth`, `metrics`, `tracing`, `obs`,
   `maintenance`, `netguard`, `bm25`. Her biri bir satır.
4. **State** — PostgreSQL'de ne var: kullanıcılar, bağlantılar (şifreli
   kimlik bilgileri), projeler, belgeler (bytea), chunk'lar, vektörler (HNSW
   indeksli, `chunk_embeddings_<dims>` tabloları boyuta göre tembel kurulur),
   istek kayıtları, kullanım sayaçları, audit. 15 migration, açılışta advisory
   lock altında uygulanır, ekleyici ve daraltmayan olmaları bir tasarım koşulu.
5. **Deployment topologies** — üç imaj varyantının topoloji farkı, tek şema
   (all-in-one tek instance; `-app` + dış PostgreSQL ölçeklenebilir).
6. **Design invariants** — `.ssot/PRD.md`'deki davranış kurallarının
   İngilizce, kısa hâli. En az şunlar: bir API key sahibinin rolünü aşamaz;
   yönetim key'i yalnız `Authorization` başlığından kabul edilir; hesabı
   devralan eylemler interaktif session ister; bilinmeyen key hiçbir şey
   sızdırmaz; kullanım geçmişi olan key/kullanıcı silinemez; istemcinin seçtiği
   URL'ler operatör muafiyetlerini miras almaz; maliyet tahmindir; retrieval
   hatası isteği düşürmez; yanlış `SECRET_KEY` açılışta yakalanır; metrik
   etiketleri kurulum büyüklüğüyle sınırlıdır, trafikle değil.
7. **Decisions on record** — beş ADR'nin her biri iki cümleyle ve neye yol
   açtığıyla: ADR-001 `/readyz` rolling update; ADR-002 metrik seri tavanı;
   ADR-003 `viewer` gateway key'i basabilir; ADR-004 fiyat tablosundan düşen
   satır; ADR-005 görsel indirme 429 ve kiracı payı. `.ssot/ADR.md`'ye GitHub
   linki (kayıtlar Türkçe — bunu bir parantezle söyle).
8. **What is deliberately not there** — OCR, SSO/OIDC, para cinsinden bütçe,
   0.1 içe aktarma aracı, Gemini explicit caching, all-in-one imajda yatay
   ölçekleme.

## Dokunulacak dosyalar

- `../wiki/RAG-Cookbook.md` (yeni)
- `../wiki/Architecture-Overview.md` (yeni)

## Dokunulmayacak

- `../wiki/` altındaki başka hiçbir dosya.
- `Ragmux/` deposundaki hiçbir dosya — `.ssot/ADR.md` ve `.ssot/PRD.md` dâhil,
  yalnız okunur.
- `git commit` / `git push`.
- Store ayarlarının tam tablosu ve migration listesi: linklenir, kopyalanmaz.

## Kabul ölçütü

- İki dosya var, ikisi de İngilizce.
- RAG Cookbook'taki her ayar adı `docs/rag.md` ile birebir aynı yazımda
  (`chunk_size`, `chunk_overlap`, `top_k`, `search_mode`, `search_backend`,
  `fts_config`, `rerank`, `rerank_backend`, `rerank_connection_id`,
  `rerank_candidates`, `max_distance`, `contextual_chunks`, `max_documents`,
  `max_bytes`) ve varsayılanları uyuşuyor.
- "Backend değişimi reprocess istemez, chunk ayarları ister" ayrımı açıkça
  yazılı.
- Architecture Overview'daki istek akışı, `internal/gateway/gateway.go`'daki
  gerçek sırayla uyuşuyor — özellikle limit kontrolünün RAG'den önce olması.
- Beş ADR'nin beşi de özetlenmiş ve hiçbiri yanlış anlatılmamış (`.ssot/ADR.md`
  okunarak doğrulanır).
- Davranış kuralları listesi `.ssot/PRD.md`'deki 10 maddeyi karşılıyor.
- Hiçbir sayfa `docs/rag.md`'nin ayar tablosunu tekrarlamıyor.

## Notlar

Kaynaklar: `Ragmux/docs/rag.md`, `Ragmux/internal/rag/` (`chunk.go`,
`ingest.go`, `retrieve.go`, `rerank.go`), `Ragmux/internal/gateway/gateway.go`,
`Ragmux/internal/store/migrations/`, `Ragmux/.ssot/PRD.md`,
`Ragmux/.ssot/ADR.md`, `Ragmux/README.md` (paket düzeni bölümü).

Architecture Overview bir kod turu değil. Dosya:satır referansı verme — paket
adı ve sorumluluk yeter. Okuyucu kodu okumak isterse repo linki var.
