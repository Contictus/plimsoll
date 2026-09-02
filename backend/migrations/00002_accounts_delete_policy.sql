-- +goose Up
-- 00001 gave accounts policies for SELECT, UPDATE and INSERT but none for DELETE, which
-- means FORCE ROW LEVEL SECURITY silently blocked deletion for the owner too. Nothing in
-- the product deletes an account -- disabled_at is the mechanism -- but an operator (and
-- a test tearing down its fixtures) needs the path to exist, and a DELETE that matches
-- nothing without saying so is exactly the kind of silence this project rejects.
--
-- Safe to add: plimsoll_app holds no DELETE grant on accounts, so the policy is
-- unreachable from the application no matter what it does.
CREATE POLICY accounts_delete_self ON accounts
  FOR DELETE USING (id = app_current_account());

-- +goose Down
DROP POLICY accounts_delete_self ON accounts;
