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
- `viewer`, kişisel limiti kadar harcama yapabilen bir kullanıcıdır;
  harcamayı sıfırlamak isteyen operatör kullanıcının limitini sıfırlar ya
  da hesabı devre dışı bırakır. Bu yol dokümanda gösterilir.

### Değerlendirilen alternatifler

Key basmayı `editor` rolüne bağlamak: `viewer`'ı gerçekten salt-okunur
yapardı. Mevcut kurulumlarda viewer'ların key üretme yolunu kapatan bir
davranış değişikliğiydi ve rollerin anlamını (admin yüzeyi yetkisi)
gateway kullanımına genişletiyordu.
