---
title: Vekalet
audience: admin
module: foundation
order: 21
---

Vekalet, bir kullanıcının (vekil) etkinken başka bir kullanıcının
(vekalet veren) onay statüsü için vekalet etmesini sağlar — örneğin
biri seyahatte veya izindeyken onayların durmaması için. Yalnızca onay
uygunluğu verir, başka hiçbir şey vermez: vekalet verenin Rolünü, varlık
erişimini, yönetici statüsünü veya erişim kontrolünün normalde
düzenlediği başka herhangi bir şeyi **devretmez**.

## Ne zaman kullanılır

Belirli bir kişinin, tanımlı bir süre boyunca — veya siz kapatana kadar
süresiz olarak — başka birinin adına onay verebilmesi gerektiğinde bir
Vekalet kurun.

## Vekalet oluşturma

1. **Vekalet**'e gidin ve **Yeni**'yi seçin.
2. Vekalet verenin ve vekilin kullanıcı tanımlayıcılarını girin.
3. İsteğe bağlı olarak bir bitiş tarihi ayarlayın — vekalet o günün
   sonuna kadar etkin kalır. Sabit bir bitişi olmayan bir vekalet için boş
   bırakın.
4. Kaydedin.

## Bilinmesi gereken kurallar

- Bir Vekalet yalnızca tek bir sıçramayı etkiler: A, B'ye vekalet
  verirse ve B ayrıca C'ye vekalet vermişse, C, B üzerinden A'nın
  statüsünü **devralmaz**. Her vekalet doğrudan, tek adımlı bir
  ilişkidir.
- Bir vekaleti bitiş tarihinden önce (veya süresiz birini) sonlandırmak
  için, silmek yerine **İptal Edildi** olarak işaretleyin — bu, kaydı
  silmek yerine kime ne zaman vekalet verildiğine dair görünür bir geçmiş
  tutar.
- Kendinize vekalet vermek form tarafından izin verilir ama hiçbir etkisi
  yoktur — yalnızca zaten sahip olduğunuz statüyü size vermiş olur.
- Vekalet, **Rol**'ün kendi konusunda açıklanan aynı yalnızca-yönetici
  kilidinin uygulandığı kayıt türlerinden biridir — o kilit etkinleştiği
  anda, bir Vekaleti yalnızca `tenant_admin` oluşturabilir veya
  düzenleyebilir, tıpkı İzin veya Alan İzni gibi. (Bir Vekalet oluşturmak
  kilidi kendisi etkinleştirmez — yalnızca bir İzin/Alan İzni satırı veya
  bir `tenant_admin` ataması bunu yapar — tam olarak ne zaman olduğunu
  görmek için **Rol**'e bakın.)

## Neyle bağlantılıdır

Bir Vekalet kendi başınadır — kullanıcıları bir Rol veya Taraf kaydı
yerine tanımlayıcılarıyla referans verir. İş Akışı onayları
kullanıldığında, bir onay kontrolü, orijinal onaylayan için başka kimin
hareket edebileceğini görmek için etkin Vekalet satırlarına başvurur.
