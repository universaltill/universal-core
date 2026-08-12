---
title: Ölçü Birimi Dönüşümü
audience: both
module: foundation
order: 11
---

Ölçü Birimi Dönüşümü, iki **Ölçü Birimi** kaydı arasındaki bir dönüşüm
faktörüdür — örneğin bir kutu on iki adete eşittir. Bu, sistemin bir
miktarı bir stoklama biriminden bir sipariş veya satış birimine ve geri
dönüştürebilmesini sağlayan şeydir.

## Ne zaman kullanılır

Birbirine dönüştürülmesi gereken iki biriminiz olduğunda — en yaygın
olarak bir toplu birim (bir kutu, bir palet) ve içerdiği tekil birim
arasında — bir Ölçü Birimi Dönüşümü oluşturun.

## Dönüşüm oluşturma

1. **Ölçü Birimi Dönüşümü**'ne gidin ve **Yeni**'yi seçin.
2. **Kaynak** birimi ve **Hedef** birimi seçin.
3. Faktörü girin — Kaynak birimdeki bir miktarı Hedef birimdeki eşdeğer
   miktarı elde etmek için çarptığınız sayı (faktörü 12 olan bir kutu, 12
   adete dönüşür).
4. Kaydedin.

## Bilinmesi gereken kurallar

- Faktör sıfır veya daha büyük olmalıdır; negatif bir faktör reddedilir.
- Kaynak→Hedef yönü, bu sistemin tutarlı bir şekilde izlemenizi beklediği
  bir kuraldır — dönüşümün başka bir yerde gerçekte nasıl kullanıldığına
  karşı çift kontrol edilmez, bu yüzden Kaynak ve Hedef birimlerini her
  seferinde aynı şekilde girin (önce toplu birim, sonra tekil birim yaygın
  bir kalıptır).
- Sıfır faktör, dönüştürülen her miktarı sıfıra indirgemesine rağmen
  teknik olarak kabul edilir — girdiğiniz değeri çift kontrol edin.

## Neyle bağlantılıdır

Bir Ölçü Birimi Dönüşümü iki **Ölçü Birimi** kaydına referans verir. Başka
bağlantısı yoktur — kalemler ve sipariş satırları Ölçü Birimlerine
doğrudan referans verir ve bir miktarın birimler arasında çevrilmesi
gerektiğinde bir Ölçü Birimi Dönüşümünün var olmasına güvenir.
