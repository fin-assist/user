# История изменений

## [Unreleased]

- FIN-371: в конфигурации Squawk отключено правило `prefer-bigint-over-smallint` вместе с ранее отключённым предпочтением bigint над integer. Разрядность выбирается по предметному диапазону и росту; SQL-миграции, runtime и API не меняются.

- FIN-148: CI сохраняет stage-бинарь из проверенного образа с SHA, manifest и контрольной суммой.

- FIN-225: подключён опубликованный go-idempotency v0.6.0: cached phone RATE_LIMITED сохраняет typed Status/Any и false, ожидание пересчитывается от абсолютного срока без продления ключа. Старые отсутствие advice, неизвестные COMMIT и фазовое разрешение usecase сохраняются; TTL и SQL не меняются. По графу модулей go-migrate повышен с v0.1.1 до v0.2.0, новые зависимости и локальные replace не добавлены. Документация и реальный API regression описывают использование нового codec.

- FIN-225: регрессии для отказа лимитера с неуспешным COMMIT, сохранения false при повторе cached Register400, удалённых аккаунтов и безопасного преобразования обёрнутых ошибок. Тесты явно различают отсутствие advice у общего admission guard и запрет повтора в usecase.

- FIN-225: contracts/gen/go v0.5.0; RequestRetryInfo формируется в явной фазе сценария: начальный отказ лимитера до эффекта разрешает повтор после ожидания, неизменённые неверные данные/секрет/согласие запрещают. Неопределённые COMMIT, storage, provider и старые cached исходы не получают нового разрешения. Адаптер сохраняет решение до кодирования результата; каталог UI остаётся в BFF. Ключи, TTL, протокол провайдера и схема БД не меняются. Специальный cached phone RATE_LIMITED не получает положительного advice по одному факту причины; детали — docs/errors.md.

### Несовместимо

- FIN-325: идентичность собирает `identity.NewAssembly` из go-identity v0.2.0; режим сервисной идентичности выбирает `ENV` (prod — ServiceAccount Kubernetes, stage — JWT из файлов ключей), `AUTH_MODE=verify` при `ENV=dev` не запускается. Переменные переименованы в стандартный набор `AUTH_*`: `SERVICE_TOKEN_ISSUER` → `AUTH_KUBERNETES_ISSUER`/`AUTH_STAGE_ISSUER`, `KUBERNETES_JWKS_URL`/`KUBERNETES_CA_FILE`/`KUBERNETES_TOKEN_FILE` → `AUTH_KUBERNETES_JWKS_URL`/`AUTH_KUBERNETES_CA_FILE`/`AUTH_KUBERNETES_API_TOKEN_FILE`, `STAGE_SIGNING_KEY_FILE`/`STAGE_PUBLIC_KEYS_FILE` → `AUTH_STAGE_*`; добавлена `AUTH_KUBERNETES_NAMESPACE`, `SERVICE_AUTH_MODE` удалена. Старое имя с непустым значением — ошибка запуска с именем замены; окружение стенда меняется вместе с выкладкой (`deploy/systemd/user.service`, таблица перехода — `docs/config.md`). Issuer/audience входящих access-токенов — по-прежнему `JWT_ISSUER`/`JWT_AUDIENCE`. Новая метрика библиотеки `finassist_identity_jwks_skipped_keys`.

### Исправлено

- FIN-346: `serve` занимает адреса gRPC, HTTP и постбэка до начала обслуживания и закрывает уже занятые при ошибке bind; фактические адреса (в том числе для порта `0`) попадают в журнал `serving`. API- и интеграционные тесты запуска больше не занимают и не освобождают порт заранее, а слушают `127.0.0.1:0` и берут фактический адрес после bind, поэтому не падают с `bind: address already in use`.
- FIN-349: пример unit стенда `deploy/systemd/user.service` передаёт ключ идемпотентности через `LoadCredential=auth-idempotency` и `AUTH_IDEMPOTENCY_KEY_FILE=%d/auth-idempotency` (раньше переменной не было, а root-файл 0600 недоступен `DynamicUser`, и `serve` на стенде не запускался), задаёт `SERVICE_NAME=user` (kid/sub ключа подписи стенда) и перечисляет в шапке все файлы ключей с режимами; env-файл не должен задавать то, что задаёт unit. Ключи AES-256 и ключ подписи access JWT читаются через `internal/secretfile`: credential systemd прямо в `$CREDENTIALS_DIRECTORY` принимается с правами 0400 и 0440 (на tmpfs с ACL маска ACL видна в битах группы), вне этого каталога правило прежнее — без доступа группе и остальным. Тесты `TestUnitStandUnit…` сверяют unit с config: секреты только `*_FILE=%d/<credential>` из `/etc/finassist/user/`, `Environment=` только в простой форме и только переменные конфигурации, без `SetCredential*`/`ImportCredential`/`LoadCredentialEncrypted`, один env-файл; конфигурация загружается с credentials 0400 и 0440, ключ стенда подписывает с kid `user`; шаблон Zvonok проверяется в раскомментированном виде. Integration- и API-тесты собирают и запускают сервис с credentials в `$CREDENTIALS_DIRECTORY`: с 0440 `serve` в stage отвечает на вызов, подписанный ключом стенда bank-bff; с 0444 и с 0440 вне каталога запуск отказывает до открытия listener-ов. Журнал `serving` пишет адрес постбэка (`callback`, пусто без Zvonok; оба случая проверены API-тестами). Версии go-postgres (v0.3.0) и go-identity (v0.2.0, FIN-293) в документации приведены к `go.mod`.
- FIN-347: при `AUTH_MODE=verify` и `ENV=stage` отсутствие `AUTH_STAGE_ISSUER`, `AUTH_STAGE_SIGNING_KEY_FILE` или `AUTH_STAGE_PUBLIC_KEYS_FILE` отклоняет `config.Load` с именем каждой недостающей переменной, как в остальных сервисах; раньше это ловила только сборка identity. Версии библиотек в `AGENTS.md`, README и `docs/server.md` приведены к `go.mod` (go-grpc v0.2.0).
- FIN-289: во время `SHUTDOWN_DELAY` HTTP `/readyz` отвечает 503 вместе с gRPC health `NOT_SERVING`; раньше `/readyz` до конца паузы отвечал 200. `DB_MAX_CONNS` вне диапазона int32 — ошибка конфигурации, а не молча усечённое значение; глобальное исключение gosec G115 снято. Перенос go-service-template#25; проверка `insecure-dev` вне `ENV=dev` в сборке уже была.
- FIN-313: API-тесты лимитов больше не зависят от границы фиксированного окна: `apitest` закрепляет часы лимитера (`app.WithLimitClock`), проверки счётчиков суммируют окна. Поведение сервиса не меняется.
- FIN-327: интеграционный тест запуска и остановки `serve` ждёт готовности `/readyz` условием с дедлайном вместо фиксированных 500 мс и больше не падает на нагруженной машине.

### Изменено

- FIN-290: `make test-names` проверяет и fuzz-цели (`FuzzUnit…`/`FuzzIntegration…`/`FuzzAPI…`, seed-корпус — в своём наборе) и отклоняет `Example…`, которые выполнялись бы вне трёх наборов покрытия; перенос go-service-template#26. Неклассифицированных Fuzz/Example в репозитории не было.

- FIN-306: постбэк Zvonok собирает цепочку X-Forwarded-For из всех строк заголовка; подделанная первая строка больше не выдаёт клиента за IP провайдера, если доверенный прокси дописал свой адрес отдельной строкой.

- FIN-285: Logout требует access-токен пользователя (ModelUser по grpc-api.md 3.3) и удаляет цепочку refresh только своего пользователя; чужой или неизвестный refresh — OK без эффекта, без токена — INVALID_TOKEN, удалённый пользователь — TOKEN_REVOKED.

- FIN-315: исправлен порядок ротации ключа подписи access JWT в `docs/config.md` и `docs/schema.md`: подписывать новым ключом — не раньше чем через 10 минут после того, как последняя реплика `user` со старым JWKS перестала принимать вызовы (SIGTERM плюс `SHUTDOWN_DELAY`), плюс до 5 с загрузки, а не через минуту; иначе при недоступном `user` валидные новые токены отклоняются как `INVALID_TOKEN`. Описано поведение закреплённой go-identity v0.1.0 и выпуска с FIN-293. Код не менялся.

- FIN-255: подключён опубликованный go-migrate v0.1.1 с исправлением FIN-262; полная проверка миграций источников с трёхзначными номерами работает без локальных замен модуля.

- FIN-255: добавлены integration-сценарии записи логов в файл с единым reqctx и развёртывания signing keys с атомарной заменой файла, удержанием предыдущих публичных ключей и отказом небезопасных файлов.

- FIN-255: общие server/client/reqctx, runner миграций и supervisor заданий подключены через go-grpc/go-worker v0.1.0 и go-migrate v0.1.1; lifecycle пулов — через go-postgres v0.2.0. Identity, ACL, tombstones, SQL, границы auth/outbox-транзакций и shutdown сохранены; библиотечные копии удалены. Тесты используют публичный migratetest и service-owned PostgreSQL 18. Логи transport/worker содержат безопасные причины без raw ошибок и panic payload.

- FIN-240: транзакции переведены на go-postgres v0.1.1; ожидаемые нарушения БД обрабатываются адаптером всей операции при запросе и COMMIT. Участники auth-транзакции явно используют её выделенный пул, чужой транзакционный контекст отклоняется.
