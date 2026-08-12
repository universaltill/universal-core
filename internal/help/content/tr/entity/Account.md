---
title: Hesap
audience: admin
module: finance
order: 1
---

Hesap, hesap planınızdaki tek bir satırdır — işletmenizin parasının ve
yükümlülüklerinin sınıflandırıldığı kutuların listesi (Kasa, Alacak
Hesapları, Satış Geliri ve benzeri). Sistemin işlediği her yevmiye kaydı
Hesapları borçlandırır ve alacaklandırır; başka hiçbir yere işlem
yapılmaz.

## Ne zaman kullanılır

Çoğu kiracı hesap planını erkenden bir kez kurar ve sonrasında nadiren
dokunur — çoğunlukla yeni bir hesap eklemek veya artık kullanılmayan
birini pasife almak için. Kendiniz kurmadığınız bazı hesaplar sistem
tarafından zaten bekleniyor olabilir: **Satış Geliri (kod 4100)** ve
**Alacak Hesapları (kod 1200)**, bir Müşteri Faturası'nı kesmenin işlem
yaptığı kodlardır; hesap planınız tam olarak bu kodları kullanmıyorsa, bu
işlem onları bulamaz — bkz. Müşteri Faturası'nın kendi konusu. Bu
sürümde, yepyeni bir Hesap, kaydettiğiniz anda defter tarafından
kullanılabilir hale gelmez — nedeni için aşağıdaki "Neyle bağlantılı"
bölümüne bakın.

## Hesap oluşturma

1. **Hesap**'a gidin ve **Yeni**'yi seçin.
2. **Kod**unu (hesap planı numaralandırmanız, örn. "1000") ve **Ad**ını
   girin.
3. **Tür**ünü seçin: Varlık, Yükümlülük, Özkaynak, Gelir veya Gider.
4. İsteğe bağlı olarak, başka bir hesabın altına yerleştirmek için bir
   **Üst Hesap** (örn. "1200 Alacak Hesapları"nı "1000 Varlıklar"ın
   altına) ve bu hesap belirli bir para biriminde izlenecekse bir **Para
   Birimi** ayarlayın.
5. Hesabı kapatmıyorsanız **Aktif** işaretli kalsın.
6. Kaydedin.

## Bilinmesi gereken kurallar

- Kod ve Ad zorunludur.
- Üst Hesap başka herhangi bir Hesap olabilir — iç içe geçirme sınırsızdır,
  ancak sistem döngüsel bir zincir (bir hesabın kendi atası olarak
  ayarlanması) kontrol etmez, bunu elle oluşturmaktan kaçının.
- **Aktif** kutucuğu bu kayıt üzerinde bir etikettir — bu sürümde tek
  başına defterin bu hesaba karşı işlemleri kabul etmesini durdurmaz.
  Defterin pasife alınmış bir hesabın kodunu gerçekten reddedip
  reddetmeyeceği, işareti kaldırdığınızdan bu yana bir yöneticinin
  defterin kendi hesap planı kopyasını yeniden eşitleyip eşitlemediğine
  bağlıdır (aşağıdaki "Neyle bağlantılı" bölümüne bakın); bu gerçekleşene
  kadar işlemler tam olarak öncekiyle aynı şekilde geçmeye devam eder.
- Kod, şema düzeyinde benzersizliği zorunlu kılınmaz — kendi
  numaralandırmanızı tutarlı tutmayı bir kural olarak benimseyin, sistem
  sizin için bir yinelemeyi yakalamaz.

## Neyle bağlantılı

Bir **Yevmiye Kaydı**'nın her satırı bir Hesabı kendi koduyla
referanslar — bir Müşteri Faturası kesmek, bir Satın Alma Siparişi'ne
karşı mal almak ve vadesi gelen amortismanı işlemek, hepsi hesap
planınızın gerçekten sahip olması gereken Hesap kodları üzerinden işlem
yapar. Bir **Dönem**'in açık/kapalı/kilitli durumu — bu kayıt değil —
defterin bir işlemi kabul etmeden önce kontrol ettiği şeydir; bkz.
Dönem'in kendi konusu. Hesap ve Vergi Kodu, ikisi de SAF-T yasal dışa
aktarımına veri sağlar.

**Bu sürümde bilinmesi gereken gerçek bir kısıtlama**: defter ve SAF-T
dışa aktarımı Hesap kayıtlarını doğrudan okumaz — yalnızca bir yöneticinin
(bu ekranın dışında, burada kendinizin tetikleyebileceği bir şey değil)
çalıştırdığı bir eşitleme adımının güncel tuttuğu, ayrı, iç bir hesap
planı kopyasını okurlar. Bir Hesap eklemek, adını değiştirmek, türünü
değiştirmek veya pasife almak, bu eşitleme bir sonraki kez çalışana kadar
defterin neyi kabul edeceği veya bir SAF-T dışa aktarımının ne
göstereceği üzerinde hiçbir etki yapmaz. Burada az önce yaptığınız bir
değişiklik bir işlemde veya dışa aktarımda yansımıyorsa, nedeni budur —
yöneticinizden yeniden eşitlemesini isteyin.
