---
title: Alan İzni
audience: admin
module: foundation
order: 20
---

Alan İzni, tek bir **Rol**e sahip olanlardan, bir kayıt türü üzerindeki
belirli bir alanı gizler — örneğin, Taraf üzerindeki vergi kimliği
alanını genç bir rolden gizlemek, Tarafa herhangi bir erişimi olan herkes
diğer alanlarını görmeye devam ederken. Bilerek **İzin**den bağımsızdır:
aksi hâlde herkese tamamen açık olan bir kayıt türünde bir alanı
gizleyebilirsiniz.

Bu şekilde gizlenen bir alan, o Rol için otomatik oluşturulmuş düzenleme
formundan tamamen kaldırılır — yalnızca devre dışı bırakılmaz veya boş
gösterilmez — aynı formdaki diğer her alan ise normal şekilde görünmeye
devam eder.

## Ne zaman kullanılır

Tüm bir kayıt türünün bir Role görünür kalması gerektiğinde ama üzerindeki
belirli bir alanın özellikle o Rolden gizlenecek kadar hassas olduğunda —
bir kredi limiti, bir banka bilgisi, belirli bir Rolün o kayıt türüyle
başka türlü çalışabilmesine rağmen görmesini istemediğiniz herhangi bir
şey — bir Alan İzni kullanın.

## Bir alanı gizleme

1. **Alan İzni**'ne gidin ve **Yeni**'yi seçin.
2. Rolü, kayıt türünü ve tam alan adını seçin.
3. **Gizli** olarak işaretleyin.
4. Kaydedin.

## Bilinmesi gereken kurallar

- Bir alan, yalnızca sahip olduğu **her** Rol onu gizli olarak
  işaretlediğinde bir kullanıcıdan gizlenir — kullanıcı alanı gizlemeyen
  ikinci bir Role de sahipse, alanı yine de görür. Alan İzni görünürlüğü
  rol başına daraltır; aynı kullanıcının sahip olduğu başka bir rolden
  gelen daha geniş bir yetkiyi geçersiz kılmaz.
- Alan adı serbest metindir, kayıt türünün gerçek alan adlarıyla tam
  olarak eşleştirilir — bir yazım hatası, İzin'in kayıt türü alanıyla aynı
  riskle sessizce hiçbir şey yapmaz.
- Bir alanı gizlemek, onu otomatik oluşturulmuş formdan tamamen kaldırır;
  salt okunur bir görüntü değildir ve düzen veya biçimlendirmenin kontrol
  ettiği bir şey de değildir — gizleme, erişim kontrolü katmanının
  kendisinde gerçekleşir.
- (İzin gibi) çok ilk Alan İzni satırınızı oluşturmak, Rol, Kullanıcı
  Rolü, İzin, Alan İzni, Vekalet, Kayıt Sistemi ve Dış Kimliği yalnızca
  yönetici tarafından düzenlenebilir hâle getirir — önce neden kendinize
  `tenant_admin` vermeniz gerektiği için **Rol**'ün kendi kurallarına
  bakın.

## Neyle bağlantılıdır

Her Alan İzni bir **Rol**e referans verir. **İzin**'in yanında çalışır,
ancak ikisi bağımsızdır — bir kayıt türünde hiç varlık düzeyinde İzin
satırı olmadan alan gizleme yapılandırılabilir.
