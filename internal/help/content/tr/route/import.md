---
title: Kayıtları İçe Aktar
audience: user
module: import
order: 1
---

İçe Aktar, bir CSV veya XLSX dosyasından tek bir varlık türü için toplu
kayıt oluşturmanızı sağlar — hiçbir şey yazılmadan önce onayladığınız
bir eşleme adımıyla birlikte.

## Ne zaman kullanılır

Kayıtları tek tek elle girmek yerine, bir tedarikçi listesi, başlangıç
ürün kataloğu veya toplu bir kişi grubu gibi aynı anda oluşturulacak çok
sayıda kayıt olduğunda kullanın.

## Baştan sona bir dosya içe aktarma

1. Varlığın kendi liste sayfasından (**Yeni** ve **Dışa Aktar**'ın
   yanında) **İçe Aktar**'ı açın, veya doğrudan `/import/{VarlıkTürü}`
   adresine gidin.
2. Bir `.csv` veya `.xlsx` dosyası (en fazla 50 MiB) seçin ve
   **Önizle**'yi seçin.
3. Sistem, dosyanızın sütunlarını varlığın alanlarıyla eşleştirmeyi
   önerir — sütun adlarını alan adlarıyla otomatik olarak eşleştirerek.
   Bu kiracı için bir yapay zeka sağlayıcısı yapılandırılmışsa, ada göre
   eşleştirilemeyen sütunlar için yapay zeka önerisi kullanılır ve
   **(Yapay zeka önerisi — lütfen onaylayın)** olarak işaretlenir —
   devam etmeden önce bunları her zaman iki kez kontrol edin.
4. Her sütunun yanındaki açılır menüyü kullanarak eşlemeyi gözden geçirin
   ve gerekirse düzeltin, ardından tekrar **Önizle**'yi seçin. Hiçbir
   şey işlenmeden önce her satır varlığın kendi kurallarına göre
   doğrulanır ve **Tamam** veya **Hata** durumuyla gösterilir.
5. Eşleme tamamlandığında ve önizlemeden memnun olduğunuzda,
   **Uygula**'yı seçin. Doğrulamayı geçen satırlar oluşturulur;
   başarısız olanlar oluşturulmaz ve asla kısmen yazılmaz.
6. Sonuç ekranı kaç satırın başarılı olduğunu, kaç satırın başarısız
   olduğunu ve her başarısızlığın nedenini bildirir.

## Bilinmesi gereken kurallar

- **Önizleme sırasında hiçbir şey yazılmaz** — yalnızca Uygula gerçekten
  kayıt oluşturur. Eşlemeyi düzenledikten sonra yeniden önizlemek her
  zaman güvenlidir.
- **Doğrulamada başarısız olan bir satır diğerlerini engellemez.**
  Uygula satır satır çalışır: aynı çalıştırmada bazı satırlar başarılı
  olurken bazıları başarısız olabilir.
- **Görme izniniz olmayan bir alan eşleme hedefi olamaz.** Eşleme açılır
  menüleri asla gizli bir alanı sunmaz ve bir alanı elle eşlemek (veya
  gizli bir Zorunlu alanı eşlemesiz bırakmak) alanı adlandırmadan genel
  bir mesajla reddedilir.
- Uygula, seçtiğiniz aynı dosyayı yeniden gönderir — bu sayfadaki dosya
  girişi Önizle adımıyla asla temizlenmez, bu yüzden Önizle ile Uygula
  arasında olduğu gibi bırakın.

## Neyle bağlantılı

İçe Aktar, elle başka bir şekilde oluşturulan kayıtlara zaten uygulanan
CRUD kuralları ve alan izinleriyle, aynı anda tek bir varlık türünü
hedefler — içe aktarılan bir satır, elle girilen bir kayıtla tamamen
aynı şekilde doğrulanır ve yazılır. Verileriniz bir dosya yerine başka
bir canlı veritabanında yaşıyorsa, bu sayfadan ulaşılan alternatif için
**SQL Kaynağından İçe Aktar**'a bakın.
