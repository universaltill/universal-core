---
title: Para Birimi
audience: both
module: foundation
order: 12
---

Para Birimi, işletmenizin kullandığı bir para birimidir — kodu, adı ve
pratikte kaç ondalık basamak kullandığı (kuruş için 2 gibi, alt birimi).
Kiracınızdaki bir Para Birimi, temel para birimi olarak işaretlenir: diğer
her şeyin nihayetinde ölçüldüğü birim.

## Ne zaman kullanılır

İşletmenizin ticaret yaptığı, fatura kestiği veya raporladığı her para
birimi için bir Para Birimi kurun. Çoğu kiracı bunu yalnızca bir kez,
erkenden kurar.

## Para birimi oluşturma

1. **Para Birimi**'ne gidin ve **Yeni**'yi seçin.
2. Para birimi kodunu (standart üç harfli kodu, ör. "USD" veya "QAR") ve
   adını girin.
3. Alt birimi ayarlayın — kaç ondalık basamak kullandığını (çoğu para
   birimi için 2, pratikte alt birimi olmayan para birimleri için 0).
   Varsayılan olarak 2'dir.
4. Bu, kiracınızın temel para birimiyse **Temel Para Birimi** olarak
   işaretleyin.
5. Kaydedin.

## Bilinmesi gereken kurallar

- Kod ve ad zorunludur. Alt birim 0 ile 6 arasında olmalıdır.
- Kod benzersiz olmalıdır — sistem, başka bir Para Birimi tarafından zaten
  kullanılan bir kodu yeniden kullanan ikinci bir Para Birimini reddeder.
- Tam olarak bir Para Biriminin **Temel Para Birimi** olarak işaretlenmesi
  gerekir. Sistem ikincisini kesin olarak engellemez, bu yüzden bunu bir
  yönetimsel kural olarak görün — temel para birimine dayanan özellikler
  (defter senkronizasyonu ve yasal dışa aktarım gibi), durum belirsizse
  tahmin yürütmek yerine güvenli bir şekilde geri çekilir.
- Bir Para Birimi kaydı kendi başına döviz kurları taşımaz — bunlar ayrı,
  tarihli **Döviz Kuru** kayıtlarıdır, çünkü kurlar günlük değişirken bir
  para biriminin kodu ve adı değişmez.

## Neyle bağlantılıdır

**Döviz Kuru** kayıtları bir Para Birimi çiftine referans verir. Finans,
Satış veya Satın Alma etkinleştirildiğinde, belgelerdeki parasal alanlar
bir Para Birimine referans verir.
