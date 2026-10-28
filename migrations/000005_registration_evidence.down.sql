-- Runtime evidence cannot be dropped before its independently defined retention.
-- Lock closes the admission race between the empty check and the DDL below.
LOCK TABLE phone_consent_receipt IN ACCESS EXCLUSIVE MODE;
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM phone_consent_receipt LIMIT 1) THEN
        RAISE EXCEPTION '000005 rollback forbidden: durable consent evidence must be retained';
    END IF;
END $$;
-- Cutover invalidation cannot safely be undone; old phone results stay invalid.
ALTER TABLE phone_verification DROP COLUMN start_terminal,DROP COLUMN campaign,
    DROP COLUMN known_busy_until,DROP COLUMN declared_provider_expiry,
    DROP COLUMN dispatch_not_after,DROP COLUMN first_dispatch_at,DROP COLUMN unknown_deadline,
    DROP COLUMN consent_receipt_id;
DROP TABLE phone_known_busy;
DROP TABLE phone_provider_binding;
DROP TABLE phone_consent_receipt;
