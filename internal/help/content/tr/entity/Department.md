---
title: Departman
audience: both
module: foundation
order: 8
---

Departman, kuruluşunuzun yapısındaki tek bir düğümdür — Operasyon, Depo,
Finans ve benzeri. Departmanlar iç içe geçebilir: bir Depo departmanı bir
Operasyon departmanının altında yer alabilir, böylece düz bir liste yerine
gerçek bir organizasyon şeması oluşur.

## Ne zaman kullanılır

Kuruluşunuzun nasıl yapılandırıldığını temsil etmek için Departmanlar
kurun ve bunları rolleri, pozisyonları ve (destekleyen modüllerde) onay
yönlendirmesini işletmenin belirli bir bölümüne göre kapsamlandırmak için
kullanın.

## Departman oluşturma

1. **Departman**'a gidin ve **Yeni**'yi seçin.
2. Bir kod ve bir ad girin.
3. İsteğe bağlı olarak, hiyerarşide başka bir departmanın altına
   yerleştirmek için bir üst departman seçin.
4. Kaydedin.

## Bilinmesi gereken kurallar

- Kod ve ad zorunludur; üst departman isteğe bağlıdır — üst departmanı
  olmayan bir Departman, organizasyon şemasının en üst düzeyinde yer alır.
- Bir Departmanın güvenle var olması için başka bir kuruluma gerek
  olmadığından, başka bir Departmanın formundaki üst departman seçicisinden
  bulunduğunuz sayfadan ayrılmadan yeni bir tane oluşturabilirsiniz.
- Bir departmanın hiyerarşinin ne kadar derine inebileceğine dair yerleşik
  bir sınır yoktur ve sistem döngüsel bir referansı (bir departmanın
  yanlışlıkla kendi atası olarak ayarlanması) kontrol etmez veya önlemez —
  yapıyı düzenlerken mantıklı tutun.

## Neyle bağlantılıdır

**Pozisyon** kayıtları isteğe bağlı olarak bir Departmana aittir.
**Kullanıcı Rolü** yetkileri isteğe bağlı olarak tek bir Departmanla
kapsamlandırılabilir. İK etkinleştirildiğinde çalışanlar da bir Departmana
referans verir. Departmanın kendisi yalnızca, varsa, kendi üst
departmanına referans verir.
