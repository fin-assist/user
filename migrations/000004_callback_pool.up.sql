-- Nullable additions allow the previous binary and existing requests to coexist.
ALTER TABLE phone_verification
    ADD COLUMN allowed_phones_for_call text[],
    ADD COLUMN callback_number text;
