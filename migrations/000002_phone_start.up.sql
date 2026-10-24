ALTER TABLE phone_verification ADD COLUMN start_key uuid, ADD COLUMN start_request_hash bytea, ADD COLUMN start_reserved_windows jsonb;
CREATE UNIQUE INDEX phone_verification_start_key ON phone_verification(start_key) WHERE start_key IS NOT NULL;
COMMENT ON COLUMN phone_verification.start_key IS 'Ключ собственной попытки создания; ответы и связи busy-ключей хранятся в go-idempotency';
COMMENT ON COLUMN phone_verification.start_request_hash IS 'Хеш полного логического запроса собственной попытки для восстановления после потери резерва';
COMMENT ON COLUMN phone_verification.start_reserved_windows IS 'Четыре исходных окна лимитов незавершённой попытки; пустой массив после завершения или дубля провайдера';
