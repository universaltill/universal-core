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
işlem onları bulamaz — bkz. Müşteri Faturası'nın kendi konusu. Yepyeni
bir Hesap, kaydettiğiniz anda defter tarafından kullanılabilir hale
gelir — ayrı bir eşitleme adımına gerek yoktur.

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
- **Aktif** kutucuğu kaydettiğiniz anda etkiye girer — işareti
  kaldırmak, kaydettiğiniz anda defterin bu hesaba karşı yeni işlemleri
  kabul etmesini durdurur.
- Kod benzersiz olmalıdır — sistem, başka bir Hesap tarafından zaten
  kullanılan bir kodu yeniden kullanan ikinci bir Hesabı reddeder.

## Neyle bağlantılı

Bir **Yevmiye Kaydı**'nın her satırı bir Hesabı kendi koduyla
referanslar — bir Müşteri Faturası kesmek, bir Satın Alma Siparişi'ne
karşı mal almak ve vadesi gelen amortismanı işlemek, hepsi hesap
planınızın gerçekten sahip olması gereken Hesap kodları üzerinden işlem
yapar. Bir **Dönem**'in açık/kapalı/kilitli durumu — bu kayıt değil —
defterin bir işlemi kabul etmeden önce kontrol ettiği şeydir; bkz.
Dönem'in kendi konusu. Hesap ve Vergi Kodu, ikisi de SAF-T yasal dışa
aktarımına veri sağlar.

Defter ve SAF-T dışa aktarımı Hesap kayıtlarını doğrudan okumaz —
hesap planınızın ayrı, iç bir kopyasını okurlar; bu kopya, bir Hesap
eklediğinizde, pasife aldığınızda veya türünü değiştirdiğinizde otomatik
olarak güncel tutulur. Bunların hiçbiri için ayrı bir eşitleme adımına
gerek yoktur.

**Bilinmesi gereken daha dar bir özel durum**: mevcut bir Hesabın
**Kod**unu değiştirmek artık defterdeki iç kaydını yerinde yeniden
etiketler — aynı kayıt, hesabı yeni kod altında izlemeye devam eder ve
eski kod artık hiçbir şeye karşılık gelmez. Bir numaralandırma hatasını
düzeltmek için artık yeni bir Hesap oluşturma geçici çözümüne gerek
yok; sadece kodu değiştirmeniz yeterli. Bir yeniden adlandırmanın
reddedilebileceği tek durum: yeni kod, yeniden adlandırmanın bu şekilde
çalışmasından önce kalan, bağlantısız eski bir iç kayıt tarafından hâlâ
tutuluyorsa, kayıt işlemi o kaydı sessizce yeniden kullanmak yerine
tamamen başarısız olur — bu durumda yöneticinize danışın.
