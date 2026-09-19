# Faz 02 — Metrik ve trace etiket sınırları

- **Durum:** beklemede
- **Sahip:** backend-engineer
- **Bağımlılık:** Faz 01 (tavan davranışı kararı)
- **İlgili karar:** PRD davranış kuralı 10

## Amaç

Etiket ve span girdilerini kurulumun büyüklüğüyle sınırlamak; PRD kuralı
10'un kodda gerçekten tuttuğunu testle göstermek.

## Kapsam

1. **`method` etiketi** (`internal/obs/http.go:69`) sabit bir listeye
   eşlenir: `GET POST PUT PATCH DELETE HEAD OPTIONS`, gerisi `other`.
2. **`error.type`** (`internal/obs/obs.go:266`) `provider` paketinde
   bilinen tiplere daraltılır; tanınmayan `other` olur.
3. **`runtime/metrics` yarışı** (`internal/metrics/runtime.go:25-38`):
   `sample(name)` closure'ı paylaşılan bir `[]metrics.Sample`'a yazıyor;
   iki eşzamanlı scrape aynı backing array'e yazar. `-race` yakalayamıyor
   çünkü `metrics.Read` runtime'a linkname ile giriyor. Her çağrıda lokal
   slice ayır ya da mutex koy.
4. **`ragmux.request_id` span attribute'ü** (`internal/obs/http.go:106`)
   chi'nin RequestID'sinden geliyor, o da istemcinin `X-Request-Id`
   header'ını aynen kullanıyor. Şekil doğrulaması + uzunluk kırpması, ya da
   RequestID'yi header'ı yok sayacak şekilde yapılandır.
5. **Dosya adı `exception.message`'a sızıyor**: `internal/rag/parse.go:122`
   `fmt.Errorf("unsupported file type %q", filepath.Ext(filename))`
   üretiyor, `internal/rag/ingest.go:451` bunu `span.RecordError` ile
   collector'a yazıyor. `retrieve.go`'daki gibi sentinel hata kullan ya da
   `RecordError`'a uzunluk tavanı koy. PRD kuralı 10 bunu yasaklıyor.
6. **Kapalıyken tahsis**: `internal/rag/retrieve.go:179` (`gen_ai.system`,
   her retrieval) ve `internal/provider/http.go:275`
   (`http.response.status_code`, her upstream çağrısı) `IsRecording()` ile
   korunmuyor. İkisini de sar; `TestDisabledTracerAllocatesNothing`'i
   `SetAttributes`'i de kapsayacak şekilde genişlet.
7. **Kapanıştan sonra enqueue edilen span** (`internal/tracing/otlp.go:86`)
   sessizce kayboluyor, `ragmux_tracing_spans_dropped_total` artmıyor.

## Dokunulacak dosyalar

- `internal/obs/http.go`, `internal/obs/obs.go`
- `internal/metrics/runtime.go`
- `internal/tracing/tracing.go`, `internal/tracing/otlp.go`
- `internal/rag/parse.go`, `internal/rag/ingest.go`, `internal/rag/retrieve.go`
- `internal/provider/http.go` (yalnız span attribute sarmalama)
- ilgili testler, `docs/observability.md` kardinalite listesi

## Dokunulmayacak

- Metrik **adları**, bucket sınırları ve etiket **isimleri** — yayınlanmış
  yüzey; değişirse mevcut dashboard'lar kırılır.
- `internal/metrics` exposition biçimi ve `Handler` yetki mantığı.
- `internal/provider`'ın hata **mesajları** ve istemciye dönen gövdeler —
  yalnız span'e ne yazıldığı değişir.

## Kabul ölçütü

- Yeni bir test, uydurma bir HTTP method'u ve tanınmayan bir upstream
  `error.type` ile istek üretip `/metrics` çıktısında `other` gördüğünü
  ve seri sayısının artmadığını iddia ediyor.
- Bir e2e testi, bilinen bir gizli dizgiyi dosya adı üzerinden ingest'e
  sokup yakalanan OTLP yükünde **bulunmadığını** iddia ediyor.
- `TestDisabledTracerAllocatesNothing` `SetAttributes` çağrısını da içerip
  hâlâ 0 alloc/op veriyor.
- `make test` (race), `go vet`, `golangci-lint run ./...` temiz.

## Notlar

3. madde `-race` ile gösterilemez; yorumla ve statik gerekçeyle bırakılır.
