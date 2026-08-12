---
title: Dönem
audience: admin
module: finance
order: 3
---

Dönem, bir **Mali Yıl** içindeki tek bir muhasebe dönemidir (genellikle
bir ay) — ay sonu kapanışının gerçekte üzerinde işlem yaptığı şey. Mali
Yıl'ın kendi Açık/Kapalı bayrağından farklı olarak, Dönem'in durumu
defterin gerçekten zorunlu kıldığı durumdur: bir Dönem'i kapatmak veya
kilitlemek, içine tarihli yeni işlemleri durduran şeydir.

## Ne zaman kullanılır

Bunları her Mali Yıl için, ayrı ayrı kapatabilmek istediğiniz tarih
aralıklarını kapsayacak şekilde kurun — çoğu kiracı ay başına bir Dönem
oluşturur.

## Dönem oluşturma

1. **Dönem**'e gidin ve **Yeni**'yi seçin.
2. Ait olduğu **Mali Yıl**'ı seçin.
3. Bir Ad (örn. "2026-01"), Başlangıç Tarihi ve Bitiş Tarihi girin.
4. Durum varsayılan olarak **Açık**tır.
5. Kaydedin.

## Bir dönemi kapatmak

Bir döneme karşı artık başka bir şeyin işlenmemesi gerektiğinden emin
olduğunuzda (genellikle ay sonu kapanış anı), Durumunu **Kapalı** veya
**Kilitli** olarak ayarlayın ve kaydedin. Bu andan itibaren, bir Müşteri
Faturası kesmek, mal almak, vadesi gelen amortismanı işlemek veya
deftere ulaşan başka herhangi bir şey olsun, o dönemin Başlangıç
Tarihi/Bitiş Tarihi aralığı içinde tarihli her yevmiye kaydı, hiçbir
geçersiz kılma olmadan doğrudan reddedilir. Bunun yanlış olduğu ortaya
çıkarsa, işlemin kendi tarihini açık bir döneme ileriye taşıyın veya bu
dönemi yeniden açın.

## Bilinmesi gereken kurallar

- Mali Yıl, Ad, Başlangıç Tarihi ve Bitiş Tarihi zorunludur.
- Durum **Açık**, **Kapalı** veya **Kilitli**dir — Kapalı ve Kilitli
  bugün işlemleri aynı şekilde engeller; şu an güvenebileceğiniz bir
  davranış farkı yoktur.
- Tarihi birden fazla Dönem'in içine düşen bir işlem (çakışan dönemler
  oluşturmanızı engelleyen hiçbir şey yoktur), aynı tarihi kapsayan
  başka biri hâlâ Açık olsa bile, kapsayan Dönemlerden *herhangi biri*
  Kapalı veya Kilitli ise engellenir.

## Neyle bağlantılı

Her Dönem bir **Mali Yıl**'a aittir. Defter, herhangi bir yevmiye
kaydının tarihini kabul etmeden önce — o işlemi hangi modül veya eylem
tetiklemiş olursa olsun — Mali Yıl'ın değil, her Dönem'in durumunu
kontrol eder.
