---
title: Rol
audience: admin
module: foundation
order: 17
---

Rol, kiracınız için tanımladığınız bir erişim kontrolü rolüdür — "Depo
Sorumlusu," "Finans Müdürü" veya kuruluşunuzun gerçekten ihtiyaç duyduğu
başka herhangi bir şey. Roller sabit bir sistem listesi değildir; her
kiracı tam olarak istediği rolleri, ihtiyacı kadar az veya çok, oluşturur.

Bu yönetimsel bir konudur: Rolleri, İzinleri ve ilgili erişim kontrolü
kayıtlarını kurmak normalde günlük kullanıcılar tarafından değil, bir
kiracı yöneticisi tarafından yapılır.

## Ne zaman kullanılır

Bir dizi izni, sonradan bir veya daha fazla kullanıcıya verebileceğiniz
bir ad altında gruplamak istediğinizde bir Rol oluşturun. Roller belirli
bir kullanıcıdan bağımsız olarak var olur — önce rolü kurar, ardından
**Kullanıcı Rolü** ile verirsiniz.

## Rol oluşturma

1. **Rol**'e gidin ve **Yeni**'yi seçin.
2. Bir kod (kısa, sabit bir tanımlayıcı) ve bir görünen ad girin. Açıklama
   isteğe bağlıdır.
3. Kaydedin. Tek başına yeni bir Rol hiçbir şey vermez — gerçekte ne
   yapabileceğini tanımlamak için **İzin** ve **Alan İzni** satırları
   ekleyin, ardından **Kullanıcı Rolü** ile kullanıcılara verin.

## Bilinmesi gereken kurallar — ilk İzninizi oluşturmadan önce okuyun

- **Bir Rol kodu özeldir: `tenant_admin`.** Kodu tam olarak `tenant_admin`
  olan bir Role sahip bir kullanıcı, herhangi bir İzin veya Alan İzni
  satırından bağımsız olarak her zaman tam erişime sahiptir — bu, özellikle
  bir yöneticinin kendini asla dışarıda bırakamaması için vardır.
- **İki ayrı şeyden her biri aynı kilidi etkinleştirir: kiracınızın ilk
  İzin veya Alan İzni satırına kavuşması, ya da herhangi birine
  `tenant_admin` Rolünün verilmesi.** Bu ikisinden biri gerçekleştiği anda,
  birkaç erişim kontrolü kayıt türü — Rol, Kullanıcı Rolü, İzin, Alan İzni,
  Vekalet ve Kayıt Sistemi — sıradan kullanıcılar tarafından düzenlenebilir
  olmaktan çıkar ve yalnızca `tenant_admin` taşıyan biri tarafından
  düzenlenebilir hâle gelir. Bu ikisinden hiçbiri gerçekleşmeden önce,
  bunlar herhangi bir kimliği doğrulanmış kiracı üyesine açıktır — ilk
  yöneticinin kurulmasını sağlayan da budur. **Başka bir şey için ilk İzin
  veya Alan İzni satırınızı oluşturmadan önce kendinize (veya birine)
  `tenant_admin` rolünü verin** — bu rolü vermek, kilidi etkinleştiren iki
  şeyden biridir, bu yüzden önce bunu yapmak hiç kimseyi dışlamaz; ama
  kimse `tenant_admin` taşımadan önce bir İzin veya Alan İzni satırı
  oluşturmak dışlar.
- Hiç İzin satırı olmayan bir Rol tek başına hiçbir şeyi kısıtlamaz —
  varlık düzeyinde erişimin gerçekte nasıl kilitlendiğine dair
  **İzin**'in kendi kurallarına bakın.

## Neyle bağlantılıdır

**İzin** ve **Alan İzni** satırları bir Role varlık ve alan düzeyinde
erişim verir. **Kullanıcı Rolü** bir Rolü belirli bir kullanıcıya verir.
**Vekalet** ayrıdır — bir kullanıcının başka birinin onay statüsü için
vekalet etmesini sağlar ve kendisi Rolü hiç kullanmaz.
