-- squawk-ignore ban-drop-column
ALTER TABLE phone_verification DROP COLUMN callback_number;
-- squawk-ignore ban-drop-column
ALTER TABLE phone_verification DROP COLUMN allowed_phones_for_call;
