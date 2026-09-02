# Rakip Araştırması — Crypto Portfolio & Risk Infrastructure

Tarih: 2026-09-01

---

## 1. Pazar Dört Segmente Ayrılıyor

### A) Retail tracker
**CoinStats, Delta (eToro), CoinMarketCap/CoinGecko portfolio, Crypto Pro, Kubera**

Ne yapıyorlar: balance aggregation, çoklu cüzdan/borsa, fiyat alarmı, mobil-öncelikli
UI, NFT, bazıları hisse/ETF ile birlikte net-worth görünümü.

Ne yapmıyorlar: pozisyon-seviyesi accounting yok veya zayıf; realized/unrealized ayrımı
yüzeysel; risk metriği yok; türev desteği yeni ve sığ. CoinStats 2026 başında Hyperliquid,
Aster ve Lighter perp DEX'lerini ekledi — açık pozisyon, açık emir, PnL gösteriyor;
ama bu "izleme", risk hesabı değil.

Zayıf noktaları belgelenmiş: fiyat kimliği hataları, eksik token, ekranlar arası tutarsız
toplam, geçmiş sorgularda kırılma.

### B) Tax & accounting
**Koinly, CoinTracker, CoinTracking, CoinLedger, rotki (açık kaynak, AGPLv3)**

Ne yapıyorlar: transaction-level ledger, tax lot takibi, FIFO/LIFO/HIFO/ACB/share-pooling,
maliyet bazı, transfer eşleştirme, "review suggested" ile işaretlenmiş kayıt akışı,
ülke bazlı kural setleri.

ABD'de 1 Ocak 2025'ten itibaren universal cost tracking kaldırıldı; cüzdan/hesap bazlı
maliyet takibi zorunlu. Bu, ledger'ın venue-bazlı lot tutması gerektiği anlamına geliyor
— mimari bir kısıt, sadece vergi detayı değil.

Ne yapmıyorlar: gerçek zamanlı değil (çoğu dakikalar/saatler gecikmeli), türev pozisyon
riski yok, likidasyon/margin kavramı yok, canlı exposure yok.

rotki teknik olarak bize en yakın açık kaynak referans: local-first, SQLCipher ile şifreli
yerel DB, EVM transaction decoding, dönem bazlı PnL raporu. İncelenmeye değer;
ama Python + Electron ve realtime risk hedefi yok.

### C) On-chain / DeFi
**DeBank, Zapper, Zerion, Nansen Portfolio**

Ne yapıyorlar: protokol pozisyonu çözümleme (lending, LP, staking, vault), çoklu zincir,
cüzdan birleştirme (bundle), airdrop/spam token yönetimi.

Ne yapmıyorlar: CEX tarafı zayıf, accounting yok, risk motoru yok.

### D) Kurumsal PMS / RMS
**1Token (CAM), Elwood, HedgeGuard, Talos, prime broker platformları**

Bizim mimarimizin gerçek referansı bu segment. Ortak özellik seti:

| Yetenek | Detay |
|---|---|
| IBOR | Investment Book of Record — risk, NAV ve raporlamayı besleyen tek gerçek kaynak |
| Multi-entity | Fon, SPV, strateji sleeve, sub-account hiyerarşisi |
| Multi-denomination | BTC, ETH, USD veya stablecoin bazında portföy |
| Collateral & margin | Venue-içi kaldıraç (MMR), venue-dışı kaldıraç (LTV), margin buffer |
| Risk | Realtime PnL, exposure, Greeks (delta/gamma/vega/theta) |
| Stress testing | Fiyat/volatilite/korelasyon şoku senaryoları, VaR, kayıp projeksiyonu |
| Reconciliation | Equity ve pozisyon mutabakatı, **rehberli tutarsızlık çözümü**, veri bütünlük kontrolü, anomali tespiti |
| Alerting | PnL/exposure/leverage/collateral eşikleri; Telegram, Slack, e-posta, SMS |
| Lineage | Audit-ready portföy geçmişi, veri soy ağacı |
| Backfill | Tüm venue'lerde boşluksuz tarihsel veri |

Bunlar kapalı, pahalı ve fon operasyonlarına satılan ürünler. Kaldıraçlı bireysel/pro
trader bu araçlara erişemiyor.

---

## 2. Boşluk Neresi

```
                 spot/balance          türev + risk
                 odaklı                odaklı
realtime    │  CoinStats, Delta   │   ← BOŞLUK →
            │  DeBank, Zapper     │   (1Token/Elwood fiyat dışı)
────────────┼─────────────────────┼──────────────────────
tarihsel /  │  Koinly, CoinTracker│   —
accounting  │  CoinTracking, rotki│
```

**Sonuç: kaldıraç kullanan CEX trader'ı için türev-farkında, gerçek zamanlı,
mutabakatlı risk konsolu diye bir şey yok.** Retail tracker'lar bakiye gösteriyor,
tax araçları geçmişi doğru tutuyor, kurumsal platformlar ikisini de yapıyor ama
ulaşılabilir değil.

**Rekabet edilmeyecek eksen: entegrasyon sayısı.** 1Token 72 borsa/OTC, 10 kustodi,
163 zincir, 4224 DeFi protokolü listeliyor. Bu yarışa girmek anlamsız. Farklılaşma
kapsamda değil, **doğrulukta ve risk derinliğinde** olacak.

---

## 3. Rakiplerin Kaybettiği Yer = Bizim Test Edilecek Yerimiz

Sektörün kendi dokümanlarından çıkan başarısızlık modları:

1. **Kimlik hatası fiyat hatası olarak görünüyor.** Kullanıcı varlığa gerçekten sahip,
   miktar doğru, ama holding yanlış market kimliğine eşleşmiş: ticker çakışması,
   wrapped asset, sembol edge-case'i. CCXT bile spot ve türev marketlerin aynı sembolü
   paylaşmasını bilinen bir çakışma olarak belgeliyor.
2. **Transfer, satış sanılıyor.** Alıcı cüzdan bilinmiyorsa giden işlem elden çıkarma
   sayılıyor; kullanıcıdan elle düzeltmesi isteniyor. Bu, tüm sektörün 1 numaralı
   destek konusu.
3. **Her ekran farklı toplam gösteriyor.** Farklı değerleme kaynakları uzlaştırılmadığı
   için ürün yalan söylüyormuş gibi hissettiriyor.
4. **Zaman ürünü kırıyor.** Canlı fiyat kolay, tarihsel doğruluk zor; pencere
   değişince PnL değişiyor.
5. **Ürün büyüdükçe yavaşlıyor.** Refresh döngüsü ve istek mantığı dağılıyor.

Bunların hepsi bizim event-sourced tasarımımızın doğal olarak iyi çözebileceği
problemler — yeter ki açıkça hedeflensin.

---

## 4. Kullanım Senaryosu: Delta-Nötr Basis Trade

Hedef kullanıcının en yaygın kurgusu, ve mevcut araçların en yanlış gösterdiği şey.

```
BTC spot long    +$50,000
BTC perp short   -$50,000
```

Naif hesap: gross exposure $100k, equity $50k → **leverage 2x, riskli**.
Doğru hesap: net BTC delta ≈ 0 → yönsel risk yok; gerçek riskler funding yönü dönmesi,
short bacağın likidasyonu, basis riski ve venue riski.

Bir sistem bu iki bacağı aynı stratejiye bağlayamıyorsa kullanıcıya sürekli yanlış
uyarı verir. **Bu yüzden `strategy` boyutu V1'e giriyor.**

---

## 5. Boşluk Analizi — Bizim Taslakta Eksik Olanlar

Öncelik sırasıyla. "V1" = ilk sürümde olmalı, "V1.5/V2" = sonra.

| # | Eksik | Neden kritik | Ne zaman |
|---|---|---|---|
| G1 | **Transfer eşleştirme** | Venue'ler arası çekim+yatırım tek transferdir; eşleşmezse satış sayılır ve PnL çöker | V1 (venue-içi), M8 (venue-arası) |
| G2 | **Asset registry** (instrument'tan ayrı) | Değerleme hatalarının kökü kimlik hatası; ticker çakışması, wrapped asset | V1 |
| G3 | **Tek değerleme politikası** | "Her ekran farklı toplam" problemi; her yanıt aynı `as_of` ve fiyat kaynağını taşımalı | V1 |
| G4 | **Veri kalitesi katmanı** | Negatif bakiye, sequence boşluğu, bilinmeyen asset, fiyat boşluğu = eksik event alarmı | V1 |
| G5 | **Strateji/sleeve boyutu** | Delta-nötr kurgular yanlış raporlanıyor | V1 |
| G6 | **Lineage / explain endpoint** | Event-sourced olduğumuz için bedava; kurumsal ürünlerin "audit-ready" dediği şey | V1 |
| G7 | **Alert kanalı + histerezis** | Tablo yazmak alarm değil; eşik salınımında spam olmamalı | V1 |
| G8 | **Collateral ayrı domain** | MMR (venue-içi) ve LTV (venue-dışı) ayrı kavramlar; leverage tek sayı değil | M5 |
| G9 | **Mutabakat sınıflandırma + çözüm akışı** | Sadece "mismatch" demek yetmiyor; neden ve aksiyon lazım | M7 |
| G10 | **Senaryo şoku** | "BTC %20 düşerse ne olur" — pure function olduğumuz için ucuz, değeri çok yüksek | M7.5 |
| G11 | **TWR / performans** | Yatırma-çekme PnL'i bozuyor; "pencere değişince getiri değişiyor" problemi | V2 |
| G12 | **Tax lot projeksiyonu** | ABD'de cüzdan bazlı maliyet takibi zorunlu; ledger lot-türetilebilir kalmalı | V2 |
| G13 | **Referans para birimi + FX** | USD/USDT/BTC/TRY görünümü; stablecoin depeg durumunda fark eder | V1 (karar), V2 (çoklu) |
| G14 | **Sub-account yapısı** | Binance sub-account, birden çok API key aynı kullanıcıda | V1 (model), V2 (UI) |
| G15 | VaR, Greeks, options | Kapsam dışı; opsiyon eklenmedikçe Greeks anlamsız | Kapsam dışı |

---

## 6. Teknoloji Notu — CCXT

CCXT'nin Go portu var (`github.com/ccxt/ccxt/go/v4`) ama:

- WebSocket desteği CCXT Pro'da ve ticari kullanımda ücretli
- Go portu transpile edilmiş; idiomatik Go değil
- Account/user-data stream normalizasyonu bu projenin **asıl domain işi** — dışarı
  verilirse geriye CRUD kalır

**Karar: exchange client'ları elle yaz.** Ama CCXT'nin market metadata ve sembol
normalizasyon modelini referans olarak oku; spot/türev çakışması gibi problemleri
zaten çözmüşler.

---

## 7. Ürün Konumu (tek cümle)

> Kaldıraç kullanan CEX trader'ı için, canonical ledger üzerine kurulmuş,
> mutabakatı yapılan, strateji-farkında gerçek zamanlı risk motoru.

Rakip listesi değil, doğruluk iddiası satıyor. Bu yüzden ürünün en görünür özelliği
"kaç borsa desteklediği" değil, **"sayılarımın neden doğru olduğunu gösterebiliyorum"**
olmalı: reconciliation durumu, veri kalitesi paneli ve her rakamın event'lere kadar
izlenebilirliği ön yüzde birinci sınıf vatandaş.
