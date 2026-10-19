-- Schema of the user service (FIN-112). Design notes, invariants and the expected queries
-- with their indexes: docs/schema.md. Tables of the libraries (outbox FIN-109, idempotency
-- FIN-78, tombstone FIN-110) come with their own migration sources and are not created here.
--
-- Times that drive business rules (expiry, verification, rotation, revocation) are written
-- by the service from its clock; now() defaults only on technical created_at columns.
-- Secrets are never stored in the clear: tokens as SHA-256 hashes, results to deliver again
-- as ciphertext of the service key, JWT signing keys not in the database at all.

-- The exclusion constraint "one provider request per phone at a time" needs btree equality
-- in a GiST index. btree_gist is a trusted extension: the owner of the database creates it.
-- The down file leaves it in place (database infrastructure, docs/schema.md).
CREATE EXTENSION IF NOT EXISTS btree_gist;

-- Consent documents (api.md 1.6). A new version is a new row plus a switch of
-- consent_current, both by a data migration.
CREATE TABLE consent_document (
    type text NOT NULL
        CONSTRAINT consent_document_type_format CHECK (type ~ '^[a-z][a-z0-9_]{0,31}$'),
    version text NOT NULL
        CONSTRAINT consent_document_version_format CHECK (version ~ '^[A-Za-z0-9._-]{1,64}$'),
    title text NOT NULL
        CONSTRAINT consent_document_title_length CHECK (length(title) BETWEEN 1 AND 500),
    url text NOT NULL
        CONSTRAINT consent_document_url_https CHECK (url ~ '^https://' AND length(url) <= 2000),
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT consent_document_pkey PRIMARY KEY (type, version)
);

-- The current version of each document type: the one 1.6 returns and 1.3 / 2.3 accept.
CREATE TABLE consent_current (
    type text NOT NULL,
    version text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT consent_current_pkey PRIMARY KEY (type),
    CONSTRAINT consent_current_document_fkey FOREIGN KEY (type, version)
        REFERENCES consent_document (type, version)
);

-- Users and their profile settings. Account deletion deletes the row: the foreign keys
-- below cascade to every row that references the user (consents, requests verified for
-- the existing user or closed by Register, sessions), and a late write for the user fails
-- on its foreign key. Other requests of the phone (unverified, or verified for a new user
-- who never registered) have no user_id and stay until cleanup (docs/schema.md, question 6). That the user_id was deleted is remembered by the tombstone library.
CREATE TABLE app_user (
    id uuid NOT NULL DEFAULT uuidv7(),
    -- E.164, Russian numbers only (api.md 1.1); the login of the user.
    phone text NOT NULL
        CONSTRAINT app_user_phone_format CHECK (phone ~ '^\+7[0-9]{10}$'),
    theme text NOT NULL DEFAULT 'system'
        CONSTRAINT app_user_theme_known CHECK (theme IN ('system', 'light', 'dark')),
    -- IANA name, validated by the service (the Go time zone database the consumers use).
    timezone text NOT NULL DEFAULT 'Europe/Moscow'
        CONSTRAINT app_user_timezone_length CHECK (length(timezone) BETWEEN 1 AND 64),
    -- Monotonic per user; SetUserTimezone receivers apply only a larger one (grpc-api.md 6.1).
    settings_version bigint NOT NULL DEFAULT 1
        CONSTRAINT app_user_settings_version_positive CHECK (settings_version > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT app_user_pkey PRIMARY KEY (id),
    CONSTRAINT app_user_phone_key UNIQUE (phone)
);

-- Consents of a user: the personal data consent of registration and the profile
-- consents[]. One row per grant, kept as history; at most one active grant per type.
CREATE TABLE user_consent (
    user_id uuid NOT NULL,
    id bigint GENERATED ALWAYS AS IDENTITY,
    type text NOT NULL,
    version text NOT NULL,
    granted_at timestamptz NOT NULL,
    revoked_at timestamptz,
    CONSTRAINT user_consent_pkey PRIMARY KEY (user_id, id),
    CONSTRAINT user_consent_user_fkey FOREIGN KEY (user_id)
        REFERENCES app_user (id) ON DELETE CASCADE,
    CONSTRAINT user_consent_document_fkey FOREIGN KEY (type, version)
        REFERENCES consent_document (type, version),
    CONSTRAINT user_consent_revoked_after_granted
        CHECK (revoked_at IS NULL OR revoked_at >= granted_at)
);

-- Also answers GetAssistantConsentStatus on the primary without a heap fetch of old grants.
CREATE UNIQUE INDEX user_consent_active_key ON user_consent (user_id, type) INCLUDE (version)
    WHERE revoked_at IS NULL;
-- The foreign key to consent_document: a document version is never deleted while referenced.
CREATE INDEX user_consent_document_idx ON user_consent (type, version);

-- Phone verification requests (api.md 1.1-1.3, grpc-api.md 7.1).
--   pending   verified_at IS NULL (expired: pending and expires_at has passed);
--   verified  verified_at set, the result is delivered again for 15 minutes from it;
--   closed    closed_at set: the result was used (first RefreshTokens or Register).
-- Tokens (256 random bits) are stored as SHA-256. The result to deliver again (the token
-- pair of an existing user or the registration token of a new one) is ciphertext of the
-- service key (AES-GCM, key id in result_key_id), wiped when the request is closed or its
-- 15 minutes pass.
CREATE TABLE phone_verification (
    id bigint GENERATED ALWAYS AS IDENTITY,
    phone text NOT NULL
        CONSTRAINT phone_verification_phone_format CHECK (phone ~ '^\+7[0-9]{10}$'),
    token_hash bytea NOT NULL
        CONSTRAINT phone_verification_token_hash_sha256 CHECK (octet_length(token_hash) = 32),
    provider text NOT NULL
        CONSTRAINT phone_verification_provider_format CHECK (provider ~ '^[a-z][a-z0-9_]{0,31}$'),
    -- The request at the provider (Zvonok call_id): NULL until the provider accepted it.
    provider_call_id text
        CONSTRAINT phone_verification_provider_call_id_length
            CHECK (length(provider_call_id) BETWEEN 1 AND 128),
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    verified_at timestamptz,
    is_new_user boolean,
    -- The existing user found at verification, or the user created by Register.
    user_id uuid,
    registration_token_hash bytea
        CONSTRAINT phone_verification_registration_token_hash_sha256
            CHECK (octet_length(registration_token_hash) = 32),
    result_ciphertext bytea,
    result_key_id text
        CONSTRAINT phone_verification_result_key_id_format
            CHECK (result_key_id ~ '^[A-Za-z0-9._-]{1,32}$'),
    closed_at timestamptz,
    CONSTRAINT phone_verification_pkey PRIMARY KEY (id),
    CONSTRAINT phone_verification_user_fkey FOREIGN KEY (user_id)
        REFERENCES app_user (id) ON DELETE CASCADE,
    CONSTRAINT phone_verification_token_hash_key UNIQUE (token_hash),
    CONSTRAINT phone_verification_registration_token_hash_key UNIQUE (registration_token_hash),
    CONSTRAINT phone_verification_provider_call_key UNIQUE (provider, provider_call_id),
    CONSTRAINT phone_verification_expires_after_created CHECK (expires_at > created_at),
    CONSTRAINT phone_verification_verified_in_term
        CHECK (verified_at IS NULL OR (verified_at >= created_at AND verified_at <= expires_at)),
    CONSTRAINT phone_verification_verified_needs_provider
        CHECK (verified_at IS NULL OR provider_call_id IS NOT NULL),
    CONSTRAINT phone_verification_outcome_iff_verified
        CHECK ((verified_at IS NULL) = (is_new_user IS NULL)),
    CONSTRAINT phone_verification_existing_user_known
        CHECK (is_new_user IS DISTINCT FROM false OR user_id IS NOT NULL),
    CONSTRAINT phone_verification_registration_token_iff_new_user
        CHECK ((registration_token_hash IS NOT NULL) = (is_new_user IS TRUE)),
    CONSTRAINT phone_verification_result_key_pair
        CHECK ((result_ciphertext IS NULL) = (result_key_id IS NULL)),
    CONSTRAINT phone_verification_result_only_verified
        CHECK (result_ciphertext IS NULL OR verified_at IS NOT NULL),
    CONSTRAINT phone_verification_closed_after_verified
        CHECK (closed_at IS NULL OR (verified_at IS NOT NULL AND closed_at >= verified_at)),
    CONSTRAINT phone_verification_closed_has_no_result
        CHECK (closed_at IS NULL OR result_ciphertext IS NULL),
    CONSTRAINT phone_verification_closed_new_user_registered
        CHECK (closed_at IS NULL OR user_id IS NOT NULL),
    -- The provider accepts one request per phone until the previous one expires (api.md
    -- 1.1, "номер занят у провайдера"), closed or not: requests of a phone never overlap in
    -- time. INSERT ... ON CONFLICT ON CONSTRAINT phone_verification_one_per_phone DO NOTHING
    -- turns a busy phone into an empty RETURNING, other conflicts stay errors;
    -- the index also answers "the request of this phone at moment t".
    CONSTRAINT phone_verification_one_per_phone
        EXCLUDE USING gist (phone WITH =, tstzrange(created_at, expires_at) WITH &&)
);

-- Account deletion (cascade) and "requests of this user".
CREATE INDEX phone_verification_user_idx ON phone_verification (user_id)
    WHERE user_id IS NOT NULL;
-- Cleanup of old requests: they hold the phone number.
CREATE INDEX phone_verification_created_idx ON phone_verification (created_at);

-- WatchPhoneVerification listens on the channel phone_verification; the payload is the id
-- of the request. NOTIFY is delivered when the transaction that verified it commits.
CREATE FUNCTION phone_verification_notify() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('phone_verification', NEW.id::text);
    RETURN NULL;
END
$$;

CREATE TRIGGER phone_verification_verified_notify
    AFTER UPDATE OF verified_at ON phone_verification
    FOR EACH ROW
    WHEN (OLD.verified_at IS NULL AND NEW.verified_at IS NOT NULL)
    EXECUTE FUNCTION phone_verification_notify();

-- Refresh sessions: a session is one rotation chain, refresh_token its links. A rotated
-- link is kept until it expires, so presenting it again is recognised as reuse and revokes
-- the session (api.md 1.4). The idempotent retry of RefreshTokens within 60 s never gets
-- here: the idempotency step before the token check answers it from the stored response.
-- Logout deletes the session; account deletion deletes it by cascade.
CREATE TABLE auth_session (
    id bigint GENERATED ALWAYS AS IDENTITY,
    user_id uuid NOT NULL,
    -- The verification whose result issued the first link; the first rotation closes it.
    phone_verification_id bigint,
    created_at timestamptz NOT NULL DEFAULT now(),
    -- Set when reuse revoked the chain; its tokens then get TOKEN_REVOKED until they expire.
    revoked_at timestamptz,
    CONSTRAINT auth_session_pkey PRIMARY KEY (id),
    CONSTRAINT auth_session_user_fkey FOREIGN KEY (user_id)
        REFERENCES app_user (id) ON DELETE CASCADE,
    CONSTRAINT auth_session_phone_verification_fkey FOREIGN KEY (phone_verification_id)
        REFERENCES phone_verification (id) ON DELETE SET NULL
);

-- Account deletion (cascade) and "sessions of this user".
CREATE INDEX auth_session_user_idx ON auth_session (user_id);
-- ON DELETE SET NULL when old verification requests are cleaned up.
CREATE INDEX auth_session_phone_verification_idx ON auth_session (phone_verification_id)
    WHERE phone_verification_id IS NOT NULL;

CREATE TABLE refresh_token (
    token_hash bytea NOT NULL
        CONSTRAINT refresh_token_hash_sha256 CHECK (octet_length(token_hash) = 32),
    session_id bigint NOT NULL,
    -- 1 for the link issued at sign-in, +1 on each rotation.
    generation bigint NOT NULL
        CONSTRAINT refresh_token_generation_positive CHECK (generation > 0),
    issued_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    rotated_at timestamptz,
    CONSTRAINT refresh_token_pkey PRIMARY KEY (token_hash),
    CONSTRAINT refresh_token_session_fkey FOREIGN KEY (session_id)
        REFERENCES auth_session (id) ON DELETE CASCADE,
    CONSTRAINT refresh_token_generation_key UNIQUE (session_id, generation),
    CONSTRAINT refresh_token_expires_after_issued CHECK (expires_at > issued_at),
    CONSTRAINT refresh_token_rotated_after_issued CHECK (rotated_at IS NULL OR rotated_at >= issued_at)
);

-- At most one live (not rotated) link per session: of two concurrent rotations of the same
-- link only one inserts its successor.
CREATE UNIQUE INDEX refresh_token_one_live_key ON refresh_token (session_id)
    WHERE rotated_at IS NULL;
-- Cleanup of expired links.
CREATE INDEX refresh_token_expires_idx ON refresh_token (expires_at);

-- Fixed-window counters of the limits (FIN-81): verifications per phone and per x-client-ip,
-- the global ceiling of new requests (subject ''), the request limits of
-- WatchPhoneVerification, Register and RefreshTokens per x-client-ip. scope names the
-- limit and its window length ('verification_phone_1h'); subject is the phone, the address
-- or ''. A row lives until window_end plus a margin.
CREATE TABLE rate_limit_window (
    scope text NOT NULL
        CONSTRAINT rate_limit_window_scope_format CHECK (scope ~ '^[a-z][a-z0-9_]{0,63}$'),
    subject text NOT NULL
        CONSTRAINT rate_limit_window_subject_length CHECK (length(subject) <= 64),
    window_start timestamptz NOT NULL,
    window_end timestamptz NOT NULL,
    hits bigint NOT NULL
        CONSTRAINT rate_limit_window_hits_positive CHECK (hits > 0),
    CONSTRAINT rate_limit_window_pkey PRIMARY KEY (scope, subject, window_start),
    CONSTRAINT rate_limit_window_end_after_start CHECK (window_end > window_start)
);

-- Cleanup of finished windows.
CREATE INDEX rate_limit_window_end_idx ON rate_limit_window (window_end);
