---
title: Ek
audience: user
module: foundation
order: 6
---

Ek, sistemdeki herhangi bir kayda — bir Taraf, bir satın alma siparişi, bir
fatura veya başka herhangi bir şeye — eklenebilen bir dosya referansıdır.
Kayıt türü başına ayrı bir "ekler" özelliği yerine tek, genel bir varlık
olarak var olur; böylece bunu benimseyen sistemin her parçası dosya
eklerini bedavaya alır.

## Ne zaman kullanılır

Bir kayıtla birlikte bir dosyayı — taranmış bir belge, bir sözleşme,
imzalı bir form — saklamanız gerektiğinde bir Ek kullanın. Yaygın bir
örnek, bir tedarikçinin vergi formunu Taraf kaydına veya imzalı bir
anlaşmayı bir satın alma siparişine eklemektir.

## Ek ekleme

1. **Ek**'e gidin ve **Yeni**'yi seçin (veya mümkün olan yerde, ait olduğu
   kayıttan ekle eylemini kullanın).
2. Ait olduğu kaydı (türü ve kimliği), dosya adını, MIME türünü, bayt
   cinsinden boyutunu ve dosyanın nerede depolandığını kaydedin.
3. Kaydedin.

## Bilinmesi gereken kurallar

- Dosya boyutu sıfır veya daha büyük olmalıdır — negatif bir boyut
  reddedilir.
- Bir eki kimin yüklediği, Ek kaydının kendisinde bir alan değildir; bu
  bilgi, diğer her kayıt türünde olduğu gibi otomatik olarak kaydın denetim
  geçmişinde yakalanır, dolayısıyla bir dosyayı kimin eklediğini bilmek
  için ayrı bir "yükleyen" alanına ihtiyacınız yoktur.
- Bir Ek, sistemdeki mevcut veya gelecekteki herhangi bir kayıt türüne
  işaret edebilir — sabit bir varlık türleri listesiyle sınırlı değildir.

## Neyle bağlantılıdır

Bir Ek, ait olduğu kaydı türü ve kimliğiyle referans verir — bir
**Taraf**a, bir **Sorun Bildirimi**ne veya diğer modüller
etkinleştirildiğinde satın alma siparişleri gibi belgelere işaret
edebilir. Bir Ekin ait olduğu kaydı silmek, Eki otomatik olarak silmez.
