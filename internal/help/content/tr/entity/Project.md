---
title: Proje
audience: user
module: projects
order: 1
---

Proje, bir başlangıç tarihi, isteğe bağlı bir bitiş tarihi ve kendi
bütçesi olan bir iş parçasıdır — bu modüldeki diğer her şeyin (Görevler,
kaydedilen Zaman Kayıtları, planlanan Bütçe Kalemleri) bağlı olduğu
kayıt.

## Ne zaman kullanılır

Bir iş bütününü tek bir birim olarak takip etmeniz gerektiğinde bir
Proje açın: takvimi, kimin ne üzerinde çalıştığı ve ne kadara mal olması
planlandığı. İş bir müşteri içinse, projeye bakan herkesin bu bağlamı
hemen görmesi için müşteriyi bağlayın.

## Proje oluşturma

1. **Proje**'ye gidin ve **Yeni**'yi seçin.
2. Bir Proje Kodu (kendi referansınız) ve bir Ad girin.
3. İsteğe bağlı olarak bu projenin ait olduğu **Müşteri**'yi seçin.
4. Bir **Başlangıç Tarihi** belirleyin. **Bitiş Tarihi** isteğe bağlıdır,
   ancak belirlerseniz başlangıçtan önce olamaz.
5. İsteğe bağlı olarak bir **Para Birimi** ve bir **Bütçe** belirleyin —
   bu projenin toplamda ne kadara mal olması planlandığı.
6. Kaydedin.

Proje oluşturulduktan sonra **Görevler**'ini ve **Bütçe Kalemleri**'ni
doğrudan proje formunda ekleyin — ikisi de önce ayrıca oluşturulmaz,
projenin bir parçası olarak yerinde düzenlenir.

## Bilinmesi gereken kurallar

- Proje Kodu, Ad, Başlangıç Tarihi ve Durum zorunludur. Durum varsayılan
  olarak **Planlandı**'dır.
- Bütçe varsayılan olarak 0'dır ve negatif olamaz.
- Bitiş Tarihi, belirlenmişse, Başlangıç Tarihi'nden önce olamaz.
- Durum gerçek bir proje yaşam döngüsünü izler, düz bir çizgi değil:
  Planlandı → Aktif → Tamamlandı normal yoldur, ancak bir proje işin
  gerçekten duraklayıp yeniden başladığı kadar çok kez Aktif ile
  **Beklemede** arasında gidip gelebilir. **İptal Edildi**'ye Planlandı,
  Aktif veya Beklemede durumlarından ulaşılabilir. Tamamlandı ve İptal
  Edildi ikisi de nihaidir — yeniden başlaması gereken bir proje, yeniden
  açılan bir proje değil, yeni bir projedir.

## Görevler ve bütçe kalemleri

Proje formundaki **Görevler** ve **Bütçe Kalemleri** bölümleri, projeden
ayrılmadan satır ekleyip düzenlemenizi ve kaldırmanızı sağlar. Doğrudan
**Görev** veya **Bütçe Kalemi**'ni açıp oradan da Projesini seçerek bir
tane oluşturabilirsiniz — her iki yol da aynı kayda ulaşır; her ikisi de
yalnızca kendi projesine bağlıyken anlam taşıyan planlama detaylarıdır.

Hiçbir bölüm projenin kendisinde gösterilen bir toplama yuvarlanmaz:
bir projenin tahmini saat toplamı veya planlanan maliyet toplamı,
baktığınız andaki Görevleri veya Bütçe Kalemleri'nin toplamıdır, Proje
kaydında saklanan ve onlarla sessizce senkronizasyondan çıkabilecek bir
sayı değildir. Projedeki **Bütçe**, sizin belirlediğiniz ayrı, tek bir
rakamdır — projenin bütün olarak taahhüt ettiği miktar, Bütçe Kalemleri
bölümünün kaydettiği kategori kategori planlamadan farklıdır.

## Neyle bağlantılı

Bir Proje bir **Müşteri**'ye (herhangi bir Taraf) referans verebilir.
**Görevleri** ve **Bütçe Kalemleri** doğrudan ona aittir (projenin
kaydını silmek onları da kademeli olarak silmez, ama kendi projelerinin
dışında gösterildiklerinde bir anlamları yoktur). Bir Görevin kendi
**Kaydedilen Zamanı** (Zaman Kayıtları) projeye değil, göreve aittir.
