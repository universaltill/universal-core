---
title: Çalışan
audience: admin
module: hr
order: 1
---

Bir Çalışan kaydı bir istihdamdır — bir kişi değil. Kişinin kendisi bir
**Taraf** kaydıdır; bu varlık yalnızca istihdama özgü olanı taşır: hangi
pozisyon, hangi departman, ne zaman başladığı ve hâlâ aktif olup
olmadığı. İkisini ayrı tutmak, aynı kişinin zaman içinde iki istihdam
kaydına (bir yeniden işe alım) sahip olabilmesini sağlar; adını veya
iletişim bilgilerini asla çoğaltmadan, ve ilk istihdamın geçmişi
ikincisi tarafından üzerine yazılmadan.

## Ne zaman kullanılır

Birini bir role işe aldığınızda bir Çalışan kaydı oluşturun — kişi zaten
bir Taraf olarak var olduktan sonra (yoksa önce Tarafı oluşturun). Biri
ayrılır ve daha sonra geri dönerse, yeniden işe alım için eskisini
tekrar Aktif'e düzenlemek yerine **yeni** bir Çalışan kaydı oluşturun —
eski kaydın tarihleri ve geçmişi tam olarak olduğu gibi kalmalıdır.

## Çalışan kaydı oluşturma

1. **Çalışan**'a gidin ve **Yeni**'yi seçin.
2. Bir **Çalışan No** (Tarafın kendi kimliğinden ayrı, kendi
   referansınız) girin ve **Kişi**yi (Tarafı) seçin.
3. İsteğe bağlı olarak **Pozisyon** ve **Departman**ı belirleyin.
4. **İşe Giriş Tarihi**ni girin.
5. Bu istihdama karşı işgücü maliyeti takip ediyorsanız (örneğin,
   kaydedilen proje süresini fiyatlandırırken kullanılır), Ücretlendirme
   bölümünde isteğe bağlı olarak bir **Maliyet Oranı** girin.
6. Kaydedin.

## Bilinmesi gereken kurallar

- Çalışan No, Kişi ve İşe Giriş Tarihi zorunludur. Durum varsayılan
  olarak **Deneme Süresi**'dir.
- Durum bir yaşam döngüsüdür: Deneme Süresi → Aktif, ve Aktif **İzinde**
  durumuna geçip geri dönebilir (uzun süreli izin bir işten çıkarma
  değildir). **İşten Ayrıldı**, üç canlı durumun herhangi birinden
  ulaşılabilir ve çıkmaz bir sokaktır — hiçbir şey ondan dışarı çıkmaz.
  Yeniden işe alım her zaman yeni bir Çalışan kaydıdır, asla yeniden
  açılmış eski bir kayıt değil.
- Çıkış Tarihi belirlenirse, İşe Giriş Tarihi'nden önce olamaz. Maliyet
  Oranı belirlenirse negatif olamaz.
- Maliyet Oranı'nın varsayılanı yoktur — oranı belirlenmemiş bir Çalışan
  kaydı, oranı sıfır olan bir kayıttan farklıdır. Eksik bir oran
  "bilinmiyor" anlamına gelir, ve bu çalışanın zamanını fiyatlandıran
  her şey bunu eksik bilgi olarak ele alır, ücretsiz emek olarak değil.
  Oranı bilmiyorsanız, 0 girmek yerine alanı boş bırakın.
- **İzin Geçmişi** bölümü, bu istihdamın İzin Taleplerini listeler — bu,
  istihdamın kendisinden daha uzun ömürlü kararların salt okunur bir
  görünümüdür.

## Neyle bağlantılı

Her Çalışan, **Kişi**ye — Çalışan rolünü taşıyan bir Taraf olması
amaçlanır, ancak (Destek Kaydı'nın Müşteri alanı gibi) kayıt sırasında
bu gerçekten kontrol edilmez — ve isteğe bağlı olarak bir **Pozisyon**
ve **Departman**a referans verir. **İzin
Talebi** ve **Devam Kaydı**, doğrudan Tarafa değil Çalışana referans
verir, çünkü bunlar belirli bir istihdamla ilgilidir — zaman içinde iki
istihdamı olan birinin iki ayrı izin ve devam geçmişi vardır.
