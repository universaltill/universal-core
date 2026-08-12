---
title: Durum Türü
audience: both
module: foundation
order: 14
---

Durum Türü, bir kayıt türünün yaşam döngüsünü adlandırır — örneğin, "Satın
Alma Siparişi Durumu." Bir kayıt türünün, "taslak"tan "iptal edildi"ye
arada hiçbir şey olmadan doğrudan atlamasına izin veren bir durum alanı
yerine, gerçek ve uygulanan bir durum kümesine ve aralarındaki yasal
hareketlere sahip olmasını sağlayan üç parçalı bir mekanizmanın
(Durum Türü, **Durum**, **Durum Geçişi**) en üstündedir.

## Ne zaman kullanılır

Genellikle kendiniz bir Durum Türü oluşturmazsınız — bu mekanizmayı
kullanan çoğu kayıt türü (Satın Alma etkinleştirildiğinde satın alma
siparişleri ve tedarikçi faturaları gibi), modül kiracınız için
etkinleştirildiğinde zaten Durum Türü, Durum ve Durum Geçişi kayıtlarıyla
birlikte otomatik olarak gelir. Yeni bir Durum Türü yalnızca siz veya bir
entegrasyon, kendi yaşam döngüsüne ihtiyaç duyan tamamen yeni bir kayıt
türü tanıtıyorsa eklersiniz.

## Kurulum

1. **Durum Türü**'ne gidin ve **Yeni**'yi seçin.
2. Yönettiği kayıt türünü ve yaşam döngüsü için bir kod ve ad girin
   (örneğin kod "purchase_order_status", ad "Satın Alma Siparişi
   Durumu").
3. Kaydedin, ardından tek tek **Durum** değerlerini ve bunları birbirine
   bağlayan **Durum Geçişi** satırlarını ekleyin.

## Bilinmesi gereken kurallar

- Tek başına bir Durum Türü hiçbir şey yapmaz — bir kayıt türü, ancak
  özellikle bunu kullanacak şekilde inşa edildiğinde ve kiracınız için
  Durum ile Durum Geçişi kayıtları gerçekten var olduğunda uygulanan durum
  davranışı kazanır. Eksikseler, o türden bir kayıt oluşturmak veya
  güncellemek, durum yaşam döngüsünün "bu kiracı için yayımlanmadığını"
  söyleyen bir hatayla reddedilir.
- Burada seçtiğiniz kod, bir kayıt türünün kendi tanımının bu yaşam
  döngüsüne katılmak için adlandırdığı şeydir — tam olarak eşleşmelidir.

## Neyle bağlantılıdır

Her **Durum**, bir Durum Türüne aittir. Her **Durum Geçişi**, parçası
olduğu grafiğin ait olduğu Durum Türünü adlandırır.
