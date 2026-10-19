DROP TABLE rate_limit_window;
DROP TABLE refresh_token;
DROP TABLE auth_session;
DROP TRIGGER phone_verification_verified_notify ON phone_verification;
DROP FUNCTION phone_verification_notify();
DROP TABLE phone_verification;
DROP TABLE user_consent;
DROP TABLE app_user;
DROP TABLE consent_current;
DROP TABLE consent_document;
-- btree_gist stays: it is database infrastructure (the up file creates it only IF NOT
-- EXISTS, a cluster may install it beforehand), and dropping it needs its ownership.
