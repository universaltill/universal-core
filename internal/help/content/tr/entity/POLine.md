---
title: Sipariş Satırı
audience: user
module: purchasing
order: 3
---

Sipariş Satırı, sipariş edilen bir ürünü, miktarını ve fiyatını temsil
eder — bir Satın Alma Siparişi'nin Kalemler bölümünün bir satırıdır. Her
Satın Alma Siparişi'nin gerçekten bir şey sipariş edebilmesi için en az
bir tanesine ihtiyacı vardır.

## Ne zaman kullanılır

Neredeyse her zaman bir Satın Alma Siparişi'nin kendi **Kalemler**
bölümünden eklenir, sipariş edilen her ürün için bir satır. Tek başına
da oluşturulabilir veya düzenlenebilir — toplu içe aktarma için
kullanışlıdır — ancak ait olacağı bir Satın Alma Siparişi olmadan bir
anlamı yoktur.

## Satır ekleme

1. Bir Satın Alma Siparişi'nin Kalemler bölümünden yeni bir satır ekleyin
   (veya **Sipariş Satırı**'na gidip **Yeni**'yi seçin, ardından ait
   olduğu Satın Alma Siparişi'ni seçin).
2. **Ürün**'ü seçin.
3. **Miktar** ve **Birim Fiyat**'ı girin.
4. **Satır Toplamı**'nı kendiniz girin — sizin için Miktar × Birim
   Fiyat'tan hesaplanmaz.
5. Kaydedin.

Üst Satın Alma Siparişi'nin kendi Toplamı, o sipariş bir sonraki açılıp
kaydedildiğinde her satırın Satır Toplamı'ndan yeniden hesaplanır.

## Bilinmesi gereken kurallar

- Satın Alma Siparişi, Ürün, Miktar ve Birim Fiyat'ın hepsi zorunludur.
- Miktar ve Birim Fiyat negatif olamaz.
- Miktar × Birim Fiyat'a uymayan bir Satır Toplamı'nı hiçbir şey
  engellemez — bu, doğru doldurmanıza güvenilen ayrı bir alandır.

## Neyle bağlantılı

Her Sipariş Satırı bir **Satın Alma Siparişi**'ne aittir ve bir
**Ürün**'e referans verir. Bir **Mal Kabul Satırı**, karşısında teslim
alındığı belirli Sipariş Satırı'na referans verir ve onun Birim Fiyatı,
hem bir mal kabulün defter kaydında hem de bir Tedarikçi Faturası'nın
eşleştirmesinde kullanılır.
