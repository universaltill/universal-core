---
title: Taraf
audience: both
module: foundation
order: 1
---

Taraf, bir iş ilişkisinde yer alabilecek herhangi bir kişi veya kuruluş
için tutulan tek kayıttır: bir gerçek kişi ya da bir kuruluş. Gerçek
dünyadaki bir şirketin veya kişinin sistemde var olduğu tek yerdir — Core,
aynı şirketin birden fazla kopyasının oluşabileceği ayrı Müşteri, Tedarikçi
ve Çalışan tabloları tutmaz. Bunun yerine tek bir Taraf kaydı aynı anda
birden fazla role sahip olabilir (bkz. **Taraf Rolü**); yani sonradan
sizden alım yapmaya başlayan bir tedarikçi, iki ayrı kayıt değil, hâlâ tek
bir kayıttır.

## Ne zaman kullanılır

İşletmenin ilişki kurduğu bir kişi veya kuruluşu — bir müşteri, tedarikçi,
çalışan, potansiyel müşteri ya da sade bir irtibat kişisini — kaydetmeniz
gerektiğinde bir Taraf oluşturun. Aynı kişi veya kuruluşla kurulan her
ilişki için yeni bir Taraf oluşturmazsınız; Taraf'ı bir kez oluşturur,
ardından bu tarafın hareket ettiği her kapasite için bir Taraf Rolü
eklersiniz.

## Taraf oluşturma

1. **Temel** menüsünden **Taraf**'ı açın ve **Yeni**'yi seçin.
2. Türü — **Kişi** veya **Kuruluş** — belirleyin ve adı girin. Her ikisi de
   zorunludur.
3. İsteğe bağlı olarak bir vergi numarası, tercih edilen bir dil ve
   (aşağıda açıklanan, kendi kiracınızı temsil eden kuruluş için) bir
   sicil numarası ile yasal irtibat adı kaydedebilirsiniz.
4. Durum varsayılan olarak **Aktif**'tir; geçmişini silmeden bir Tarafı
   devre dışı bırakmak için **Pasif** olarak ayarlayın.
5. Kaydedin. Ardından Tarafı sistemin geri kalanında kullanılabilir hâle
   getirmek için bir **Adres**, bir **İletişim Yöntemi** ve bir veya daha
   fazla **Taraf Rolü** kaydı ekleyin.

## Bilinmesi gereken kurallar

- Tek başına bir Taraf, "bu kişi veya kuruluş var" ötesinde bir iş anlamı
  taşımaz — yalnızca uygun bir Taraf Rolü eklendiğinde müşteri, tedarikçi,
  çalışan, irtibat kişisi veya potansiyel müşteri hâline gelir.
- Sicil numarası ve yasal irtibat adı alanları yalnızca kendi şirketinizi
  temsil eden tek Taraf için önemlidir (aşağıdaki **Taraf Rolü**'nün
  **Kendi Kuruluşum** rolüne bakın) — bunlar yasal dışa aktarımlarda
  kullanılır ve aksi hâlde boş bırakılması güvenlidir.
- Bir kiracıda tam olarak bir Tarafın kendi kuruluşunuz olarak
  işaretlenmesi beklenir. Sistem ikinci bir işaretlemeyi kesin olarak
  engellemez, bu yüzden bunu yöneticinizin uyması gereken bir kural olarak
  görün — durum belirsizleşirse buna dayanan özellikler (yasal dışa
  aktarım gibi) tahmin yürütmek yerine güvenli bir şekilde geri çekilir.

## Neyle bağlantılıdır

Adresler, iletişim yöntemleri ve ekler bir Tarafa referans verir. Taraf
Rolü, bir Tarafın hangi kapasitede hareket ettiğini kaydeder; Taraf
İlişkisi, iki Tarafın birbiriyle nasıl ilişkili olduğunu kaydeder (ör. bir
kuruluşun bir kişiyi istihdam etmesi). Satın Alma veya Satış gibi bir
modül etkinleştirildiğinde, satın alma siparişleri ve faturalar gibi
belgeler bir Tarafa tedarikçi veya müşteri olarak referans verir.
