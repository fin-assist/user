# Конфигурация

Всё — переменные окружения (`internal/config`). Секрет можно передать файлом: `VAR_FILE=/путь` вместо `VAR` (systemd `LoadCredential`, смонтированный Secret Kubernetes); заданы оба — ошибка запуска. Ошибки конфигурации выводятся все сразу и не содержат значений секретов.

Шаблон не опирается на Kubernetes: адреса других сервисов, DSN и файл токена — обычные настройки, одинаковые для k3s и стенда под systemd.

## Как меняется

Таблицы ниже **генерируются** из списка `Vars` в `internal/config/vars.go` — там у каждой переменной имя, значение по умолчанию, признак секрета и описание; код берёт значения по умолчанию оттуда же. Новая переменная или новое значение по умолчанию:

1. строка в `Vars`, чтение в `load` / `loadMigrate`;
2. `go test ./internal/config -run TestUnitDocIsCurrent -update` — переписывает таблицы между маркерами.

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

<!-- generated:config-serve:start (go test ./internal/config -run TestUnitDocIsCurrent -update) -->
| Переменная | По умолчанию | Что |
|---|---|---|
| `PHONE_PROVIDER` | `disabled` | `disabled`, `fake` или `zvonok`; fake/zvonok только dev/stage; prod Zvonok ждёт FIN-81 |
| `ZVONOK_PUBLIC_KEY` / `_FILE` | — | ключ API V1; обязателен при PHONE_PROVIDER=zvonok, не выводится в лог |
| `ZVONOK_CAMPAIGN_ID` | — | ID отдельной кампании pincode_incoming, управляемой только этим сервисом |
| `ZVONOK_CALLBACK_ADDR` | `:9095` | отдельный HTTP-listener, только GET /callbacks/zvonok; ingress мимо bank-bff, без query в access-логах |
| `ZVONOK_TRUSTED_PROXY_CIDRS` | — | CIDR доверенных ingress-прокси через запятую; только от них читается X-Forwarded-For, по умолчанию только RemoteAddr |
| `ZVONOK_POLL_INTERVAL` | `30s` | минимальный интервал запасной проверки одной pending-заявки в Watch (1s–1m); общий API-бюджет 20 запросов/с на ключ в PostgreSQL |
| `SERVICE_NAME` | — (обязательна) | имя сервиса: логи, `application_name`, токен в dev; при `ENV=stage` — kid/sub ключа подписи стенда, совпадает с kid в общем JWKS (unit стенда задаёт `user`) |
| `ENV` | `prod` | `dev`, `stage` или `prod`. Reflection включён только при `dev` |
| `AUTH_MODE` | `verify` | `verify` — проверка JWT и сервисных токенов через go-identity `NewAssembly`: при `ENV=prod` — ServiceAccount Kubernetes, при `ENV=stage` — JWT из файлов ключей, при `ENV=dev` не запускается; `insecure-dev` — токены `dev:<имя>` без проверки, только при `ENV=dev` |
| `JWT_SIGNING_KEY_FILE` | — | PKCS#8 PEM Ed25519, приватный ключ access JWT; файл 0600, обязателен при старте |
| `JWT_PUBLIC_KEYS_FILE` | — | необязательный публичный JWKS Ed25519 для перекрытия ключей при ротации |
| `JWT_KEY_ID` | — | kid активного ключа access JWT, обязателен при старте |
| `JWT_ISSUER` | — | iss access JWT, обязателен; одинаков у всех потребителей (у них — `AUTH_USER_ISSUER`); этот же iss user проверяет у входящих access-токенов |
| `JWT_AUDIENCE` | — | aud access JWT, обязателен; одинаков у всех потребителей (у них — `AUTH_USER_AUDIENCE`); эту же aud user проверяет у входящих access-токенов |
| `AUTH_IDEMPOTENCY_KEY_FILE` | — | файл 0600 с 32 raw-байтами AES-256 для шифрования ответов Register/Refresh; обязателен, общий для реплик; сохранять ключ не менее 24 ч после последней записи |
| `PHONE_VERIFICATION_UNKNOWN_TIMEOUT` | `5m` | локальный срок неизвестного исхода (Go duration, 1s..5m); абсолютный deadline фиксируется один раз при приёме; provider TTL 5m не меняется |
| `PHONE_RESULT_KEY_FILE` | — | файл 0600 с 32 raw-байтами AES-256 для результата подтверждения; обязателен, общий для реплик, отличается от AUTH_IDEMPOTENCY_KEY_FILE; хранить до истечения всех 15-минутных результатов |
| `PHONE_RESULT_KEY_ID` | `primary` | ID ключа результата: 1–64 символа A-Z/a-z/0-9/._-; сохраняется вместе с ciphertext |
| `PHONE_WATCH_MAX_STREAMS` | `1024` | предел одновременно ожидающих Watch на экземпляр (1–100000); отдельный от 60 попыток/мин на IP; насыщение видно в метриках |
| `PHONE_WATCH_RESYNC_INTERVAL` | `5s` | повторное чтение primary открытым Watch в дополнение к NOTIFY; положительный интервал не больше 30 с |
| `AUTH_DB_MAX_CONNS` | `8` | отдельный primary pool транзакций авторизации; лимиты используют обычный pool, чтобы ошибки токена не откатывали учёт попыток |
| `REFRESH_TOKEN_TTL` | `1440h` | скользящий срок refresh-токена; по умолчанию 60 суток, продлевается каждой ротацией |
| `AUTH_KUBERNETES_ISSUER` | `https://kubernetes.default.svc.cluster.local` | issuer токенов ServiceAccount при `ENV=prod`; сверить с настройкой кластера (было `SERVICE_TOKEN_ISSUER`) |
| `AUTH_KUBERNETES_JWKS_URL` | `https://kubernetes.default.svc/openid/v1/jwks` | HTTPS-адрес ключей ServiceAccount при `ENV=prod` (было `KUBERNETES_JWKS_URL`) |
| `AUTH_KUBERNETES_CA_FILE` | `/var/run/secrets/kubernetes.io/serviceaccount/ca.crt` | CA Kubernetes API, TLS-проверка обязательна (было `KUBERNETES_CA_FILE`) |
| `AUTH_KUBERNETES_API_TOKEN_FILE` | `/var/run/secrets/kubernetes.io/serviceaccount/token` | API-server-audience токен пода для чтения JWKS; отдельный от finassist-internal (было `KUBERNETES_TOKEN_FILE`) |
| `AUTH_KUBERNETES_NAMESPACE` | `finassist` | доверенный namespace ServiceAccount вызывающих при `ENV=prod` |
| `AUTH_STAGE_ISSUER` | — | общий issuer сервисных JWT стенда; обязателен при `ENV=stage`, запрещён при `ENV=prod` (было `SERVICE_TOKEN_ISSUER`) |
| `AUTH_STAGE_SIGNING_KEY_FILE` | — | приватный PKCS#8 Ed25519 ключ этого сервиса для исходящих JWT стенда; обязателен при `ENV=stage` (было `STAGE_SIGNING_KEY_FILE`) |
| `AUTH_STAGE_PUBLIC_KEYS_FILE` | — | публичный JWKS сервисов стенда, kid — имя сервиса, отдельный от ключей пользователей; обязателен при `ENV=stage` (было `STAGE_PUBLIC_KEYS_FILE`) |
| `SERVICE_TOKEN_FILE` | — | projected-токен ServiceAccount этого сервиса с audience finassist-internal для исходящих вызовов; обязателен при `ENV=prod`, отличается от `AUTH_KUBERNETES_API_TOKEN_FILE` |
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

<!-- generated:config-migrate:start (go test ./internal/config -run TestUnitDocIsCurrent -update) -->
| Переменная | По умолчанию | Что |
|---|---|---|
| `DB_PRIMARY_DSN` / `_FILE` | — (обязательна) | DSN primary, роль сервиса. Миграции никогда не идут на реплику |
| `MIGRATE_LOCK_TIMEOUT` | `5s` | `lock_timeout`: DDL, который не дождался блокировки занятой таблицы, падает, а не держит очередь запросов сервиса за собой |
| `MIGRATE_STATEMENT_TIMEOUT` | `15m` | `statement_timeout` каждой команды |
| `MIGRATE_WAIT_TIMEOUT` | `15m` | сколько ждать, пока другой запуск миграций того же источника закончит |
<!-- generated:config-migrate:end -->

Как действуют таймауты миграций — [migrations.md](migrations.md), «Таймауты».

## Ключи access JWT и ротация

Права файлов секретов (FIN-349, `internal/secretfile`): JWT_SIGNING_KEY_FILE,
AUTH_IDEMPOTENCY_KEY_FILE и PHONE_RESULT_KEY_FILE — обычный файл без доступа группе и
остальным (0600 или строже). Исключение — credential systemd прямо в каталоге
`$CREDENTIALS_DIRECTORY` (без симлинков и вложенных путей): допустимы 0400 и 0440. systemd
создаёт credential 0400 root и даёт DynamicUser чтение через ACL `user:<uid>:r--`; на tmpfs с
ACL (ядро 6.3 и новее) маска ACL видна в битах группы, и `stat` показывает 0440. На стенде
проверяется так: `systemd-run -p DynamicUser=yes -p LoadCredential=k:/etc/finassist/user/auth-idempotency.key --wait -P stat -c '%a %U' '%d/k'`.
Пример unit стенда и его проверка — `deploy/systemd/user.service`,
`internal/config/stand_unit_test.go`: секреты только `*_FILE=%d/<credential>`, `Environment=`
в простой форме `NAME=value`, env-файл не задаёт то, что задаёт unit.

JWT_SIGNING_KEY_FILE — PKCS#8 PEM Ed25519, права 0600 или строже (см. выше). JWT_KEY_ID, JWT_ISSUER
и JWT_AUDIENCE обязательны при старте, в том числе в dev. Issuer и audience задаются
оператором одинаково для user и всех проверяющих сервисов; значений по умолчанию нет.
Access JWT живёт 15 минут. Ключи загружаются при старте, изменения файлов требуют рестарта.

JWT_PUBLIC_KEYS_FILE — необязательный публичный JWKS только с Ed25519-ключами. Активный
ключ добавляется автоматически; совпавший kid должен иметь тот же публичный ключ.
Приватные поля JWK и неизвестные поля отвергаются. Приватные файлы ограничены 8 KiB,
JWKS — 1 MiB и 64 ключами, включая активный. GetSigningKeys никогда не возвращает приватный ключ.

Ротация: сначала добавить новый публичный ключ к старому, перезапустить все реплики с
прежним активным ключом и общим расширенным JWKS. Точка отсчёта T — момент, когда последняя
реплика со старым JWKS перестала принимать вызовы: её SIGTERM плюс SHUTDOWN_DELAY (после
NOT_SERVING сервер ещё принимает вызовы, в том числе GetSigningKeys, и отдаёт старый набор;
завершение rollout в Kubernetes может наступить раньше). Подписывать новым ключом можно не
раньше T плюс 10 минут (максимальный возраст снимка JWKS у потребителя, grpc-api.md 7.1)
плюс до 5 секунд на загрузку набора: возраст снимка отсчитывается от завершения загрузки.
Затем развернуть новый приватный ключ и kid, сохраняя оба публичных.

Почему не минута: минуты хватает только ограничителю обновления по неизвестному kid
(go-identity), то есть только пока user доступен потребителю. Если user недоступен, а
снимок потребителя ещё свежий и не содержит нового kid, такой токен отклоняется как
INVALID_TOKEN, и клиент с валидным новым токеном выходит из аккаунта. Минута — абсолютный
минимум для случая доступного user, а не порядок ротации. Это верно для любой версии
go-identity. В закреплённой v0.2.0 (FIN-293) отказ источника после устаревания снимка
даёт голый UNAVAILABLE, а не INVALID_TOKEN: сбой user больше не разлогинивает клиентов, но
вызовы с любым токеном отклоняются, пока свежего снимка нет. 10 минут гарантируют, что сама
ротация не добавляет отказов.

Старый публичный удалить только после истечения всех выданных им access JWT (15 минут
после переключения последней реплики) и запаса на развёртывание. Не смешивать ключи
пользователей и сервисов стенда.

## Проверка сервисных токенов

Идентичность собирает go-identity `NewAssembly` (v0.2.0, FIN-325); режим выбирает `ENV`,
`AUTH_MODE=verify` при `ENV=dev` не запускается (в dev — только `insecure-dev`).

В prod (`ENV=prod`) нужны `SERVICE_TOKEN_FILE` (projected audience finassist-internal) и
отдельный файл `AUTH_KUBERNETES_API_TOKEN_FILE` (штатный audience API-сервера), CA
`AUTH_KUBERNETES_CA_FILE` и HTTPS `AUTH_KUBERNETES_JWKS_URL`; issuer и namespace —
`AUTH_KUBERNETES_ISSUER`, `AUTH_KUBERNETES_NAMESPACE` (значения по умолчанию как в
assistant). TLS не отключается и redirects не принимаются. `AUTH_STAGE_*` в prod —
ошибка запуска. Ключи пользователя обновляются из локального снимка, кластерные —
заданием `identity-keys`. Внешняя загрузка ключей ограничена библиотекой пятью секундами.

В stage (`ENV=stage`): общий issuer `AUTH_STAGE_ISSUER`, отдельный
`AUTH_STAGE_SIGNING_KEY_FILE` этого сервиса и `AUTH_STAGE_PUBLIC_KEYS_FILE` с публичными
ключами всех разрешённых сервисов (kid — имя вызывающего). `JWT_ISSUER`/`JWT_AUDIENCE`
относятся к пользователю, `AUTH_STAGE_ISSUER` и `AUTH_KUBERNETES_ISSUER` — к сервисам;
это разные границы доверия. Для смены stage-ключей нужен согласованный перезапуск
источников и проверяющих сервисов; ротация сервиса с двумя ключами одного имени
библиотекой пока не поддерживается.

Issuer и audience входящих access-токенов — `JWT_ISSUER`/`JWT_AUDIENCE`: user сам
подписывает эти токены, отдельные `AUTH_USER_ISSUER`/`AUTH_USER_AUDIENCE` (как у
потребителей) здесь не заводятся, чтобы подпись и проверка не могли разойтись.

### Переход на AUTH_* (FIN-325)

Переименование одновременное: образ с FIN-325 и окружение стенда выкатываются вместе
(`deploy/systemd/user.service` уже на новых именах). Старое имя с непустым значением —
ошибка запуска с именем замены, а не молчаливое значение по умолчанию.

| Было | Стало |
|---|---|
| `SERVICE_AUTH_MODE` | удалена: `ENV=prod` — Kubernetes, `ENV=stage` — ключи из файлов |
| `SERVICE_TOKEN_ISSUER` | `AUTH_KUBERNETES_ISSUER` (prod) или `AUTH_STAGE_ISSUER` (stage) |
| `STAGE_SIGNING_KEY_FILE` | `AUTH_STAGE_SIGNING_KEY_FILE` |
| `STAGE_PUBLIC_KEYS_FILE` | `AUTH_STAGE_PUBLIC_KEYS_FILE` |
| `KUBERNETES_JWKS_URL` | `AUTH_KUBERNETES_JWKS_URL` |
| `KUBERNETES_CA_FILE` | `AUTH_KUBERNETES_CA_FILE` |
| `KUBERNETES_TOKEN_FILE` | `AUTH_KUBERNETES_API_TOKEN_FILE` |
| — | `AUTH_KUBERNETES_NAMESPACE` (по умолчанию `finassist`, как было зашито в коде) |
