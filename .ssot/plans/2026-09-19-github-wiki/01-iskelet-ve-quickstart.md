# Faz 01 — İskelet: Home, Sidebar, Footer, Quickstart

- **Durum:** tamam (review CHANGES REQUESTED → 6 düzeltme uygulandı)
- **Sahip:** frontend-engineer
- **Bağımlılık:** yok
- **İlgili karar:** yok (wiki kapsam kararı `00-plan.md`'de)

Bu brief kendi kendine yeter. Ortak kurallar (dil, link hedefleri, kapsam
kuralı, GitHub wiki biçimi, dokunulmayacaklar) için **önce** aynı dizindeki
`00-plan.md`'yi oku — bağlayıcıdır.

## Amaç

Wiki'nin gezinme iskeletini ve giriş kapısını kurmak. Sonraki fazlar kendi
sayfalarını bu iskelete asacak; sayfa adları ve sidebar grupları burada
sabitlenir.

## Kapsam

Dört dosya, hepsi `../wiki/` altında (bu plan dosyasına göre:
`/Users/muhammetsafak/Documents/Projects/Tunedness/Tools/Ragmux/wiki/`).

### `Home.md`

Placeholder içeriğin üzerine yazılır. Sırasıyla:

1. **Başlık ve tek cümlelik tanım.** Ragmux: tek binary, self-hosted AI
   gateway; her sağlayıcının önünde tek bir OpenAI uyumlu API, retrieval
   dâhili. Repo, website, releases linkleri.
2. **Mimari şema.** `README.md`'deki ASCII şemanın aynısı kullanılabilir (kod
   bloğu tekrarı serbest) ya da sadeleştirilmiş bir sürümü.
3. **"What do you want to do?" tablosu** — wiki'nin asıl giriş noktası.
   Satırlar görev, sütunlar hedef sayfa:

   | I want to… | Go to |
   | run Ragmux for the first time | Quickstart |
   | choose a deployment shape (single container, split Postgres, ParadeDB, several replicas) | Deployment Recipes |
   | connect OpenAI / Anthropic / Gemini / DeepSeek / Ollama / vLLM | Provider Setup |
   | call the gateway from my app | Client Integrations |
   | make retrieval answer better | RAG Cookbook |
   | back up, restore, upgrade, monitor | Operations Runbook |
   | harden a public installation | Security Hardening |
   | understand how a request flows through the gateway | Architecture Overview |
   | fix an error I am seeing | Troubleshooting |
   | ask a short question | FAQ |
   | build and contribute | Contributing and Development |

4. **Reference documentation** bölümü: bu wiki'nin referans taşımadığını bir
   cümleyle söyler ve `ragmux.com/docs/` sayfalarına tablo hâlinde link verir
   (slug listesi `00-plan.md`'de). `SECURITY.md` ve `CHANGELOG.md` de burada.
5. **Kapanış satırı:** sürüm (`v0.4.1` davranışı anlatılır), lisans
   (AGPL-3.0-or-later), `ragmux.com`.

### `_Sidebar.md`

30 satırı geçmez. Gruplar ve sıra:

- **Start** — Home, Quickstart, Deployment Recipes
- **Guides** — Provider Setup, RAG Cookbook, Client Integrations
- **Operations** — Operations Runbook, Security Hardening, Troubleshooting
- **Reference** — Architecture Overview, FAQ, Contributing and Development,
  sonra tek satır "Full docs → ragmux.com/docs"

Henüz yazılmamış sayfalara link verilir; Faz 06'ya kadar bazıları kırık
görünecek, bu beklenen durumdur.

### `_Footer.md`

Tek satır: Repository · Website · Documentation · Releases · Issues · License
(AGPL-3.0-or-later). Hepsi mutlak URL.

### `Quickstart.md`

Sıfırdan ilk cevaba giden **tek** akış. Dallanma yok — "bunu da yapabilirsin"
cümleleri Deployment Recipes'a link olur. Adımlar:

1. **Run it.** `README.md` Quick start'taki `docker run` komutu:
   `-p 127.0.0.1:8765:8765`, `-e SECRET_KEY="$(openssl rand -hex 32)"`,
   `-v ragmux-data:/data`, `ragmux/ragmux:latest`. İki uyarı kısaca: SECRET_KEY
   sağlayıcı kimlik bilgilerini şifreler ve yedekle birlikte saklanır; port
   yalnız loopback'te yayınlanır.
2. **Create the first administrator.** `http://localhost:8765/admin/` açılır,
   ilk kurulum formu çıkar (en az 12 karakter parola). Gözetimsiz kurulum için
   `ADMIN_USER` / `ADMIN_PASSWORD` alternatifi tek cümle + Configuration linki.
3. **Log in over the API** ve token'ı sakla — `README.md`'deki `curl … /admin/api/login`
   + `jq -r .token` bloğu.
4. **Add a chat model connection** — `POST /admin/api/models`. Örnek `openai`
   ya da `anthropic` üzerinden; `api_key` ve `model_name` alanları.
5. **Create a project** — `POST /admin/api/projects` ile `model_connection_id`
   ve `system_prompt`; dönen `sk-proj-…` key'inin **yalnız bir kez**
   gösterildiği açıkça yazılır.
6. **Make the first call** — `curl -N …/v1/chat/completions` (streaming) ve
   OpenAI Python SDK eşdeğeri. Python örneğinde `if chunk.choices:` koruması
   yorumuyla birlikte korunur (bir upstream kendi frame'ini gönderebilir;
   usage istendiğinde son chunk yalnız token sayısı taşır).
7. **Add retrieval (optional)** — embedding connection, `POST /admin/api/rag-stores`,
   `-F file=@handbook.pdf` ile yükleme, `pending → processing → ready` durum
   akışı, sonra projeye `rag_store_id` bağlama. Yanıt başlığı
   `x-ragmux-rag-hits`. Derinlik için RAG Cookbook linki.
8. **Where to go next** — üç link: Deployment Recipes (ciddi kurulum),
   Provider Setup (başka sağlayıcı), Client Integrations (uygulamayı bağlama).

Kısa bir **dashboard turu** bölümü de girer: `/admin/` sekmeler — Overview,
Models, RAG stores, Projects, API keys, Playground, Prices (editor), Users
(admin), Audit log (admin). Rol farkı tek cümle: viewer salt-okunur ekranlar,
editor formlar, admin kullanıcı ve audit ekranları.

## Dokunulacak dosyalar

- `../wiki/Home.md` (üzerine yazılır)
- `../wiki/_Sidebar.md` (yeni)
- `../wiki/_Footer.md` (yeni)
- `../wiki/Quickstart.md` (yeni)

## Dokunulmayacak

- `../wiki/` altındaki başka hiçbir dosya — sonraki fazların sayfaları burada
  **oluşturulmaz**, boş iskelet dosyası da bırakılmaz.
- `Ragmux/` deposundaki hiçbir dosya.
- `git commit` / `git push` — commit Faz 06'nın işi.
- Env değişkeni tablosu, endpoint referans tablosu, rol matrisi: Quickstart
  bunları taşımaz, linkler.

## Kabul ölçütü

- Dört dosya var; `Home.md` artık placeholder cümlesini taşımıyor.
- `_Sidebar.md` 30 satırın altında ve 12 sayfanın hepsini (Home + 11 içerik sayfası; `_Sidebar` ve `_Footer` kendileri listelenmez) listeliyor.
- `Home.md`'deki görev tablosu, `00-plan.md`'deki sayfa setinin tamamını
  kapsıyor — listede olup tabloda olmayan sayfa yok.
- `Quickstart.md`'deki her komut depodaki gerçeğe uyuyor: port `8765`, yollar
  `/admin/api/login`, `/admin/api/models`, `/admin/api/projects`,
  `/admin/api/rag-stores`, `/v1/chat/completions`; key önekleri `sk-proj-`.
  Doğrulama `README.md` ve `docs/api.md` okunarak yapılır, koşturarak değil.
- Wiki'de tek bir env değişkeni tablosu yok.
- Sayfalar arası her link, `00-plan.md`'deki dosya adlarıyla birebir aynı
  yazımı kullanıyor (`Deployment-Recipes`, `Provider-Setup`,
  `Client-Integrations`, `RAG-Cookbook`, `Architecture-Overview`,
  `Operations-Runbook`, `Security-Hardening`, `Troubleshooting`, `FAQ`,
  `Contributing-and-Development`).

## Notlar

Kaynaklar: `Ragmux/README.md` (Quick start, Dashboard, Documentation
bölümleri), `Ragmux/docs/api.md` (first-run setup, session, models, projects,
rag-stores), `Ragmux/web/index.html` (sekme adları, satır 269-278).

Dashboard tek sayfalık gömülü bir SPA; build adımı yok, kendi fontlarını
taşıyor ve sıkı bir CSP altında koşuyor — üçüncü parti istek yapmıyor. Tur
bölümünde bir cümleyle anılabilir.
