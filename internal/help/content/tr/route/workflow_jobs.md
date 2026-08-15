---
title: Onaylar
audience: both
module: workflow
order: 1
---

Bu sizin onay gelen kutunuzdur — her varlık türü genelinde şu anda bir
`require_approval` adımında bekleyen her iş akışı işi, harekete
geçebileceğiniz olanlar için bir Onayla düğmesiyle birlikte.

## Ne zaman kullanılır

Sizin onayınızı bekleyen şeyi görmeniz gerektiğinde, veya birisi bir iş
akışının onayda takıldığını söylediğinde kontrol edin. Üst gezinme
çubuğundaki **Onaylar** bağlantısından ulaşılır, oturum açmış herhangi
bir kullanıcı için kullanılabilir — bir iş akışı bir kiracının sahip
olduğu herhangi bir varlık türüne karşı tetiklenebildiği için tek bir
modüle özgü değildir.

## Bir şeyi onaylama

1. Üst gezinmeden **Onaylar**'ı açın.
2. Her satır iş akışının adını, beklediği varlık türünü ve kaydı (kaydın
   kendisine bir bağlantı) ve ya bir **Onayla** düğmesi ya da henüz
   kullanamayacağınızın nedenini gösterir.
3. **Onayla**'yı seçin. İş devam ettiğinde satır kaybolur; sayfadaki
   başka hiçbir şey elle yenilenmeye ihtiyaç duymaz.

## Bilinmesi gereken kurallar

- **Bu gelen kutusu yalnızca `waiting_approval` işlerini gösterir** —
  genel bir iş akışı durumu tarayıcısı değildir. Kuyrukta olan, çalışan,
  başarısız olan veya zaten devam etmiş bir iş burada hiç görünmez.
- **Yalnızca adımın gerektirdiği rolü gerçekten elinizde
  tutuyorsanız bir Onayla düğmesi alırsınız** — ve adım onayı kaydın
  kendi departmanına sınırlıyorsa, yalnızca o rolü *o departmanda*
  tutuyorsanız. Üzerinde işlem yapamadığınız bir satır yine de nedeniyle
  (hangi rol, ve varsa hangi departman) gösterilir, bu yüzden bekleyen
  bir onay yalnızca sizin onaylayacağınız bir şey olmadığı için asla
  görünmez olmaz — gösterilir, gizlenmez.
- **Bir Vekalet, başka birinin adına onay vermenize izin verebilir.**
  Başka bir kullanıcı onay yetkisini size vekalet etmişse (bkz.
  **Vekalet**), o vekalet aracılığıyla sahip olduğunuz bir rolün
  gerektirdiği bir işi, tıpkı kendi yetkinizmiş gibi onaylayabilirsiniz.
- **Bir iş akışı adımı isteğe bağlı olarak bir eskalasyon
  ayarlayabilir**: belirlenmiş bir saat sayısı beklendikten sonra, bir
  iş ikinci, daha geniş bir rol tarafından da onaylanabilir hale gelir
  — zaten onaylayabilecek olana ek olarak, asla onun yerine değil. Bu bu
  sayfadan değil, iş akışını yazan kişi tarafından yapılandırılır; bu
  rolün artık bir Onayla düğmesi de sunulmasının ötesinde, burada hiçbir
  şey belirli bir işin zaten eskale olup olmadığını göstermez.
- Burada onaylamak, tam olarak temel API'nin yaptığı şeydir — bu sayfa
  API'nin kabul etmeyeceği hiçbir şeyi sunmaz ve API'nin izin vereceği
  hiçbir şeyi reddetmez.

## Neyle bağlantılı

**İş Akışı İşi** satırlarını listeler, her birinin beklediği kayda
bağlantı verir. Belirli bir satırı kimin onaylayabileceği, iş akışının
kendi **İş Akışı Tanımı**'na (`require_approval` adımının rolü ve
isteğe bağlı departman kapsamı ve eskalasyon ayarları) ve bir yerine
onaylayıcı için aktif bir **Vekalet**'e bağlıdır — vekaletin kendisinin
nasıl kurulacağı için o konuya bakın.
