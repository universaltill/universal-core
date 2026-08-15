---
title: UBL Belge Dışa Aktarımı
audience: user
module: export
order: 2
---

UBL dışa aktarımı, tek bir iş belgesini — bir Satın Alma Siparişi, Satış
Siparişi veya Müşteri Faturası — bu sistemin kendi veri modelinin zaten
tasarlandığı açık, yargı bölgesinden bağımsız sözlük olan OASIS UBL 2.1
XML dosyası olarak indirir.

## Ne zaman kullanılır

Bir ticari ortak veya başka bir sistem, bir PDF veya ekran yerine tek bir
belgeyi standart, makine tarafından okunabilir bir formatta ihtiyaç
duyduğunda kullanın — örneğin bir Satın Alma Siparişini veya Satış
Siparişini bir Peppol tarzı e-fatura hattına beslemek, veya bir Müşteri
Faturasını yapılandırılmış bir formatta arşivlemek için.

## Bir belge indirme

Bunun için ayrı bir İçe/Dışa Aktar sayfası yoktur — belgenin kendisini
açın ve **UBL dosyasını indir**'i seçin:

1. Var olan bir **Satın Alma Siparişi**, **Satış Siparişi** veya
   **Müşteri Faturası** açın. Eylem yalnızca kayıt kaydedildikten sonra
   kullanılabilir — henüz oluşturulmamış bir belge için dışa
   aktarılacak bir şey yoktur.
2. **UBL dosyasını indir**'i seçin. Bu, herhangi bir durumda çalışır;
   belgenin iş akışı durumuyla ilgili hiçbir şey dışa aktarılıp
   aktarılamayacağını etkilemez.
3. Dosya `{belge numarası}.xml` olarak indirilir.

## Bilinmesi gereken kurallar

- **Bir Satın Alma Siparişi veya Satış Siparişi bir UBL Sipariş belgesi
  olur** (UBL'nin tek bir Sipariş türü vardır; ikisi arasında yalnızca
  hangi tarafın alıcı, hangisinin satıcı olduğu değişir). **Bir Müşteri
  Faturası bir UBL Fatura belgesi olur.**
- Bir Müşteri Faturasının UBL dosyası, toplamı için tam olarak **tek bir
  özet fatura satırı** taşır — bu sistem henüz bir Müşteri Faturasının
  kendi satır düzeyinde ayrıntısını saklamıyor ve bunun yerine satırları
  bağlı Satış Siparişinden türetmek kısmi bir faturayı olduğundan fazla
  gösterirdi. Bir Satın Alma Siparişi/Satış Siparişinin UBL dosyası
  gerçek, tek tek satırlarını taşır.
- **Bu, dosyanın açıkladığı her şeyi göremiyorsanız sessizce
  sansürlenmez — tamamen reddedilir** — SAF-T dışa aktarımının
  kullandığı aynı belge geneli engel: belgenin dokunduğu her varlık
  türü üzerinde okuma erişimi (belgenin kendisi, bir sipariş için
  satırları ve kalemleri, karşı taraf Taraf/Taraf Rolü, Para Birimi) ve
  dosyanın gerçekten okuduğu herhangi bir alanı gizleyen bir Alan
  İzninin olmaması. Gizli bir tutarı veya taraf adını sessizce boş/sıfır
  olarak gösteren şema açısından geçerli bir dosya, tamamen reddetmekten
  daha kötü olurdu.
- Dışa aktaran kiracının belgedeki kendi tarafı (ad, vergi kimliği), SAF-T
  dışa aktarımının şirket profilinin çözümlendiği aynı şekilde
  çözümlenir — vergi kimliği yalnızca kiracının kendi kuruluşu bir
  `own_organization` rolüne sahip bir Taraf aracılığıyla tanımlıysa
  görünür; aksi takdirde bu alan basitçe atlanır (UBL, SAF-T'nin aksine,
  bunu isteğe bağlı olarak ele alır).
- Her dışa aktarım, hangi belgenin dışa aktarıldığını kaydeden bir
  denetim kaydı yazar.

## Neyle bağlantılı

Dışa aktarılan belgenin kendisini, satırlarını ve Kalemlerini (bir
sipariş için), karşı taraf **Taraf**/**Taraf Rolü**'nü ve **Para
Birimi**'ni okur — ayrıca bir Müşteri Faturası için, faturanın UBL
dosyasının atıfta bulunduğu sipariş numarası için bağlı olduğu **Satış
Siparişi**'ni. Bu, Finans modülünden erişilebilen bütün defter **SAF-T
dışa aktarımı**ndan farklı bir yasal formattır — UBL her seferinde tek
bir belgedir, SAF-T bir dönem için bütün defterdir.
