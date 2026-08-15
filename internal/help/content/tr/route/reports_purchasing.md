---
title: Satın Alma Raporu
audience: user
module: purchasing
order: 15
---

Satın Alma Raporu, satın alma ve stok zekası verileri üzerinde tek,
salt okunur bir gösterge panelidir — satın alma siparişi durumu ve
tedarikçi harcaması, mevcut stok seviyeleri, tedarikçi performansı ve
yeniden sipariş sinyalleri, hepsi tek bir sayfada.

## Ne zaman kullanılır

Tek tek Satın Alma Siparişi veya Kalem kayıtlarını didiklemek yerine,
satın alma faaliyeti ve stok sağlığına genel bir bakış istediğinizde
açın.

## Raporu açma

Satın Alma modülü menüsünden **Raporlar**'ı seçin, veya doğrudan
`/reports/purchasing` adresine gidin. Yapılandırılacak hiçbir şey
yoktur — her zaman kiracının güncel verisini yansıtır.

## Bölümler, sırasıyla

- **Duruma Göre Satın Alma Siparişleri** — duruma göre gruplanmış
  sipariş sayıları ve toplam değer.
- **Harcamaya Göre En İyi Tedarikçiler** — toplam Satın Alma Siparişi
  değerine göre sıralanmış tedarikçiler.
- **Stok Özeti** — her tesis genelinde takip edilen kalemler, toplam
  eldeki miktar ve toplam taahhüt edilebilir miktar.
- **Stok Tükenme Riski** — taahhüt edilebilir hiçbir şeyi kalmamış
  kalemler, her kalemin kendi kaydına bağlantı verir.
- **Tedarikçi Tedarik Süreleri** — her tedarikçi için, o tedarikçinin
  kendi tamamlanmış Satın Alma Siparişlerinden hesaplanan gün cinsinden
  P50 ve P90 sipariş-teslim alma süresi.
- **Zamanında Teslimat** — tamamlanan siparişlerin söz verilen teslimat
  tarihinde veya öncesinde teslim alınma oranı, tedarikçi başına.
- **Kalite** — kayıtlı Mal Kabul Satırı inceleme sonuçlarından kabul-ret
  oranı, tedarikçi başına.
- **Yeniden Sipariş Sinyalleri** — mevcut envanter pozisyonu Yeniden
  Sipariş Kuralı eşiğine düşmüş veya altına inmiş kalemler, yeni bir
  siparişin kabaca ne kadar süreceğini bilmeniz için beklenen bir
  tedarik süresiyle birlikte.

## Bilinmesi gereken kurallar

- **Her istatistik, herhangi bir sayı göstermeden önce en az 2
  tamamlanmış veri noktasına ihtiyaç duyar** — Tedarikçi Tedarik
  Süreleri, Zamanında Teslimat ve Kalite'nin hepsi bunun altında yanlış
  yönlendirici derecede kesin tek örnekli bir yüzde yerine **Yetersiz
  geçmiş** gösterir.
- **Bir yeniden sipariş sinyali sadece eldekine değil pozisyona
  dayanır**: envanter pozisyonu, o kalem için herhangi bir açık Satın
  Alma Siparişindeki teslim edilmemiş miktar *artı* eldeki miktardır.
  Zaten büyük bir açık PO'su olan bir kalem, sadece raftaki fiziksel
  miktar şu anda düşük olduğu için tetiklenmez — mallar zaten yolda.
- Bir sinyal, bu pozisyon kalemin Yeniden Sipariş Kuralı'nın **Yeniden
  Sipariş Noktası**'na (ve varsa Güvenlik Stoku'na) düştüğünde veya
  altına indiğinde tetiklenir.
- Tetiklenen bir sinyalin yanında gösterilen beklenen tedarik süresi, o
  kalemin *en son* Satın Alma Siparişindeki tedarikçinin (Yeniden
  Sipariş Kuralı'nın kendi güven ayarına göre) P50 veya P90 rakamını
  kullanır. O tedarikçinin kendi yeterli geçmişi yoksa, rapor tüm
  tedarikçiler genelindeki genel rakama geri döner — **(tüm
  tedarikçiler)** olarak etiketlenir, böylece filo genelindeki bir
  rakamı asla o tek tedarikçinin kendi geçmiş performansıyla
  karıştırmazsınız.
- Bu rapor, gösterilen hiçbir şey üzerinde işlem yapmanıza asla izin
  vermez — yeniden sipariş yok, düzenleme yok. Salt okunurdur; bir stok
  tükenmesi veya yeniden sipariş satırından en hızlı sonraki adım, onun
  Kalem'e olan bağlantısını izlemektir.
- Raporu görüntülemek, üzerine kurulduğu her varlık türü için okuma
  erişimi gerektirir; bunlardan herhangi biri sizin için kısıtlıysa,
  kısmen sansürlenmez — bir bütün olarak sayfa reddedilir.

## Neyle bağlantılı

**Satın Alma Siparişi**, **Sipariş Satırı**, **Mal Kabul**, **Mal Kabul
Satırı**, **Kalem**, **Envanter Kalemi**, **Taraf** (tedarikçiler) ve
**Yeniden Sipariş Kuralı**'ndan okur — bu sayfadan hiçbir şey
düzenlenemez; her rakam sistemin başka bir yerindeki o kayıtlara kadar
izlenebilir. İlgili **Teklif Talebi Tedarikçi Karşılaştırması** raporu
(yine Raporlar altında), tek bir Teklif Talebi için tedarikçi
fiyatlandırmasını kapsar, bu sayfanın kapsamadığı bir şeydir.
