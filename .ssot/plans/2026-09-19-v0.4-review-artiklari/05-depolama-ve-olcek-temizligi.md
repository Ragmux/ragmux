# Faz 05 — Depolama ve ölçek temizliği

- **Durum:** beklemede
- **Sahip:** backend-engineer
- **Bağımlılık:** yok
- **İlgili karar:** PRD davranış kuralı 8 (retrieval hatası isteği düşürmez)

## Amaç

Çalışma anında bozulan ya da yanlış bilgi veren depolama yollarını
kapatmak.

## Kapsam

1. **`bm25Ready` hiç geçersiz kılınmıyor.**
   `internal/store/search_pgsearch.go:79`: ilk başarıdan sonra `true` olup
   bir daha bakılmıyor. `docs/configuration.md:38` operatöre tokenizer'ı
   değiştirmek için `DROP INDEX IF EXISTS idx_chunks_bm25;` demesini söylüyor
   ve yeniden başlatmaktan söz etmiyor. Index düşürülürse `Prepare` erken
   dönüyor, fallback tetiklenmiyor, `HybridQuery` `@@@` gönderiyor ve
   pg_search hata veriyor; `SearchWithBackend` bunu fallback'e çevirmiyor
   (`vector.go:306-309`) → pg_search'lü her store'da hybrid arama, **her
   replika yeniden başlatılana kadar 502**. PRD kuralı 8'i kırıyor.
   Ayrıca `search_pgsearch.go:112` her başlangıçta *yapılandırılmış*
   tokenizer'ı "bm25 index ready" diye logluyor; mevcut index farklı bir
   analyser'la kurulmuşsa log doğrudan yanlış.
   Yapılacak: `Prepare`'i `pg_class` kontrolüne bağla ya da sorgu hatasında
   `bm25Ready`'yi sıfırlayıp bir kez daha dene; DDL'den önce mevcut index'in
   `reloptions`'ını okuyup tokenizer drift'ini logla.
2. **`hashtext` nitelenmemiş** (`internal/store/caps.go:112`): belgelenmemiş
   bir iç fonksiyon ve çözümlemesi `search_path`'e tabi. En azından
   `pg_catalog.hashtext(...)`; tercihen `hashtextextended(..., 0)::bigint`
   (PG11+, tam 64 bit).
3. **`Ingester.Process` ve `ClaimDocumentByID` üretimde ölü kod**
   (`internal/rag/ingest.go:375`, `internal/store/documents.go:174`):
   tek çağıranları testler; admin'in üç yolu da `Enqueue` kullanıyor.
   `Process` `ClearDocumentClaim` çağırmıyor ve hata durumunda
   `SetDocumentStatus` yazmıyor, yani biri onu bağlarsa her hata satırı
   `processing` + canlı lease'te asılı kalır. `ClaimDocumentByID`'nin
   `claimed_by = $1` dalı aynı replikada iki eşzamanlı çağrının ikisinin de
   claim almasına izin veriyor. Ya kaldır ya üretimde kullan; testler
   dispatcher yolunu sürsün.
4. **PgBouncer transaction mode'da BM25 DDL'i için kayıtlı güvence yok**
   (`search_pgsearch.go:83-87`): kod yorumu "advisory lock bunu güvenli
   kılıyor" diyor; `docs/scaling.md:77-84` ise transaction mode'da session
   lock'un tutulamayacağını yalnız **retention pass** için gerekçelendiriyor
   (her adımı idempotent). BM25 DDL'i idempotent değil. Dokümana ekle ya da
   bu DDL'i tek bir `pgx.Conn` üzerinde `pg_advisory_xact_lock` ile koştur
   (`ensureVecTable`'ın `vector.go:88-95`'te zaten yaptığı gibi — bu iki kod
   yolu bugün farklı kalıplar kullanıyor).
5. **`api_key_projects` CHECK ile korunmuyor.** `0010_api_keys.sql:27`
   yalnız `default_project_id`'yi kısıtlıyor; bir management key'in grant
   satırı olmasını hiçbir şey engellemiyor, tek koruma uygulama katmanında.
   Bugün istismar edilebilir bir yolu yok. Ya trigger/FK ekle ya da mevcut
   durumu yeterli say — karar faz raporunda gerekçelensin.

## Dokunulacak dosyalar

- `internal/store/search_pgsearch.go`, `internal/store/vector.go`
- `internal/store/caps.go`, `internal/store/documents.go`
- `internal/rag/ingest.go`
- `docs/scaling.md`, `docs/configuration.md`
- ilgili testler; 5. madde için gerekiyorsa yeni migration (`0014_`)

## Dokunulmayacak

- `FOR UPDATE SKIP LOCKED` claim sorgusu ve lease semantiği — review
  doğruladı, çalışıyor.
- Faz 0'daki sahiplik filtreleri (`SetDocumentProgress`,
  `ReplaceDocumentChunks`) — yeni eklendi ve testli.
- `ReleaseDocuments`'ın attempt iadesi davranışı.
- Migration `0010`–`0013` içeriği; gerekirse **yeni** bir numara alınır.

## Kabul ölçütü

- Index düşürülmüş bir pg_search store'unda hybrid arama 502 yerine
  pgvector'a düşüyor (ParadeDB yoksa bu senaryo `Prepare` hatası enjekte
  edilerek test edilir).
- `caps.go` sorgusu `pg_catalog` ile nitelenmiş ve mevcut test geçiyor.
- 3. madde: ölü kod kaldırıldıysa derleme ve testler yeşil; bırakıldıysa
  faz raporu neden bırakıldığını yazıyor.
- `make test` (race), `go vet`, `golangci-lint run ./...` temiz.

## Notlar

Migration numarası alınacaksa `0014` boştur; tekillik/ardışıklık testi
(`internal/store/migrations_test.go`) hatayı yakalar.
