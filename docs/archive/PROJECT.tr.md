# Crypto Portfolio & Risk Infrastructure — Proje Taslağı

Backend odaklı position/risk engine + ingestion/reconciliation altyapısı.
UI ikincil; ürün, ledger'dan türetilen doğru portfolio state'idir.

**Konum:** kaldıraç kullanan CEX trader'ı için, canonical ledger üzerine kurulmuş,
mutabakatı yapılan, strateji-farkında gerçek zamanlı risk motoru. Retail tracker'lar
bakiye gösteriyor, tax araçları geçmişi doğru tutuyor, kurumsal PMS'ler ikisini de
yapıyor ama erişilemez. Detaylı gerekçe: `COMPETITIVE-ANALYSIS.md`.

**Rekabet edilmeyecek eksen: entegrasyon sayısı.** Farklılaşma kapsamda değil,
doğrulukta ve risk derinliğinde. Ürünün en görünür özelliği "kaç borsa desteklediği"
değil, mutabakat durumu ve her rakamın event'lere kadar izlenebilirliği olacak.

---

## 1. V1 Kapsamı (kilitli)

**İçinde**
- Binance (spot + USD-M perpetual), read-only API key
- Historical backfill + realtime user data stream
- Canonical ledger, position engine, PnL, portfolio, exposure/leverage
- Market data ingest + fiyat geçmişi + mark-to-market
- Reconciliation (bizim state ↔ exchange snapshot)
- Risk threshold + alert
- SSE ile realtime dashboard

**Dışında (V1)**
- Bybit / Coinbase (V2 — normalizasyonun asıl testi)
- EVM wallet, Solana, DeFi, LP
- COIN-M futures, options
- Hedge mode (iki yönlü pozisyon)
- Vergi amaçlı FIFO/LIFO accounting
- Multi-asset / portfolio margin
- Trade execution (asla; key permission'ı buna izin vermeyecek)

---

## 2. Temel Mimari Kararlar (ADR özet)

### K1 — Ledger append-only, geri kalan her şey türetilmiş
`ledger_events` tek gerçek kaynak. Position, portfolio, PnL, risk = ledger üzerinde
saf fonksiyon (fold). Hiçbir engine kendi kalıcı "gerçeğini" tutmaz; tuttuğu şey
yeniden üretilebilir bir projeksiyondur.

Sonuç: `position` tablosu cache'tir, silinip ledger'dan yeniden kurulabilir olmalı.
Bu, hem historical reconstruction'ı hem de hesaplama bug'larından kurtulmayı sağlar.

### K2 — Bitemporal zaman
Her event'te iki zaman var:
- `event_time` — exchange'in söylediği zaman (hesaplamalar bunu kullanır)
- `ingested_at` — bizim gördüğümüz zaman (debug, gap analizi, geç gelen event tespiti)

"25 Ağustos 14:00'te portföyüm" sorgusu `event_time <= T` üzerinden çalışır.

### K3 — Idempotent ingestion
REST backfill ile WS stream aynı event'i verir. Doğal anahtar:

```
UNIQUE (integration_id, source, external_id)
```

Insert `ON CONFLICT DO NOTHING`. Duplicate ingestion sistemi bozmamalı, aynı ledger'ı
üretmeli. Bu bir test edilecek invariant, iyi niyet değil.

### K4 — Para tipi
- Postgres: `NUMERIC(38, 18)`
- Go: `shopspring/decimal`, sqlc `overrides` ile map'lenir
- `float64` hiçbir yerde para/miktar taşımaz (JSON serialization dahil — API'de string)

### K5 — Position accounting: exchange-style average cost
Trading pozisyon PnL'i; vergi accounting'i değil. Anahtar: `(integration_id, instrument_id)`.
Sadece one-way mode. Position flip (long → short) tek trade ile olabilir; realized PnL
kapanan miktar üzerinden hesaplanır, kalan miktar yeni yönde yeni avg entry ile açılır.

### K6 — Liquidation price exchange'ten okunur
Kendimiz hesaplamıyoruz (margin tier tabloları + cross/isolated + wallet balance
etkileşimi). Exchange'in verdiği likidasyon fiyatını `position_risk` snapshot'ında
saklıyoruz; **liquidation distance**'ı mark price ile biz hesaplıyoruz.

### K7 — Fiyat geçmişi kalıcı
Redis sadece "latest price" cache'i. Historical portfolio için `price_ticks`
Postgres'te tutulur (dakikalık mark price yeterli, her tick değil). BRIN index (`ts`).
V1'de TimescaleDB/ClickHouse yok.

### K8 — Broker yok, modular monolith
`cmd/api` + `cmd/worker`. Modüller arası iletişim Go interface'i, network değil.
Kafka/NATS gerçek ihtiyaç çıkana kadar eklenmez.

### K9 — Credential güvenliği
- API key permission: **read-only**, withdrawal kapalı, trading kapalı
- DB'de AES-GCM ile şifreli (master key env/KMS'ten), plaintext asla log'a düşmez
- Bağlantı kurulurken permission doğrulanır; fazla yetkili key reddedilir

### K10 — Asset ≠ Instrument
Değerleme hatalarının çoğu fiyat hatası değil **kimlik hatası**: ticker çakışması,
wrapped/bridged varlık, aynı sembolün spot ve türev marketlerde kullanılması.
`assets` (BTC, ETH, USDT — canonical varlık) ve `instruments` (BTC-USDT-PERP —
işlem görülebilir enstrüman) ayrı tablolar. Exchange sembolü hiçbir zaman
doğrudan anahtar olarak kullanılmaz; her zaman alias tablosundan çözülür.

### K11 — Tek değerleme politikası
Her portföy/risk yanıtı tek bir `valuation_run` üzerinden üretilir ve şunları taşır:
`as_of`, `price_source`, `stale`. Aynı anda iki farklı fiyat kaynağından iki farklı
toplam üretilmesi yasak. Enstrüman başına fiyat kaynağı önceliği açıkça tanımlıdır
(perp → exchange mark price, spot → exchange last/index), fallback zinciri kayıtlıdır.

### K12 — Transfer, satış değildir
Bir venue'den çekilip başka bir venue'ye veya cüzdana yatırılan varlık **tek bir
transfer**dir. Eşleştirilmezse sistem bunu elden çıkarma + edinim olarak görür ve
PnL komple bozulur. `transfer_links` tablosu iki ledger event'ini birbirine bağlar.
Eşleştirme heuristiği: aynı asset + miktar (fee toleransı içinde) + zaman penceresi
+ varsa txid. Eşleşmeyenler kuyruğa düşer ve kullanıcı elle bağlayabilir.
V1'de venue-içi transferler (spot ↔ futures), M8'de venue-arası.

### K13 — Strateji boyutu birinci sınıf
Pozisyonlar ve event'ler opsiyonel `strategy` etiketi taşır. Delta-nötr basis trade
(spot long + perp short) tek strateji altında toplanmazsa sistem net delta ≈ 0 olan
bir kurguyu "2x kaldıraçlı, riskli" diye raporlar ve sürekli yanlış alarm üretir.
Exposure ve leverage hem portföy hem strateji seviyesinde hesaplanır.

### K14 — Veri kalitesi görünür bir özelliktir, iç detay değil
Her ürün "sayı gösterir"; farkımız sayının neden doğru olduğunu gösterebilmek.
Sürekli çalışan kontroller ve bunları servis eden bir endpoint var:
- **Negatif bakiye** — ledger sahip olunandan fazlasının satıldığını ima ediyorsa event eksik
- Sequence/WS boşluğu, keepalive kaçırma
- Bilinmeyen asset veya çözülemeyen sembol
- Fiyat boşluğu (mark price akışı kesilmiş)
- Eşleşmemiş transfer

---

## 3. Canonical Model

### Asset & Instrument
Exchange sembolü ≠ canonical instrument, ve instrument ≠ asset. İki katman:

```
assets
  id
  canonical_symbol      BTC / ETH / USDT
  kind                  native | token | stablecoin | fiat
  chain                 (token ise) ethereum
  contract_address      (token ise) 0x...
  is_wrapped            WBTC → underlying_asset_id = BTC

asset_aliases
  source                binance | bybit | coingecko
  external_symbol       BTC
  asset_id              FK

instruments
  id
  canonical_symbol      BTC-USDT-PERP / BTC-USDT-SPOT
  kind                  spot | perp
  base_asset            BTC
  quote_asset           USDT
  settle_asset          USDT
  contract_size         NUMERIC

instrument_aliases
  exchange              binance
  exchange_symbol       BTCUSDT
  market                spot | usdm
  instrument_id         FK
```

Binance'te spot `BTCUSDT` ile perp `BTCUSDT` aynı string, farklı instrument.
Bu ayrım yapılmazsa pozisyonlar birbirine karışır — ilk günden doğru kur.

### Ledger event tipleri
```
TRADE              alım/satım
TRANSFER           hesaplar arası (spot ↔ futures)
DEPOSIT
WITHDRAWAL
FUNDING_PAYMENT    perp funding (realized PnL'i etkiler)
FEE                ayrı gelirse
COMMISSION_REBATE
LIQUIDATION        trade'in özel hali, ayrı işaretlenir
POSITION_ADJUSTMENT  ADL, settlement vb.
```

### Şema (özet)

```sql
ledger_events (
  seq             BIGSERIAL PRIMARY KEY,   -- global ingest sırası
  integration_id  UUID NOT NULL,
  source          TEXT NOT NULL,           -- binance_spot_rest | binance_usdm_ws ...
  external_id     TEXT NOT NULL,
  event_type      TEXT NOT NULL,
  instrument_id   BIGINT,
  side            TEXT,                    -- buy | sell
  quantity        NUMERIC(38,18),
  price           NUMERIC(38,18),
  fee             NUMERIC(38,18),
  fee_asset       TEXT,
  event_time      TIMESTAMPTZ NOT NULL,
  ingested_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  raw             JSONB NOT NULL,
  UNIQUE (integration_id, source, external_id)
);
CREATE INDEX ON ledger_events (integration_id, event_time);

positions (               -- projeksiyon, silinip yeniden kurulabilir
  integration_id, instrument_id,
  quantity, avg_entry_price, realized_pnl,
  last_seq, updated_at,
  PRIMARY KEY (integration_id, instrument_id)
);

position_snapshots (      -- historical reconstruction hızlandırıcı
  integration_id, as_of TIMESTAMPTZ, seq BIGINT,
  state JSONB
);

price_ticks (instrument_id, ts, mark_price, index_price);

exchange_snapshots (      -- reconciliation girdisi
  integration_id, taken_at, kind, payload JSONB
);

reconciliation_runs (id, integration_id, started_at, status);
reconciliation_findings (run_id, instrument_id, ours, theirs, delta, severity);

alert_rules (integration_id, metric, operator, threshold, enabled,
             cooldown_seconds, hysteresis_pct);
alerts (rule_id, fired_at, cleared_at, snapshot JSONB);

strategies (id, account_id, name, kind);   -- basis_trade | directional | hedge

transfer_links (                 -- K12
  id,
  out_event_seq BIGINT,          -- withdrawal / transfer out
  in_event_seq  BIGINT,          -- deposit / transfer in
  match_method  TEXT,            -- txid | heuristic | manual
  confidence    NUMERIC,
  confirmed_by_user BOOLEAN
);

valuation_runs (                 -- K11
  id, as_of TIMESTAMPTZ, price_source TEXT, stale BOOLEAN
);

data_quality_findings (          -- K14
  integration_id, detected_at, check_name, severity,
  instrument_id, details JSONB, resolved_at
);

fx_rates (base_asset, quote_ccy, ts, rate);
```

`positions` ve `ledger_events` ayrıca opsiyonel `strategy_id` taşır (K13).

**Raw payload'ı saklamak zorunlu.** Normalizasyon bug'ı çıktığında ledger'ı raw'dan
yeniden üretebilmek, projeyi kurtaran şey olacak.

---

## 4. Kritik Akışlar

### 4.1 Backfill
```
integration oluştur → permission doğrula → instrument discovery
→ spot: sembol bazlı trade history (Binance spot myTrades sembol ister —
  hangi sembolleri tarayacağını balance + order history'den çıkar)
→ futures: userTrades + income history (funding, commission, realized pnl)
→ deposit/withdrawal history
→ ledger insert (idempotent) → position rebuild
```
Rate limit: her exchange client'ı weight-aware token bucket arkasında olmalı.
Backfill worker'da, API process'inde değil.

### 4.2 Realtime
```
listenKey al → user data stream aç → periyodik keepalive
→ event geldi → normalize → ledger insert → position apply → portfolio/risk invalidate
→ SSE push
```
**Gap handling:** disconnect, keepalive kaçırma veya sequence boşluğunda pozisyon
"canlı" sayılmaz; ilgili aralık için REST resync tetiklenir ve o sırada portfolio
`stale` flag'i ile servis edilir. Sessizce yanlış veri göstermek en kötü senaryo.

### 4.3 Market data → risk
Trade olmasa da fiyat hareket eder:
```
mark price WS → price cache (Redis) + price_ticks (Postgres)
→ pozisyon valuation → equity, unrealized PnL, exposure, liquidation distance
→ threshold değerlendirme → alert
```

### 4.4 Reconciliation
```
worker (örn. 5 dk) → exchange REST snapshot (balances, positionRisk)
→ bizim projeksiyon ile karşılaştır
→ tolerans dışı fark → finding kaydet + alert
```
V1 politikası: **tespit et ve raporla, otomatik düzeltme yok.** Otomatik resync
V2'de, kök neden sınıflandırması oturduktan sonra.

---

## 5. Risk Metrikleri (V1)

```
equity              = cash + spot değeri + perp unrealized PnL
gross_exposure      = Σ |notional|
net_exposure        = Σ  notional
asset_exposure      = varlık bazında net notional
leverage            = gross_exposure / equity
concentration       = |asset_exposure| / gross_exposure
unrealized_pnl      = Σ (mark - avg_entry) * qty * yön
realized_pnl        = ledger'dan kümülatif (funding dahil)
drawdown            = peak equity'den düşüş (equity time series gerekir)
liq_distance        = |mark - liq_price| / mark      (liq_price exchange'ten)
margin_utilization  = used margin / available margin (exchange snapshot)
funding_cost        = dönemsel funding toplamı
```

Drawdown için `equity_snapshots` time series'i tutulur (dakikalık yeterli).

**Strateji seviyesinde (K13):** aynı metrikler `strategy_id` bazında da hesaplanır.
Delta-nötr kurgularda portföy seviyesindeki gross exposure tek başına yanıltıcıdır;
`net_delta_per_asset` strateji içinde raporlanır.

**Collateral (M5, ayrı domain — risk motorunun içinde bir alan değil):**
```
margin_balance          venue bazında
maintenance_margin_rate MMR (venue-içi kaldıraç)
margin_buffer           equity - maintenance margin
loan_to_value           LTV (venue-dışı teminatlı borçlanma — V2)
```

**Senaryo şoku (M7.5):** engine saf fonksiyon olduğu için ucuz.
`POST /risk/scenario` gövdesi `{"BTC": -0.20, "ETH": -0.25}` alır, fiyatları şoklar,
valuation'ı yeniden çalıştırır ve dönen sonuçta equity, margin buffer ve likidasyona
kalan mesafe gösterilir. Kaldıraçlı kullanıcı için tek en değerli özellik budur.

---

## 6. API

```
GET  /portfolio                     ?at=<RFC3339>   (historical reconstruction)
GET  /portfolio/history             ?from&to&interval
GET  /positions
GET  /positions/{id}
GET  /pnl                           ?from&to
GET  /exposure
GET  /risk
GET  /transactions                  (ledger, sayfalı)
GET  /funding
GET  /integrations
POST /integrations/binance
DELETE /integrations/{id}
GET  /reconciliation
GET  /alerts
GET  /alert-rules  |  PUT /alert-rules/{id}

GET  /assets                        (canonical registry + alias çözümü)
GET  /data-quality                  (K14 — açık bulgular, severity'ye göre)
GET  /transfers                     ?unmatched=true
POST /transfers/{out}/link/{in}     (elle transfer eşleştirme)
GET  /strategies  |  POST /strategies
PUT  /positions/{id}/strategy
POST /risk/scenario                 (fiyat şoku, M7.5)
GET  /positions/{id}/lineage        (pozisyonu üreten event zinciri — K1'in ürünü)

GET  /stream/portfolio     (SSE)
GET  /stream/risk          (SSE)
GET  /stream/positions     (SSE)
```

Tüm sayısal alanlar JSON'da **string**. Her response'ta `as_of` ve `stale` alanı bulunur.

---

## 7. Stack

Taslaktaki seçimler korunuyor:

| Katman | Seçim |
|---|---|
| Backend | Go |
| API | Huma (REST + OpenAPI) |
| DB | PostgreSQL |
| DB access | pgx + sqlc (decimal override) |
| Migration | goose |
| Cache | Redis |
| Realtime (client) | SSE |
| Realtime (ingest) | Exchange WebSocket |
| Observability | OpenTelemetry + Prometheus + Grafana |
| Local | Docker Compose |
| Frontend | Next.js + TS + Tailwind + shadcn/ui + Lightweight Charts |

---

## 8. Repo Yapısı

```
backend/
├── cmd/
│   ├── api/
│   └── worker/
├── internal/
│   ├── account/
│   ├── integration/         # exchange connection + credential
│   ├── exchange/
│   │   └── binance/         # rest, ws, normalizer
│   ├── asset/               # canonical asset registry + alias çözümü (K10)
│   ├── instrument/
│   ├── ledger/
│   ├── transfer/            # transfer eşleştirme (K12)
│   ├── marketdata/
│   ├── valuation/           # tek değerleme politikası (K11)
│   ├── position/
│   ├── portfolio/
│   ├── pnl/
│   ├── strategy/            # sleeve etiketleme + strateji bazlı toplama (K13)
│   ├── risk/
│   ├── collateral/          # MMR / margin buffer (M5)
│   ├── reconciliation/
│   ├── quality/             # veri kalitesi kontrolleri (K14)
│   ├── alert/
│   └── store/               # sqlc çıktısı, migration
├── testdata/
│   └── fixtures/binance/    # kaydedilmiş gerçek payload'lar
└── migrations/
```

Klasörler önceden değil, modül yazılırken açılır.

---

## 9. Milestone Planı

| # | Çıktı | Bitiş kriteri |
|---|---|---|
| M0 | İskelet | compose up → api healthz, goose migrate, sqlc generate, otel trace görünür |
| M1 | **Asset/instrument registry + ledger + position engine (ağsız)** | fixture replay ile spot avg-cost + realized PnL testleri geçiyor; alias çözümü test edilmiş |
| M2 | Binance spot backfill | gerçek hesaptan historical trade → ledger, idempotency testi geçiyor |
| M3 | Portfolio + API + lineage | `GET /portfolio` doğru; `GET /positions/{id}/lineage` pozisyonu event'lere kadar açıyor |
| M3.5 | Veri kalitesi + venue-içi transfer | negatif bakiye/gap/unknown-asset kontrolleri çalışıyor; spot ↔ futures transferi satış sayılmıyor |
| M4 | Market data + valuation | price_ticks doluyor, tek `valuation_run`, `GET /portfolio?at=` çalışıyor |
| M5 | Perpetual + collateral | funding, MMR, margin buffer, liq distance; one-way mode |
| M6 | Strateji + risk + alert | strateji bazlı net delta, threshold + histerezis, Telegram/webhook, SSE, dashboard v1 |
| M7 | Reconciliation | sınıflandırılmış finding (missing event / duplicate / rounding / unsupported) + resync aksiyonu |
| M7.5 | Senaryo şoku | `POST /risk/scenario` fiyat şokuyla equity ve margin buffer projeksiyonu |
| M8 | Bybit + venue-arası transfer | iki kaynak tek portföyde doğru normalize; çekim-yatırım eşleşiyor |

**M1 ağa çıkmadan bitirilir.** Engine'i canlı veri üstünde debug etmek en pahalı yol.

---

## 10. Test Stratejisi

- **Golden fixture replay:** kaydedilmiş event dizisi → beklenen position state (JSON karşılaştırma)
- **Idempotency property:** aynı event seti 2× uygulanınca state değişmemeli
- **Order-independence:** `event_time`'a göre sıralanan event'ler, ingest sırasından bağımsız aynı state'i vermeli
- **Rebuild eşitliği:** `positions` tablosu ↔ ledger'dan sıfırdan fold sonucu — birebir aynı
- **Reconciliation:** Binance testnet hesabıyla entegrasyon testi
- Engine'ler saf fonksiyon olduğu için DB'siz unit test edilebilir olmalı

---

## 11. Kodlamadan Önce Netleşmesi Gerekenler

1. Multi-user mu single-user mu? (auth katmanı ve tüm sorgu tenant'ı buna bağlı)
2. `equity`'de baz para birimi: USD mü USDT mi? (stablecoin depeg durumunda fark eder — V1'de tek referans para seç, `fx_rates` ile görüntüleme para birimini ayır)
3. Fee'ler ayrı asset'te alınınca (BNB) cost basis'e nasıl yansıyacak?
4. Spot'ta "position" kavramı: sadece balance mı, yoksa avg cost tutulan pozisyon mu? (V1: avg cost tutulur, aksi halde spot PnL çıkmaz)
5. Backfill'de ne kadar geriye gidilecek? (hesap ömrü boyunca vs son 1 yıl)
6. Bir kullanıcının birden çok Binance sub-account'u olduğunda portföy nasıl toplanacak? (`account → integration → sub-account` hiyerarşisi V1'de modelde olmalı, UI'da olmasa bile)

**Karar verilmiş sayılanlar (rakip analizi sonrası):** average-cost accounting V1'de
kalıyor, ancak ledger **lot-türetilebilir** olmak zorunda — tax lot projeksiyonu
(FIFO/LIFO/HIFO, venue bazlı maliyet takibi) V2'de ledger'dan yeniden hesaplanacak,
şemaya sonradan eklenmeyecek. Bu yüzden avg-cost'u tek gerçek olarak saklamak yasak.

---

## 12. Local / Ajan Çalışma Düzeni

- Repo köküne `CLAUDE.md`: K1–K9 kararları + "float64 yasak", "ledger append-only",
  "engine'ler saf fonksiyon" kuralları yazılı olacak — ajanlar bunları her oturumda okuyor.
- Bir oturum = bir modül. Cross-cutting refactor'ları ayrı oturuma al.
- `make generate` (sqlc) + `make migrate` + `make test` build akışının parçası.
- Exchange payload'ları önce `testdata/fixtures/` altına kaydedilir; ajanlar canlı
  API'ye karşı değil fixture'a karşı çalışır.
- Binance endpoint/parametre detayları (sembol zorunluluğu, listenKey ömrü,
  positionRisk sürümü) resmi dokümandan doğrulanarak kodlanacak — ezberden değil.
