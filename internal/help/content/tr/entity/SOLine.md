---
title: Satış Sipariş Kalemi
audience: user
module: sales
order: 2
---

Satış Sipariş Kalemi, bir Satış Siparişi üzerindeki tek bir kalemdir — bir
ürün, bir miktar ve bir fiyat. Bir Satış Siparişi'nde genellikle
bunlardan birkaç tane bulunur; birlikte müşterinin gerçekte satın aldığı
şeyi oluştururlar.

## Ne zaman kullanılır

Bunları genellikle bağımsız bir kayıt olarak değil, Satış Siparişi
formunun Kalemler bölümünden eklersiniz — ama Satış Sipariş Kalemi ayrıca
bağımsız olarak listelenebilir ve içe aktarılabilir (örneğin CSV ile),
toplu sipariş girişi için bu daha uygun olduğunda.

## Kalem ekleme

1. Bir Satış Siparişi formunun Kalemler bölümündeki ekleme eylemini
   kullanın (veya **Satış Sipariş Kalemi**'ne gidip **Yeni**'yi seçin ve
   ardından Satış Siparişi'ni seçin).
2. Satılan **Ürün**'ü seçin.
3. **Miktar** ve **Birim Fiyat**'ı girin.
4. **Kalem Toplamı**'nı kendiniz girin — sistem miktarı birim fiyatla
   sizin için çarpmaz.
5. Kaydedin. Ardından Satış Siparişi formunu yeniden açtığınızda Toplamı
   bu kalemi otomatik olarak yansıtacaktır.

## Bilinmesi gereken kurallar

- Miktar, Birim Fiyat ve Kalem Toplamı negatif olamaz.
- Satış Siparişi ve Ürün her ikisi de zorunludur — referans vereceği bir
  sipariş veya ürün olmayan bir kalem reddedilir.
- Kalem Toplamı hesaplanan bir alan değil, sizin girdiğiniz düz bir
  sayıdır. Miktarı veya fiyatı sonradan değiştirirseniz, Kalem Toplamı'nı
  da buna uyacak şekilde kendiniz güncelleyin.

## Neyle bağlantılı

Her Satış Sipariş Kalemi bir **Satış Siparişi**'ne aittir ve bir
**Ürün** adlandırır. Üst Satış Siparişi'nin
Kalemler bölümü, form her açıldığında her satırın Kalem Toplamı'nı
siparişin kendi Toplam alanında toplar.
