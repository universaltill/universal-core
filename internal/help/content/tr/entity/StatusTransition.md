---
title: Durum Geçişi
audience: both
module: foundation
order: 16
---

Durum Geçişi, bir **Durum Türü**nün yaşam döngüsündeki tek bir yasal
hareketi tanımlar: bir **Durum**dan diğerine. İki durumu birbirine bağlayan
bir Durum Geçişi satırı yoksa, bir kaydı bu ikisi arasında doğrudan
taşımaya izin verilmez — bir satırın yokluğu, ayrıca yapılandırmanız
gereken ayrı bir kural değil, bir hareketi yasadışı kılan şeydir.

## Ne zaman kullanılır

Durum Türü ve Durum'da olduğu gibi, çoğu kiracı bunları hiç elle
oluşturmaz — Satın Alma gibi bir modül, etkinleştirildiğinde kendi kayıt
türlerinin ihtiyaç duyduğu geçişleri tohumlar. Durum Geçişi satırlarını
yalnızca yeni bir Durum Türü üzerinde tamamen yeni bir yaşam döngüsü
kurarken kendiniz eklersiniz.

## Bir geçiş ekleme

1. **Durum Geçişi**'ne gidin ve **Yeni**'yi seçin.
2. Durum Türünü, **Kaynak** durumu ve **Hedef** durumu seçin.
3. İsteğe bağlı olarak bu hareketi kısıtlaması gereken bir iş akışı
   adlandırın — gelecekte kullanım için kaydedilir, ancak sistem
   tarafından henüz kendiliğinden uygulanmaz.
4. Kaydedin.

## Bilinmesi gereken kurallar

- Yön önemlidir: Taslak'tan Onaylandı'ya bir satır, Onaylandı'dan geri
  Taslak'a da izin vermez — ters hareketin de yasal olması gerekiyorsa
  ikinci bir satır ekleyin.
- Satın Alma Siparişi Durumundan doğrulanmış bir örnek: Taslak →
  Gönderildi, Gönderildi → Onaylandı ve Onaylandı → Alındı normal yolu
  oluşturur. Taslak → İptal Edildi, Gönderildi → İptal Edildi ve Onaylandı
  → İptal Edildi de yasaldır ve bir siparişin alınmadan önce herhangi bir
  noktada iptal edilmesine izin verir — ancak Alındı'dan veya İptal
  Edildi'den çıkan bilerek hiçbir geçiş yoktur, çünkü ikisi de nihai kabul
  edilir: zaten alınmış veya iptal edilmiş bir satın alma siparişi başka
  bir duruma geri düzenlenmez, bunun yerine yeni bir belge oluşturulur.
- Kaydın mevcut durumundan eşleşen bir Durum Geçişi satırı olmayan bir
  durum ayarlayan bir güncelleme girişimi, yasadışı kaynak/hedef çiftini
  adlandıran bir hatayla doğrudan reddedilir.

## Neyle bağlantılıdır

Her Durum Geçişi bir **Durum Türü** ile bağladığı iki **Durum** kaydını —
başlangıç ve bitiş noktasını — adlandırır.
