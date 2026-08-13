---
title: Tesis
audience: user
module: purchasing
order: 11
---

Tesis, fiziksel veya mantıksal bir stok konumudur — bir depo, bir
mağaza zemini veya daha spesifik bir yerde takip etmediğiniz stok için
sanal bir kova. Envanterin gerçekte bulunduğu yerdir.

## Ne zaman kullanılır

İçine mal kabul etmeden, ona veya ondan stok transfer etmeden ya da
orada envanter takip etmeden önce bir Tesis kaydedin — bu modüldeki her
stok miktarı, tek başına bir Ürün'e değil, belirli bir Tesis'e bağlıdır.

## Tesis kaydetme

1. **Tesis**'e gidin ve **Yeni**'yi seçin.
2. Bir **Kod** (kendi referansınız) ve bir **Ad** girin.
3. **Tür**'ü seçin: Depo, Mağaza veya Sanal. Varsayılan olarak
   Depo'dur.
4. İsteğe bağlı olarak bir **Adres** seçin.
5. **Aktif**, varsayılan olarak açıktır.
6. Kaydedin.

## Bilinmesi gereken kurallar

- Kod, Ad ve Tür'ün hepsi zorunludur.
- Kod'un doğal, insan tarafından kullanılan bir referans olması
  amaçlanmıştır, ancak sistemde şu anda iki Tesis'in aynı Kod'u
  paylaşmasını engelleyen hiçbir şey yoktur.
- **Aktif**'i kapatmak, bir tesisi silmeden emekliye ayırmanın yoludur —
  yeni faaliyetler için seçicilerde görünmemesi gerekir, ancak zaten ona
  işaret eden herhangi bir stok geçmişi olduğu gibi kalır. Bu, o
  tesiste kaydedilen herhangi bir stoğu taşımaz veya temizlemez.

## Neyle bağlantılı

Bir **Mal Kabul**, malların hangi Tesis'e geldiğini kaydeder. Bir **Stok
Kalemi** satırı, belirli bir Ürün-ve-Tesis çiftindeki bir miktardır. Bir
**Stok Transferi**, stoğu bir Tesis'ten diğerine taşır.
