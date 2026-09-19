# Faz 06 — Contributing + link geçişi + commit

- **Durum:** tamam (review APPROVED; 4 düzeltme sonrası commit `--amend`)
- **Sahip:** backend-engineer
- **Bağımlılık:** Faz 01–05 (hepsi review'dan geçmiş olmalı)
- **İlgili karar:** yok

Bu brief kendi kendine yeter. Ortak kurallar için **önce** aynı dizindeki
`00-plan.md`'yi oku — bağlayıcıdır.

## Amaç

Son sayfayı yazmak, wiki'yi bir bütün olarak tutarlı hâle getirmek ve tek bir
commit'e bağlamak.

## Kapsam

### `Contributing-and-Development.md`

`.github/CONTRIBUTING.md`'yi tekrarlamaz, ona link verir ve geliştiricinin
Ragmux'a özgü bilmesi gerekenleri anlatır:

1. **Prerequisites** — Go 1.27+, Docker (veritabanı için).
2. **The loop** — `make dev-db` (pgvector Postgres, `localhost:5433`,
   `docker-compose.dev.yml`), `make test`, `make run`. ParadeDB yolu için
   `make dev-db-paradedb` ve `make test-paradedb`.
3. **Tests** — `TEST_DATABASE_URL` okunur (Makefile varsayılanı
   `postgres://ragmux:ragmux@localhost:5433/ragmux_test?sslmode=disable`), her
   test kendi geçici şemasını kurar, o yüzden tek sunucuda paralel koşarlar;
   değişken yoksa testler atlanır.
4. **What CI runs** — `gofmt`, `go vet`, `golangci-lint run ./...` (v2, config
   `.golangci.yml`), `govulncheck`, testler ve imaj build kontrolü.
5. **Repository layout** — `README.md`'deki ağaç kısaltılarak; her satır bir
   paket ve sorumluluğu. Derinlik için Architecture Overview linki.
6. **Images** — `make docker-build`, `docker-build-app`,
   `docker-build-paradedb`; hangisi hangi Dockerfile'dan.
7. **Schema changes** — `internal/store/migrations/` altında gömülü SQL,
   açılışta advisory lock altında uygulanır. **Ekleyici ve daraltmayan**
   kalmaları bir sözleşmedir: kolon düşüren, yeniden adlandıran, tip daraltan
   ya da mevcut bir kolona unique index ekleyen bir migration rolling upgrade
   varsayımını bozar ve aşamalı rollout ister.
8. **How decisions are recorded** — `.ssot/` düzeni: `PRD.md` (ürün otoritesi),
   `ADR.md` (numaralı mimari kararlar, geri alınınca silinmez, durumu
   güncellenir), `plans/` (faz planları). Bir davranış değişikliği bir ADR'ye
   dokunuyorsa PR'da söylenir. **Kayıtların Türkçe tutulduğunu** bir parantezle
   belirt.
9. **Pull requests** — issue önce mi (özellik: evet; yazım hatası: hayır),
   `CHANGELOG.md`'ye davranış değişikliği yazmak, testin nereye gittiği.
   Ayrıntı için `.github/CONTRIBUTING.md` linki.
10. **Security issues** — public issue açma; `SECURITY.md` yolu.

### Bütünlük geçişi

Bütün wiki'yi bir kez oku ve düzelt:

1. **Yerel linkler.** `wiki/` içindeki her `[Text](Page-Name)` linki var olan
   bir dosyayı gösteriyor mu? Sayfa adlarını dosya listesinden üret ve
   linklerle karşılaştır. Kırık link kalmayacak.
2. **Doküman linkleri.** Her `https://ragmux.com/docs/<slug>/` linkindeki slug
   şu listede: `getting-started`, `providers`, `rag`, `users-and-limits`,
   `configuration`, `api`, `changelog`, `backup-restore`, `security`,
   `observability`, `scaling`. Listede olmayan slug düzeltilir.
3. **Sidebar eksiksiz mi.** `_Sidebar.md` 12 sayfanın hepsini (Home + 11 içerik sayfası; `_Sidebar` ve `_Footer` kendileri listelenmez) listeliyor ve
   hiçbir ölü giriş yok.
4. **Kopya taraması.** Her sayfadan birkaç düzyazı cümlesi seçip `docs/`
   içinde ara; birebir ya da neredeyse birebir eşleşen paragraf linke
   çevrilir. Kod bloğu tekrarı serbest.
5. **Ton ve dil.** Hepsi İngilizce; başlık biçimi sayfalar arasında tutarlı;
   tablo ve kod bloğu dışında HTML yok.
6. **Terim tutarlılığı.** Aynı şeyin her sayfada aynı adla anılması:
   "RAG store", "model connection", "project key (`sk-proj-…`)", "gateway key
   (`sk-user-…`)", "management key (`sk-mgmt-…`)", "all-in-one image",
   "split layout".

### Commit

`wiki/` içinde tek commit. Mesaj İngilizce, anlatımın son cümlesiyle biter —
**hiçbir trailer eklenmez**: `Co-Authored-By`, `Generated-With`, oturum
satırı, atıf bloğu, `Signed-off-by` yok. Gövde ne eklendiğini ve wiki'nin
`docs/`'u tekrarlamayıp linklediğini bir paragrafta söyler.

**`git push` atılmaz.** Push kullanıcı onayına bırakılır; raporda bunu belirt.

## Dokunulacak dosyalar

- `../wiki/Contributing-and-Development.md` (yeni)
- `../wiki/` altındaki diğer sayfalar — **yalnız** bütünlük geçişinin
  gerektirdiği düzeltmeler (kırık link, yanlış slug, kopya paragraf, terim
  tutarsızlığı). İçerik yeniden yazılmaz.
- `../wiki/` içinde `git add` + `git commit`.

## Dokunulmayacak

- `Ragmux/` deposundaki hiçbir dosya.
- `git push`.
- Önceki fazların içerik kararları: bir sayfayı beğenmedin diye yeniden
  yazma. Kırık olanı düzelt, gerisini rapor et.

## Kabul ölçütü

- `wiki/` altında 14 dosya var: `Home.md`, `_Sidebar.md`, `_Footer.md`,
  `Quickstart.md`, `Deployment-Recipes.md`, `Provider-Setup.md`,
  `RAG-Cookbook.md`, `Client-Integrations.md`, `Architecture-Overview.md`,
  `Operations-Runbook.md`, `Security-Hardening.md`, `Troubleshooting.md`,
  `FAQ.md`, `Contributing-and-Development.md`.
- **Kırık yerel link yok.** Bunu kanıtla: sayfa adlarını ve linkleri toplayan
  tek seferlik bir kontrol koştur (script scratchpad'de kalır, depoya girmez)
  ve çıktısını raporla.
- **Geçersiz docs slug'ı yok.** Aynı biçimde kanıtla.
- `git -C ../wiki status` temiz; tek yeni commit var.
- Commit mesajında hiçbir trailer yok:
  `git -C ../wiki log -1 --format=%B | grep -Ei 'co-authored|generated|claude|signed-off'` boş dönüyor.
- `git push` koşturulmadı.

## Notlar

Kaynaklar: `Ragmux/README.md` (Development bölümü), `Ragmux/Makefile`,
`Ragmux/.golangci.yml`, `Ragmux/.github/workflows/`,
`Tools/Ragmux/.github/CONTRIBUTING.md` (ayrı depo — yalnız okunur, linklenir),
`Ragmux/.ssot/`.

Bütünlük geçişinde bulduğun ama düzeltmediğin şeyleri (ör. bir sayfanın
eksik kaldığını düşündüğün bölüm) rapora yaz; kullanıcı karar versin.
