---
title: Satış Siparişi
audience: user
module: sales
order: 1
---

Satış Siparişi, bir müşteriden gelen kesinleşmiş siparişi kaydeder — ne
almayı kabul ettiğini, hangi fiyattan ve ne zaman. Satın Alma
Siparişi'nin satış tarafındaki karşılığıdır: işletmenizin ne aldığını
değil, ne sattığını kaydeder.

## Ne zaman kullanılır

Müşterinin siparişi kesinleştiği anda bir Satış Siparişi oluşturun, henüz
görüşme aşamasındayken değil — ayrı bir "teklif" aşaması henüz yok, bu
yüzden Taslak durumundaki bir Satış Siparişi, çalışma tahminine en yakın
şeydir.

## Satış siparişi oluşturma

1. **Satış Siparişi**'ne gidin ve **Yeni**'yi seçin.
2. Bir Satış Sipariş No girin (bu sipariş için kendi referansınız).
3. **Müşteri**'yi seçin — burada yalnızca zaten müşteri rolüne sahip Taraf
   kayıtları seçilebilir; yalnızca tedarikçi olan bir Taraf burada
   görünmez.
4. Sipariş Tarihi'ni ve bu müşteri varsayılan para biriminizden farklı
   bir para biriminde işlem yapıyorsa Para Birimi'ni girin.
5. **Kalemler** bölümünde satır ekleyin: her ürün için miktar ve birim
   fiyat. **Kalem Toplamı sizin için hesaplanmaz** — her satır için bunu
   kendiniz girin (fiyatlandırma şekliniz buysa miktar × birim fiyat).
6. Kaydedin.

**Toplam** alanı, formu her açtığınızda tüm satırların Kalem Toplamı
değerlerinin toplamından otomatik olarak yeniden hesaplanır — satırları
elle toplamanıza gerek yoktur, ancak yeniden hesaplanan toplamın
kaydedilmesi için satırları ekledikten sonra en az bir kez kaydetmeniz
gerekir.

## Bilinmesi gereken kurallar

- Satış Sipariş No, Müşteri, Sipariş Tarihi ve Durum'un hepsi zorunludur.
- Durum **Taslak** ile başlar ve yalnızca bir Durum Geçişi'nin izin
  verdiği yolda ilerleyebilir: Taslak → Onaylandı → Tamamlandı →
  Faturalandı normal yoldur. İptal, Taslak veya Onaylandı durumundan
  mümkündür, ancak sipariş Tamamlandı veya Faturalandı durumuna geçtikten
  sonra mümkün değildir — bu noktada gerçekten bir şey sevk edilmiş veya
  faturalandırılmıştır ve bunu geri almak bir durum düzenlemesi değil,
  bir iade veya alacak dekontudur.
- Müşteri, kendi Taraf kaydında müşteri rolüne sahip olmalıdır. Hiçbir
  zaman müşteri olarak işaretlenmemiş bir Taraf seçmek reddedilir.
- Toplam negatif olamaz.
- Bir Satış Siparişi'ni kaydetmek tek başına deftere hiçbir şey
  işlemez — bir **Müşteri Faturası** buna karşı kesilene kadar hiçbir
  şey borçlanılmaz (bu işlemin gerçekte ne yaptığı için o konuya bakın).

## Neyle bağlantılı

Her Satış Siparişi'nin bir veya daha fazla **Satış Sipariş Kalemi**
satırı vardır (Kalemler bölümü). Bir **Müşteri Faturası**, karşısında
fatura kestiği Satış Siparişi'ne referans verir. **Müşteri**, müşteri
rolüne sahip bir Taraf'tır. Bir Satış Siparişi, hangi durumda olursa
olsun, formun "UBL dosyası indir" eylemiyle bir UBL sipariş belgesi
olarak dışa aktarılabilir.
