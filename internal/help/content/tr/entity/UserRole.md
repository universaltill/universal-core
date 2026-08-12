---
title: Kullanıcı Rolü
audience: admin
module: foundation
order: 18
---

Kullanıcı Rolü, bir **Rol**ü bir kullanıcıya verir. Bir kullanıcı birden
fazla Role sahip olabilir ve bir Rol birden fazla kullanıcıya verilebilir
— bu, kullanıcı başına tek bir atama değil, çoktan-çoğa bir yetkidir.

## Ne zaman kullanılır

Taşıması gereken İzinlerle bir Rol kurduğunuzda ve belirli bir kişinin
gerçekten o role sahip olması gerektiğinde bir Kullanıcı Rolü verin.

## Rol verme

1. **Kullanıcı Rolü**'ne gidin ve **Yeni**'yi seçin.
2. Kullanıcının tanımlayıcısını girin ve Rolü seçin.
3. Kaydedin.

## Bilinmesi gereken kurallar

- Kullanıcı tanımlayıcısı ve Rol ikisi de zorunludur. Bu ekranda seçilecek
  ayrı bir kullanıcı dizini yoktur — tanımlayıcı, sistemin geri kalanının
  o kişi için zaten kullandığı aynı oturum açma kimliğidir.
- Bir Departman isteğe bağlı olarak bir yetkiyi kapsamlandırabilir, ancak
  bu henüz bu formda gösterilmez veya erişim kontrolleri tarafından
  uygulanmaz — gelecekteki departman tabanlı onay yönlendirmesi için
  saklıdır. Bugün bir yetkiyi kısıtlamak için buna güvenmeyin.
- `tenant_admin` vermek, başka herhangi bir yapılandırmadan bağımsız olarak
  tam erişim verir — bu rolü herhangi bir İzin veya Alan İzni satırı
  oluşturmadan önce en az bir kullanıcıya neden vermeniz gerektiği için
  **Rol**'ün kendi kurallarına bakın.
- Bir Kullanıcı Rolünü kaldırmak, o yetkiyi anında iptal eder; Rolün
  kendisini silmez veya buna sahip başka bir kullanıcıyı etkilemez.

## Neyle bağlantılıdır

Her Kullanıcı Rolü bir **Rol**e referans verir. Bir Rolün gerçekten bir
kullanıcıya ulaştığı tek yerdir — İzin ve Alan İzni satırları bir Rolün ne
yapabileceğini tanımlar, ancak bir kullanıcı bu erişimi ancak bir
Kullanıcı Rolü kendisine Rolün kendisini verdiğinde alır.
