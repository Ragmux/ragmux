# Faz 01 — Hazır olma ve seri tavanı kararları

- **Durum:** beklemede
- **Sahip:** backend-engineer
- **Bağımlılık:** yok
- **İlgili karar:** ADR-001, ADR-002 (ikisi de `.ssot/ADR.md`'de kabul
  edildi, 2026-09-19). PRD davranış kuralı 10.

## Amaç

Kullanıcının verdiği iki işletme kararını uygulamak.

## Karar (verildi — 2026-09-19)

- **(a) `/readyz`:** ADR-001. `applied < head` → 503 (`migrating`);
  `applied > head` → **200**, gövdede `degraded` girdisiyle. Yani aşağıdaki
  seçeneklerden **1.** uygulanacak.
- **(b) Seri tavanı:** ADR-002. Tavan davranışı aynı kalır — yeni seri
  düşer — ama olay sessiz olmaktan çıkar: `Error` seviyesinde loglanır ve
  düşen seri sayısı dışarı verilen bir sayaçla görünür olur. Yani aşağıdaki
  seçeneklerden **3.'ün alarm yarısı**, tavanı yükseltmeden. LRU tahliye
  **reddedildi** (gerekçe ADR-002'de).

## Kapsam

### (a) `/readyz` rolling update'te filoyu düşürüyor

`cmd/ragmux/main.go:520` `applied != head` karşılaştırmasını **iki yönlü**
yapıyor. `maxSurge=1` bir rolling update'te ilk yeni pod migration'ı
uygular; o anda çalışan N eski replikanın hepsi `applied > head` olup 503
döner ve load balancer hepsini rotasyondan çıkarır. Sıfır kesintili olması
beklenen deploy kesintiye dönüşür. Rollback sırasında eski imaj hiç hazır
olamaz.

Seçenekler:

1. `applied < head` → 503 (migrating), `applied > head` → 200 +
   `degraded` girdisi. Deploy akar; eski replika yeni şemayla çalışmayı
   sürdürdüğünü bildirir.
2. Mevcut davranış korunur, `docs/scaling.md#rolling-restarts` bunun
   `maxUnavailable=0` ile bile filoyu düşürdüğünü açıkça yazar.

### (b) Metrik seri tavanı sınırsız girdiyle doluyor

İki etiket trafikle büyüyor:

- `internal/obs/http.go:69` — `r.Method` ham geçiyor. Go sunucusu RFC7230
  token'ı olan **her** method'u kabul ediyor.
- `internal/obs/obs.go:266` — `ErrorType`, upstream JSON'un `error.type`
  alanı; uzunluk sınırı ya da allowlist yok.

Kimliksiz 5000 istek `MaxSeries`'i doldurur. `admitSeries`
(`internal/metrics/metrics.go:108`) geri sayım ya da tahliye yapmadığı için
**bundan sonra her yeni meşru seri kalıcı olarak düşer**: yeni bir proje,
yeni bir model, `ragmux_gateway_cost_usd_total`'ın yeni serisi bir daha
görünmez. Fatura metrikleri sessizce donar.

Etiketleri sınırlamak (Faz 02) girdiyi keser. Bu fazın kararı: **tavan
dolduğunda ne olmalı?**

1. Mevcut: yeni seri düşer, sayaç artar, bir kez loglanır.
2. Tavan dolduğunda en eski/en az kullanılan seriyi tahliye et.
3. Tavanı yükselt ve dolmayı sert bir alarm say (log seviyesi `Error`).

## Dokunulacak dosyalar

- `cmd/ragmux/main.go` — `/readyz` gövdesi
- `internal/metrics/metrics.go` — `admitSeries` davranışı, karara göre
- `docs/observability.md` — `/readyz` bölümü ve kardinalite bölümü
- `docs/scaling.md` — rolling restart bölümü
- `cmd/ragmux/observability_test.go` — `/readyz` durum matrisi

## Dokunulmayacak

- `/healthz` — liveness olarak kalır, konteyner HEALTHCHECK'i ona bağlı.
- `internal/obs/*` etiket üretimi — bu Faz 02'nin işi; burada yalnız tavan
  davranışı kararlaştırılır.
- Metrik adları ve mevcut etiket **isimleri** — yayınlanmış yüzey.

## Kabul ölçütü

- `go test ./cmd/ragmux/ -run Readyz -count=1` yeşil ve test, seçilen
  davranışı her iki yönde de (`applied < head`, `applied > head`) iddia
  ediyor.
- `docs/observability.md` ve `docs/scaling.md` seçilen davranışı ve
  **gerekçesini** yazıyor.
- Karar `.ssot/ADR.md`'ye iki ADR olarak, kullanıcı onayıyla giriyor.

## Notlar

Aşağıdaki "Seçenekler" listeleri kararın alındığı bağlamı gösteriyor;
artık tarihsel. Uygulanacak olan yukarıdaki **Karar** bölümüdür.

ADR-001 bir koşula bağlı: Ragmux migration'larının **ekleyici** kalması.
`docs/scaling.md` bu koşulu açıkça yazsın — kolon düşüren bir migration
gelirse bu karar yeniden ele alınmalı.

ADR-002'nin sayacı yayınlanmış metrik yüzeyine yeni bir isim ekliyor;
`docs/observability.md`'deki metrik listesine de girsin.
