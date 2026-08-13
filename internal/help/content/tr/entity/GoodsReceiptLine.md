---
title: Mal Kabul Satırı
audience: user
module: purchasing
order: 5
---

Mal Kabul Satırı, belirli bir Sipariş Satırı'na karşı teslim alınan bir
ürünü ve miktarını temsil eder — bir Mal Kabul'ün Kalemler bölümünün bir
satırı ve stoğu gerçekten alacaklandıran ve deftere işleyen kayıttır.

## Ne zaman kullanılır

Neredeyse her zaman bir Mal Kabul'ün kendi **Kalemler** bölümünden
eklenir, o teslimatta gerçekten teslim alınan her ürün için bir satır.

## Satır ekleme

1. Bir Mal Kabul'ün Kalemler bölümünden yeni bir satır ekleyin.
2. Bu teslimatın karşısında olduğu **Sipariş Satırı**'nı ve **Ürün**'ü
   seçin (normalde o satırın kendi ürünüyle aynı).
3. **Teslim Alınan Miktar**'ı girin.
4. İsteğe bağlı olarak, geleni denetlerseniz, **Kabul Edilen Miktar** ve
   **Reddedilen Miktar**'ı girin — ne kadarının denetimi geçtiğini ve ne
   kadarının geçmediğini. Bu teslimat için bir kalite ayrımı
   kaydetmiyorsanız ikisini de boş bırakın.
5. Kaydedin. Bu, stoğu hemen alacaklandırır ve deftere işler — tam olarak
   ne olduğu için Mal Kabul'ün kendi konusuna bakın.

## Bilinmesi gereken kurallar

- Mal Kabul, Sipariş Satırı, Ürün ve Teslim Alınan Miktar'ın hepsi
  zorunludur. Teslim Alınan Miktar negatif olamaz.
- Kabul Edilen Miktar ve Reddedilen Miktar'ın ikisi de ayarlanmalı ya da
  ikisi de boş bırakılmalıdır — birini diğeri olmadan kaydetmek
  reddedilir.
- İkisi de ayarlandığında, Kabul Edilen Miktar + Reddedilen Miktar,
  Teslim Alınan Miktar'a eşit olmalıdır. Bu kontrolü geçemeyen bir satır,
  sonraki bir düzenlemede de dahil olmak üzere doğrudan reddedilir —
  sayıları düzeltin ve tekrar kaydedin.
- Bir satırın miktarlarını ilk kaydedildikten sonra düzenlemek, defterde
  hiçbir şeyi yeniden işlemez veya geri almaz. Bir düzenlemede yalnızca
  kalite kontrolü (kabul edilen + reddedilen, teslim alınana eşit
  olmalıdır) yeniden kontrol edilir.

## Neyle bağlantılı

Bir Mal Kabul Satırı bir **Mal Kabul**'e aittir ve karşısında teslim
alındığı **Sipariş Satırı**'na ve **Ürün**'e referans verir. Birini
kaydetmek, o ürün için mal kabulün Tesisi'ndeki **Stok Kalemi**'ni
alacaklandırır ve referans verilen Sipariş Satırı'nın Birim Fiyatı'nı
kullanarak bir yevmiye kaydı işler.
