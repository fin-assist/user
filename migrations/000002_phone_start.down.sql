DROP INDEX phone_verification_start_key;
ALTER TABLE phone_verification DROP COLUMN start_reserved_windows, DROP COLUMN start_request_hash, DROP COLUMN start_key;
