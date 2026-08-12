---
title: Ölçü Birimi
audience: both
module: foundation
order: 10
---

Ölçü Birimi, sistemin diğer bölümlerinin (envanter, satın alma, satış,
üretim) bir miktar kaydederken referans verdiği bir temel birimdir — adet,
kutu, kilogram, litre. Bir miktarın salt bir sayı olmak yerine her zaman
tanımlı, ortak bir anlama sahip olması için vardır.

## Ne zaman kullanılır

İşletmenizin sipariş ettiği, stokladığı veya sattığı her farklı birim için
bir Ölçü Birimi kurun. Çoğu kiracı bunu yalnızca bir kez, erkenden yapar
ve ardından her yerde aynı birim setine referans verir.

## Ölçü birimi oluşturma

1. **Ölçü Birimi**'ne gidin ve **Yeni**'yi seçin.
2. Kısa bir kod (örneğin "EA" veya "BOX") ve tam bir ad girin.
3. Kaydedin.

## Bilinmesi gereken kurallar

- Kod ve ad ikisi de zorunludur; yerleşik bir standart birim listesi
  yoktur — işletmenizin tam olarak ihtiyaç duyduğu birimleri siz
  tanımlarsınız.
- Tek başına bir Ölçü Birimi başka bir birime nasıl dönüştürüleceğini
  bilmez — bu ilişki ayrı olarak bir **Ölçü Birimi Dönüşümü** ile
  kaydedilir.

## Neyle bağlantılıdır

Bir **Ölçü Birimi Dönüşümü**, bir dönüşüm faktörüyle iki Ölçü Birimini
birbirine bağlar. Envanter, Satın Alma, Satış veya Üretim
etkinleştirildiğinde, kalemler ve sipariş satırları miktarları için bir
Ölçü Birimine referans verir.
