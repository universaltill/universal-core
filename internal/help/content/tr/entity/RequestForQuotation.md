---
title: Teklif Talebi
audience: user
module: purchasing
order: 7
---

Teklif Talebi (RFQ), bir Satın Alma Siparişi'ne bağlanmadan önce bir grup
ürün için fiyatlandırma talebiyle bir veya daha fazla tedarikçiye
gönderilen bir istektir. Tedarikçileri karşılaştırırken kullanın, kimden
satın alacağınıza zaten karar verdiğinizde değil.

## Ne zaman kullanılır

Hâlâ fiyatlandırma topluyorken bir Teklif Talebi oluşturun — bir
tedarikçi ve fiyat seçtiğinizde bunun yerine bir **Satın Alma Siparişi**
oluşturun. Sistemde bir Teklif Talebi'ni sizin için bir Satın Alma
Siparişi'ne dönüştüren hiçbir şey yoktur; teklifleri burada
karşılaştırmak bu kaydın yaptığı son adımdır.

## Teklif talebi oluşturma ve gönderme

1. **Teklif Talebi**'ne gidin ve **Yeni**'yi seçin.
2. Bir **Teklif Talebi Numarası** ve bir **Son Tarih** girin.
3. Kaydı oluşturmak için bir kez kaydedin, ardından **Tedarikçiler**
   bölümüne — teklif vermeye davet ettiğiniz her tedarikçi için bir
   satır — ve **Kalemler** bölümüne — fiyatlandırılmasını istediğiniz
   her ürün için bir miktarla bir satır — satır ekleyin.
4. Davet ettiğiniz tedarikçilere gerçekten gönderdikten sonra, Durum'u
   **Gönderildi**'ye taşıyın ve kaydedin. Bu, olanın bir kaydıdır,
   sistemin sizin için yaptığı bir şey değil — göndermenin kendisi bu
   kaydın dışında gerçekleşir.
5. Yanıtlar geldikçe, her tedarikçinin her satır için fiyatını bir
   **Tedarikçi Teklif Satırı** olarak kaydedin (o konuya bakın).
   Yanıtları kaydetmeye başladığınızda, Durum'u **Teklifler Alındı**'ya
   taşıyın.
6. Her tedarikçinin fiyatını yan yana görmek için **Teklifleri
   Karşılaştır** (bu formdaki bir eylem) kullanın.
7. Karar verdikten sonra — bir tedarikçi seçtiğinizde veya elinizde bir
   cevapla Teklif Talebi'nden vazgeçtiğinizde — Durum'u **Kapatıldı**'ya
   taşıyın.

## Teklifleri karşılaştırma

**Teklifleri Karşılaştır** eylemi salt okunur bir rapor açar: talep
edilen her satır için bir satır, davet edilen her tedarikçi için bir
sütun, yanıt verdikleri yerde her tedarikçinin teklif ettiği fiyat
(yanıt vermedikleri yerde gerçek bir boşluk — asla uydurma bir sıfır
değil), her satırda en düşük fiyat işaretlenmiş ve gerçekten teklif
verdikleri satırlar genelinde her tedarikçinin kendi teklif toplamını
toplayan bir alt bilgiyle birlikte. Bu rapor kararınızı bilgilendirmekten
başka hiçbir şey yapmaz — burada bir "kazanan teklifi seç" eylemi yoktur
ve bir Teklif Talebi'ni kapatmak bir Satın Alma Siparişi oluşturmaz.

## Bilinmesi gereken kurallar

- Teklif Talebi Numarası, Son Tarih ve Durum'un hepsi zorunludur.
- Normal yol Taslak → Gönderildi → Teklifler Alındı → Kapatıldı'dır.
  İptal, Taslak, Gönderildi veya Teklifler Alındı durumundan
  mümkündür, ancak Kapatıldı'dan sonra mümkün değildir — bu noktada
  karşılaştırma tamamlanmıştır ve yeniden açmak yeni bir Teklif
  Talebi'dir.
- Gönderildi ve Teklifler Alındı üzerinden ilerlemek eldedir — bir
  tedarikçi, bir satır veya bir teklif satırı eklemekle ilgili hiçbir
  şey Durum'u otomatik olarak ilerletmez.

## Neyle bağlantılı

Bir Teklif Talebi'nin kendi bölümlerinde **Tedarikçi** satırları (kimin
davet edildiği) ve **Kalem** satırları (neyin fiyatlandırıldığı) vardır.
Davet edilen her tedarikçinin her satıra verdiği yanıt, ayrı bir
**Tedarikçi Teklif Satırı** kaydıdır. Bunların hiçbiri tek başına bir
**Satın Alma Siparişi** oluşturmaz — bu, karar verdikten sonra ayrı,
elle yapılan bir adımdır.
