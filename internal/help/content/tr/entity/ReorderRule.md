---
title: Yeniden Sipariş Kuralı
audience: user
module: purchasing
order: 14
---

Yeniden Sipariş Kuralı, ürün başına bir yenileme politikasıdır — satın
alma raporunun size bir üründen daha fazla satın almanın zamanı
geldiğini söylediği nokta ve bunun üzerine ne kadar tampon tutmak
istediğiniz.

## Ne zaman kullanılır

Satın alma raporunun izlemesini ve sinyal vermesini istediğiniz herhangi
bir Ürün için bir Yeniden Sipariş Kuralı kurun. Biri olmadan, o Ürün,
stoğu ne kadar düşerse düşsün, raporun yeniden sipariş bölümünde asla
görünmez.

## Kural oluşturma

1. **Yeniden Sipariş Kuralı**'na gidin ve **Yeni**'yi seçin.
2. **Ürün**'ü seçin.
3. **Yeniden Sipariş Noktası**'nı girin — kendisinde veya altında
   yeniden sipariş vermeniz gerektiğinin söylenmesini istediğiniz stok
   pozisyonu.
4. İsteğe bağlı olarak **Emniyet Stoku**'nu girin — Yeniden Sipariş
   Noktası'nın üzerine eklenen ekstra bir tampon. Varsayılan olarak
   0'dır, yani tampon yok demektir.
5. **Tedarik Süresi Güven Düzeyi**'ni seçin: P50 veya P90. Bu, satın
   alma raporundaki "şu tarihe kadar sipariş ver" rehberliğinin ne kadar
   ihtiyatlı olduğuna karar verir — P90 (varsayılan), medyan tedarikçi
   teslimatından daha yavaş, daha az elverişli bir tedarik süresi
   varsayar, bu yüzden size daha erken sipariş vermenizi söyler; P50 ise
   bunun yerine yazı-tura medyanıdır.
6. Kaydedin.

## Sinyali ne tetikler

Satın alma raporu, bir ürünün envanter pozisyonunu — eldeki miktar artı
açık Satın Alma Siparişleri'nde zaten olan miktar — bu kuralın Yeniden
Sipariş Noktası artı Emniyet Stoku'na karşı karşılaştırır. Bu pozisyon
birleşik eşiğe veya altına düştüğünde, ürün, seçtiğiniz Tedarik Süresi
Güven Düzeyi'ne dayalı önerilen zamanlamayla raporun yeniden sipariş
bölümünde görünür. Bu kural yalnızca politikayı tutar; tüm bu
karşılaştırma burada değil, raporun kendisinde gerçekleşir.

## Bilinmesi gereken kurallar

- Ürün, Yeniden Sipariş Noktası ve Tedarik Süresi Güven Düzeyi'nin
  hepsi zorunludur.
- Yeniden Sipariş Noktası ve Emniyet Stoku negatif olamaz.
- Bir Ürün'ün pratikte en fazla bir aktif kurala sahip olması beklenir —
  sistemde aynı Ürün için ikinci bir tane oluşturmanızı engelleyen
  hiçbir şey yoktur, ancak ürün başına birden fazla kural olduğunda
  raporun kendi davranışı güvenilecek bir şey değildir.

## Neyle bağlantılı

Bir Yeniden Sipariş Kuralı bir **Ürün**'e referans verir. Sinyali,
tetiklendiğinde, o Ürün'ün mevcut **Stok Kalemi** pozisyonu ve herhangi
bir açık **Satın Alma Siparişi** miktarıyla birlikte satın alma
raporunda gösterilir.
