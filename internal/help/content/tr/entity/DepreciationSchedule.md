---
title: Amortisman Planı
audience: user
module: assets
order: 2
---

Bir Amortisman Planı satırı, bir Sabit Kıymet'in amortismanının bir
dönemidir — bir sıra numarası, kapsadığı dönemin son günü, o dönemde ne
kadar amortismana tabi tutulacağı ve sonrasında kalan defter değeri.

## Ne zaman kullanılır

Bu satırları normalde elle eklemezsiniz — bir **Sabit Kıymet**'i
kaydetmek onun tüm planını otomatik olarak oluşturur (o kaydın kendi
yardım konusuna bakın). Buraya planı incelemek veya işlenmeden önce
yanlış bir dönemi düzeltmek için gelin. Doğrusal bir plan, bir kıymetin
amortismana tabi tutarını (maliyet eksi hurda değeri) kullanım ömrü
boyunca ay ay eşit olarak yayar; her satırın defter değeri, bir önceki
satırın defter değeri eksi o satırın amortisman tutarına eşittir ve
hurda değerinde sona erer.

## Bilinmesi gereken kurallar

- Sabit Kıymet, Sıra, Dönem Sonu, Amortisman Tutarı ve Defter Değeri
  hepsi zorunludur. Amortisman Tutarı ve Defter Değeri negatif olamaz.
- Sıra satırları sıralar (1, 2, 3, …); kendisi bir tutar veya miktar
  değil, bir görüntüleme/sıralama değeri olduğundan sınırlandırılmamıştır.
- Dönem Sonu, satırın kapsadığı ayın son günüdür — o satır için bir
  işlemenin taşıyacağı tarih.
- **İşlendi**, bir kez ayarlandığında, satırın zaten muhasebeye
  işlendiğini belirtir. İşlenmiş bir satır gerçek bir muhasebe kaydını
  yansıtır — onu, işlenmiş başka herhangi bir kaydı düzeltirken
  göstereceğiniz aynı özenle düzeltin.
- Burada bir satırı düzeltmek bilinçli bir geçersiz kılma olarak
  hatırlanır: satırın kendi Sabit Kıymet'inin daha sonraki, ilgisiz bir
  kaydı, satırı geçerli koşulların hesaplayacağı değere sessizce geri
  oluşturmak yerine düzeltmeyi korur ve artık böyle bir kaydın tek
  başına reddedilmesine neden olmaz. Kıymetin gerçek amortisman
  koşullarındaki (Maliyet, Hurda Değeri, Kullanım Ömrü, Amortisman
  Yöntemi, Edinme Tarihi, Para Birimi) bir değişiklik, Sabit Kıymet
  konusunda anlatıldığı gibi planı yine de yeniden oluşturur veya
  reddeder — bir düzeltme yalnızca kendi satırını bu kontrolden muaf
  tutar, planın geri kalanını değil. Düzeltilmiş bir satır, Sabit
  Kıymet'in kendi Amortisman Planı özetinde **Elle Düzeltildi** olarak
  görünür, böylece hangi dönemlerin elle değiştirildiği bir bakışta
  görülebilir.

## Neyle bağlantılı

Bir Amortisman Planı satırı tam olarak bir **Sabit Kıymet**'e aittir.
