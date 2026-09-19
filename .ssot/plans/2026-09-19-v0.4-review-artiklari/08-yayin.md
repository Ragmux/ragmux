# Faz 08 — Yayın: push, PR, tag

- **Durum:** beklemede
- **Sahip:** kullanıcı onayı (uygulama: ana oturum)
- **Bağımlılık:** Faz 01–07 tamam ve review'dan temiz
- **İlgili karar:** yok

## Amaç

`v0.4` branch'ini dışarıya açmak.

## Kapsam

Bugüne kadar **hiçbir şey push edilmedi**, `main` dokunulmadı, tag yok.
Binary şu an `git describe` ile `v0.3.1-NN-g<sha>` diyor.

1. `CHANGELOG.md`'yi Faz 01–07'nin rapor ettiği davranış değişiklikleriyle
   tek seferde güncelle. Özellikle bu turda eklenen güvenlik düzeltmeleri
   (`### Security`) ve yanıt şekli daralan yerler (`### Changed`).
2. `ragmux.com` deposunda `npm run sync-docs` koştur ve üretilen dosyaları
   commit et — CHANGELOG değiştiyse `changelog.md` de değişir.
3. Remote'u ve hedef branch'i **kullanıcıya göster**, onay al.
4. Push et, PR aç. PR gövdesine atıf bloğu eklenmez.
5. `v0.4.0` tag'i: sürüm kararı kullanıcınındır; onay çıkmadan atılmaz.
6. Tag sonrası `release.yml`'nin ürettiği imajları (`:0.4.0`,
   `:0.4.0-paradedb`, `:latest`) kontrol et.

## Dokunulacak dosyalar

- `CHANGELOG.md`
- `ragmux.com/src/content/docs/en/*.md` (sync çıktısı)
- git referansları (branch, tag) — dosya değil

## Dokunulmayacak

- Kod. Bu faz yalnız yayınlar; bir kusur çıkarsa kendi fazına döner.
- `main` branch'i doğrudan — değişiklik PR üzerinden gider.
- `.ssot/ADR.md` — kararlar ayrı bir turda, kullanıcı onayıyla yazılır.

## Kabul ölçütü

- Push öncesi `make test` (race), `go vet`, `golangci-lint run ./...`,
  `govulncheck` ve `make docker-build` temiz.
- Remote ve branch kullanıcıya gösterilip onaylandı.
- PR açık ve CI yeşil (pgvector job'ı, ParadeDB job'ı, docker job'ı).
- Tag yalnız kullanıcı "at" dedikten sonra var.

## Notlar

Bu fazın tamamı dışarıya açılan ve geri alması zor işler içeriyor; her
adımda onay alınır, hiçbiri varsayımla yapılmaz.
