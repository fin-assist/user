-- Schema of the user service (FIN-112). Design notes, keys, the expected queries with their
-- indexes and the invariants the service code keeps: docs/schema.md. Tables of the libraries
-- (outbox FIN-109, idempotency FIN-78, tombstone FIN-110) come with their own migration
-- sources and are not created here.
--
-- The schema holds only keys, foreign keys (with their cascades), NOT NULL and defaults
-- (owner's decision of 19.10.2026): no CHECK, EXCLUDE, triggers or functions. Formats,
-- ranges, the consistency of states and notifications are the service's code.
-- Times that drive business rules (expiry, verification, rotation, revocation, updated_at)
-- are written by the service from its clock; now() defaults only on technical columns.
-- Secrets are never stored in the clear: tokens as SHA-256 hashes, results to deliver again
-- as ciphertext of the service key, JWT signing keys not in the database at all.

-- Consent documents (api.md 1.6). A new version is a new row plus a switch of
-- consent_current, both by a data migration.
CREATE TABLE consent_document (
    type text NOT NULL,
    version text NOT NULL,
    title text NOT NULL,
    url text NOT NULL,
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
-- below cascade to every row that references the user (consents, requests verified for the
-- existing user or closed by Register, sessions), and a late write for the user fails on
-- its foreign key. Other requests of the phone (unverified, or verified for a new user who
-- never registered) have no user_id and stay until cleanup (docs/schema.md, question 6).
-- That the user_id was deleted is remembered by the tombstone library.
CREATE TABLE app_user (
    id uuid NOT NULL DEFAULT uuidv7(),
    -- E.164; the login of the user.
    phone text NOT NULL,
    theme text NOT NULL DEFAULT 'system',
    -- IANA name.
    timezone text NOT NULL DEFAULT 'Europe/Moscow',
    -- Monotonic per user; SetUserTimezone receivers apply only a larger one (grpc-api.md 6.1).
    settings_version integer NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now(),
    -- Last change of the profile settings; set by the service with every UPDATE.
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT app_user_pkey PRIMARY KEY (id),
    CONSTRAINT app_user_phone_key UNIQUE (phone)
);

-- Consents of a user: the personal data consent of registration and the profile
-- consents[]. One row per grant, kept as history; at most one active grant per type.
CREATE TABLE user_consent (
    user_id uuid NOT NULL,
    id integer GENERATED ALWAYS AS IDENTITY,
    type text NOT NULL,
    version text NOT NULL,
    granted_at timestamptz NOT NULL,
    revoked_at timestamptz,
    CONSTRAINT user_consent_pkey PRIMARY KEY (user_id, id),
    CONSTRAINT user_consent_user_fkey FOREIGN KEY (user_id)
        REFERENCES app_user (id) ON DELETE CASCADE,
    CONSTRAINT user_consent_document_fkey FOREIGN KEY (type, version)
        REFERENCES consent_document (type, version)
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
-- 15 minutes pass. The transaction that verifies a request also calls
-- pg_notify('phone_verification', id) for WatchPhoneVerification.
--
-- The provider takes one request per phone until the previous one expires, closed or not
-- (api.md 1.1). A request holds its phone until released_at is set; at most one holding
-- request per phone is the partial unique index below. The service releases a holder whose
-- expires_at has passed in the transaction that creates the next request, then inserts with
-- ON CONFLICT (phone) WHERE released_at IS NULL DO NOTHING (docs/schema.md).
CREATE TABLE phone_verification (
    id integer GENERATED ALWAYS AS IDENTITY,
    phone text NOT NULL,
    token_hash bytea NOT NULL,
    provider text NOT NULL,
    -- The request at the provider (Zvonok call_id): NULL until the provider accepted it.
    provider_call_id text,
    created_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    verified_at timestamptz,
    is_new_user boolean,
    -- The existing user found at verification, or the user created by Register.
    user_id uuid,
    registration_token_hash bytea,
    result_ciphertext bytea,
    result_key_id text,
    closed_at timestamptz,
    -- Set once expires_at has passed and the next request of the phone (or cleanup) frees it.
    released_at timestamptz,
    CONSTRAINT phone_verification_pkey PRIMARY KEY (id),
    CONSTRAINT phone_verification_user_fkey FOREIGN KEY (user_id)
        REFERENCES app_user (id) ON DELETE CASCADE,
    CONSTRAINT phone_verification_token_hash_key UNIQUE (token_hash),
    CONSTRAINT phone_verification_registration_token_hash_key UNIQUE (registration_token_hash),
    CONSTRAINT phone_verification_provider_call_key UNIQUE (provider, provider_call_id)
);

-- One holding request per phone; also finds the holder for retry_at of the duplicate.
CREATE UNIQUE INDEX phone_verification_phone_held_key ON phone_verification (phone)
    WHERE released_at IS NULL;
-- Account deletion (cascade) and "requests of this user".
CREATE INDEX phone_verification_user_idx ON phone_verification (user_id)
    WHERE user_id IS NOT NULL;
-- Cleanup of old requests: they hold the phone number.
CREATE INDEX phone_verification_created_idx ON phone_verification (created_at);

-- Refresh sessions: a session is one rotation chain, refresh_token its links. A rotated
-- link is kept until it expires, so presenting it again is recognised as reuse and revokes
-- the session (api.md 1.4). The idempotent retry of RefreshTokens within 60 s never gets
-- here: the idempotency step before the token check answers it from the stored response.
-- Logout deletes the session; account deletion deletes it by cascade.
CREATE TABLE auth_session (
    id integer GENERATED ALWAYS AS IDENTITY,
    user_id uuid NOT NULL,
    -- The verification whose result issued the first link; the first rotation closes it.
    phone_verification_id integer,
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
    token_hash bytea NOT NULL,
    session_id integer NOT NULL,
    -- 1 for the link issued at sign-in, +1 on each rotation.
    generation integer NOT NULL,
    issued_at timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    rotated_at timestamptz,
    CONSTRAINT refresh_token_pkey PRIMARY KEY (token_hash),
    CONSTRAINT refresh_token_session_fkey FOREIGN KEY (session_id)
        REFERENCES auth_session (id) ON DELETE CASCADE,
    CONSTRAINT refresh_token_generation_key UNIQUE (session_id, generation)
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
    scope text NOT NULL,
    subject text NOT NULL,
    window_start timestamptz NOT NULL,
    window_end timestamptz NOT NULL,
    hits integer NOT NULL,
    CONSTRAINT rate_limit_window_pkey PRIMARY KEY (scope, subject, window_start)
);

-- Cleanup of finished windows.
CREATE INDEX rate_limit_window_end_idx ON rate_limit_window (window_end);
