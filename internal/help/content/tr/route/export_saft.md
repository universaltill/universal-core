---
title: SAF-T Mali Dışa Aktarımı
audience: admin
module: export
order: 1
---

Bu sayfa, kiracının genel muhasebe defterini seçtiğiniz bir tarih
aralığı için bir SAF-T Mali denetim dosyası (Norveç SAF-T v1.30) olarak
indirir — bazı yargı bölgelerinin talep üzerine üretebilmenizi istediği
yasal bir format.

## Ne zaman kullanılır

Bir vergi dairesi veya denetçi belirli bir dönem için genel muhasebe
defterini SAF-T formatında istediğinde kullanın. Doğrudan defterden
okur — dosyaya ait olan kayıtları göndermiş olmak dışında önceden
hazırlanacak bir şey yoktur.

## Bir dosya indirme

1. Finans modülü menüsünden **SAF-T dışa aktarımı**'nı açın, veya
   doğrudan `/export/saft/form` adresine gidin.
2. **Başlangıç** ve **Bitiş** tarihleri varsayılan olarak içinde
   bulunulan takvim yılının başı ve bugüne ayarlıdır — ihtiyacınız olan
   döneme göre düzenleyin. Her ikisi de zorunludur ve **Başlangıç**,
   **Bitiş**'ten sonra olamaz.
3. O aralık için XML dosyasını indirmek üzere **SAF-T dosyasını
   indir**'i seçin.

## Bilinmesi gereken kurallar

- Tüm dosya, indirme başlamadan önce derlenir, bu yüzden onu
  oluştururken bir sorun (kötü bir tarih aralığı, aşağıdaki bir izin
  engeli) gerçek bir hata olarak bildirilir, asla eksik bir dosya
  olarak değil.
- **Dosyanın açıkladığı her şeyi göremiyorsanız, bu sessizce
  sansürlenmez — tamamen reddedilir.** Hesaplar, Vergi Kodları,
  Taraflar ve Taraf Rolleri üzerinde okuma erişimi ve — dosyanın
  gerçekten okuduğu alanlar için (bir Taraf'ın adı, vergi kimliği,
  sicil numarası ve iletişim adı; bir Vergi Kodu'nun kendi alanları) —
  bunlardan hiçbirinin bir Alan İzni tarafından sizden gizlenmemiş
  olmasını gerektirir. Sessizce boş bırakılmış yasal bir alan taşıyan
  şema açısından geçerli bir dosya, hiç dosya olmamasından daha kötü
  olurdu, bu yüzden bu sayfa (ve indirmenin kendisi) bir tane üretmek
  yerine reddeder.
- Kiracının kendi kuruluşu tanımlıysa (bir `own_organization` rolüne
  sahip, tekil bir şekilde belirlenmiş bir Taraf), dosyanın şirket
  profili ondan doldurulur; aksi takdirde şirket alanları boş
  bırakılmak yerine SAF-T standardına göre "NA"ya düşer.
- Her dışa aktarım — indirme gerçekten tamamlansa da tamamlanmasa da —
  kimin çalıştırdığını, hangi tarih aralığı için ve kaç defter kaydı
  içerdiğini kaydeden bir denetim kaydı yazar.

## Neyle bağlantılı

Doğrudan defterin kendi **Yevmiye Kaydı**/Hesap verisini, ticari ortak
ana dosyaları ve kiracının kendi şirket profili için **Taraf**/**Taraf
Rolü**'nü, ve vergi tablosu için **Vergi Kodu**'nu okur. Bu, tek tek
Satın Alma Siparişleri, Satış Siparişleri ve Müşteri Faturaları
üzerinde bulunan belge başına **UBL dışa aktarımı**ndan farklı bir yasal
formattır — SAF-T bir dönem için bütün bir defter dosyasıdır, UBL ise
her seferinde tek bir belgedir.
