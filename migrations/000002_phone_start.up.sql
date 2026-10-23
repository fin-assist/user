CREATE TABLE phone_verification_start (
    idempotency_key uuid PRIMARY KEY,
    request_hash bytea NOT NULL,
    verification_id integer NOT NULL REFERENCES phone_verification(id) ON DELETE CASCADE,
    reserved_windows jsonb NOT NULL
);
CREATE INDEX phone_verification_start_request_idx ON phone_verification_start(verification_id);
COMMENT ON TABLE phone_verification_start IS 'Создание заявки и связь ключей, включая отказы по занятости, с удаляемыми данными телефона';
COMMENT ON COLUMN phone_verification_start.idempotency_key IS 'Ключ StartPhoneVerification; ответ хранится только в go-idempotency';
COMMENT ON COLUMN phone_verification_start.request_hash IS 'Хеш полного логического запроса';
COMMENT ON COLUMN phone_verification_start.verification_id IS 'Заявка собственной попытки либо занятая заявка для адресного удаления ключа';
COMMENT ON COLUMN phone_verification_start.reserved_windows IS 'Четыре исходных окна резервируемых лимитов; пустой массив после завершения или отказа по занятости';
