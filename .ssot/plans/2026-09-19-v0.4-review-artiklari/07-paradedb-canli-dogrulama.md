# Faz 07 — ParadeDB yolunun canlı doğrulaması

- **Durum:** beklemede
- **Sahip:** backend-engineer
- **Bağımlılık:** yok
- **İlgili karar:** yok

## Amaç

pg_search yolunun gerçekten çalıştığını, kodun kendi çıktısına değil canlı
bir ParadeDB'ye karşı göstermek.

## Kapsam

Bu bir doğrulama fazı; kod değişikliği ancak bir kusur çıkarsa yapılır.

**Neden gerekli — iki rapor çelişiyor:**

- Faz 4'ü uygulayan ajan `paradedb/paradedb:pg17` imajını `--network none`
  bir konteynerde açtığını, AIO imajını kurup çalıştırdığını, gateway'in
  `pg_search=true` loglandığını ve tam hibrit sorgunun **gerçek BM25
  skorları döndürdüğünü** bildirdi.
- Review ise `search_pgsearch_test.go`'nun golden string karşılaştırması
  yaptığını ve golden dosyaların **kodun kendi çıktısından** üretildiğini
  (dosyanın kendi yorumu bunu kabul ediyor, satır 11-15), dolayısıyla
  `paradedb.boolean(must => ARRAY[...])` + `paradedb.score(c.id)` +
  `ROW_NUMBER() OVER (...)` birleşiminin ParadeDB'de parse edildiğinin
  **doğrulanmadığını** söylüyor.

İkisi birbirini dışlamıyor — ajan canlı koşmuş olabilir, testler yine de
döngüsel olabilir — ama koşum kimse tarafından ikinci kez görülmedi ve
yayın öncesi tek bir hibrit arama bunu kesinleştirir.

Yapılacak:

1. `make dev-db-paradedb` ile ParadeDB'yi ayağa kaldır (host portu 5434).
2. `make test-paradedb` koştur; `testdb.RequirePgSearch` ile kapılı testler
   artık atlanmamalı.
3. Elle bir doğrulama: bir RAG store'u `search_backend: "pg_search"`
   yaparak birkaç belge yükle ve `POST /admin/api/rag-stores/{id}/search`
   çağır. Yanıtta `backend: "pg_search"` ve hit'lerde sıfırdan farklı
   `lex_score` görülmeli.
4. `Dockerfile.aio.paradedb` imajını kur, açılışta `pg_search=true`
   loglandığını ve `/readyz`'in 200 döndüğünü gör.
5. Tokenizer drift'i: `PG_SEARCH_TOKENIZER=en_stem` ile index'i yeniden
   kurup aramanın çalıştığını doğrula.
6. Yedek tuzağı: ParadeDB veritabanından `pg_dump` alıp pgvector-only bir
   sunucuya restore etmeyi dene; `docs/backup-restore.md`'deki iki reçetenin
   (`DROP INDEX` ya da `pg_restore -L` ile TOC filtresi) gerçekten
   çalıştığını gör.

## Dokunulacak dosyalar

Kusur çıkmadıkça **hiçbiri**. Çıkarsa:
`internal/store/search_pgsearch.go`, ilgili testler, `docs/rag.md`,
`docs/backup-restore.md`, `Dockerfile.aio.paradedb`.

## Dokunulmayacak

- pgvector yolu ve onun CI job'ı — fallback'in extension'sız bir sunucuda
  test edilmeye devam etmesi bu fazın ön koşulu.
- Golden SQL dosyalarının kendisi; canlı koşum onları doğrular ya da
  çürütür, tersi değil.
- `docker-compose.yml` (varsayılan AIO layout'u).

## Kabul ölçütü

- `make test-paradedb` yeşil ve çıktısında pg_search kapılı testlerin
  **atlanmadığı** görülüyor.
- Elle arama çağrısı `backend: "pg_search"` ve sıfırdan farklı `lex_score`
  döndürüyor; çıktı faz raporuna yapıştırılıyor.
- Dump/restore reçetelerinden en az biri uçtan uca çalıştığı gösteriliyor.
- Bir kusur bulunursa: düzeltilip regresyon testi ekleniyor; bulunmazsa
  rapor "doğrulandı" diyor ve `search_pgsearch_test.go`'daki döngüsellik
  uyarısına canlı koşumun tarihi düşülüyor.

## Notlar

Bu faz ağ ve imaj indirmesi gerektiriyor; ParadeDB imajı digest ile pinli.
İndirme mümkün değilse faz "doğrulanamadı" olarak kapanır ve yayın notuna
bu sınır yazılır — tahminle "çalışıyor" denmez.
