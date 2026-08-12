---
title: Taraf İlişkisi
audience: both
module: foundation
order: 3
---

Taraf İlişkisi iki **Taraf** kaydını birbirine bağlar ve aralarındaki
ilişkiyi belirtir: bir kuruluş bir kişiyi istihdam eder, bir tedarikçi
başka bir tarafa mal sağlar, bir kuruluş başka birinin ana kuruluşudur ya
da bir kişi bir kuruluş için irtibat kişisidir. Kuruluşlar ve kişiler
arasındaki her bağlantı türü için ayrı, özel bir bağlantı yerine bu
sistemin kullandığı genel amaçlı mekanizmadır.

## Ne zaman kullanılır

İki Tarafın belirli bir şekilde bağlı olduğunu kaydetmeniz gerektiğinde bir
Taraf İlişkisi kullanın — örneğin bir bağlı şirketi ana şirketine bağlamak
veya bir müşteri kuruluşu için irtibat kişisinin kim olduğunu kaydetmek
için.

## Bir ilişki kaydetme

1. **Taraf İlişkisi**'ne gidin ve **Yeni**'yi seçin.
2. **Kaynak** Tarafı ve **Hedef** Tarafı seçin.
3. İlişki türünü seçin: **İstihdam Eder**, **Tedarik Eder**, **Ana
   Kuruluşudur** veya **İrtibat Kişisidir**.
4. Kaydedin.

## Bilinmesi gereken kurallar

- **Yön önemlidir ve sizin için kontrol edilmez.** Her ilişki türü belirli
  bir yönde okunur:
  - *İstihdam Eder*: Kaynak Taraf (bir kuruluş) Hedef Tarafı (bir kişiyi)
    istihdam eder.
  - *Tedarik Eder*: Kaynak Taraf (bir tedarikçi) Hedef Tarafa (bir
    müşteriye) mal sağlar.
  - *Ana Kuruluşudur*: Kaynak Taraf (bir ana kuruluş) Hedef Tarafın (bir
    bağlı şirketin) ana kuruluşudur.
  - *İrtibat Kişisidir*: Kaynak Taraf (bir kişi) Hedef Taraf (bir kuruluş)
    için irtibat kişisidir — bu yönün *İstihdam Eder*'in tersi olduğuna
    dikkat edin.
  Kaynak/Hedef sırasını ters girmek hatasız kaydedilir, bu yüzden
  kaydetmeden önce hangi Tarafın hangi alana ait olduğunu iki kez kontrol
  edin.
- Bir ilişki, her iki Tarafın da uygun bir Taraf Rolüne sahip olmasını
  gerektirmez — örneğin bir *İrtibat Kişisidir* ilişkisi kaydetmek, kişiye
  kendiliğinden bir İrtibat rolü vermez; uygunsa bunu Taraf Rolü ekranında
  ayrıca ekleyin.

## Neyle bağlantılıdır

Bir Taraf İlişkisinin her iki ucu da **Taraf** kaydıdır. **Taraf Rolü**'nü
(bir Tarafın kendi kapasitesini tanımlayan) tamamlar, onun yerini almaz:
bir kuruluş için irtibat kişisi olan bir kişi, tipik olarak hem bir İrtibat
Taraf Rolüne hem de o kuruluşa işaret eden bir *İrtibat Kişisidir* Taraf
İlişkisine sahiptir.
