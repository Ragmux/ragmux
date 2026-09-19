# Faz 03 — Provider: şema ve görsel doğrulama

- **Durum:** beklemede
- **Sahip:** backend-engineer
- **Bağımlılık:** yok
- **İlgili karar:** PRD davranış kuralı 6 (istemci URL'leri operatör
  muafiyetini miras almaz)

## Amaç

Sanitizer'ın verdiği sözü tutmasını ve görsel girdisinin her yoldan
doğrulanmasını sağlamak.

## Kapsam

1. **Sanitize edilmiş şema hâlâ Gemini'nin reddedeceği çıktı üretiyor.**
   `internal/provider/gemini_schema.go:109-113, 140-141, 168-171`.
   `geminiSchemaScalars` değerleri `json.RawMessage` olarak **doğrulanmadan**
   geçiyor. Ölçülmüş örnekler:
   - `{"type":"integer","enum":[1,2,3]}` → aynen geçiyor; Gemini'nin
     `Schema.enum` alanı `repeated string`.
   - `{"const":7}` → `{"enum":[7],"type":"string"}` — sanitizer'ın kendi
     ürettiği tip/değer çelişkisi.
   - `{"description":{"nested":"object"}}` → aynen geçiyor.
   - `{"type":"integer","minimum":"not-a-number"}` → aynen geçiyor.
   Yapılacak: `enum`'u `[]string`'e unmarshal et, olmazsa düşür; `const`'u
   yalnız string ise enum'a çevir, değilse `type`'ı değerden türet;
   `description`'ı string'e, sayısal sınırları `json.Number`'a doğrula.
   Dosyanın tek varlık nedeni bu garanti (bkz. dosya başı yorumu).
2. **`$defs` yalnız son yol parçasıyla anahtarlanıyor**
   (`gemini_schema.go:288-292`): `#/$defs/Foo`, `#/definitions/Foo` ve harici
   `https://…#/definitions/Foo` çakışıyor; farklı derinlikte aynı isimli iki
   def varsa kazananı Go map iterasyonu belirliyor → çıktı deterministik
   değil. Tam yolla anahtarla.
3. **`data:` URL'leri doğrulamadan muaf.** `internal/provider/images.go:92`
   ↔ `243-255`: `imageMediaTypes` yalnız uzak indirmede uygulanıyor.
   Ölçülmüş: `data:text/html;base64,…` → `MediaType="text/html"`;
   `data:,plainpayload` → boş tip, base64 değil;
   `data:image/png;base64,!!!` → bozuk payload. Hepsi provider'a gidip
   karışık bir upstream 400'ü olarak dönüyor. `;base64` sonekini zorunlu
   tut, medya tipi beyaz listesini uygula, `base64.StdEncoding` ile doğrula.
4. **Süreç geneli eşzamanlılık tavanı yok** (`images.go:199-235`,
   `cmd/ragmux/main.go:173-181`): istek başına 8 görsel × 10 sn ve
   transport'ta `MaxConnsPerHost` yok. Bir istemci isteği keyfi bir hedefe
   gateway IP'sinden 8 GET üretiyor. Semaphore + `MaxConnsPerHost` ekle.
5. **Cache bayt tavanı sabit 64 MiB** (`imagecache.go:86`) ama
   `IMAGE_FETCH_MAX_MB` yapılandırılabilir; 64 MiB üstüne çıkarılırsa cache
   sessizce hiçbir şey saklamaz. Tavanı türet ya da yapılandırılabilir yap.
6. **`imageBudget` tavanının testi yok** (`images.go:222-235`).
   `images_test.go:216` yalnız getter'ı kontrol ediyor. Elle doğrulandı,
   çalışıyor; regresyon testi ekle.
7. **Redirect'te şema düşürmesi** (`internal/netguard/netguard.go:214`):
   yalnız host karşılaştırılıyor, aynı host'ta `https→http` izleniyor.

## Dokunulacak dosyalar

- `internal/provider/gemini_schema.go` + testi
- `internal/provider/images.go`, `imagecache.go` + testleri
- `internal/netguard/netguard.go` + testi
- `cmd/ragmux/main.go` (yalnız transport ayarı ve semaphore wiring)
- `internal/config/config.go` (yeni bir tavan ayarı gerekiyorsa)
- `docs/providers.md`, `docs/configuration.md`, `.env.example`

## Dokunulmayacak

- `sanitizeGeminiSchema`'nın **asla hata vermeme** sözleşmesi: kötü bir
  şema `{"type":"object"}`'e iner, isteği düşürmez.
- Faz 0'da eklenen düğüm bütçesi (`geminiSchemaMaxNodes`) ve derinlik
  tavanı — sınırlar korunur, yalnız doğrulama eklenir.
- Görsel indirmenin kendi dialer'ı ve koşulsuz private-address filtresi.
- Ollama'nın mevcut inline-only 400 mesajı.

## Kabul ölçütü

- `TestGeminiSchemaSanitize` tablosu yukarıdaki dört ölçülmüş girdiyi
  içeriyor ve her biri için çıktının Gemini'nin alt kümesinde kaldığını
  iddia ediyor.
- Aynı isimli iki def'li bir şema için `sanitizeGeminiSchema` 100 koşumda
  bayt düzeyinde aynı çıktıyı veriyor.
- `data:` tablosu: geçersiz medya tipi, eksik `;base64` ve bozuk payload
  için gateway'in kendi 400'ü dönüyor, provider'a hiçbir şey gitmiyor.
- `make test` (race), `go vet`, `golangci-lint run ./...` temiz.

## Notlar

1. ve 3. maddeler kullanıcıya dönen hata kalitesini değiştirir —
   `CHANGELOG.md` için faz raporunda listele.
