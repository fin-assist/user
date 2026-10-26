# user

- [docs/releasing.md](docs/releasing.md) — версии сервиса, CHANGELOG и повторяемый GitHub Release ([FIN-239](https://finassist.youtrack.cloud/issue/FIN-239)); публикация образов и deployment выполняются отдельно.

Сервис `user` «Понятных финансов»: вход по телефону с подтверждением обратным звонком, регистрация, токены (access JWT и refresh с ротацией), профиль, согласия и их документы, удаление аккаунта; ключи подписи access-токенов для остальных сервисов (`GetSigningKeys`). Инициатор удаления данных (`PurgeUserData`) и синхронизации часового пояса (`SetUserTimezone`) в другие сервисы через outbox. Контракт — пакет `finassist.user.v1` в [`fin-assist/contracts`](https://github.com/fin-assist/contracts) (`docs/api.md` 1–2, `docs/grpc-api.md` 7.1), модуль `github.com/fin-assist/contracts/gen/go`.

Создан из шаблона [`fin-assist/go-service-template`](https://github.com/fin-assist/go-service-template) ([FIN-117](https://finassist.youtrack.cloud/issue/FIN-117)) в состоянии `main` 740a202 ([FIN-191](https://finassist.youtrack.cloud/issue/FIN-191)). Изменения шаблона после этого коммита в сервис не приходят сами — переносятся отдельной задачей.

- [docs/server.md](docs/server.md) — gRPC-сервер и интерцепторы, идентичность, таблица доступа, клиенты, транзакции, логи и метрики.
- [docs/config.md](docs/config.md) — переменные окружения.
- [docs/migrations.md](docs/migrations.md) — миграции и запросы на pgx ([FIN-115](https://finassist.youtrack.cloud/issue/FIN-115)).
- [docs/schema.md](docs/schema.md) — схема БД `user`: сущности, инварианты, ожидаемые запросы и индексы ([FIN-112](https://finassist.youtrack.cloud/issue/FIN-112)).
- [docs/deletion.md](docs/deletion.md) — атомарное удаление аккаунта и доставка PurgeUserData.
- [docs/verification.md](docs/verification.md) — создание и восстановление заявки, общее подтверждение, Watch и очистка.
- [docs/zvonok.md](docs/zvonok.md) — Zvonok V1, постбэк, fallback, выбор номера и границы активации.
- [docs/fake-phone.md](docs/fake-phone.md) — вход без звонка в dev/stage, тестовые телефоны и защита prod.
- [AGENTS.md](AGENTS.md) — карта и правила для агентов, Code Review Rules.

FIN-255 подключает `go-grpc v0.2.0` (server/client/reqctx), `go-migrate v0.1.1` и `go-worker v0.1.0` вместо локальных копий. Identity, AccessPolicy, tombstones, прикладные jobs и SQL остаются в `internal/app` и адаптерах сервиса. Перезапуск jobs явно ограничен прежними 5 секундами; расписания используют переданные logger и clock. Тестовые базы и up → down → up обслуживает публичный `go-migrate/migratetest`, выбор PostgreSQL 18 — `internal/testdb`.

Транзакции выполняет `go-postgres v0.2.0` ([FIN-240](https://finassist.youtrack.cloud/issue/FIN-240)). Lifecycle пулов передан `go-postgres.OpenPools`; сервис задаёт параметры и сохраняет маршрутизацию primary/replica; каждый участник auth-транзакции явно привязан к выделенному auth-пулу. Нарушения ограничений переводятся в доменные ошибки адаптерами конкретной операции, включая фактический COMMIT; usecase/transport не разбирают PostgreSQL-ошибки. См. [docs/migrations.md](docs/migrations.md).

## Автоматизация обновлений зависимостей

[Политика Renovate](docs/renovate-policy.md) (FIN-211): одна актуальная release/main, автомерж полностью ботских PR после обязательного CI, один повтор красного CI и закрытие с явным повтором через Dependency Dashboard. AndroidX/Compose остаются на ручной проверке. Исходные группировки, ограничения версий и расписания — `renovate-base.json`; generated target и quarantine — ветка `renovate-policy`. Активация требует отдельного токена и строгой защиты веток; по умолчанию выключена.

## Реализованная часть FIN-219

Первый этап: проверка сервисных и пользовательских токенов через `identity.NewAssembly` go-identity v0.2.0 (FIN-325: режим по `ENV`, переменные `AUTH_*`),
Ed25519-подпись access JWT на 15 минут, GetSigningKeys и GetConsentDocument; tombstone
через go-tombstone v0.1.0 и его миграции. Для документа assistant требуется валидный
пользовательский токен и отсутствие tombstone. Документы читаются с primary.

Реализованы GetProfile, UpdateProfile, GrantAssistantConsent, RevokeAssistantConsent
и GetAssistantConsentStatus. Смена пояса атомарно пишет событие go-outbox v0.1.0;
доставщик независимо отправляет SetUserTimezone в bank-statement-data и assistant.
Маршруты настраиваются без изменения производителя события, для доставки выделен
отдельный primary-пул. Детали и ограничения — [docs/profile.md](docs/profile.md).

StartPhoneVerification, Register и RefreshTokens работают через go-idempotency v0.4.1
с владельцем результата (FIN-226): зашифрованный
повтор, одноразовая регистрация, скользящие refresh-сессии и отзыв цепочки при reuse.
Logout идемпотентен без ключа, требует access-токен и завершает только сессию
владельца токена (FIN-285).
Start связывает резерв и ответы с заявкой через Resource (FIN-230), включая отказы
по занятости номера, чтобы удалить все связанные ключи вместе с данными аккаунта.
Регистрация атомарно сохраняет согласие на ПДн, первую сессию и начальный timezone.
Две подтверждённые заявки одного нового номера получают один аккаунт и отдельные
сессии; настройки и согласия уже созданного аккаунта сохраняются. Детали —
[docs/auth.md](docs/auth.md).

DeleteAccount синхронно удаляет локальные данные, отзывает сессии и сохраняет tombstone
вместе с заданием PurgeUserData для bank-statement-data, analytics и assistant. Повтор
успешен без нового задания. Детали — [docs/deletion.md](docs/deletion.md).

Реализованы все 13 RPC. WatchPhoneVerification читает primary, слушает общий
LISTEN/NOTIFY, отдаёт результат повторно 15 минут и применяет IP-лимит до секрета.
Общее подтверждение атомарно сохраняет результат и сессию существующего пользователя;
новый получает токен регистрации. Детали — [docs/verification.md](docs/verification.md).

StartPhoneVerification резервирует попытку до внешнего вызова и завершает её отдельной
транзакцией: повтор отдаёт прежний секрет без списания, неопределённый исход
восстанавливается своим ключом, явный отказ возвращает исходный резерв лимита.
До запуска новой сборки нужны миграции 000002_phone_start и 000003_phone_start_key_index;
частичный UNIQUE создаётся CONCURRENTLY отдельным шагом.
FIN-139/216 добавляют Zvonok V1 и случайный выбор/хранение пула (миграция 000004).
По умолчанию создание выключено, без резервирования попытки. Для dev/stage
конкретная сборка включается через PHONE_PROVIDER=zvonok или PHONE_PROVIDER=fake.
Фейк принимает только служебный диапазон +7000000xxxx, сразу подтверждает через
общий Confirm и запрещён в prod. Локально: `PHONE_PROVIDER=fake make up` или
`PHONE_PROVIDER=fake make run`; подробности — [docs/fake-phone.md](docs/fake-phone.md).
HTTP-постбэк требует IP провайдера и проверки call_by_id, обхода звонка по параметрам
запроса нет. API-тесты проходят полный вход через HTTPS-сервер провайдера и gRPC user.

Подготовлен общий механизм лимитов аутентификации (`internal/ratelimit`): фиксированные
UTC-окна в primary PostgreSQL, атомарная проверка нескольких окон, настройки и метрики.
При старте публикуются настройки семи политик; фоновая очистка удаляет старые окна.
Watch, Register и RefreshTokens считают попытки до проверки токена, включая отказы 401/403/422;
повтор сохранённого ответа и конфликт ключа лимит не расходуют. Start применяет
четыре окна до занятости номера, а возвраты резерва видны отдельным счётчиком.
Правила интеграции и наблюдаемость — [docs/limits.md](docs/limits.md).

Документы согласий публикуются миграциями данных в consent_document: новая строка с
версией, утверждённым названием и URL; прежний is_current снимается перед включением
нового в одной транзакции. В этом этапе нет выдуманных юридических текстов или ссылок;
неопубликованный тип возвращает NOT_FOUND. Перед запуском регистрации нужны реальные
согласованные документы.

## Раскладка

```
cmd/user/                          main: serve and migrate commands, listeners, shutdown
internal/app/                      assembly: identity, pools, server, wire() (handlers, clients, jobs), access table, migration sources
internal/app/apitest/              the whole service in-process for API tests
internal/signing/                  access JWT signing and public JWKS loaded from trusted files
internal/consent/                  current consent-document usecase
internal/verification/             phone creation/recovery, common confirmation, Watch and cleanup
internal/adapter/verificationcrypto/ independent AES-GCM result encryption
internal/profile/                  profile settings and assistant-consent usecases
internal/deletion/                 atomic account deletion and remote purge scheduling
internal/adapter/outbox/            timezone and deletion event encoding
internal/ratelimit/                authentication limit policies and commit-aware metrics
internal/domain/                   entities and invariants, no infrastructure imports
internal/<feature>/                usecases; interfaces of repositories and clients they need
internal/adapter/grpc/<feature>/   gRPC handlers: messages <-> usecases
internal/adapter/postgres/         pools, transaction manager, repositories on pgx
internal/config/                   configuration from the environment
internal/testdb/                   PostgreSQL server selection for the public migration test harness
internal/logx/                     slog with values from the context
internal/ops/                      /metrics, /healthz, /readyz, readiness
migrations/                        SQL migrations, embedded into the binary
deploy/systemd/                    example unit for the stand
```

Общая инфраструктура импортируется напрямую: `go-grpc/server`, `go-grpc/client`, `go-grpc/reqctx`, `go-migrate` и `go-worker`. Прикладные фоновые задания и настройки клиентов собирает `internal/app`.

Раскладка — по руководству go.dev «Organizing a Go module» (серверный проект), слои и интерфейсы на стороне потребителя — по go-clean-arch v4: сценарий в `internal/<feature>` объявляет интерфейсы нужных ему репозиториев и клиентов, адаптеры их реализуют, связывает всё `wire()`.

## Запуск

```bash
# PostgreSQL 18, migrations and the service in Docker; creates .local/jwt-signing.pem (0600)
make up
# the same service on the host against the compose PostgreSQL
make run
```

- **Образ** — `Dockerfile`: статический бинарь на distroless, пользователь `nonroot`. Один бинарь: `serve` в Deployment, `migrate up` в Job. Публикация в GHCR — FIN-148.
- **Стенд** (FIN-149) — тот же бинарь под systemd: пример unit-файла с секретами через `LoadCredential` — [deploy/systemd/user.service](deploy/systemd/user.service). Скрипт деплоя запускает `migrate up` перед перезапуском unit.
- **k3s** — манифесты в `fin-assist/infra` (FIN-147): ServiceAccount, projected token, headless Service, пробы `grpc`.

При make up контейнер user работает с UID/GID локального пользователя, чтобы читать
его ключ 0600; Dockerfile по-прежнему использует nonroot. Для прямого docker compose up
сначала выполните make dev-key и передайте LOCAL_UID/LOCAL_GID. Ключ создаёт Go-команда cmd/dev-key; OpenSSL не требуется.
Каталог .local исключён из git и Docker build context. make run использует тот же ключ.

Настройки stage/prod и ротация ключей — [docs/config.md](docs/config.md).
PHONE_RESULT_KEY_FILE — новый обязательный отдельный AES-256 ключ (32 raw-байта, 0600),
общий для реплик и отличный от ключа идемпотентности. make dev-key создаёт его локально
без перезаписи; пример systemd использует отдельный credential. При плановой замене
нужно дождаться окончания всех 15-минутных результатов, сохранив прежние ключ/ID до
этого момента. Ключи и ID одинаковы на всех репликах.
Миграции go-tombstone и go-outbox применяются до запуска новой сборки.
Для каждого получателя OUTBOX_TIMEZONE_TARGETS и OUTBOX_DELETION_TARGETS обязателен
PEER_<NAME>_ADDR, в том числе новый обязательный PEER_ANALYTICS_ADDR. Удаление всегда
включает bank-statement-data, analytics и assistant. Пул outbox — не меньше числа
уникальных соседей + 3 (по умолчанию минимум 6, настройка 8). Примеры compose/make run
задают адреса соседей, но сами соседние сервисы не поднимают: до их готовности события
остаются в outbox и повторяются. Конкретные адреса стенда задаются в его окружении.

## Библиотеки и обновления

Общий код сервисов — отдельные Go-библиотеки из шаблона [`fin-assist/go-library-template`](https://github.com/fin-assist/go-library-template) ([FIN-118](https://finassist.youtrack.cloud/issue/FIN-118)): [`go-identity`](https://github.com/fin-assist/go-identity) (FIN-119), [`go-idempotency`](https://github.com/fin-assist/go-idempotency) (FIN-78), [`go-outbox`](https://github.com/fin-assist/go-outbox) (FIN-109), [`go-tombstone`](https://github.com/fin-assist/go-tombstone) (FIN-110).

- **Подключение** — по разделу «Внедрение» README библиотеки: `go get github.com/fin-assist/go-<имя>@vX.Y.Z`, источник миграций в `MigrationSources()` до источника сервиса, сборка в `wire()` — библиотека получает от сервиса транзакцию (`db.Tx`), логгер, реестр метрик, часы и настройки.
- **Версии** — semver-теги библиотеки, `go.mod` сервиса закрепляет версию; до первого прода — `v0`, несовместимое изменение — новый MINOR с записью «Несовместимо» в CHANGELOG библиотеки. Правила — `docs/releasing.md` в `fin-assist/go-library-template`.
- **Обновления приносит Renovate** (`renovate.json`): модули `github.com/fin-assist/*` (библиотеки и `contracts/gen/go`) — отдельный PR сразу после тега, остальные зависимости — один PR в неделю, версия Go — вручную. PR Renovate проходит CI сервиса и обрабатывается по [политике FIN-211](docs/renovate-policy.md). При несовместимом обновлении человеческий коммит в открытом PR переводит его на ручное сопровождение; если ботский PR уже закрыт после повторного падения, исправления сервиса идут отдельным PR с задачей YouTrack. Порядок миграции — по CHANGELOG библиотеки.
- **Проверить библиотеку до выпуска** — локально `go mod edit -replace github.com/fin-assist/go-<имя>=../go-<имя>`; `replace` в PR сервиса не попадает.

## Проверки

CI (`.github/workflows/ci.yml`):

| Job | Что |
|---|---|
| `go` | `go mod tidy` без изменений, `go vet`, `go test -race` с PostgreSQL 18 в Docker (testcontainers), включая «вверх → вниз → вверх» для миграций; тесты скриптов; три независимых порога 80% statements (unit/integration/API), дополнительный combined; отчёты и артефакты в Actions |
| `lint` | golangci-lint (`.golangci.yml`), в том числе `forbidigo` против обхода `finerr` и `depguard` против фреймворков, ORM и DI-контейнеров |
| `image` | сборка образа и запуск бинаря в нём |
| `migrations` (PR) | существующие миграции не изменены, squawk по новым |

Локально:

```bash
make test   # Docker for testcontainers, or TEST_DATABASE_URL=postgres://... to use your PostgreSQL
make lint
```

### Покрытие statements (FIN-238)

Имена проверяет `make test-names` до прогонов: каждый тест — `TestUnit…`/`TestIntegration…`/`TestAPI…`, каждая fuzz-цель — `FuzzUnit…`/`FuzzIntegration…`/`FuzzAPI…` (её seed-корпус выполняется в своём наборе, FIN-290). `Example…` в сервисе отклоняется: публичного Go API у сервиса нет, а префикс набора в имени примера невозможен, и он выполнялся бы вне всех трёх наборов.
Нужны Go, `jq` и PostgreSQL 18 (Docker для testcontainers либо `TEST_DATABASE_URL`).
`make cover-unit`, `make cover-integration`, `make cover-api` запускают отдельные наборы
с `-race -count=1 -timeout=8m`, `-coverpkg=./...` и профилями `unit.out`, `integration.out`,
`api.out`. `make cover` выполняет все три проверки и дополнительный `make cover-combined`.

Классификация задаётся именами Go: `TestUnit*` — изолированные функции и сценарии на
фейках без БД и сети; `TestIntegration*` — настоящая PostgreSQL, миграции, транспортные
клиенты и фоновые задания; `TestAPI*` — полная сборка сервиса и вызовы публичного gRPC.
Тест прямого handler на фейках остаётся unit. Helpers и metrics/jobs без публичного
RPC не становятся API только из-за расположения в `internal/app/apitest`.

Каждый набор отдельно должен покрывать **80% statements всего production-кода**.
Знаменатель одинаковый: все пакеты `./...`, включая generated production-код и `.go`
тестовые helpers. Исключений для достижения порога нет. Суммарное покрытие —
дополнительная информация и не компенсирует провал отдельного набора.
Это заменяет прежние 80% изменённых строк; старый процент с новым напрямую не сравнивается.

Проверяет закреплённый `github.com/vladopajic/go-test-coverage/v2@v2.20.1`.
Принимается штатное округление до десятых: **79,95% считается 80,0%** (решение владельца
в FIN-238). Инструмент сам объединяет comma-separated профили, без собственного merge
или подсчёта. Процент не доказывает полноту бизнес-сценариев.

Пустой набор запрещён: JSON-лог `go test -json` должен содержать успешно выполненный
тест и не должен содержать пропущенных или упавших Test-событий; профиль — блок с ненулевым числом statements. Отсутствующий, пустой или header-only
профиль отклоняется до запуска анализатора. `CI=true` делает недоступную PostgreSQL
ошибкой, а не успешным пропуском. Ошибки тестов/порогов завершают обязательный `go` job
красным, при этом остальные наборы продолжают сбор отчётов. Права записи в PR не нужны,
в том числе для fork: отчёты видны в сводке Actions и артефакте `coverage`
(профили, JSON-логи, штатные текстовые отчёты и breakdown).

FIN-255 добавляет integration-сценарии записи настоящего log-файла с общим `go-grpc/reqctx` и файлового развёртывания/ротации signing keys. Они проверяют сохранение identity в логах, отсутствие её переноса в фоновые записи, совместимость старых/новых токенов и отказ небезопасного key deployment. Исправление FIN-262 убирает ошибки up/down/up у трёхзначных миграций; отдельный дефицит integration-покрытия после удаления общих копий закрывается этими сценариями. Проверки используют опубликованный `go-migrate v0.1.1`: поддержка трёхзначных миграций проверяется штатным `make test` и тремя независимыми CI-профилями, без временного modfile или replace.

Актуальные значения каждого уровня находятся в сводке Actions и артефакте `coverage` текущего коммита.
Локальная версия Go может считать statements иначе, чем версия из `go.mod` в CI: приёмка требует
успешных трёх gates в CI, а высокий combined не заменяет ни один из них.

Модули `github.com/fin-assist/*` берутся из git, минуя прокси (`GOPRIVATE`, задан в
`Makefile` и CI). Для приватных модулей CI использует `GO_MODULES_TOKEN`, docker build —
секрет `netrc`. Значения секретов не хранятся в репозитории.

## Zvonok (FIN-139 / FIN-216)

`PHONE_PROVIDER=zvonok` подключает API V1, равномерный выбор и сохранение номера,
проверяемый по API постбэк на отдельном listener и fallback открытого Watch.
По умолчанию провайдер выключен; prod блокируется до FIN-81. Нужны отдельная кампания
`pincode_incoming`, общий ключ у реплик и ingress с маскированием query.
Подробности и ограничения — [docs/zvonok.md](docs/zvonok.md).
Собственный контроль баланса отложен в [FIN-233](https://finassist.youtrack.cloud/issue/FIN-233):
пока достаточно уведомлений Zvonok о низком остатке. Он не блокирует FIN-139/216.
Перед активацией остаётся проверка на реальной кампании.
