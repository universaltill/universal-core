---
title: İzin
audience: admin
module: foundation
order: 19
---

İzin, bir **Rol**e tek bir kayıt türüne okuma ve/veya yazma erişimi
verir — örneğin Finans Müdürü rolünün Tedarikçi Faturası kayıtlarını
okuyup yazabilmesini sağlamak gibi. İzinler birikimlidir: bir kullanıcının
gerçek erişimi, sahip olduğu her Rolün verdiklerinin birleşimidir.

## Ne zaman kullanılır

Bir Role sahip olduğunuzda ve ona sahip olan herkes için belirli bir kayıt
türüne erişimi açmak (veya kısıtlamak) istediğinizde bir İzin ekleyin.

## Bir kayıt türüne erişim verme

1. **İzin**'e gidin ve **Yeni**'yi seçin.
2. Rolü seçin ve uygulandığı kayıt türünü, sistemde başka yerde göründüğü
   şekilde tam olarak (ör. "VendorInvoice") girin.
3. Uygun şekilde **Okuyabilir** ve/veya **Yazabilir** olarak işaretleyin.
4. Kaydedin.

## Bilinmesi gereken kurallar — erişim kontrolünü gerçekten açan şey budur

- **Hiç İzin satırı olmayan bir kayıt türü, kimliği doğrulanmış her
  kullanıcıya tamamen açık kalır** — bu, erişim kontrolünü kurmadan önce
  var olan her kayıt türünün tam olarak eskisi gibi çalışmaya devam
  etmesini sağlar.
- **Bir kayıt türü için ilk İzin satırını oluşturduğunuz anda, o kayıt
  türü varsayılan olarak reddedilir hâle döner**: o andan itibaren yalnızca
  eşleşen bir İzin satırı olan bir Role (veya `tenant_admin`) sahip bir
  kullanıcı onu okuyabilir veya yazabilir. Dar bir yetki oluşturmak, aynı
  zamanda o kayıt türünü kilitleme eylemidir — bunu kendi hâline
  bırakmaktan ayrı olarak bir "herkese izin ver" İzni eklemenin bir yolu
  yoktur.
- "Reddet" satırı diye bir şey yoktur — bir İzni, başka bir İznin (veya
  aynı kullanıcının sahip olduğu başka bir Rolün) zaten verdiği bir
  erişimi bir Rolden geri almak için kullanamazsınız.
- **Buraya yazdığınız kayıt türü serbest metindir, bir seçici değil** —
  yanlış yazılmış bir ad, gerçek bir kayıt türüyle asla eşleşmediğinden
  sessizce hiçbir şey yapmayan bir İzin oluşturur. Tam adı çift kontrol
  edin.
- (Herhangi bir kayıt türü için) çok ilk İzin satırınızı oluşturmak, aynı
  zamanda Rol, Kullanıcı Rolü, İzin, Alan İzni, Vekalet, Kayıt Sistemi ve
  Dış Kimliği yalnızca yönetici tarafından düzenlenebilir hâle getirir —
  önce neden kendinize `tenant_admin` vermeniz gerektiği için **Rol**'ün
  kendi kurallarına bakın.

## Neyle bağlantılıdır

Her İzin bir **Rol**e referans verir. **Alan İzni**, tüm kayıt türü
erişimi yerine daha ince, alan başına kontrol için bunun yanında çalışır.
