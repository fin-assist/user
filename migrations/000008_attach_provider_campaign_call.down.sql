-- Offline downgrade only: retained proof forbids changing its binding schema.
LOCK TABLE phone_consent_receipt IN ACCESS EXCLUSIVE MODE;
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM phone_consent_receipt LIMIT 1) THEN
        RAISE EXCEPTION '000008 rollback forbidden: durable consent evidence must be retained';
    END IF;
END $$;
ALTER TABLE phone_verification DROP CONSTRAINT phone_verification_provider_call_key;
-- DROP CONSTRAINT removed its attached index. Restore the standalone 000007
-- index so down(8)->up(8) can attach it again without replaying migration 000007.
CREATE UNIQUE INDEX phone_verification_provider_campaign_call_uidx ON phone_verification(provider,campaign,provider_call_id);
ALTER TABLE phone_verification ADD CONSTRAINT phone_verification_provider_call_key
    UNIQUE(provider,provider_call_id);
