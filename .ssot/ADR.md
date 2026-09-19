# Mimari Kararlar

Her karar `## ADR-00N — <başlık>` biçiminde, numaralar artarak eklenir.
Bir karar geri alındığında silinmez; durumu `geri alındı (bkz. ADR-00M)`
olarak güncellenir.

Bir karar şunları taşır:

```markdown
## ADR-00N — <başlık>

- **Durum:** kabul edildi | geri alındı (bkz. ADR-00M) | önerildi
- **Tarih:** YYYY-MM-DD

### Bağlam
### Karar
### Sonuçları
### Değerlendirilen alternatifler
```

> Aşağıdaki kararlar dışında, depoda hâlihazırda alınmış görünen mimari kararlar
> vardır, ama bir ADR ancak kullanıcı kararı açıkça bildirdikten sonra buraya
> yazılır. Aday kararların listesi için kurulum çıktısına bakın.

---

## ADR-001 — `/readyz` eski replikayı yeni şemayla hazır sayar

- **Durum:** kabul edildi
- **Tarih:** 2026-09-19

### Bağlam

`/readyz`, uygulanmış migration sürümü ile binary'nin gömülü `head`
sürümünü **iki yönlü** karşılaştırıyordu. `maxSurge=1` bir rolling
update'te ilk yeni pod migration'ı uygular; o andan itibaren hâlâ trafik
taşıyan eski replikaların hepsi `applied > head` görüp 503 döner ve load
balancer hepsini rotasyondan çıkarır. Sıfır kesintili olması beklenen
deploy kesintiye dönüşür; rollback sırasında eski imaj hiç hazır olamaz.

### Karar

`applied < head` → 503 (`migrating`): binary'nin beklediği şema henüz
yok, bu replika trafik alamaz. `applied > head` → **200**, gövdede
`degraded` girdisiyle: şema binary'nin bildiğinden yeni, ama Ragmux
migration'ları ekleyici olduğu için eski kod çalışmayı sürdürebilir.

### Sonuçları

- Rolling update ve rollback filoyu düşürmeden akar.
- Eski kod, yeni şemayla bir süre trafik alır. Bu, migration'ların
  ekleyici kalması koşuluna bağlıdır: kolon düşüren ya da tip daraltan
  bir migration bu kararı geçersiz kılar ve ayrıca ele alınmalıdır.
- `degraded` girdisi operatöre durumu gösterir; `/readyz` 200 döndüğü
  için alarm kurmak operatörün işidir.
- `/healthz` (liveness) değişmez.

### Değerlendirilen alternatifler

Mevcut iki yönlü 503 davranışını korumak: en katı yorum, eski kod yeni
şemayla asla trafik almaz. Bedeli, `maxUnavailable=0` ile bile filonun
tamamının rotasyondan çıkması ve rollback'in imkânsızlaşmasıydı —
korunmak istenen riskten daha büyük bir kesinti üretiyordu.

## ADR-002 — Metrik seri tavanı dolduğunda seri düşer ve olay alarmdır

- **Durum:** kabul edildi
- **Tarih:** 2026-09-19

### Bağlam

`internal/metrics` kendi registry'sini taşıyor ve `MaxSeries` (varsayılan
5000) ile kardinaliteyi sınırlıyor. `admitSeries` tahliye yapmadığı için
tavan bir kez dolduğunda **bundan sonraki her yeni meşru seri kalıcı
olarak düşer**: yeni bir proje, yeni bir model, yeni bir maliyet serisi
bir daha görünmez. Fatura metrikleri sessizce donar.

### Karar

Tavan davranışı değişmez — yeni seri düşer — ama olay sessiz olmaktan
çıkar: `Error` seviyesinde loglanır ve düşen seri sayısı bir sayaçla
dışarı verilir. Girdinin kendisi ayrıca sınırlanır (bkz. Faz 02:
`method` ve `error.type` etiketleri kurulum büyüklüğüne bağlanır), böylece
tavana çarpmak olağan bir durum değil, bir hata durumu olur.

### Sonuçları

- Scrape yolu kilitsiz ve öngörülebilir kalır.
- Tavana çarpan bir kurulum bunu operatörün göreceği bir şekilde bildirir;
  çözüm `METRICS_MAX_SERIES`'i yükseltmek ya da kardinaliteyi azaltmaktır.
- Tavan dolduktan sonra eklenen seriler hâlâ kaybolur; bu bilinçli olarak
  kabul edilmiş bir kayıptır, artık sessiz değildir.

### Değerlendirilen alternatifler

LRU tahliye: fatura metriklerinin donmasını tümden engellerdi. İki
bedeli vardı — her gözlemde erişim zamanı yazmak scrape yoluna kilit ya
da atomik maliyeti getirir, ve tahliye edilip geri gelen bir sayaç
sıfırdan başlar. Prometheus bunu `rate()` içinde bir counter reset olarak
okur, yani sessizce yanlış grafik üretirdi. Sessiz yanlış veri, görünür
eksik veriden kötü kabul edildi.

## ADR-003 — `viewer` kendi gateway key'ini üretebilir

- **Durum:** kabul edildi
- **Tarih:** 2026-09-19

### Bağlam

`keys` rota grubu bilinçli olarak rolsüz: her kullanıcı kendi API
key'lerini yönetir. Sonuç olarak `viewer` rolündeki bir kullanıcı
`sk-user-…` üretip `/v1` üzerinden para harcayabiliyor.
`docs/users-and-limits.md` rol matrisi ise `viewer`'ı salt-okunur diye
tanımlıyordu. Kod ile doküman aynı şeyi söylemiyordu.

### Karar

Davranış korunur, doküman düzeltilir. Rol matrisi `viewer`'ın kendi
gateway key'ini üretebildiğini ve harcamasının kendi limitine yazıldığını
açıkça yazar. Roller **admin yüzeyine** erişimi sınırlar; gateway
kullanımı kullanıcının kendi limitiyle sınırlanır.

### Sonuçları

- Mevcut kurulumlarda hiçbir şey kırılmaz.
- `viewer`, kendisine açılmış proje ve key sub-limit'leri kadar harcama
  yapabilen bir kullanıcıdır. Harcamayı durdurmak isteyen operatörün
  araçları: key'i revoke etmek, key'e küçük bir sub-limit vermek, ya da
  hesabı deaktive etmek (`401 key_owner_inactive`). Bu yol dokümanda
  gösterilir.
- İki şey bu amaçla **işe yaramaz** ve dokümanda öyle yazılır: kullanıcının
  project membership'ini düşürmek (grant'lar membership'ten bağımsız
  yaşar, key çalışmaya devam eder) ve bir limiti `0` yapmak (bu kod
  tabanında `0` = **sınırsız** demektir, yani tavanı kaldırır).

### Değerlendirilen alternatifler

Key basmayı `editor` rolüne bağlamak: `viewer`'ı gerçekten salt-okunur
yapardı. Mevcut kurulumlarda viewer'ların key üretme yolunu kapatan bir
davranış değişikliğiydi ve rollerin anlamını (admin yüzeyi yetkisi)
gateway kullanımına genişletiyordu.

## ADR-004 — Fiyat tablosundan düşen satırlar tek seferlik migration'la silinir

- **Durum:** kabul edildi
- **Tarih:** 2026-09-19

### Bağlam

`prices.json` `ollama` ve `custom_openai` için `"*": 0/0` catch-all'ı
taşıyordu. Ücretli bir API'ye bakan bir `custom_openai` bağlantısı bu
yüzden `cost_source:"builtin"` ile 0,00 USD raporluyordu; doküman ise
"eşleşme yoksa `none`" diyordu. Catch-all'ı dosyadan çıkarmak yalnız yeni
kurulumlara ulaşır — mevcut kurulumlarda satır `model_prices` tablosunda
durmaya devam eder.

Uygulayan ajan bunu genel bir mekanizmayla çözmüştü: `Seed`, her
yükseltmede shipped tablodan düşen ne varsa siler. Review bu mekanizmada
veri kaybı yolu bulamadı (operatörün düzenlediği satır `source='user'`e
dönüşüyor, downgrade guard'lı), ama sözleşme kayması tespit etti.

### Karar

Genel mekanizma kaldırılır. Yerine yalnız `custom_openai/*` satırını
silen tek seferlik bir migration gelir; silme `source = 'builtin'`
satırlarıyla sınırlıdır. `docs/api.md`'nin "Built-in rows cannot be
deleted" sözleşmesi olduğu gibi kalır.

### Sonuçları

- Bugünkü kusur mevcut kurulumlarda da düzelir.
- Gelecekteki sürümler için genel bir silme yetkisi açılmaz: v0.5'te bir
  pattern yeniden adlandırılırsa eski satır tüm kurulumlarda sessizce yok
  olmaz.
- Silme, migration'ların zaten görünür ve tek seferlik olduğu yerde olur;
  operatör ne silindiğini migration dosyasında okuyabilir.
- Gelecekte gerçekten bir satırın emekliye ayrılması gerekirse, bu yine
  kendi migration'ıyla ve kendi kararıyla yapılır.

### Değerlendirilen alternatifler

**Genel `retireRows` mekanizması:** bakımı kolay, her sürümde kendiliğinden
çalışır. Bedeli, `prices.json`'ı düzenleyen herkese tüm kurulumlardan satır
silme yetkisi vermesi ve boş bir dosyanın tüm builtin satırları
silebilmesiydi. **Hiç silmemek:** düzeltme mevcut kurulumlara hiç ulaşmaz,
operatör satırı elle silmek zorunda kalır — yani sessiz yanlış maliyet
raporlaması sürer.

## ADR-005 — Görsel indirme doygunluğu 429'dur ve kiracı payıyla sınırlanır

- **Durum:** kabul edildi
- **Tarih:** 2026-09-19

### Bağlam

Görsel indirmede süreç geneli bir eşzamanlılık tavanı yoktu: bir istemci
isteği, keyfi bir hedefe gateway'in IP'sinden 8 eşzamanlı GET
ürettirebiliyordu. Tavan eklendi, ama iki seçim açıkta kaldı — doygunlukta
hangi durum kodunun döneceği ve tavanın kaç olacağı.

Ölçülen: tek bir istek slotları tek başına doyuramıyor (adapter'lar
görselleri sırayla çekiyor), ama 4 eşzamanlı istek yavaş bir görsel
host'una yöneldiğinde tüm süreci doyuruyor ve başka bir projenin görselli
isteği 10 sn asılı kalıp hata alıyor.

### Karar

Doygunlukta **429** dönülür, `Retry-After` başlığıyla ve
`image_fetch_saturated` koduyla. Tavan varsayılanı **16**'dır ve tek bir
key ya da proje slotların yarısından fazlasını tutamaz.

### Sonuçları

- Dışa doğru amplifikasyon kapalı kalırken içe doğru kiracılar-arası
  açlık da kapanır.
- 429 semantik olarak doğru olanı söyler: bu bir kaynak sınırlaması,
  sunucu arızası değil. OpenAI SDK'ları ≥500'ü üssel backoff ile otomatik
  retry ediyor; 503 seçilseydi tavan dolu kaldığı sürece her istek 3×
  kuyrukta bekleyecekti.
- `docs/api.md` durum kodu tablosuna yeni bir satır girer.
- Pay ayrımı biraz kod maliyeti getirir ve tek takımlı kurulumlarda hiç
  devreye girmez.

### Değerlendirilen alternatifler

**503 + `Retry-After`:** "geçici olarak kapasitem yok" demenin standart
yolu, ama SDK'ların ağır retry davranışını tetikliyordu. **Tavanı 4'te
bırakmak:** dışa doğru en sıkı koruma, ama ölçülen kiracılar-arası açlığı
kabul etmek demekti.
