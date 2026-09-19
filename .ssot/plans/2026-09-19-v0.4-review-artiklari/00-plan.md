# v0.4 review artıkları — genel plan

- **Durum:** beklemede
- **Tarih:** 2026-09-19
- **Öncül:** `.ssot/plans/2026-09-19-v0.4/` (kod tamam, yayın bekliyor)

## Neden

Yayın öncesi üç reviewer `v0.4` diff'ini okudu ve üçü de `BLOCKED` verdi.
5 blocker ile 3 major kapatıldı ve testlere bağlandı; geriye buradaki
maddeler kaldı. **Hiçbiri güvenlik sınırını delmiyor** — o yüzden sürüm
bunlarla da yayınlanabilir — ama her biri raporlanmış, çoğu ölçülmüş
gerçek bir kusur ve hiçbiri "sonra bakarız" diye kaybolmamalı.

İki madde kullanıcı kararı bekliyor (Faz 01). Kalanlar teknik.

## Faz tablosu

| Faz | Başlık | Sahip | Durum | Bağımlılık |
|---|---|---|---|---|
| 01 | Hazır olma ve seri tavanı kararları | backend-engineer | **karar bekliyor** | — |
| 02 | Metrik ve trace etiket sınırları | backend-engineer | beklemede | 01 |
| 03 | Provider: şema ve görsel doğrulama | backend-engineer | beklemede | — |
| 04 | Token muhasebesi ve fiyat tablosu | backend-engineer | beklemede | — |
| 05 | Depolama ve ölçek temizliği | backend-engineer | beklemede | — |
| 06 | Yapılandırma ve kapanış küçükleri | backend-engineer | beklemede | — |
| 07 | ParadeDB yolunun canlı doğrulaması | backend-engineer | beklemede | — |
| 08 | Yayın: push, PR, tag | — (kullanıcı onayı) | beklemede | 01–07 |

02–07 birbirinden bağımsız; paralel verilebilir. 01 karar çıkmadan
başlamaz. 08 hepsinin review'dan temiz geçmesini bekler.

## Ortak kurallar

- Her faz bittiğinde diff `reviewer`'a verilir; bulgular kullanıcıya
  özetlenir, onay çıkmadan sonraki faz kapanmış sayılmaz.
- `make test` (race), `gofmt -l .`, `go vet ./...` ve
  `golangci-lint run ./...` her fazın sonunda temiz olmalı.
  golangci-lint `v2.13.2` ile bu makinede kurulu.
- Commit mesajlarına trailer eklenmez.
- `CHANGELOG.md`'ye yazılacak davranış değişiklikleri faz raporunda
  listelenir; dosyayı Faz 08 tek seferde günceller.
- PRD'deki davranış kuralları (`.ssot/PRD.md`) bağlayıcıdır; bir fazın
  onları kırması gerekiyorsa durup sorar.

## Kaynak

Bulgular üç review raporundan geliyor; her maddenin yanında raporun
verdiği dosya:satır ve ölçüm varsa o da faz dosyasında duruyor.
