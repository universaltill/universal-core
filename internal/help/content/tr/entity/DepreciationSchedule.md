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

Bir kıymet kaydettiğinizde veya yanlış bir dönemi düzeltmeniz
gerektiğinde **Sabit Kıymet** formunun Amortisman Planı bölümünde plan
satırları ekleyin veya düzeltin. Doğrusal bir plan, bir kıymetin
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

## Neyle bağlantılı

Bir Amortisman Planı satırı tam olarak bir **Sabit Kıymet**'e aittir.
