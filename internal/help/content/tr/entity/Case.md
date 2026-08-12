---
title: Destek Kaydı
audience: user
module: crm
order: 4
---

Destek Kaydı, bir müşteri için açılan destek veya satış-sonrası
sorunudur — bir soru, bir şikayet veya satın aldıkları bir şeyle ilgili
bir problem. Belirli bir ürün veya siparişle ilgili olsun ya da olmasın,
bir sorunu kaydedip çözüme kadar takip edebileceğiniz tek yerdir.

## Ne zaman kullanılır

Bir müşteri bir sorun bildirdiğinde veya çözüme kadar takip edilmesi
gereken bir soru sorduğunda — sadece e-posta yazışması değil — bir
Destek Kaydı açın. Sorun onlara sattığınız bir şeyle ilgiliyse, Satış
Siparişini ve biliyorsanız Ürünü bağlayın; böylece kaydı devralan
herkes tüm bağlamı hemen görür.

## Destek kaydı açma

1. **Destek Kaydı**'na gidin ve **Yeni**'yi seçin.
2. Bir Kayıt No (kendi referansınız) ve bir Konu girin.
3. **Müşteri**'yi seçin — burada herhangi bir Taraf seçilebilir (aşağıdaki
   Kurallar'a bakın).
4. İsteğe bağlı olarak bu kaydın ilgili olduğu **Ürün**'ü ve **Satış
   Siparişi**'ni bağlayın.
5. **Öncelik**'i belirleyin — Düşük, Normal, Yüksek veya Acil — ve
   **Açılış** tarihini girin.
6. İsteğe bağlı olarak taahhüt ettiğiniz bir **SLA Son Tarihi** girin.
7. Kaydedin.

## Bilinmesi gereken kurallar

- Kayıt No, Konu, Müşteri, Açılış ve Öncelik zorunludur. Durum
  varsayılan olarak **Yeni**'dir. Öncelik formda **Normal** olarak
  önceden doldurulur, ancak yine de zorunlu bir alandır — başka bir
  yoldan (içe aktarma, API) öncelik belirtilmeden oluşturulan bir kayıt,
  sessizce varsayılana düşmek yerine reddedilir.
- Durum düz bir çizgi değil, bir destek iş akışını izler: Yeni → İşlemde
  → Çözüldü → Kapatıldı normal yoldur, ancak bir kayıt İşlemde
  durumundan **Müşteri Bekleniyor**'a geçebilir ve geri dönebilir; bir
  **Çözüldü** kayıt, müşteri sorunun aslında çözülmediğini söylerse
  İşlemde durumuna geri dönebilir — bunu yeni bir kayıtta geçmişini
  kaybetmek yerine yeniden açmak için. **İptal Edildi**'ye, yanlışlıkla
  açılmış veya mükerrer bir kayıt için, Çözüldü dahil her açık durumdan
  geçilebilir.
- Satış Siparişi'nin müşteri alanının aksine, buradaki Müşteri'nin
  müşteri rolüne sahip olup olmadığı kontrol edilmez — herhangi bir
  Taraf seçilebilir, çünkü destek yalnızca sizden alım yapmış kişiler
  için değildir.
- SLA Son Tarihi belirlerseniz, Açılış'tan önce olamaz.
- Ürün ve Satış Siparişi isteğe bağlıdır ve birbirinden bağımsızdır — bir
  kayıt ikisiyle de, yalnızca biriyle veya hiçbiriyle ilgili olabilir.

## Neyle bağlantılı

Bir Destek Kaydı, bağlam için bir **Satış Siparişi** ve bir **Ürün**'e
ve gerçekte kaydı ele alan Taraf olan bir **Atanan**'a
referans verebilir — bu bir istihdam kaydı olmak zorunda değildir, çünkü
destek genellikle taşeronlar ve iş ortakları tarafından yürütülür.
**Müşteri**, kaydın kendisi için açıldığı Taraftır.
