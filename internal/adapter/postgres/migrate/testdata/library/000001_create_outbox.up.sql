CREATE TABLE lib_outbox (
    id      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    payload jsonb  NOT NULL
);
