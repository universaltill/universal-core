---
title: Durum
audience: both
module: foundation
order: 15
---

Durum, bir **Durum Türü**nün yaşam döngüsü içindeki tek bir izin verilen
durumdur — örneğin, Satın Alma Siparişi Durumu içinde "Taslak,"
"Gönderildi" veya "Onaylandı." Bir kaydın hangi Durum değerleri arasında
gerçekten hareket edebileceği, bu kaydın kendisi tarafından değil, **Durum
Geçişi** tarafından tanımlanır.

## Ne zaman kullanılır

Durum Türünde olduğu gibi, bunları genellikle Satın Alma veya başka bir
modülün sizin için zaten kurduğu bir kayıt türü için elle
oluşturmazsınız — önceden tohumlanmış olarak gelirler. Durum kayıtlarını
yalnızca yeni bir Durum Türü üzerinde sıfırdan tamamen yeni bir yaşam
döngüsü tanımlarken kendiniz eklersiniz.

## Durum oluşturma

1. **Durum**'a gidin ve **Yeni**'yi seçin.
2. Ait olduğu Durum Türünü seçin, bir kod ve ad girin.
3. Bir sıra numarası yalnızca görüntüleme sırasını kontrol eder — başka
   bir etkisi yoktur.
4. Yeni bir kayda bu durumda başlanmasına izin veriliyorsa **Başlangıç**
   olarak işaretleyin.
5. Bu durumdan başka bir hareket beklenmediğine dair bir ipucu olarak
   **Bitiş** olarak işaretleyin — bu yalnızca açıklayıcıdır; sistem daha
   sonra Bitiş olarak işaretlenmiş bir durumdan çıkan bir Durum Geçişi
   eklemenizi engellemez.
6. Kaydedin.

## Bilinmesi gereken kurallar

- Yepyeni bir kayıt, **Başlangıç** olarak işaretlenmiş bir Durumda
  başlamalıdır — başka bir durumla oluşturmak reddedilir. Bir kayıt türü
  gerçekten birden fazla geçerli başlangıç noktasına sahipse bir Durum
  Türünün birden fazla Başlangıç durumu olabilir.
- Bir kaydı bir durumdan diğerine taşımak, yalnızca o tam kaynak/hedef
  çifti için eşleşen bir **Durum Geçişi** satırı varsa başarılı olur — bir
  yaşam döngüsü kurulduğunda "varsayılan olarak izin verilen" diye bir
  hareket yoktur.
- Örnek olarak: Satın Alma Siparişi Durumu, tek başlangıç durumu olarak
  Taslak'ı tohumlar; Taslak → Gönderildi → Onaylandı → Alındı normal
  yoldur ve Taslak, Gönderildi veya Onaylandı (ama Alındı değil) İptal
  Edildi'ye geçebilir. Bir satın alma siparişini Gönderildi ve Onaylandı'yı
  atlayarak doğrudan Taslak'tan Alındı'ya taşımaya çalışmak, ikisini
  doğrudan bağlayan bir Durum Geçişi olmadığından reddedilir.

## Neyle bağlantılıdır

Her Durum bir **Durum Türü**ne aittir. **Durum Geçişi** kayıtları bir
Durumu ya başlangıç ya da bitiş noktası olarak adlandırır.
