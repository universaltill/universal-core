---
title: Vergi Kodu
audience: admin
module: finance
order: 4
---

Vergi Kodu, bir belge satırının işaret edebileceği adlandırılmış bir
vergi oranıdır — "KDV %20", "Stopaj %5" ve benzeri. Yalnızca referans
verisidir: bu sistem, belirli bir işleme hangi vergi kodunun uygulandığını
hesaplamaz veya sizin için vergi tutarlarını hesaplamaz. Hangi kodun
hangi satış veya alıma uygulandığı ve ülkeye özgü herhangi bir vergi
mantığı, bilinçli olarak temel üründen dışarıda tutulur — bu karar başka
bir yerde (sizin tarafınızdan veya bölgeye özgü bir eklenti tarafından)
verilir, bu kayıt tarafından değil.

## Ne zaman kullanılır

İşletmenizin gerçekten kullandığı Vergi Kodlarını erkenden bir kez kurun
— çoğu kiracının yalnızca birkaçına ihtiyacı vardır (standart bir oran,
indirimli bir oran, belki bir stopaj oranı).

## Vergi kodu oluşturma

1. **Vergi Kodu**'na gidin ve **Yeni**'yi seçin.
2. Bir Kod ve Ad girin (örn. "KDV20", "Standart KDV").
3. Oranı yüzde olarak girin — %5 için "5" girin, "0.05" değil.
4. Vergi Türü'nü seçin: KDV, Stopaj veya Satış Vergisi.
5. İsteğe bağlı olarak bir Yetki Alanı girin (serbest metin bir not —
   ülke, bölge, kodları birbirinden ayırt etmenize yardımcı olacak her
   ne ise).
6. Kaydedin.

## Bilinmesi gereken kurallar

- Kod, Ad, Oran ve Vergi Türü zorunludur.
- Oran negatif olamaz, ancak bilinçli olarak bir üst sınır yoktur — bazı
  bölgelerde %100'ün üzerinde bileşik veya lüks oranlar vardır ve bu alan
  bunu sorgulamaz.
- Oranı tam bir yüzde sayısı olarak girin (%5 için 5), kesir olarak
  değil.
- Kod, şema düzeyinde benzersizliği zorunlu kılınmaz — kendi
  numaralandırmanızı tutarlı tutun.

## Neyle bağlantılı

Vergi Kodu, **Hesap** ile birlikte SAF-T yasal dışa aktarımına veri
sağlar. Ürünün bu ilk sürümünde henüz hiçbir şey bir Satış Siparişi'ne
veya Müşteri Faturası satırına bir Vergi Kodu atamıyor — bu bağlantı
ayrı, gelecekteki bir iştir.
