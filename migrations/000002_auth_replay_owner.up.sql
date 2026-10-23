-- Authentication methods have no verified user scope at key acquisition. Keep a
-- minimal account association so a replay checks tombstone and account deletion
-- can purge only this user's encrypted results, without wiping the no-user scope.
CREATE TABLE auth_replay_owner (
    endpoint text NOT NULL,
    key uuid NOT NULL,
    user_id uuid NOT NULL,
    CONSTRAINT auth_replay_owner_pkey PRIMARY KEY (endpoint,key)
);
CREATE INDEX auth_replay_owner_user_idx ON auth_replay_owner(user_id);
COMMENT ON TABLE auth_replay_owner IS 'Владелец зашифрованного результата аутентификации в общем no-user scope';
COMMENT ON COLUMN auth_replay_owner.endpoint IS 'Полное имя RPC из фиксированного allowlist идемпотентности';
COMMENT ON COLUMN auth_replay_owner.key IS 'Ключ идемпотентности, нормализованный UUID';
COMMENT ON COLUMN auth_replay_owner.user_id IS 'Реальный аккаунт результата; без FK, чтобы tombstone проверялся и после удаления app_user';
