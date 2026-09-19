# Faz 06 — Yapılandırma ve kapanış küçükleri

- **Durum:** beklemede
- **Sahip:** backend-engineer
- **Bağımlılık:** yok
- **İlgili karar:** ADR-003 (5. madde)

## Amaç

Tek tek küçük ama her biri gerçek olan kusurları kapatmak.

## Kapsam

1. **`METRICS_ENABLED` yalnız tam olarak `"true"` kabul ediyor**
   (`internal/config/config.go`, `loadMetrics`). `METRICS_ENABLED=1`,
   `TRUE` ya da `yes` sessizce **kapalı** demek; oysa `TRACING_ENABLED`
   aynı dosyada geçersiz değeri hatayla reddediyor. Aynı katılığı uygula:
   tanınmayan değer açılışta hata.
2. **`errc` tek slotlu, iki yazan var** (`cmd/ragmux/main.go:335-349`).
   Hem `srv` hem `metricsSrv` hata verirse ikincisi sonsuza kadar bloke bir
   goroutine bırakıyor. `make(chan error, 2)`.
3. **`GET /admin/api/setup` kurulum bittikten sonra da bilgi sızdırıyor**
   (`internal/admin/setup.go:52`). Endpoint tasarım gereği kimliksiz ve
   `needs_setup` false olduğunda da tüm alanları döndürüyor; v0.4 buna
   `has_connections` ve `has_projects`'i ekledi. İnternete açık bir
   kurulumda kimliksiz bir `curl`, `migrations_version` + `database_role` +
   `secret_key_source`'un yanına kurulumun ne kadar ilerlediğini de veriyor.
   Tek başına açık değil, keşif yüzeyi. `n == 0` değilse yalnız
   `{"needs_setup":false}` döndür; sihirbazın diğer alanlara zaten yalnız
   kurulum sırasında ihtiyacı var.
4. **`store.DefaultGatewayScopes` paket düzeyinde `var`** ve `clipScopes`
   (`internal/admin/keys.go:146`) onu doğrudan döndürüyor. Bugün kimse
   mutate etmiyor; ileride edecek biri tüm süreci etkiler. Kopya döndür ya
   da sabit bir diziye çevir.

5. **`viewer` kendine gateway key basabiliyor, rol matrisi dokümanıyla
   çelişiyor.** `keys` rota grubu bilinçli olarak rolsüz (herkes kendi
   key'ini yönetir), yani bir `viewer` `sk-user-…` üretip `/v1`'de para
   harcayabiliyor. `internal/auth/policy.go:18-20` ve
   `docs/users-and-limits.md` rol matrisi `viewer`'ı salt-okunur diye
   tanımlıyor. Davranış kasıtlı görünüyor; **doküman ile kodun aynı şeyi
   söylemesi** gerekiyor. Ya matris "viewer kendi gateway key'ini
   üretebilir" diye netleşir, ya da key basma `editor`'a bağlanır.
   **Karar verildi (ADR-003, 2026-09-19): davranış korunur, doküman
   düzeltilir.** Kod değişmez; `docs/users-and-limits.md` rol matrisi
   `viewer`'ın kendi gateway key'ini üretebildiğini ve harcamasının kendi
   limitine yazıldığını yazar, rollerin **admin yüzeyini** sınırladığını
   netleştirir. `internal/auth/policy.go` bu ayrımı bir yorumla kaydeder.
6. **Session taşınabilir bir credential hâline geliyor.**
   `internal/admin/admin.go:349-352`, `{"bearer":true}` login yanıtında
   token'ı gövdeye koyuyor. `sessionOnly` koruması "session = klavyedeki
   insan" varsayımına dayanıyor; bu varsayım bearer token elden ele
   geçtiğinde zayıflıyor. Bugün bir açık değil — token yine 24 saatte
   sönüyor ve şifre değişiminde iptal ediliyor — ama varsayımın kaydedilmesi
   gerekiyor. En azından `docs/api.md`'de yazılı hâle getir; ADR adayı.

## Dokunulacak dosyalar

- `internal/config/config.go` + `config_test.go`
- `cmd/ragmux/main.go`
- `internal/admin/setup.go` + testi
- `internal/store/keys.go`, `internal/admin/keys.go`
- `internal/auth/policy.go` (yalnız yorum, davranış değişmiyorsa)
- `docs/api.md` (setup yanıtı, session/bearer notu),
  `docs/configuration.md`, `docs/users-and-limits.md` (rol matrisi)

## Dokunulmayacak

- Kurulum akışının kendisi: `needs_setup` alanı, kimliksiz olması ve
  `CreateFirstUser`'ın atomikliği. Yalnız **kurulum bittikten sonraki**
  yanıt daralır.
- Üç adımlı sihirbazın kurulum sırasındaki davranışı — `has_connections`
  ve `has_projects` `needs_setup` true iken dönmeye devam eder.
- `TRACING_ENABLED`'ın mevcut varsayılan mantığı.

## Kabul ölçütü

- `METRICS_ENABLED=yes` ile `config.Load` hata veriyor; testi var.
- Kurulum tamamlandıktan sonra `GET /admin/api/setup` yalnız
  `{"needs_setup":false}` döndürüyor; e2e testi bunu iddia ediyor ve
  sihirbazın kurulum sırasındaki testi hâlâ geçiyor.
- `make test` (race), `go vet`, `golangci-lint run ./...` temiz.

## Notlar

3. madde yayınlanmış bir yanıt şeklini daraltıyor — `CHANGELOG.md` için
faz raporunda listele.

5. madde karara bağlandı (ADR-003): doküman düzeltilir, kod değişmez.

6. madde kod değişikliği değil: `{"bearer":true}` davranışı korunur,
`sessionOnly`'nin dayandığı varsayım `docs/api.md`'de yazılı hâle gelir.
Yeni bir kısıt getirme; yalnız varsayımı kaydet ve faz raporunda ADR adayı
olarak bildir.
