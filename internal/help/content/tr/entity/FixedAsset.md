---
title: Sabit Kıymet
audience: user
module: assets
order: 1
---

Sabit Kıymet, kuruluşunuzun sahip olduğu ve zamanla amortismana tabi
tuttuğu bir mülktür — ekipman, bir araç, bir maliyeti, kullanım ömrü ve
kullanıldıkça azalan bir değeri olan her şey. Amortisman planının ve
bakım geçmişinin etrafında kurulduğu kayıttır.

## Ne zaman kullanılır

Amortismana tabi tutacağınız bir şey edindiğinizde bir Sabit Kıymet
kaydedin: neye mal olduğunu, ne kadar kullanmayı beklediğinizi ve
amortismanının hangi hesaplara işleneceğini girin.

## Kıymet kaydetme

1. **Sabit Kıymet**'e gidin ve **Yeni**'yi seçin.
2. Bir Kıymet Numarası (kendi referansınız) ve bir Ad girin.
3. İsteğe bağlı olarak bir **Konum** belirleyin.
4. **Edinme Tarihi**, **Para Birimi**, **Maliyet** ve (isteğe bağlı)
   **Hurda Değeri**'ni girin — kullanım ömrünün sonunda hâlâ ne kadar
   değerli olacağı. Hurda Değeri varsayılan olarak 0'dır.
5. **Kullanım Ömrü (ay)**'nü girin — amortisman süresi, yıl yerine ay
   olarak.
6. **Amortisman Yöntemi** varsayılan olarak Doğrusal'dır ve bugün
   yalnızca bunu sunar.
7. Üç **Muhasebe Hesapları**'nı seçin: Kıymet Hesabı (orijinal
   maliyetin tutulduğu yer), Amortisman Gideri Hesabı ve Birikmiş
   Amortisman Hesabı.
8. Kaydedin.

## Amortisman planı

Kıymet formundaki **Amortisman Planı** bölümü, kıymetin dönemlerini
listeler — her biri amortismana tabi tutulacak tutar ve sonucunda ortaya
çıkan defter değeriyle birlikte, ayda bir satır. Kıymeti kaydetmek bu
bölümü otomatik olarak oluşturur: Maliyet, Hurda Değeri, Kullanım Ömrü,
Amortisman Yöntemi, Edinme Tarihi ve Para Birimi ayarlanır ayarlanmaz,
kıymetin kullanım ömrünün her dönemi sizin için tek seferde, doğrusal
olarak oluşturulur — satırları elle eklemezsiniz. Bu alanlardan
herhangi birini daha sonra düzenlemek, henüz hiçbir şey işlenmemiş
olduğu sürece, tüm planı buna uyacak şekilde yeniden oluşturur; planı
değiştirmeyen bir düzenleme (örneğin Konum'u değiştirmek) ona hiç
dokunmaz. Kıymet Hizmette olduğunda bir amortisman işleme çalışmasının
okuyup işlediği şey — kıymetin maliyeti ve kullanım ömrü tek başına
değil — bu bölümdeki satırlardır.

**Herhangi bir satır işlendiğinde**, plan kilitlenir: artık Maliyet,
Hurda Değeri, Kullanım Ömrü, Amortisman Yöntemi, Edinme Tarihi veya Para
Birimi'ni değiştiremezsiniz (kayıt reddedilir) — işlenmiş satırlar
gerçek, zaten kaydedilmiş defter kayıtlarıdır ve onları üreten şeyi
değiştirmek o geçmişi sessizce geçersiz kılar. Kıymet üzerindeki diğer
her alan (Konum, Ad, muhasebe hesapları) hâlâ serbestçe düzenlenebilir.

## Bilinmesi gereken kurallar

- Kıymet Numarası, Ad, Edinme Tarihi, Maliyet, Kullanım Ömrü, Amortisman
  Yöntemi ve Durum hepsi zorunludur. Maliyet, Hurda Değeri ve Kullanım
  Ömrü negatif olamaz.
- Üç Muhasebe Hesabı zorunlu alan olarak işaretlenmemiştir, ama Hizmette
  bir kıymet için bunlardan herhangi biri eksikse, bir amortisman işleme
  çalışması o kıymetin vadesi gelen tüm satırlarını sessizce atlar
  (günlüğe yazılır, size gösterilmez) — kıymet hizmete girdikten sonra
  üçünü de ayarlı tutun.
- Durum bir belge onayını değil, bir kıymetin gerçek yaşamını modeller:
  **Taslak** tek başlangıç noktasıdır (kaydedilmiş ama henüz amortismana
  tabi değil). **Hizmette**, amortismanın işlendiği durumdur. **Tamamen
  Amortismana Tabi**, plan tükendiğinde ulaşılır. **Elden Çıkarıldı** ve
  **Kayıttan Düşüldü**, ikisi de Taslak, Hizmette veya Tamamen
  Amortismana Tabi'den ulaşılabilir — bir kıymet yaşamının herhangi bir
  noktasında satılabilir veya hurdaya çıkarılabilir — ve ikisi de
  nihaidir: bunlardan geri dönüş yoktur, çünkü bir kıymetin elden
  çıkarılmasını geri almak yeni bir edinimdir, bir durum değişikliği
  değil.

## Neyle bağlantılı

Bir Sabit Kıymet muhasebeleştirme için bir **Para Birimi**ne ve üç
**Hesap** kaydına referans verebilir. **Amortisman Planı** satırları
doğrudan ona aittir. **Bakım Geçmişi**, ona karşı açılan her **Bakım

Emri**'ni gösterir — kıymet formunda salt okunur bir liste; bakım
emirleri kendi başlarına bağımsız kayıtlardır, buradan düzenlenmez.
