-- +goose Up

-- A transfer said how much of what, and nothing about where.
--
-- 00012 gave balance events an asset_id, which is enough for a deposit: money arrives from
-- outside and the account holds more of it. A transfer is different. Binance reports an
-- intra-venue move as ONE row whose direction lives in its `type` -- MAIN_UMFUTURE is spot
-- to USD-M futures, UMFUTURE_MAIN is the way back (F10). There are no two halves to match,
-- so the whole movement is on the event, and the only thing the fold needs in order to
-- know whether the account's holdings changed is which two wallets it ran between.
--
-- That question has a surprising answer for the common case: nothing changed. asset_balances
-- is keyed per integration, not per wallet, so moving USDT from spot to futures leaves the
-- account holding exactly what it held. The event is history, not arithmetic -- and the
-- endpoints are what let the fold prove that rather than assume it.
ALTER TABLE ledger_events ADD COLUMN transfer_from TEXT;
ALTER TABLE ledger_events ADD COLUMN transfer_to   TEXT;

-- Both endpoints, or neither, and which one it is follows from the event type. The second
-- constraint is the one a schema written only against the missing case would omit, and it
-- guards the more insidious error: a fill that acquired a wallet nobody reads would pass
-- every fold silently, because no fold looks for a wallet on a TRADE.
--
-- Validated rather than NOT VALID -- unlike 00012, which had rows whose asset lived only in
-- raw, every row that exists now has NULL in both columns and satisfies all four checks. A
-- constraint Postgres has actually proven beats one it has merely promised to enforce next
-- time.
ALTER TABLE ledger_events ADD CONSTRAINT transfers_name_both_endpoints
  CHECK (event_type <> 'TRANSFER'
         OR (transfer_from IS NOT NULL AND transfer_to IS NOT NULL));

ALTER TABLE ledger_events ADD CONSTRAINT endpoints_belong_to_transfers
  CHECK (event_type = 'TRANSFER'
         OR (transfer_from IS NULL AND transfer_to IS NULL));

-- The vocabulary is closed for the same reason event_type is: a wallet stored under a name
-- no fold recognizes is an event that quietly does nothing. `external` is in it from the
-- first day though nothing writes one yet -- it is the far side of a cross-venue transfer
-- (M8), and a vocabulary that has to grow to admit the known case was the wrong vocabulary.
ALTER TABLE ledger_events ADD CONSTRAINT transfer_endpoints_are_known_wallets
  CHECK ((transfer_from IS NULL
          OR transfer_from IN ('spot', 'usdm', 'coinm', 'margin', 'funding', 'external'))
     AND (transfer_to IS NULL
          OR transfer_to   IN ('spot', 'usdm', 'coinm', 'margin', 'funding', 'external')));

-- A wallet cannot transfer to itself. Binance has no such type, so a row saying otherwise
-- is a normalizer that failed to parse a direction and defaulted -- which would fold to
-- nothing and look, from the balance alone, exactly like the internal transfer that is
-- supposed to fold to nothing.
ALTER TABLE ledger_events ADD CONSTRAINT transfer_endpoints_differ
  CHECK (transfer_from IS NULL OR transfer_from <> transfer_to);

-- +goose Down
ALTER TABLE ledger_events DROP CONSTRAINT transfer_endpoints_differ;
ALTER TABLE ledger_events DROP CONSTRAINT transfer_endpoints_are_known_wallets;
ALTER TABLE ledger_events DROP CONSTRAINT endpoints_belong_to_transfers;
ALTER TABLE ledger_events DROP CONSTRAINT transfers_name_both_endpoints;
ALTER TABLE ledger_events DROP COLUMN transfer_to;
ALTER TABLE ledger_events DROP COLUMN transfer_from;
