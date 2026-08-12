---
title: Devam Kaydı
audience: admin
module: hr
order: 3
---

Bir Devam Kaydı, bir istihdamın bir günlük devamıdır — çalışılan saatler
ve bu sayının nereden geldiği: bir kart okuyucu sistemi, içe aktarılmış
bir puantaj veya elle yapılan bir düzeltme.

## Ne zaman kullanılır

Birinin belirli bir günde kaç saat çalıştığını kaydetmek veya düzeltmek
için Devam Kaydı'nı kullanın. Bu, kimsenin onaylamadığı bir talep değil,
olan biteni kaydeden bir kayıttır — onay gerektiren izin için bunun
yerine **İzin Talebi**'ni kullanın.

## Devam kaydetme

1. **Devam Kaydı**'na gidin ve **Yeni**'yi seçin.
2. Bu devamın ait olduğu **Çalışan**ı (belirli istihdamı) ve **Tarih**i
   seçin.
3. **Çalışılan Saat**i girin.
4. Bu sayının gerçekte nereden geldiğini kaydeden **Kaynak**ı seçin —
   Kart Okuyucu, Puantaj veya Manuel.
5. Elle yapılan bir düzeltmeyi açıklamak için yararlı olan isteğe bağlı
   **Notlar** ekleyin.
6. Kaydedin.

## Bilinmesi gereken kurallar

- Çalışan, Tarih, Çalışılan Saat ve Kaynak zorunludur. Kaynak formda
  **Puantaj** olarak önceden doldurulur, ancak yine de zorunlu bir
  alandır — başka bir yoldan (içe aktarma, API) kaynak belirtilmeden
  oluşturulan bir kayıt, sessizce varsayılana düşmek yerine reddedilir.
- **Çalışan başına günde yalnızca bir Devam Kaydı'na izin verilir** —
  aynı Çalışan ve Tarih için ikinci bir satır kaydetmek reddedilir.
  Mevcut bir günü, başka bir tane eklemek yerine kaydını düzenleyerek
  düzeltin.
- Çalışılan Saat 0 ile 24 arasında olmalıdır.
- Burada bir onay adımı ve durum alanı yoktur — bu varlığın, İzin
  Talebi'nin aksine bir yaşam döngüsü yoktur. Bir olguyu kaydeder, ve
  yanlış bir olguyu düzeltmek mevcut satırı düzenlemek anlamına gelir.

## Neyle bağlantılı

Her Devam Kaydı, karşısında olduğu **Çalışan**a (belirli istihdama)
referans verir. Çalışan formunun kendisinde ilişkili bir liste olarak
gösterilmez — bunun yerine Çalışana göre filtrelenmiş kendi listesinden
arayın.
