---
title: Satın Alma Siparişi
audience: user
module: purchasing
order: 2
---

Satın Alma Siparişi, bir tedarikçiye verilen kesinleşmiş bir siparişi
kaydeder — işletmenizin ne almayı, kimden ve hangi fiyattan kabul
ettiğini. Satış Siparişi'nin satın alma tarafındaki karşılığıdır: ne
sattığınızı değil, ne aldığınızı kaydeder.

## Ne zaman kullanılır

Bir tedarikçiden satın almaya karar verdiğinizde bir Satın Alma Siparişi
oluşturun, henüz fiyat karşılaştırırken değil — birden fazla tedarikçiden
hâlâ fiyat topluyorsanız önce bir **Teklif Talebi** kullanın.

## Satın alma siparişi oluşturma

1. **Satın Alma Siparişi**'ne gidin ve **Yeni**'yi seçin.
2. Bir **Sipariş Numarası** girin — bu sipariş için kendi referansınız.
   Oluşturduğunuz her Satın Alma Siparişi arasında benzersiz olmalıdır.
3. **Tedarikçi**'yi seçin — burada yalnızca zaten tedarikçi rolüne sahip
   Taraf kayıtları seçilebilir; yalnızca müşteri olan bir Taraf burada
   görünmez.
4. **Sipariş Tarihi**'ni ve isteğe bağlı olarak **Söz Verilen Teslimat
   Tarihi**'ni girin (tedarikçinin sipariş anında taahhüt ettiği tarih) —
   bu, Sipariş Tarihi'nden önce olamaz.
5. Tedarikçi varsayılan para biriminizden farklı bir para biriminde işlem
   yapıyorsa bir **Para Birimi** seçin.
6. **Kalemler** bölümünde satır ekleyin: her ürün için miktar ve birim
   fiyat. **Satır Toplamı sizin için hesaplanmaz** — her satır için bunu
   kendiniz girin.
7. Kaydedin.

**Toplam** alanı, formu her açtığınızda tüm satırların Satır Toplamı
değerlerinin toplamından otomatik olarak yeniden hesaplanır — satırları
elle toplamanıza gerek yoktur, ancak yeniden hesaplanan toplamın
kaydedilmesi için satırları ekledikten sonra en az bir kez kaydetmeniz
gerekir.

## Teslim Süresi Aşamaları

**Teslim Süresi Aşamaları** bölümü, bu siparişin tedarik sürecinde
gerçekte hangi aşamalardan geçtiğini kaydeder — Tedarik Edildi, Üretim
Başladı, Üretim Hazır, Sevk Edildi, Gümrükten Çekildi ve Teslim Alındı.
Hepsi isteğe bağlıdır ve elle girilir, her biri kendinden önceki
aşamadan daha erken olamaz ve yolda olan bir siparişte normalde bunların
yalnızca bir başlangıç kısmı doldurulmuş olur. Bunlar satın alma
raporunun tedarik süresi rakamlarını besler; Teslim Alındı dahil hiçbiri
sizin için otomatik olarak doldurulmaz — burada bir aşama kaydetmek,
aşağıdaki siparişin Durum'unu tek başına değiştirmez.

## Bilinmesi gereken kurallar

- Durum **Taslak** ile başlar ve yalnızca bir Durum Geçişi'nin izin
  verdiği yolda ilerleyebilir: Taslak → Gönderildi → Onaylandı → Teslim
  Alındı normal yoldur. İptal, Taslak, Gönderildi veya Onaylandı
  durumundan mümkündür, ancak sipariş Teslim Alındı durumuna geçtikten
  sonra mümkün değildir — bu noktada mallar gerçekten teslim alınmıştır
  ve bunu geri almak bir durum düzenlemesi değil, bir iadedir.
- Sipariş Numarası, Tedarikçi, Sipariş Tarihi ve Durum'un hepsi
  zorunludur ve Sipariş Numarası benzersiz olmalıdır.
- Tedarikçi, kendi Taraf kaydında tedarikçi rolüne sahip olmalıdır.
  Hiçbir zaman tedarikçi olarak işaretlenmemiş bir Taraf seçmek
  reddedilir.
- Toplam negatif olamaz.
- Bir Satın Alma Siparişi'ni burada Teslim Alındı olarak işaretlemek
  yalnızca bir durum değişikliğidir — tek başına fiziksel olarak neyin
  geldiğini kaydetmez. Bunun için **Mal Kabul**'ü kullanın; tek bir
  sipariş genellikle birden fazla teslimatla teslim alınır.

## Neyle bağlantılı

Her Satın Alma Siparişi'nin bir veya daha fazla **Sipariş Satırı**
satırı vardır (Kalemler bölümü). Bir **Mal Kabul**, karşısında fiziksel
teslimatları kaydeder ve bir **Tedarikçi Faturası**, karşısında fatura
keser ve gerçekte teslim alınanla eşleştirilir. **Tedarikçi**, tedarikçi
rolüne sahip bir Taraf'tır. Bir Satın Alma Siparişi, hangi durumda
olursa olsun, formun "UBL dosyasını indir" eylemiyle bir UBL sipariş
belgesi olarak dışa aktarılabilir. Satın alma raporu (Raporlar altında),
her Satın Alma Siparişi genelinde tedarikçi harcamasını ve tedarik
sürelerini özetler.
