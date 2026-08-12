---
title: Yapay Zeka Sağlayıcı Bağlantısı
audience: admin
module: foundation
order: 25
---

Yapay Zeka Sağlayıcı Bağlantısı, içe aktarma sihirbazındaki önerilen sütun
eşleme gibi metin üretimi destekli özellikler için kiracınızın kendi
yapılandırılmış yapay zeka arka ucudur. Her kiracı zaten paylaşılan,
kendi barındırılan bir varsayılanı bedava alır; bu, bunun yerine kendi
Anthropic veya OpenAI hesabınıza (kendi maliyetiniz, kendi anahtarınız, o
sağlayıcının veri işlemesi hakkında kendi açık seçiminiz) veya
paylaşılan sunucu yerine ayrı barındırılan kendi sunucunuza nasıl
geçtiğinizdir.

## Ne zaman kullanılır

Bunu yalnızca paylaşılan varsayılan yerine özellikle kendi yapay zeka
sağlayıcı hesabınızı kullanmak istediğinizde yapılandırın — çoğu kiracının
buna hiç dokunması gerekmez.

## Bağlantınızı yapılandırma

1. **Ayarlar → Yapay Zeka Sağlayıcısı**'na gidin. Bu sayfa özellikle
   `tenant_admin` rolünü gerektirir — farklı, yönetici olmayan bir role
   sahip bir kullanıcı, aynı kullanıcının sistemde başka gerçek erişimi
   olmasına rağmen burada erişim reddedildi sayfası alır.
2. Sağlayıcınızı seçin: paylaşılan kendi barındırılan seçenek veya kendi
   Anthropic ya da OpenAI hesabınız.
3. Kendi barındırılan sunucunuz için adresini girin. Anthropic veya
   OpenAI için modeli ve API anahtarınızı girin.
4. Kaydedin. API anahtarı saklanmadan önce şifrelenir ve daha sonra size
   asla geri gösterilmez.

## Bilinmesi gereken kurallar

- Kiracı başına tam olarak bir Yapay Zeka Sağlayıcı Bağlantısı vardır — bu
  Dış SQL Kaynağı gibi bir liste değil, tek bir ayarlar kaydıdır. Yeniden
  kaydetmek, yeni bir kayıt oluşturmak yerine aynı kaydı günceller.
- Bu kayıt türünün kendi otomatik oluşturulmuş veri girişi formu yoktur ve
  bir yönetici tarafından bile sıradan kayıt ekranları aracılığıyla
  oluşturulamaz veya düzenlenemez — Ayarlar → Yapay Zeka Sağlayıcısı,
  bunu değiştirmenin tek yoludur; tam olarak API anahtarının şifreli
  kalması ve bir tarayıcıya asla düz metin olarak geri gitmemesi
  gerektiği için.

## Neyle bağlantılıdır

Bir Yapay Zeka Sağlayıcı Bağlantısı kendi başınadır — başka kayıtlar
tarafından referans verilmek yerine, yapay zeka destekli özellikler
(içe aktarma eşleme önerileri gibi) tarafından başvurulur.
