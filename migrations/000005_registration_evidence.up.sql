-- FIN-364: receipts are intentionally independent of working requests/accounts.
-- Application code validates states, timestamps and ownership; no DB business logic.
CREATE TABLE phone_consent_receipt (
    attempt_id integer PRIMARY KEY,
    phone text NOT NULL,
    client_ip text NOT NULL,
    consent_type text NOT NULL DEFAULT 'personal_data',
    consent_version text NOT NULL,
    consent_accepted_at timestamptz NOT NULL,
    age_confirmed_at timestamptz NOT NULL,
    attempt_end timestamptz NOT NULL,
    retain_until timestamptz,
    linked_user_id uuid,
    linked_at timestamptz,
    start_key uuid,
    request_hash bytea,
    key_expires_at timestamptz NOT NULL,
    CONSTRAINT phone_consent_receipt_document_fkey FOREIGN KEY(consent_type,consent_version)
        REFERENCES consent_document(type,version)
);
CREATE INDEX phone_consent_receipt_key_idx ON phone_consent_receipt(start_key);
CREATE INDEX phone_consent_receipt_user_idx ON phone_consent_receipt(linked_user_id);
CREATE INDEX phone_consent_receipt_retention_idx ON phone_consent_receipt(retain_until);
CREATE TABLE phone_provider_binding (
    provider text NOT NULL,
    campaign text NOT NULL,
    call_id text NOT NULL,
    attempt_id integer NOT NULL REFERENCES phone_consent_receipt(attempt_id) ON DELETE CASCADE,
    provider_created_at timestamptz NOT NULL,
    declared_expiry timestamptz NOT NULL,
    public_deadline timestamptz NOT NULL,
    busy_until timestamptz NOT NULL,
    PRIMARY KEY(provider,campaign,call_id)
);
CREATE INDEX phone_provider_binding_attempt_idx ON phone_provider_binding(attempt_id);
CREATE TABLE phone_known_busy (
    phone text PRIMARY KEY,
    busy_until timestamptz NOT NULL
);
ALTER TABLE phone_verification
    ADD COLUMN consent_receipt_id integer REFERENCES phone_consent_receipt(attempt_id),
    ADD COLUMN unknown_deadline timestamptz,
    ADD COLUMN first_dispatch_at timestamptz,
    ADD COLUMN dispatch_not_after timestamptz,
    ADD COLUMN declared_provider_expiry timestamptz,
    ADD COLUMN known_busy_until timestamptz,
    ADD COLUMN campaign text NOT NULL DEFAULT '',
    ADD COLUMN start_terminal boolean NOT NULL DEFAULT false;
ALTER TABLE phone_verification DROP CONSTRAINT phone_verification_provider_call_key;
ALTER TABLE phone_verification ADD CONSTRAINT phone_verification_provider_call_key UNIQUE(provider,campaign,provider_call_id);
-- Invalidate pre-evidence pending/results, including undelivered verified pairs.
-- Already-issued session chains survive regardless of first Refresh timing.
UPDATE phone_verification SET closed_at=COALESCE(closed_at,expires_at),released_at=COALESCE(released_at,expires_at),
    result_ciphertext=NULL,result_key_id=NULL,registration_token_hash=NULL,start_terminal=true;
