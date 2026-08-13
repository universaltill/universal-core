---
title: Stok Transferi
audience: user
module: purchasing
order: 13
---

Stok Transferi, bir ürünün bir Tesis'ten diğerine taşınmasını kaydeder —
bir konumdan ayrılmış ama diğerine henüz varmamış ara durum da dahil.

## Ne zaman kullanılır

İki tesis arasında stok taşırken bir Stok Transferi oluşturun — bir
depodan bir mağazaya veya iki depo arasında — ve özellikle taşıma
gerçek zaman alıyorsa bunu anlık bir düzenlemeden fazlası olarak takip
etmek istediğinizde.

## Transfer kaydetme

1. **Stok Transferi**'ne gidin ve **Yeni**'yi seçin.
2. **Ürün**'ü, **Kaynak Tesis**'i ve **Hedef Tesis**'i seçin — bunlar
   iki farklı tesis olmalıdır.
3. **Miktar**'ı girin — sıfırdan büyük olmalıdır.
4. **Transfer Tarihi**'ni girin.
5. İsteğe bağlı olarak **Notlar** ekleyin.
6. Kaydedin. Bu, transferi **Taslak** olarak oluşturur.
7. Stok kaynak tesisten gerçekten ayrıldığında, Durum'u **Yolda**'ya
   taşıyın.
8. Hedefe vardığında, Durum'u **Teslim Alındı**'ya taşıyın.

## Bilinmesi gereken kurallar

- Ürün, Kaynak Tesis, Hedef Tesis, Miktar, Transfer Tarihi ve Durum'un
  hepsi zorunludur.
- Kaynak Tesis ve Hedef Tesis farklı olmalıdır — kendine bir transfer
  reddedilir.
- Miktar kesinlikle sıfırdan büyük olmalıdır.
- Normal yol Taslak → Yolda → Teslim Alındı'dır. İptal, Taslak veya
  Yolda durumundan mümkündür, ancak Teslim Alındı'dan sonra mümkün
  değildir — bu noktada stok gerçekten varmıştır ve bunu geri almak
  yeni, ters yönde bir transferdir.
- Bir Stok Transferi'ni kaydetmek, hangi durumda olursa olsun, şu anda
  tek başına herhangi bir Stok Kalemi miktarını borçlandırmaz veya
  alacaklandırmaz — kaynak ve hedef Tesis'teki miktarlar otomatik olarak
  ayarlanmaz. Buna güveniyorsanız, stok seviyeleri üzerindeki etkiyi
  şimdilik ayrı olarak takip edin.

## Neyle bağlantılı

Bir Stok Transferi bir **Ürün**'e ve iki **Tesis** kaydına — kaynak ve
hedef — referans verir.
