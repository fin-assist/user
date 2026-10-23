# gRPC-сервер, клиенты, наблюдаемость

Правила межсервисного взаимодействия — в `fin-assist/contracts`, [`docs/grpc-api.md`](https://github.com/fin-assist/contracts/blob/main/docs/grpc-api.md) (разделы 3, 4, 5, 9). Здесь — как шаблон их реализует.

## Порядок интерцепторов

Каждый вызов (unary и поток) проходит по порядку:

1. **`x-request-id`** — из metadata или новый; значение неожиданной формы (не `[A-Za-z0-9._-]{1,128}`) заменяется.
2. **Логи и метрики** — после обработчика (см. «Наблюдаемость»).
3. **Перехват паники** — голый `INTERNAL`, стек в лог.
4. **Сервисный токен** `x-service-authorization` → имя вызывающего. Нет или неверный — голый `UNAUTHENTICATED`.
5. **Таблица доступа** (`AccessPolicy()` в `internal/app/wire.go`, копия таблицы grpc-api.md 3.3). Метода нет в таблице или вызывающего нет в правиле — голый `PERMISSION_DENIED`, строка в лог уровня `ERROR` и метрика `finassist_grpc_denied_total{cause="caller_not_allowed"}` (на неё — алерт). Сервер **не стартует**, если у зарегистрированного метода нет правила.
6. **Подсказки `bank-bff`** — `x-client-ip`, `x-client-version`, `x-read-primary` берутся только от `bank-bff`; от других вызывающих игнорируются.
7. **Токен пользователя** `authorization` — по модели метода:

   | Модель | `authorization` | Пользователь в обработчике |
   |---|---|---|
   | `ModelUser` | обязателен; нет — `INVALID_TOKEN` | `reqctx.UserID(ctx)` = `sub` |
   | `ModelMixed` | есть — проверяется; нет — системный вызов | `UserID` или поле `user_id` запроса |
   | `ModelSystem` | игнорируется | поле `user_id` запроса |
   | `ModelNoUser` | игнорируется | нет (вход: `AuthService`) |

   Ошибки проверки — из реестра (`INVALID_TOKEN`, `TOKEN_EXPIRED`, `TOKEN_REVOKED`), они доходят до клиента. Токен, принятый верификатором без `sub`, — тоже `INVALID_TOKEN`: пользовательский вызов не превращается в системный.
8. **Tombstone** — удалённый пользователь в вызове от его имени получает `TOKEN_REVOKED`. Сбой проверки — голый `UNAVAILABLE` (обработчик ещё не выполнялся, повтор безопасен). Системные вызовы для удалённого пользователя обрабатывает сам обработчик: OK без действий (grpc-api.md 6.1).
9. **protovalidate** — нарушения аннотаций `.proto` → `VALIDATION_ERROR` с `BadRequest` по полям; `reason` нарушения: `required` → `REQUIRED`, формат (`pattern`, `email`, `uuid`, `prefix`, `defined_only`, …) → `INVALID_FORMAT`, остальное (длины, диапазоны, размеры, CEL) → `OUT_OF_RANGE`. В потоках проверяется каждое входящее сообщение.

10. **Нормализация ошибки обработчика** — вызывающему уходит только статус: обёрнутый `fmt.Errorf("…: %w", finerr.NotFound())` разворачивается до самого статуса (текст обёртки не уходит), ошибка без статуса (`pgx`, `io`, своя) становится голым `INTERNAL`, `context.Canceled` / `DeadlineExceeded` — голыми `CANCELLED` / `DEADLINE_EXCEEDED`. Без этого grpc-go отправил бы `UNKNOWN` с `err.Error()` в `status.message`. Исходная ошибка — в строке лога вызова (`error`).

Голые коды без `ErrorInfo` (шаги 3–5, 8, 10) — внутренние отказы, `bank-bff` отдаёт по ним 500 (grpc-api.md 5.1, правило 3). Строит их только `bare()` в `internal/adapter/grpc/server/status.go`; остальные ошибки — конструкторы `finerr`, обход запрещён линтером (`forbidigo`, `.golangci.yml`).

Health (`grpc.health.v1`) и reflection (только `ENV=dev`) открыты без токенов.

## Идентичность

Верификаторы go-identity v0.1.0 подключены к прежним интерфейсам ServiceVerifier и
UserVerifier. Таблица доступа и reqctx остаются у сервера; вторая цепочка идентичности
не устанавливается. Ошибка access JWT, включая истечение, — INVALID_TOKEN.

- AUTH_MODE=insecure-dev разрешён только при ENV=dev.
- AUTH_MODE=verify, SERVICE_AUTH_MODE=kubernetes: projected finassist-internal токен
  для исходящих вызовов; JWKS API-сервера по HTTPS с cluster CA и отдельным штатным
  API-server-audience токеном. Namespace finassist, audience finassist-internal,
  issuer из конфигурации. Ошибка ключей не включает dev-режим.
- SERVICE_AUTH_MODE=stage: отдельные Ed25519 ключи сервисов, общий issuer и доверенный
  публичный JWKS (kid — имя сервиса). Библиотека отвергает этот режим в prod.
- Пользовательские ключи берутся из локального public JWKS signing.Keys; bootstrap
  не вызывает собственный GetSigningKeys. Задания кэша запускаются общим worker lifecycle.
- Tombstone — go-tombstone v0.1.0 на primary; NoTombstones больше не используется в сборке.

GetConsentDocument проходит сервисный ACL как NoUser. Для типа ASSISTANT обработчик
сам требует единственный корректный Bearer access-токен и проверяет tombstone на primary
до чтения документа; PERSONAL_DATA доступен без access-токена. Неизвестный/неопубликованный
тип — NOT_FOUND. GetSigningKeys требует проверенный сервисный токен, пользовательского
токена не требует, отдаёт только публичные ключи.

## Сервер

- Keepalive: `MaxConnectionAge = 5 мин`, `MaxConnectionAgeGrace = 4 мин` (больше 3-минутного потока ответа), клиентский ping не чаще 20 с.
- Размер сообщения — `GRPC_MAX_RECV_MSG_SIZE` (4 МиБ, для `SearchOperations` — 16 МиБ).
- Готовность: раз в 5 с ping primary; результат — в gRPC health (`""`) и `/readyz`. До первого успешного ping — `NOT_SERVING`.
- Остановка по `SIGTERM`: `NOT_SERVING` → пауза `SHUTDOWN_DELAY` → `GracefulStop` не дольше `SHUTDOWN_TIMEOUT` → фоновые задания → пулы БД.

## Клиенты других сервисов

`client.Dial(cfg.Peers["<name>"], client.Options{...})` (`internal/adapter/client`):

- `round_robin` по всем адресам цели: в k3s сервисы headless (`dns:///<svc>.finassist.svc.cluster.local:9090`), на стенде — адрес процесса.
- Ретраи — только `UNAVAILABLE`, до 3 попыток с джиттером, только для методов из `Options.Retry`: чтения и идемпотентные по конструкции (grpc-api.md 6). Методы с `idempotency-key` ретраит приложение.
- В каждый вызов — `x-service-authorization` и `x-request-id`. Токен пользователя и подсказки клиента дальше не передаются: вызов из сервиса — системный, `user_id` — полем запроса.
- Дедлайн входящего вызова передаётся сам (контекст); вызов без дедлайна (задания, outbox) получает `DefaultTimeout`, 5 с. Потоки — без дедлайна по умолчанию, у них свои сроки.
- Keepalive клиента — 30 с.

## Транзакции и чтение

`postgres.DB` — пулы primary и реплик, [migrations.md](migrations.md), «Запросы на pgx»:

- `db.Run(ctx, func(ctx) error)` — транзакция на primary: commit при `nil`, rollback при ошибке и панике; вложенный `Run` присоединяется к внешнему.
- `db.Writer(ctx)` — транзакция контекста или primary; `db.Reader(ctx)` — транзакция, primary при `x-read-primary: 1` от `bank-bff`, иначе реплики.
- `db.Tx(ctx)` — транзакция контекста для библиотек, которые пишут в одной транзакции с эффектом (идемпотентность, outbox): без транзакции они возвращают ошибку, а не пишут мимо неё.

## Наблюдаемость

При AUTH_MODE=verify библиотека добавляет finassist_identity_rejected_total{kind,reason}:
kind user/service/access и фиксированные причины invalid/expired/signature/audience/issuer/
key_unavailable/forbidden/user_id. ACL выполняет сервер, поэтому его отказы остаются
в finassist_grpc_denied_total. Идентификаторы пользователей, ключи и токены в метки не идут.


- **Логи** — `slog`, JSON (text в dev), в каждой строке `service`, `request_id`, `caller`, `user_id` из контекста (методы `*Context`). Строка на вызов: `method`, `code`, `reason`, `duration_ms`, `error` (исходная ошибка обработчика — только здесь, клиенту не уходит). На `debug` — metadata и запрос: `authorization`, `x-service-authorization`, `idempotency-key`, ключи и поля с `token` в имени маскируются, поля `bytes` (файл выписки, ключ) очищаются независимо от имени.
- **Метрики** (`/metrics` на `HTTP_ADDR`): `grpc_server_handled_total{method,code,reason}`, `grpc_server_handling_seconds{method}`, `finassist_grpc_denied_total{method,caller,cause}`, `grpc_client_handled_total{peer,method,code}`, `grpc_client_handling_seconds{peer,method}`, метрики Go и процесса. **Без `user_id` и других неограниченных значений в метках.**

Лимиты FIN-219: `finassist_rate_limit_configured_limit{scope}`,
`finassist_rate_limit_window_seconds{scope}`, `finassist_rate_limit_checks_total{scope,result}`,
`finassist_rate_limit_consumed_total{scope}`, `finassist_rate_limit_window_utilization_ratio{scope}`
(histogram). Семь фиксированных scope, result — allowed/denied/error. Настройки публикуются
сразу; списания и заполнение — только после коммита через Receipt.Committed. Смысл
показателей, границы применения и выражения алертов — [limits.md](limits.md).

- **Проверки**: `/healthz` — процесс жив, `/readyz` — БД отвечает; то же в gRPC health. Работает и под systemd без проб Kubernetes.

## Фоновые задания

`outbox` доставляет `user.timezone_changed` через go-outbox v0.1.0, на отдельном primary-пуле.
Метрики: `finassist_outbox_rows{target}`, `finassist_outbox_oldest_age_seconds{target}`,
`finassist_outbox_delivery_errors_total{target}`; target ограничен настройкой получателей,
пустой строкой до fanout и unconfigured. Содержимое событий и user_id метками не становятся.
Обязателен внешний алерт на возраст > 3600 секунд для всех target; включение в infra
учитывается отдельно. Сборка, остановка, повторы и тесты — [profile.md](profile.md).

`internal/worker`: `worker.Run(ctx, log, jobs...)` — задание с ошибкой или паникой перезапускается через 5 с; `worker.Every(name, interval, log, fn)` — периодическое. Задания работают в каждом поде; единственность — блокировкой или `FOR UPDATE SKIP LOCKED`, как в outbox. Задания возвращает `wire()`.
