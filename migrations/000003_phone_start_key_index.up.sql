CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS phone_verification_start_key ON phone_verification(start_key) WHERE start_key IS NOT NULL;
