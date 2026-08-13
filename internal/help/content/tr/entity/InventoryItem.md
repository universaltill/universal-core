---
title: Stok Kalemi
audience: user
module: purchasing
order: 12
---

Stok Kalemi, bir stok miktarıdır — belirli bir Tesis'te bir Ürün'den ne
kadarınızın elinizde olduğu ve ne kadarının taahhüt edilebilir olduğu.
Çoğu zaman bunları doğrudan kendiniz oluşturmazsınız; mallar teslim
alındıkça sizin için güncel tutulurlar.

## Ne zaman kullanılır

Nadiren birini elle oluşturmanız gerekir. Bir ürüne karşı bir **Mal
Kabul** kaydetmek, sizin için eşleşen Stok Kalemi'ni zaten oluşturur
veya günceller, onu teslim alınan miktar kadar alacaklandırır. Yalnızca
bir açılış bakiyesi ayarlamak veya bir miktarı elle düzeltmek için
doğrudan bir tane oluşturun veya düzenleyin.

## Miktar görüntüleme veya düzeltme

1. **Stok Kalemi**'ne gidin.
2. Aradığınız **Ürün** ve **Tesis** için satırı bulun (veya henüz var
   olmayan bir stok seviyesi kaydetmek için **Yeni**'yi seçin).
3. **Eldeki Miktar**, o tesiste gerçekte bulunan fiziksel miktardır.
   **Taahhüt Edilebilir Miktar**, yeni talebe taahhüt etmek için serbest
   olan miktardır — bu sistemde ikisi birlikte hareket eder, çünkü henüz
   hiçbir şey stoğu satış siparişlerine karşı ayrı olarak rezerve etmez.
4. Kaydedin.

## Bilinmesi gereken kurallar

- Ürün, Tesis, Eldeki Miktar ve Taahhüt Edilebilir Miktar'ın hepsi
  zorunludur.
- Eldeki Miktar negatif olamaz. Taahhüt Edilebilir Miktar olabilir —
  talep, eldeki ve gelmekte olan miktarı aştığında gerçek, anlamlı bir
  durumdur.
- Her (Ürün, Tesis) çiftinin tam olarak bir Stok Kalemi satırı olmalıdır
  — aynı çift için ikinci bir satır tamamen reddedilir. Mal kabuller,
  zaten stoğu olan bir çift için ikinci bir satır oluşturmak yerine her
  zaman mevcut satırı günceller.
- Tesis her zaman zorunludur — bu sistemde "konumsuz stok" diye bir şey
  yoktur; her miktar gerçek bir Tesis'e aittir.

## Neyle bağlantılı

Bir Stok Kalemi bir **Ürün**'e ve bir **Tesis**'e referans verir. Bir
**Mal Kabul Satırı**, teslim alındığında onu alacaklandırır. Aynı Ürün
için bir **Yeniden Sipariş Kuralı**, yeniden sipariş sinyali verip
vermeyeceğine karar vermek için pozisyonunu bu miktarla karşılaştırır.
