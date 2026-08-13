---
title: Proje Bütçesi: Planlanan ve Gerçekleşen
audience: user
module: projects
order: 5
---

Bu rapor, bir projenin Bütçe Kalemleri'nde planlananı, kategori kategori,
şimdiye kadar gerçekleşenle karşılaştırır — projenin kendisinden
ulaşılan, salt okunur bir görünümdür.

## Ne zaman kullanılır

Bir projenin, yalnızca projedeki tek bir Bütçe rakamına değil, kategori
bazında planlanan bütçeye göre nasıl ilerlediğini görmek istediğinizde
açın.

## Raporu açma

Var olan bir projenin formunda **Bütçe/Gerçekleşen** seçeneğini seçin.
Henüz kaydedilmemiş bir proje için açılacak bir şey yoktur — bir
projenin karşılaştırılacak bir şeyi olması için Bütçe Kalemleri'ne ve,
İşçilik kategorisi için, kaydedilmiş zamana ihtiyacı vardır.

## Rakamların okunması

- **Planlanan**, o kategorideki Bütçe Kalemi satırlarının toplamıdır.
- **Gerçekleşen**, sistemin o kategori için şu anda hesaplayabildiği
  değerdir. Bugün bu yalnızca **İşçilik** kategorisidir — projenin
  Görevleri üzerinde kaydedilen her Zaman Girişi saatinin, o saati
  kaydeden kişinin saatlik maliyet oranıyla fiyatlandırılmış değeri.
- Diğer her kategori (Malzeme, Seyahat, Ekipman, Diğer) Gerçekleşen için
  sıfır yerine **Mevcut değil** gösterir. Bu sistemin o kategorilerde
  gerçekte ne harcandığına dair henüz bir kaynağı yok — sıfır göstermek
  "harcama olmadığı doğrulandı" anlamına gelir ki bu doğru olmaz.
- Gerçek, doğrulanmış bir Gerçekleşen sıfır değeri (maliyetsiz kaydedilen
  işçilik) yine de **0** olarak, **Mevcut değil**'den ayrı şekilde
  gösterilir.
- Kaydedilen bazı saatler fiyatlandırılamadıysa (saatleri kaydeden
  kişinin kayıtlı bir maliyet oranı yoksa) İşçilik satırı bunu doğrudan
  belirtir. Bu durumda İşçilik için gösterilen Gerçekleşen değeri yine de
  gerçektir, ancak kısmidir — fiyatlandırılamayan saatleri içermez.
- Rolünüzün Bütçe Kalemlerinin planlanan tutarlarını görme izni yoksa,
  **Planlanan** her satırda bir rakam yerine **Mevcut değil** gösterir —
  doğrulanmış sıfır bütçe olarak yanlış anlaşılabilecek bir **0**
  asla gösterilmez. **Fark** da bu durumda, Gerçekleşen'i gösterilen bir
  satırda bile, her satırda **Mevcut değil** gösterir: Fark, Planlanan
  ile Gerçekleşen'in karşılaştırılmasıdır, dolayısıyla Planlanan sizin
  için görünür değilken gerçek bir rakam olamaz.

## Bilinmesi gereken kurallar

- Bu rapor hiçbir şeyin düzenlenmesine izin vermez — Bütçe Kalemlerini
  değiştirin veya projenin Görevleri üzerinde zaman kaydedin, ardından
  güncellenmiş karşılaştırmayı görmek için buraya dönün.
- Bu raporu görüntülemek; Projeler, Bütçe Kalemleri, Görevler, Zaman
  Girişleri ve Çalışanlar üzerinde okuma erişimi gerektirir. Bir kişinin
  saatlik maliyet oranını hiçbir zaman doğrudan göstermez, yalnızca
  katkıda bulunduğu kategori toplamını gösterir.

## Neyle bağlantılı

Bu rapor bir **Proje** ve onun **Bütçe Kalemleri**'nden okur, İşçilik
gerçekleşenlerini o projenin **Görevleri**'nin kaydedilen **Zaman
Girişleri**'nden, kaydı yapan kişinin **Çalışan** kaydına göre
fiyatlandırarak hesaplar.
