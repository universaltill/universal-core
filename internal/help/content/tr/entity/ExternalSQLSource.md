---
title: Dış SQL Kaynağı
audience: admin
module: foundation
order: 23
---

Dış SQL Kaynağı, harici bir veritabanına kayıtlı bir bağlantıdır — eski
bir ERP'nin SQL Server'ı veya içe aktarma sihirbazı aracılığıyla kayıt
çekmek istediğiniz herhangi bir Postgres/SQL Server veritabanı. **Yapay
Zeka Sağlayıcı Bağlantısı**'nın aksine, bu tek bir ayarlar kaydı değil bir
listedir: birden fazla kaynak kaydedebilir ve her içe aktarma
çalıştırdığınızda birini seçebilirsiniz.

## Ne zaman kullanılır

SQL tabanlı bir içe aktarma çalıştırmadan önce — örneğin müşteri ve kalem
verilerini eski bir sistemden bu platforma taşırken — bir Dış SQL Kaynağı
kaydedin.

## Bir kaynak kaydetme

1. **Ayarlar → SQL Kaynakları**'na gidin ve yeni bir kaynak eklemeyi
   seçin.
2. Bir ad, sürücü (SQL Server veya Postgres), sunucu, veritabanı ve
   gerekiyorsa bağlantı noktası, kullanıcı adı ve parola girin.
3. Kaydedin. Parola saklanmadan önce şifrelenir ve daha sonra size asla
   geri gösterilmez — sonraki bir düzenlemede parola alanını boş bırakmak,
   mevcut parolayı temizlemek yerine değişmeden bırakır.
4. Bir içe aktarma için güvenmeden önce bağlantının gerçekten çalıştığını
   doğrulamak için **Test**'i kullanın.

## Bilinmesi gereken kurallar

- Ad, sürücü, sunucu ve veritabanı zorunludur. Kullanıcı adı, bağlantı
  noktası, parola ve ek sürücüye özgü seçenekler isteğe bağlıdır — bazı
  kaynaklar meşru olarak hiçbir parolaya ihtiyaç duymaz (örneğin
  parolasız yerel bir veritabanı).
- Bu ayarlar sayfası özellikle `tenant_admin` rolünü gerektirmez — Yapay
  Zeka Sağlayıcı Bağlantısının aksine, bu ayarlar alanına erişimi olan
  herhangi bir kullanıcı kaynakları yönetebilir, bu yüzden bu erişime
  kimin sahip olduğunu göz önünde bulundurun.
- Bağlantı bilgileri tek bir bağlantı dizesi yerine ayrı alanlar olarak
  saklanır, böylece ayarlar ekranı bunları tek tek düzenleyebilir ve
  parola diğer her şeyi yeniden girmeden değiştirilebilir.

## Neyle bağlantılıdır

**Kayıt Sistemi** ve **Dış Kimlik** ikisi de bir Dış SQL Kaynağına
referans verir — birincisi içe aktarılan kayıtların sahibini bildirmek
için, ikincisi belirli bir içe aktarmanın hangi eski kayıttan geldiğini
hatırlamak için.
