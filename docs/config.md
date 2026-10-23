# Конфигурация

Всё — переменные окружения (`internal/config`). Секрет можно передать файлом: `VAR_FILE=/путь` вместо `VAR` (systemd `LoadCredential`, смонтированный Secret Kubernetes); заданы оба — ошибка запуска. Ошибки конфигурации выводятся все сразу и не содержат значений секретов.

Шаблон не опирается на Kubernetes: адреса других сервисов, DSN и файл токена — обычные настройки, одинаковые для k3s и стенда под systemd.

## Как меняется

Таблицы ниже **генерируются** из списка `Vars` в `internal/config/vars.go` — там у каждой переменной имя, значение по умолчанию, признак секрета и описание; код берёт значения по умолчанию оттуда же. Новая переменная или новое значение по умолчанию:

1. строка в `Vars`, чтение в `load` / `loadMigrate`;
2. `go test ./internal/config -run TestDocIsCurrent -update` — переписывает таблицы между маркерами.

Руками блоки между маркерами не правятся. CI (`go test`) падает, если таблица устарела (`TestDocIsCurrent`), если код читает необъявленную переменную или объявленная нигде не читается (`TestVarsMatchWhatLoadReads`).

## `serve`

PHONE_RESULT_KEY_FILE обязателен: отдельный 32-байтный AES-256 ключ 0600, общий для
реплик; совпадение с материалом AUTH_IDEMPOTENCY_KEY_FILE запрещает запуск. Сохраните
ключ и PHONE_RESULT_KEY_ID до истечения всех 15-минутных результатов. Порядок замены
и ограничения подписок — [verification.md](verification.md).

Для каждого получателя OUTBOX_TIMEZONE_TARGETS и OUTBOX_DELETION_TARGETS задаётся
PEER_<NAME>_ADDR; по умолчанию нужны PEER_BANK_STATEMENT_DATA_ADDR, PEER_ANALYTICS_ADDR
и PEER_ASSISTANT_ADDR. Worker использует отдельный primary-пул OUTBOX_DB_MAX_CONNS;
его ёмкость не меньше числа уникальных соседей обоих маршрутов + 3 (минимум 6).
Подробности — [profile.md](profile.md), [deletion.md](deletion.md). Адреса проверяются при сборке; доступность соседей — через retry outbox.

<!-- generated:config-serve:start (go test ./internal/config -run TestDocIsCurrent -update) -->
| Переменная | По умолчанию | Что |
|---|---|---|
| `SERVICE_NAME` | — (обязательна) | имя сервиса: логи, `application_name`, токен в dev |
| `ENV` | `prod` | `dev`, `stage` или `prod`. Reflection включён только при `dev` |
| `AUTH_MODE` | `verify` | `verify` — проверка JWT и сервисных токенов через go-identity; `insecure-dev` — токены `dev:<имя>` без проверки, только при `ENV=dev` |
| `JWT_SIGNING_KEY_FILE` | — | PKCS#8 PEM Ed25519, приватный ключ access JWT; файл 0600, обязателен при старте |
| `JWT_PUBLIC_KEYS_FILE` | — | необязательный публичный JWKS Ed25519 для перекрытия ключей при ротации |
| `JWT_KEY_ID` | — | kid активного ключа access JWT, обязателен при старте |
| `JWT_ISSUER` | — | iss access JWT, обязателен; одинаков у всех потребителей |
| `JWT_AUDIENCE` | — | aud access JWT, обязателен; одинаков у всех потребителей |
| `AUTH_IDEMPOTENCY_KEY_FILE` | — | файл 0600 с 32 raw-байтами AES-256 для шифрования ответов Register/Refresh; обязателен, общий для реплик; сохранять ключ не менее 24 ч после последней записи |
| `PHONE_RESULT_KEY_FILE` | — | файл 0600 с 32 raw-байтами AES-256 для результата подтверждения; обязателен, общий для реплик, отличается от AUTH_IDEMPOTENCY_KEY_FILE; хранить до истечения всех 15-минутных результатов |
| `PHONE_RESULT_KEY_ID` | `primary` | ID ключа результата: 1–64 символа A-Z/a-z/0-9/._-; сохраняется вместе с ciphertext |
| `PHONE_WATCH_MAX_STREAMS` | `1024` | предел одновременно ожидающих Watch на экземпляр (1–100000); отдельный от 60 попыток/мин на IP; насыщение видно в метриках |
| `PHONE_WATCH_RESYNC_INTERVAL` | `5s` | повторное чтение primary открытым Watch в дополнение к NOTIFY; положительный интервал не больше 30 с |
| `AUTH_DB_MAX_CONNS` | `8` | отдельный primary pool транзакций авторизации; лимиты используют обычный pool, чтобы ошибки токена не откатывали учёт попыток |
| `REFRESH_TOKEN_TTL` | `1440h` | скользящий срок refresh-токена; по умолчанию 60 суток, продлевается каждой ротацией |
| `SERVICE_AUTH_MODE` | `kubernetes` | kubernetes — ServiceAccount; stage — подписанные JWT сервисов, только dev/stage |
| `SERVICE_TOKEN_ISSUER` | — | iss сервисных токенов; для kubernetes — issuer кластера, для stage — общий issuer стенда |
| `STAGE_SIGNING_KEY_FILE` | — | приватный PKCS#8 Ed25519 ключ этого сервиса для SERVICE_AUTH_MODE=stage |
| `STAGE_PUBLIC_KEYS_FILE` | — | публичный JWKS сервисов стенда; kid — имя сервиса; отдельный от ключей пользователей |
| `KUBERNETES_JWKS_URL` | `https://kubernetes.default.svc/openid/v1/jwks` | доверенный HTTPS endpoint JWKS API-сервера |
| `KUBERNETES_CA_FILE` | `/var/run/secrets/kubernetes.io/serviceaccount/ca.crt` | доверенный CA API-сервера, TLS-проверка обязательна |
| `KUBERNETES_TOKEN_FILE` | `/var/run/secrets/kubernetes.io/serviceaccount/token` | штатный API-server-audience токен пода для чтения JWKS; не finassist-internal |
| `SERVICE_TOKEN_FILE` | — | файл с токеном ServiceAccount этого сервиса для исходящих вызовов (FIN-119) |
| `DB_PRIMARY_DSN` / `_FILE` | — (обязательна) | DSN primary (`-rw`) |
| `DB_REPLICA_DSN` / `_FILE` | — | DSN реплик (`-ro`); пусто — чтение тоже с primary. На стенде оба указывают на один PostgreSQL |
| `DB_MAX_CONNS` | `10` | размер каждого пула |
| `OUTBOX_TIMEZONE_TARGETS` | `bank-statement-data,assistant` | получатели user.timezone_changed через запятую без пробелов; каждому нужен PEER_<NAME>_ADDR; 1–64 уникальных имени |
| `OUTBOX_DELETION_TARGETS` | `bank-statement-data,analytics,assistant` | получатели user.deleted; обязательны bank-statement-data, analytics, assistant; каждому нужен PEER_<NAME>_ADDR; дополнительные получатели допустимы |
| `OUTBOX_DB_MAX_CONNS` | `8` | размер отдельного primary-пула доставщика, не меньше числа уникальных получателей обоих типов событий + 3 |
| `OUTBOX_POLL_INTERVAL` | `3s` | запасной опрос outbox при отсутствии уведомлений, положительный интервал |
| `LIMIT_VERIFICATION_PHONE_SHORT` | `3` | StartPhoneVerification: предел короткого окна на телефон |
| `WINDOW_VERIFICATION_PHONE_SHORT` | `1h` | StartPhoneVerification: короткое UTC-окно на телефон |
| `LIMIT_VERIFICATION_PHONE_LONG` | `10` | StartPhoneVerification: предел длинного окна на телефон |
| `WINDOW_VERIFICATION_PHONE_LONG` | `24h` | StartPhoneVerification: длинное UTC-окно на телефон |
| `LIMIT_VERIFICATION_IP_SHORT` | `30` | StartPhoneVerification: предел короткого окна на IP |
| `WINDOW_VERIFICATION_IP_SHORT` | `1m` | StartPhoneVerification: короткое UTC-окно на IP |
| `LIMIT_VERIFICATION_IP_LONG` | `300` | StartPhoneVerification: предел длинного окна на IP |
| `WINDOW_VERIFICATION_IP_LONG` | `1h` | StartPhoneVerification: длинное UTC-окно на IP |
| `LIMIT_WATCH_IP` | `60` | WatchPhoneVerification: предел попыток открыть поток на IP, до проверки токена |
| `WINDOW_WATCH_IP` | `1m` | WatchPhoneVerification: UTC-окно попыток открыть поток |
| `LIMIT_REGISTER_IP` | `30` | Register: предел запросов на IP, после идемпотентности, до проверки токена |
| `WINDOW_REGISTER_IP` | `1m` | Register: UTC-окно запросов на IP |
| `LIMIT_REFRESH_IP` | `120` | RefreshTokens: предел запросов на IP, после идемпотентности, до проверки токена |
| `WINDOW_REFRESH_IP` | `1m` | RefreshTokens: UTC-окно запросов на IP |
| `GRPC_ADDR` | `:9090` | gRPC |
| `GRPC_MAX_RECV_MSG_SIZE` | `4194304` | байт; 16 МиБ там, где обслуживается `SearchOperations` (grpc-api.md 9) |
| `HTTP_ADDR` | `:9091` | `/metrics`, `/healthz`, `/readyz` |
| `PEER_<NAME>_ADDR` | — | адрес другого сервиса: `PEER_BANK_STATEMENT_DATA_ADDR` → `cfg.Peers["bank-statement-data"]`. k3s: `dns:///bank-statement-data.finassist.svc.cluster.local:9090`; стенд: `dns:///127.0.0.1:9092` |
| `LOG_LEVEL` | `info` | `debug` добавляет в строку вызова metadata и запрос с замаскированными токенами |
| `LOG_FORMAT` | `json` | `json` или `text` |
| `SHUTDOWN_DELAY` | `5s` | после `NOT_SERVING` сервер ещё принимает вызовы, пока балансировщики не уберут под |
| `SHUTDOWN_TIMEOUT` | `20s` | сколько ждать завершения вызовов; оставшиеся обрываются (потоки ответа клиент переоткроет) |
<!-- generated:config-serve:end -->

## `migrate`

<!-- generated:config-migrate:start (go test ./internal/config -run TestDocIsCurrent -update) -->
| Переменная | По умолчанию | Что |
|---|---|---|
| `DB_PRIMARY_DSN` / `_FILE` | — (обязательна) | DSN primary, роль сервиса. Миграции никогда не идут на реплику |
| `MIGRATE_LOCK_TIMEOUT` | `5s` | `lock_timeout`: DDL, который не дождался блокировки занятой таблицы, падает, а не держит очередь запросов сервиса за собой |
| `MIGRATE_STATEMENT_TIMEOUT` | `15m` | `statement_timeout` каждой команды |
| `MIGRATE_WAIT_TIMEOUT` | `15m` | сколько ждать, пока другой запуск миграций того же источника закончит |
<!-- generated:config-migrate:end -->

Как действуют таймауты миграций — [migrations.md](migrations.md), «Таймауты».

## Ключи access JWT и ротация

JWT_SIGNING_KEY_FILE — PKCS#8 PEM Ed25519, права 0600 или строже. JWT_KEY_ID, JWT_ISSUER
и JWT_AUDIENCE обязательны при старте, в том числе в dev. Issuer и audience задаются
оператором одинаково для user и всех проверяющих сервисов; значений по умолчанию нет.
Access JWT живёт 15 минут. Ключи загружаются при старте, изменения файлов требуют рестарта.

JWT_PUBLIC_KEYS_FILE — необязательный публичный JWKS только с Ed25519-ключами. Активный
ключ добавляется автоматически; совпавший kid должен иметь тот же публичный ключ.
Приватные поля JWK и неизвестные поля отвергаются. Приватные файлы ограничены 8 KiB,
JWKS — 1 MiB и 64 ключами, включая активный. GetSigningKeys никогда не возвращает приватный ключ.

Ротация: сначала добавить новый публичный ключ к старому, перезапустить все реплики с
прежним активным ключом и общим расширенным JWKS; выдержать минимум минуту для обновления
кэшей потребителей. Затем развернуть новый приватный ключ и kid, сохраняя оба публичных.
Старый публичный удалить только после истечения всех выданных им access JWT (15 минут
после переключения последней реплики) и запаса на развёртывание. Не смешивать ключи
пользователей и сервисов стенда.

## Проверка сервисных токенов

При AUTH_MODE=verify требуется SERVICE_TOKEN_ISSUER. В kubernetes также нужны
SERVICE_TOKEN_FILE (projected audience finassist-internal), KUBERNETES_TOKEN_FILE
(штатный audience API-сервера), CA и HTTPS JWKS URL. TLS не отключается и redirects
не принимаются. Ключи пользователя обновляются из локального снимка, кластерные —
заданием кэша go-identity. Внешняя загрузка ключей ограничена библиотекой пятью секундами.

В stage: ENV=stage, SERVICE_AUTH_MODE=stage, отдельный STAGE_SIGNING_KEY_FILE этого
сервиса и STAGE_PUBLIC_KEYS_FILE с публичными ключами всех разрешённых сервисов (kid —
имя вызывающего). JWT_ISSUER/JWT_AUDIENCE относятся к пользователю, SERVICE_TOKEN_ISSUER
— к сервисам; это разные границы доверия. Для смены stage-ключей нужен согласованный
перезапуск источников и проверяющих сервисов; ротация сервиса с двумя ключами одного
имени библиотекой пока не поддерживается.
