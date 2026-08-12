---
title: Müşteri Faturası
audience: user
module: sales
order: 3
---

Müşteri Faturası, bir Satış Siparişi'ne karşı müşteriye gönderilen
faturadır. Satış Siparişi neyin kabul edildiğini kaydeder; Müşteri
Faturası ise bunun karşılığında gerçekten ödeme talep eden belgedir — ve
kesildiği andan itibaren gerçek muhasebe sonuçları olan taraf odur.

## Ne zaman kullanılır

Bir Satış Siparişi'ni faturalandırmaya hazır olduğunuzda bir Müşteri
Faturası oluşturun — genellikle sipariş sevk edildikten veya iş
tamamlandıktan sonra, ancak sistemde bu sırayı zorunlu kılan hiçbir şey
yoktur. Bir tahmin gibi henüz hiçbir mali etkisi olmayan Taslak
durumunda başlar.

## Fatura oluşturma ve kesme

1. **Müşteri Faturası**'na gidin ve **Yeni**'yi seçin.
2. Bir Fatura No girin ve karşısında fatura kesilecek **Satış
   Siparişi**'ni seçin — Müşteri normalde o siparişinkiyle aynıdır, ancak
   bunu sizin için otomatik dolduran bir mekanizma yoktur.
3. Fatura Tarihi'ni ve gerekiyorsa Para Birimi'ni girin.
4. Toplam'ı girin.
5. Kaydedin — bu, faturayı **Taslak** olarak oluşturur. Henüz deftere
   hiçbir şey işlenmemiştir.
6. Göndermeye hazır olduğunuzda Durumu **Kesildi**'ye taşıyın ve tekrar
   kaydedin. Gerçek muhasebe etkisi olan adım budur (aşağıya bakın).

## Bir faturayı kesmek gerçekte ne yapar

Bir faturanın durumu Kesildi'ye taşındığı anda, sistem sizin için bir
yevmiye kaydı işler: fatura Toplamı için **Alacak Hesapları'na borç,
Satış Gelirine alacak**. Sade bir dille — işletmeye artık o para
borçludur (bir varlık olan Alacak Hesapları artar) ve o kadar gelir elde
edilmiştir (gelir artar); aynı olayın iki tarafıdır. Bu yalnızca fatura
başına bir kez gerçekleşir: zaten kesilmiş bir faturayı yeniden
kaydetmek ikinci kez işlem yapmaz.

Taslak bir fatura hiçbir şey işlemez — henüz mali bir taahhüt değildir,
yalnızca bir çalışma kopyasıdır.

**Durumu Ödendi'ye taşımak henüz *ne yapmaz***: bir faturayı Ödendi
olarak kaydetmek şu anda ikinci bir yevmiye kaydı işlemez (nakit tahsilat
tarafı — Nakit'e borç, Alacak Hesapları'na alacak — henüz kurulmamış,
gerçek gelecek iştir). Bu kaydın var olduğuna güveniyorsanız, gerçek
nakit tahsilatını şimdilik sistemin dışında takip edin.

## Bilinmesi gereken kurallar

- Durum **Taslak** ile başlar; normal yol Taslak → Kesildi → Ödendi'dir.
  **İptal** Taslak veya Kesildi durumundan ulaşılabilir, ancak Ödendi
  durumundan değil — para gerçekten el değiştirdikten sonra bunu geri
  almak bir durum düzenlemesi değil, bir alacak dekontu veya iadedir.
- Toplam negatif olamaz.
- Faturanın tarihi durumu Kapalı veya Kilitli olan bir **Dönem**'in
  içine düşüyorsa, faturayı kesmek doğrudan reddedilir — nedeni için
  Dönem'in kendi konusuna bakın.
- Satış Siparişi ve Müşteri her ikisi de zorunludur.
- Toplamı sıfır olan bir fatura, kesildiğinde hiçbir şey işlemez — işlenecek
  bir kayıt yoktur ve atlandığı konusunda hiçbir uyarı verilmez.
- Yevmiye kaydı her zaman kuruluşunuzun temel para biriminde, Toplam
  tam olarak girildiği şekilde işlenir — faturada farklı bir Para Birimi
  ayarlasanız bile hiçbir döviz kuru dönüşümü uygulanmaz. Temel para
  biriminizden farklı bir para biriminde fatura keserseniz, deftere
  işlenen tutar faturanın kendi para birimindeki kendi toplamıyla
  eşleşmeyecektir.

## Neyle bağlantılı

Her Müşteri Faturası, karşısında fatura kestiği **Satış Siparişi**'ne ve
faturayı kestiği **Müşteri**'ye referans verir. Satış Siparişi'nin kendi
Müşteri alanından farklı olarak, bu alan Taraf'ın gerçekten müşteri
rolüne sahip olup olmadığını kontrol etmez — burada herhangi bir Taraf
seçilebilir, bu yüzden dikkatli seçin. Kesmek, kiracınızın hesap
planının Alacak Hesapları ve Satış Geliri için kullandığı **Hesap**
kodlarına karşı bir Yevmiye Kaydı oluşturur — hesap planınız varsayılan
kodları kullanmıyorsa, kesme işlemi bunları bulamayacaktır; bkz. Hesap'ın
kendi konusu (yeni eklenen bir Hesabın ne kadar çabuk kullanılabilir hale
geldiğine dair gerçek bir kısıtlama dahil). Bir fatura, hangi durumda
olursa olsun, formun "UBL dosyası indir" eylemiyle bir UBL fatura belgesi
olarak dışa aktarılabilir.
