# Faz 04 — Token muhasebesi ve fiyat tablosu

- **Durum:** beklemede
- **Sahip:** backend-engineer
- **Bağımlılık:** yok
- **İlgili karar:** PRD davranış kuralı 7 (maliyet tahmindir)

## Amaç

Token ve maliyet sayılarının bozuk bir upstream karşısında da anlamlı
kalması; fiyat tablosunun dokümanın söylediği şeyi yapması.

## Kapsam

1. **Negatif token sayıları clamp edilmiyor.** Ölçülmüş: test DB'sine
   `completion_tokens=-100, cost_micros=-1500` satırı sorunsuz yazıldı,
   `Summarize` `completion_tokens=-78, cost_usd=-0.001423` döndü.
   `CostMicros(0,0,0,-100) = -1500`. Tetikleyici bozuk ya da kötü niyetli
   bir upstream, özellikle operatörün işaret ettiği `custom_openai`.
   `fillUsage` (`internal/gateway/gateway.go:914-922`) içinde negatifleri
   0'a çek; `CostMicros`'ta (`internal/pricing/pricing.go:100-110`) sonucu
   `[0, math.MaxInt64]` aralığına doyur — `int64(math.Round(...))` aralık
   dışı float için implementation-defined ve arm64 ile amd64 farklı
   davranıyor.
2. **`ResetModelPrice` seed'in NULL sözleşmesini bozuyor.**
   `internal/admin/prices.go:194-198` + `internal/pricing/pricing.go:197-207`:
   `BuiltinPrice` `resolve()`'dan geçmiş, yani cache fiyatı yoksa
   `CacheWrite = Input` olarak doldurulmuş bir `Price` dönüyor; reset bunu
   NOT NULL yazıyor. Ölçülmüş (`openai/gpt-5*`): seed sonrası
   `cache_write=NULL`, reset sonrası `cache_write=1.25`. Reset edilmiş satır
   ileride `input` değişirse onu izlemez, NULL'lu kardeşi izler.
   Çözülmemiş (`*float64`) bir varyant döndür ya da reset'te `nil` durumunu
   NULL olarak yaz.
3. **Fiyat önceliğinde `Source` terimi yok; doküman yanlış.**
   `internal/pricing/pricing.go:5-6` *"operatörün kendi satırları builtin'lere
   göre öncelikli"* diyor; `NewTable` sıralaması yalnız (tam > en uzun
   literal önek > en küçük ID) bakıyor. Ölçülmüş: builtin tam `gpt-4o` vs
   kullanıcı wildcard `gpt-4o*` → builtin kazanıyor. Ya sıralamaya
   `Source == SourceUser` terimini ekle, ya da doküman cümlesini gerçeğe
   uyarla.
4. **`TestPricesVersionPinned` sürüm artışını zorlamıyor**
   (`internal/pricing/pricing_test.go:12-27`): yalnız sha256'yı sabit bir
   constant'a pinliyor. Fiyatı değiştirip constant'ı elle güncelleyen biri
   `version` bump'ını atlayarak yeşil geçer; `Seed`'in
   `builtin_version < EXCLUDED.builtin_version` koşulu hiç tetiklenmez ve
   mevcut kurulumlar eski fiyatta kalır. Constant'ı `map[int]string{version: sha}`
   yap; checksum eşleşmiyor **ve** version haritada zaten varsa hata ver.
5. **`prices.json` catch-all'ı gerçek sıfırı maskeliyor.** `ollama` ve
   `custom_openai` için `"*": 0/0`, ücretli bir API'ye bakan bir
   `custom_openai` bağlantısını `cost_source:"builtin"` ile 0,00 USD
   raporlatıyor; doküman "eşleşme yoksa none" diyor.
6. **Gemini'de `prompt + completion == total` garanti değil.**
   `internal/provider/types.go:159-161` ↔ `internal/provider/gemini.go:523-534`:
   `TotalTokenCount` aynen kopyalanıyor, `normalize()` yalnız sıfırsa
   hesaplıyor, `usageMetadata.toolUsePromptTokenCount` hiç parse edilmiyor.
   Araç turlarında `total > prompt + completion` olur ve fark maliyete
   yansımaz. Anthropic yolu doğru (total'i kendi türetiyor).
   **Doğrulanamadı:** `toolUsePromptTokenCount`'un `totalTokenCount`'a dahil
   olup olmadığı canlı Gemini olmadan test edilemedi; önce bunu netleştir.
7. **OpenAI dışı adapter'lar `include_usage` sorulmadan son usage chunk'ı
   gönderiyor** (`anthropic.go:484`, `gemini.go:514`, `ollama.go:347`;
   `gateway.go:762-777` filtrelemiyor). OpenAI `include_usage` olmadan
   `"choices":[]` içeren chunk göndermez; `chunk.choices[0]`'a doğrudan
   indeksleyen katı istemciler kırılır. Ya filtrele ya dokümante et.

## Dokunulacak dosyalar

- `internal/gateway/gateway.go` (`fillUsage`), `internal/pricing/*`,
  `internal/admin/prices.go`, `internal/provider/types.go`,
  `internal/provider/gemini.go`, `internal/store/metrics.go` (gerekirse)
- `internal/pricing/prices.json`
- `docs/api.md`, `docs/providers.md`

## Dokunulmayacak

- `request_logs` şeması ve CSV kolon sırası — yayınlanmış yüzey,
  importer'lar pozisyona göre okuyor.
- Anthropic token muhasebesi (`anthropic.go:96-106`) — doğru çalışıyor ve
  `cache_test.go` ile pinli.
- Maliyetin hesaplandığı yer: gateway'in mevcut deferred chokepoint'i.
- `fillUsage`'ın saf kalması (DB erişimi yok).

## Kabul ölçütü

- Negatif ve aşırı büyük token sayıları için tablo testi: `CostMicros` asla
  negatif dönmüyor, taşmada doyuyor; `Summarize` negatif toplam üretmiyor.
- Seed → reset → seed döngüsünden sonra `cache_write_per_mtok` NULL'luğu
  korunuyor.
- `prices.json`'da fiyat değiştirip `version` bumplamayan bir değişiklik
  testi **kırıyor**.
- `make test` (race), `go vet`, `golangci-lint run ./...` temiz.

## Notlar

6. madde doğrulanamadıysa kapsamdan çıkarılıp raporda öyle bildirilir;
tahminle kod yazılmaz.
