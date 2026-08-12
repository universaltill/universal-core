---
title: Kayıt Sistemi
audience: admin
module: foundation
order: 22
---

Kayıt Sistemi, belirli bir kayıt türü için efendinin kim olduğunu bildirir
— bu platform mu, yoksa bir **Dış SQL Kaynağı** olarak kaydettiğiniz harici
bir sistem mi. Sisteme "Kalemler eski sistemimizde yönetiliyor; kimse
burada elle düzenlemesin" veya tam tersini, "bu kayıt türü artık tamamen
bizim, serbestçe düzenle" demenin yoludur.

## Ne zaman kullanılır

Harici bir sistemden veri içe aktarırken (CSV veya SQL içe aktarma
sihirbazı aracılığıyla) ve o sistemden gelen kayıtların burada düzenlenebilir
kalması mı, yoksa eski sistemin salt okunur bir aynası olarak mı ele
alınması gerektiğine karar vermeniz gerektiğinde bunu kurun.

## Sahiplik bildirme

1. **Kayıt Sistemi**'ne gidin ve **Yeni**'yi seçin.
2. Uygulandığı kayıt türünü girin.
3. Salt okunur bir ayna bildiriyorsanız, geldiği Dış SQL Kaynağını seçin.
4. Modu seçin: **Salt Okunur** (harici sistem efendidir; ondan gelen
   kayıtlarda buradaki elle düzenlemeler engellenir) veya **Platform
   Sahipliğinde** (bu platform efendidir; serbestçe düzenleyin). Üçüncü
   bir mod olan **İki Yönlü**, gelecekteki iki yönlü senkronizasyon için
   saklıdır ve bugün kaydetmeye çalışırsanız reddedilir.
5. Kaydedin.

## Bilinmesi gereken kurallar

- Aynı kayıt türü, her biri farklı bir Dış SQL Kaynağı adlandırdığı sürece
  meşru olarak birden fazla Salt Okunur bildirimine sahip olabilir —
  örneğin Tarafı iki farklı eski sistemden aynalıyorsanız. Gerçekte
  engellenen şey, tam olarak aynı kayıt türü ve kaynak kombinasyonunu iki
  kez bildirmektir — bu, kombinasyonun zaten kullanımda olduğunu söyleyen
  bir hatayla reddedilir.
- Aynı kayda birden fazla bildirim uygulanırsa, en kısıtlayıcı olan
  kazanır — uygulanan herhangi bir bildirimin salt okunur işaretlediği bir
  kayıt, sistemin hangi bildirime güveneceğini tahmin etmesi yerine
  korunmuş kalır.
- Bu bir kontrol düzlemi kayıt türüdür: kiracınızın erişim denetimi
  önyükleme kilidi etkinleştiği anda, yalnızca `tenant_admin` taşıyan biri
  Kayıt Sistemi satırları oluşturabilir veya düzenleyebilir — bu kilidi
  tam olarak neyin etkinleştirdiğini ve bunun ima ettiği önyükleme
  sıralamasını görmek için **Rol**'ün kendi kurallarına bakın.

## Neyle bağlantılıdır

Bir Kayıt Sistemi isteğe bağlı olarak bir **Dış SQL Kaynağı**na referans
verir. Belirli bir türden kayıtların, içe aktarıldıktan sonra normal
otomatik oluşturulmuş formlar aracılığıyla elle düzenlenip
düzenlenemeyeceğini yönetir.
