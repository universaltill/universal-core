---
title: Ürün
audience: user
module: purchasing
order: 1
---

Ürün, satılabilir veya stoklanabilir bir şeydir — bu modüldeki diğer her
satın alma kaydının nihayetinde referans verdiği ürün veya hizmet. Bir
Sipariş Satırı, bir Teklif Talebi, bir Stok Kalemi'nin stok seviyesi:
hepsi bir Ürün'e geri işaret eder.

## Ne zaman kullanılır

Sipariş vermeden, teklif almadan veya stok takibi yapmadan önce bir Ürün
kaydedin — bu modülde "ne"ye yapılan her referans (bir satır, bir teklif,
bir stok seviyesi) zaten var olan bir Ürün ile başlar.

## Ürün kaydetme

1. **Ürün**'e gidin ve **Yeni**'yi seçin.
2. Bir **SKU** (kendi referans kodunuz) ve bir **Ad** girin.
3. **Tür**'ü seçin: Stok (envanterini tuttuğunuz fiziksel bir şey), Hizmet
   (işçilik veya emek, stoklanacak bir şey yok) veya Stoksuz (satın alıp
   yeniden sattığınız veya eldeki miktarını takip etmeden kullandığınız bir
   şey). Varsayılan olarak Stok'tur.
4. İsteğe bağlı olarak bir **Ölçü Birimi** seçin — adet, kilogram, kutu,
   bu ürünün sayıldığı her neyse.
5. Kaydedin.

## Bilinmesi gereken kurallar

- SKU, Ad ve Tür'ün hepsi zorunludur.
- SKU benzersiz olmalıdır — bir alıcının veya tedarikçinin referans
  vereceği doğal anahtardır — ve aynı SKU'ya sahip ikinci bir Ürün
  tamamen reddedilir.
- Hizmet veya Stoksuz seçmek bugün kayıtla ilgili başka hiçbir şeyi
  değiştirmez — Stok Kalemi ve Yeniden Sipariş Kuralı, Tür'den bağımsız
  olarak herhangi bir Ürün'e hâlâ referans verebilir, bu yüzden bir stok
  raporunda bırakılan bir hizmet ürünü, sistemin yakalayacağı bir şey
  değil, bir veri girişi tercihidir.

## Neyle bağlantılı

Bir **Sipariş Satırı**, **Mal Kabul Satırı**, **Teklif Talebi Satırı**,
**Stok Transferi**, **Stok Kalemi** ve **Yeniden Sipariş Kuralı**, hepsi
bir Ürün'e referans verebilir. Ölçü Birimi, temel modülün Ölçü Birimi
kayıtlarından gelir.
