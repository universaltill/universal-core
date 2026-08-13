---
title: Görev
audience: user
module: projects
order: 2
---

Görev, bir Proje içindeki bir iş birimidir — bir başlığı, isteğe bağlı
bir sorumlusu ve bitiş tarihi olan, ilerlemesini izleyen bir durumu
bulunan bir şey. Görevler bir üst Görev altında iç içe geçebilir, böylece
büyük bir iş parçası proje dışına çıkmadan alt bölümlere ayrılabilir.

## Ne zaman kullanılır

Bir proje içinde ayrıca takip etmeye değer her şey için bir Görev
ekleyin: bir teslim edilecek şey, bir adım, bitiş tarihi ve sahibi olsun
istediğiniz bir iş parçası. Bir görev aslında daha büyük bir görevin alt
adımıysa Üst Görev kullanın — ikisi bağlı kalır ve raporlama veya
filtreleme hiyerarşiyi izleyebilir.

## Görev ekleme

1. **Proje** formunun Görevler bölümünde bir satır ekleyin (veya
   doğrudan **Görev**'i açıp **Yeni**'yi seçin).
2. Bir **Başlık** girin.
3. İsteğe bağlı olarak bir **Sorumlu** seçin — burada yalnızca çalışan
   rolüne sahip bir Taraf seçilebilir.
4. İsteğe bağlı olarak **Tahmini Saat** ve bir **Bitiş Tarihi**
   belirleyin.
5. Bu görev aynı projedeki başka bir görevin alt adımıysa, **Üst
   Görev**'i belirleyin.
6. Kaydedin.

## Bilinmesi gereken kurallar

- Proje, Başlık ve Durum zorunludur. Durum varsayılan olarak
  **Yapılacak**'tır. Tahmini Saat varsayılan olarak 0'dır ve negatif
  olamaz.
- **Üst Görev**, görevin kendisiyle aynı projeye ait olmalıdır — bir
  görevi farklı bir projedeki bir görevin altına yerleştiremezsiniz.
- Durum düz bir çizgi değildir: Yapılacak → İşlemde → Tamamlandı normal
  yoldur, ancak **Engellendi**'ye hem Yapılacak'tan hem de İşlemde'den
  ulaşılabilir (bir görev, üzerinde çalışılmaya başlanmadan önce de
  engellenebilir, sadece iş sürerken değil) ve bu ikisinden birine geri
  döner — grafik hangisinden engellendiğini ayrıca izlemez. Bu üründeki
  çoğu belge iş akışının aksine, **Tamamlandı** bir
  görev tekrar İşlemde'ye dönebilir — işin aslında bitmediği ortaya
  çıktığında, üzerinde zaten kaydedilmiş zamanı kaybetmek yerine aynı
  görevi yeniden açar. **İptal Edildi**'ye Yapılacak, İşlemde veya
  Engellendi'den ulaşılabilir.

## Neyle bağlantılı

Bir Görev bir **Proje**'ye ve isteğe bağlı olarak aynı projedeki bir
**Üst Görev**'e aittir. Bir **Sorumlu**su (yalnızca çalışan rolündeki
Taraf) olabilir. Bir görevin **Kaydedilen Süre** bölümü, ona karşı
kaydedilen her **Zaman Kaydı**'nı gösterir — bu, görev formunda salt
okunur bir görünümdür; zaman doğrudan göreve yazılmaz, Zaman Kaydı
tarafından girilir.
