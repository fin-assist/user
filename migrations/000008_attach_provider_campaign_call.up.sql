-- 000007 already built the replacement unique index without a blocking heap scan.
-- This atomic switch preserves the canonical constraint name used by error mapping.
-- squawk-ignore ban-drop-constraint
ALTER TABLE phone_verification DROP CONSTRAINT phone_verification_provider_call_key;
ALTER TABLE phone_verification ADD CONSTRAINT phone_verification_provider_call_key
    UNIQUE USING INDEX phone_verification_provider_campaign_call_uidx;
