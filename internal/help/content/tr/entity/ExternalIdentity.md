---
title: Dış Kimlik
audience: admin
module: foundation
order: 24
---

Dış Kimlik, buradaki bir kaydın harici eski bir sistemdeki hangi kayıttan
geldiğini hatırlar — kaynağını, hangi kayıt türü olarak ortaya çıktığını
ve eski sistemin ona ait kendi anahtarını. Bu, tekrarlanan bir içe
aktarmanın, her seferinde bir kopya oluşturmak yerine son seferinde
oluşturduğu aynı kaydı güncellemesini sağlayan şeydir.

## Ne zaman kullanılır

Bunları asla kendiniz oluşturmaz veya düzenlemezsiniz — SQL veya CSV içe
aktarma sihirbazı aracılığıyla bir içe aktarma çalıştırmasını her
uyguladığında içe aktarma motoru tarafından otomatik olarak yazılır. Bu
kayıt türü için bir "Yeni" ekranı yoktur.

## Pratikte nasıl göründüğü

Her satır, geldiği kaynağı, okunduğu tam tablo veya görünümü, burada
ürettiği kayıt türü ve kaydı, ve eski sistemin ona ait kendi tanımlayıcı
anahtarını (örneğin eski sistemden bir müşteri numarası) kaydeder. Aynı
kaynaktan bir sonraki içe aktarmanızda aynı anahtarı tekrar
gördüğünüzde, içe aktarma ikinci bir kopya eklemek yerine zaten
oluşturduğu kaydı günceller.

## Bilinmesi gereken kurallar

- Kaynağın, geldiği tablo veya görünümün, kayıt türünün ve eski
  anahtarın kombinasyonu benzersiz olmalıdır — içe aktarma motoru,
  güncellenecek doğru kaydı bulmak için buna dayanır ve yinelenen bir
  kombinasyon, kombinasyonun zaten kullanımda olduğunu söyleyen bir
  hatayla reddedilir.
- Bilerek bu kayıt türü için bir veri girişi formu yoktur — bir kimlik
  satırını elle düzenlemek, bir yeniden içe aktarmanın dayandığı
  "güncelle, kopyalama" davranışını sessizce bozar. Yalnızca içe aktarma
  motoru tarafından yazılır.
- Dış Kimliğe olağan kayıt ekranları üzerinden yazma erişimi herkes için,
  her zaman reddedilir — `tenant_admin` dahil. Diğer kontrol düzlemi
  kayıt türlerinin çoğunun aksine, bu türün hiçbir yönetici istisnası
  yoktur; güvenilen tek yazar içe aktarma motorudur, ve bu da yukarıdaki
  "Yeni" ekranının hiç var olmamasının nedenidir.

## Neyle bağlantılıdır

Her Dış Kimlik bir **Dış SQL Kaynağı**na ve ürettiği kayda (türü ve
kimliğiyle) referans verir. Tekrarlanabilir bir içe aktarmanın
arkasındaki muhasebedir, doğrudan etkileşimde bulunduğunuz bir şey değil.
