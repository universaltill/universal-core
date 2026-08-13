---
title: Tedarikçi Faturası
audience: user
module: purchasing
order: 6
---

Tedarikçi Faturası, bir Satın Alma Siparişi'ne karşı bir tedarikçiden
alınan bir faturadır — Müşteri Faturası'nın satın alma tarafındaki
karşılığıdır. Ödenebilmesi için, toplamı gerçekte teslim alınanla
karşılaştırılır.

## Ne zaman kullanılır

Zaten en azından kısmen teslim aldığınız bir Satın Alma Siparişi için bir
tedarikçiden fatura geldiğinde bir Tedarikçi Faturası oluşturun. Taslak
durumunda başlar, henüz hiçbir şey kontrol edilmemiştir.

## Fatura oluşturma ve eşleştirme

1. **Tedarikçi Faturası**'na gidin ve **Yeni**'yi seçin.
2. Bir **Fatura No** girin ve karşısında fatura kesildiği **Satın Alma
   Siparişi**'ni seçin.
3. **Tedarikçi**'yi seçin — bu, Satın Alma Siparişi'nin kendi
   tedarikçisiyle aynı olmalıdır, aksi halde eşleştirme bunu reddeder
   (aşağıya bakın).
4. **Fatura Tarihi**'ni ve gerekiyorsa **Para Birimi**'ni girin.
5. **Toplam**'ı girin.
6. Kaydedin — bu, faturayı **Taslak** olarak oluşturur.
7. Teslim alınanla karşılaştırmaya hazır olduğunuzda Durum'u
   **Eşleştirildi**'ye taşıyın ve kaydedin.

## Eşleştirildi'ye taşımak gerçekte ne yapar

Durum Eşleştirildi olarak ayarlanmış şekilde kaydettiğiniz anda, sistem
iki şeyi kontrol eder: faturanın Tedarikçisi'nin gerçekten Satın Alma
Siparişi'nin kendi tedarikçisi olup olmadığını ve faturanın Toplamı'nın,
o Satın Alma Siparişi'ne karşı gerçekten teslim alınan her şeyin toplam
değeriyle (her Mal Kabul Satırı'nın miktarı çarpı kendi satırının Birim
Fiyatı) kuruşuna kadar uyuşup uyuşmadığını.

İkisi de uyuşuyorsa, fatura Eşleştirildi durumuna geçer. **Uyuşmuyorsa,
fatura Taslak'ta kalmaz ve kaydetme reddedilmez — bunun yerine Eşleşme
İstisnası'na geçer**, ve **Eşleşme İstisnası Nedeni** alanı nedeniyle
doldurulur (yanlış tedarikçi, henüz hiçbir şey teslim alınmamış, Satın
Alma Siparişi'nde hiç satır yok veya uyuşmayan bir değer). Eşleşme
İstisnası, ödenme yolunda kaçınmanız gereken bir hata durumu değil,
normal, beklenen bir duraktır.

Bu kontrol, Durumu Eşleştirildi'ye çözümlenen bir faturayı her
kaydettiğinizde yeniden çalışır — zaten Eşleştirildi durumundaki bir
faturanın Toplamı'nı düzenlemek de dahil, bu düzenleme artık
uyuşmuyorsa faturayı yeniden Eşleşme İstisnası'na itebilir.

## Eşleşme İstisnası'ndan çıkmak

Nedenin açıkladığı şeyi düzeltin — Toplam'ı düzeltin, eksik Mal
Kabul'ün kaydedilmesini bekleyin veya Tedarikçi'yi doğrulayın — ve Durum
Eşleştirildi olarak ayarlanmış şekilde tekrar kaydedin. Bu sefer
uyuşursa, neden temizlenir ve fatura Eşleştirildi durumuna geçer.

**Eşleşme İstisnası'ndan Ödendi'ye doğrudan bir yol yoktur.** Bu bir
gözden kaçırma değildir — çözülmemiş bir faturanın ödendi olarak
işaretlenmesini durduran gerçek kontroldür. Önce istisnayı çözün.

## Bilinmesi gereken kurallar

- Fatura No, Satın Alma Siparişi, Tedarikçi, Fatura Tarihi ve Durum'un
  hepsi zorunludur.
- Toplam negatif olamaz.
- Normal yol Taslak → Eşleştirildi → Ödendi'dir. İptal durumuna
  Taslak, Eşleştirildi veya Eşleşme İstisnası'ndan ulaşılabilir, ancak
  Ödendi'den değil — para gerçekten el değiştirdikten sonra bunu geri
  almak bir durum düzenlemesi değil, bir alacak dekontudur.
- Eşleştirme tek başına deftere hiçbir şey işlemez — Stok'a borç / Borç
  Hesapları'na alacak kaydı, mallar teslim alındığında zaten
  işlenmiştir (bkz. Mal Kabul). Eşleştirme bir kontroldür, ikinci bir
  işleme değil.
- Durum'u Ödendi'ye taşımak şu anda bir nakit tahsilat yevmiye kaydı
  işlemez — buna güveniyorsanız, gerçek ödemeyi şimdilik sistemin
  dışında takip edin.

## Neyle bağlantılı

Bir Tedarikçi Faturası, karşısında fatura kestiği **Satın Alma
Siparişi**'ne ve gönderen **Tedarikçi**'ye referans verir ve o siparişe
karşı kaydedilen her **Mal Kabul Satırı**'na göre kontrol edilir.
