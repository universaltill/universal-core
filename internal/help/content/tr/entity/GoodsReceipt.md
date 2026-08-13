---
title: Mal Kabul
audience: user
module: purchasing
order: 4
---

Mal Kabul, malların bir Satın Alma Siparişi'ne karşı fiziksel olarak
geldiğini kaydeder — nereye, ne zaman geldiklerini ve (satırları
aracılığıyla) ne ve ne kadar geldiğini. Tek bir Satın Alma Siparişi
genellikle birden fazla teslimatla teslim alınır, bu yüzden bir mal
kabul kaydetmek, siparişin kendisinde bir durum değişikliği değil,
tekrarlanabilir bir olaydır.

## Ne zaman kullanılır

Bir Satın Alma Siparişi'ne karşı bir teslimat gerçekten geldiğinde her
seferinde bir Mal Kabul oluşturun — kısmi bir teslimat da dahil. Gelen
kısmı kaydetmek için siparişin tamamının gelmesini beklemenize gerek
yoktur.

## Mal kabul kaydetme

1. **Mal Kabul**'e gidin ve **Yeni**'yi seçin.
2. Bu teslimatın karşısında olduğu **Satın Alma Siparişi**'ni seçin.
3. **Teslim Alma Tarihi**'ni girin.
4. Malların geldiği **Tesis**'i seçin — bu zorunludur ve hangi Stok
   Kalemi kaydının alacaklandırılacağını belirleyen şey budur.
5. **Kalemler** bölümünde satır ekleyin: gerçekten teslim alınan her ürün
   için miktar. İsteğe bağlı olarak, geleni denetlerseniz, **Kabul Edilen
   Miktar** ve **Reddedilen Miktar**'ı kaydedin — aşağıya bakın.
6. Kaydedin.

## Bir satırı kaydetmek gerçekte ne yapar

Bir satır kaydedildiği anda, yalnızca o satır için iki şey aynı anda
gerçekleşir:

- **Stok artar.** Teslim alınan miktar, o ürün için o tesisteki Stok
  Kalemi kaydına alacaklandırılır (henüz yoksa bir tane oluşturularak).
- **Bir yevmiye kaydı işlenir**, satırın değeri için (miktar × Sipariş
  Satırı'nın Birim Fiyatı) **Stok'a borç, Borç Hesapları'na alacak**
  kaydedilerek. İşlenecek bir değeri olmayan bir satır (örneğin ücretsiz
  bir numune) yine de stoğu alacaklandırır, defterde hiçbir şey
  işlenmese bile.

Bu, satır ilk kaydedildiğinde bir kez gerçekleşir — ayrı bir "işleme"
adımı yoktur ve bir satır teslim alındıktan sonra işlemi geri almanın
bir yolu yoktur.

## Bilinmesi gereken kurallar

- Satın Alma Siparişi, Teslim Alma Tarihi ve Tesis, başlıkta zorunludur;
  her satırda bir satırın Ürün'ü ve Teslim Alınan Miktar'ı zorunludur.
- Kabul Edilen Miktar ve Reddedilen Miktar isteğe bağlıdır — çoğu mal
  kabul bunları hiç kaydetmez. İkisinden birini kaydederseniz, ikisini de
  kaydetmeniz gerekir ve toplamları Teslim Alınan Miktar'a eşit olmalıdır:
  bunlar gelenin bir kalite ayrımıdır, ikinci, bağımsız bir sayım değil.
- Bir Mal Kabul'ün kendi durumu yoktur — bir tane kaydetmek olayın
  kendisidir; içinden geçirilecek bir taslak veya onay aşaması yoktur.
- Bir Satın Alma Siparişi'ne karşı teslim almak, o siparişin kendi Durum
  alanını değiştirmez — bir Satın Alma Siparişi'ni Teslim Alındı'ya
  taşımak ayrı, elle yapılan bir adımdır.

## Neyle bağlantılı

Bir Mal Kabul bir **Satın Alma Siparişi**'ne aittir ve bir veya daha
fazla **Mal Kabul Satırı** satırına sahiptir. Satırları, bir
**Tedarikçi Faturası**'nın eşleştirmesinin karşılaştırdığı ve seçilen
**Tesis**'teki bir **Stok Kalemi**'nin miktarını alacaklandıran şeydir.
