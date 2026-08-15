---
title: SQL Kaynağından İçe Aktar
audience: user
module: import
order: 2
---

Bu, kayıt getirmenin ikinci yoludur — bir CSV/XLSX dosyası yerine, kayıtlı
bir dış veritabanından (ağ üzerinden ulaşılan, eski bir sistemin kendi
veritabanı) doğrudan satır çeker ve bir dosya içe aktarımıyla aynı eşleme
ve önizleme adımlarında size yol gösterir.

## Ne zaman kullanılır

Kaynak veri, önce bir dosyaya aktarmanız gereken bir şey yerine
bağlanabildiğiniz canlı bir veritabanında (eski bir ERP, eski bir
muhasebe sistemi) yaşıyorsa kullanın. Bir yönetici bu veritabanını önce
**Dış SQL Kaynağı** olarak kaydetmelidir (Ayarlar → SQL Kaynakları); bu
sayfa yalnızca zaten kayıtlı kaynaklardan okur.

## Bir kaynaktan baştan sona içe aktarma

1. Normal İçe Aktar sayfasından **SQL kaynağından içe aktar**'ı açın
   (veya doğrudan `/import/{VarlıkTürü}/sql` adresine gidin). Henüz
   kayıtlı bir kaynak yoksa, bu sayfa bunu belirtir ve bir tarayıcı
   göstermek yerine Ayarlar → SQL Kaynakları'na bağlantı verir.
2. Kayıtlı bir kaynak seçin ve tablolarını ve görünümlerini görmek için
   **Tabloları tara**'yı seçin.
3. İçe aktarmak istediğiniz tabloyu veya görünümü seçin. Bilinen bir
   tedarikçi şablonuyla eşleşiyorsa (örneğin bir NAV 2009 kalem veya
   müşteri tablosu), sütun eşlemesi bu şablondan önceden doldurulur ve
   bu şekilde işaretlenir — herhangi bir önerilen eşleme gibi gözden
   geçirin. Aksi takdirde eşleme, dosya içe aktarımıyla aynı şekilde
   sütun adlarından tahmin edilir.
4. İsteğe bağlı olarak bir **Anahtar sütun** seçin. Bir tane seçiliyken,
   aynı içe aktarmayı daha sonra tekrar çalıştırmak yinelenen kayıtlar
   oluşturmak yerine daha önce oluşturduğu kayıtları *günceller*;
   seçilmediğinde, kaynak verisi aynı olsa bile her çalıştırma yeni
   kayıtlar oluşturur.
5. Eşlemeyi gözden geçirin, ardından içe aktarılacak satırları görmek
   için **Önizle**'yi seçin (bir dosya içe aktarımının satırlarının
   doğrulandığı gibi doğrulanır) ve bunları gerçekten yazmak için
   **Uygula**'yı seçin.

## Bilinmesi gereken kurallar

- **Bir çalıştırma en fazla 10.000 satır içe aktarır.** Önizlenen
  satırlar uygulama sırasında yeniden getirilir, bu yüzden kaynak
  verisi önizleme ile uygulama arasında değişmemelidir.
- Yeniden içe aktarmada güncelleme (Anahtar sütun) özelliğinin
  yayınlanmış bir kiracı geneli kimlik özelliğine ihtiyacı vardır;
  yayınlanmamışsa bu sayfa bunu belirtir ve içe aktarma yine de çalışır,
  yalnızca her zaman yeni kayıtlar olarak.
- Bir dosya içe aktarımıyla aynı gizli alan korumaları geçerlidir:
  görme izniniz olmayan bir alan asla eşleme hedefi olarak sunulmaz.
- Dış veritabanıyla konuşurken yaşanan bir bağlantı sorunu burada genel
  bir şekilde bildirilir — teknik ayrıntı yalnızca sunucu günlüğündedir
  — ayarlar sayfasının kendi **Bağlantıyı Test Et** eyleminde olduğu
  gibi.

## Neyle bağlantılı

Kaynağın kendisi, bu sayfanın taranacak bir şeyi olmadan önce **Dış SQL
Kaynağı** ayarlar sayfasında (yalnızca yönetici) yapılandırılır. Getirme
işleminin ötesindeki her şey — eşleme, önizleme, uygulama, gizli alan
koruması — düz CSV/XLSX **İçe Aktar** sayfasının kullandığı aynı
mekanizmadır; satırları okunduktan sonra dış bir tablo ile yüklenen bir
dosya aynı şekilde ele alınır.
