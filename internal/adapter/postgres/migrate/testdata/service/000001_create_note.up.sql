CREATE TABLE note (
    id   bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    body text   NOT NULL
);
