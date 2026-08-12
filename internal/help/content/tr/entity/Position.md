---
title: Pozisyon
audience: both
module: foundation
order: 9
---

Pozisyon, organizasyon şemanızdaki bir koltuk veya roldür — "Depo
Sorumlusu," "Finans Müdürü" — onu dolduran kişiden ayrı bir kavramdır. Bir
Pozisyon, henüz kimse ona atanmadan önce var olabilir ve bir raporlama
hattında yer alabilir.

## Ne zaman kullanılır

Kuruluşunuzun raporlama yapısını, insanları işe almadan veya bu
pozisyonlara atamadan önce ya da bundan bağımsız olarak modellemek için
Pozisyonlar oluşturun. İK modülü etkinleştirildiğinde bir Çalışan kaydı bir
Pozisyonu doldurur; Pozisyonun kendisi o an kimin sahip olduğunu izlemez.

## Pozisyon oluşturma

1. **Pozisyon**'a gidin ve **Yeni**'yi seçin.
2. Bir unvan girin.
3. İsteğe bağlı olarak ait olduğu Departmanı ve raporlandığı Pozisyonu
   seçin.
4. Kaydedin.

## Bilinmesi gereken kurallar

- Yalnızca unvan zorunludur. Departman bilerek isteğe bağlıdır — şirket
  genelinde veya matris bir pozisyon (tek bir departmana raporlamayan bir
  Finans Direktörü gibi) hiç departmana ihtiyaç duymaz.
- "Raporlanan" alanı, Departmanlardan bağımsız bir Pozisyon raporlama
  zinciri kurmanızı sağlar — bir Pozisyon farklı bir Departmandaki veya
  hiçbirindeki başka bir Pozisyona raporlanabilir.
- Pozisyon ve Çalışan farklı şeylerdir: bu kayıt koltuğu temsil eder,
  içindeki kişiyi değil.

## Neyle bağlantılıdır

Bir Pozisyon bir **Departman**a ve başka bir **Pozisyon**a (raporlandığı)
referans verebilir. İK etkinleştirildiğinde, Çalışan kayıtları kimin o
pozisyonu doldurduğunu göstermek için bir Pozisyona referans verir.
