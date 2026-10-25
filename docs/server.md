# gRPC-сервер, клиенты, наблюдаемость

Правила межсервисного взаимодействия — в `fin-assist/contracts`, [`docs/grpc-api.md`](https://github.com/fin-assist/contracts/blob/main/docs/grpc-api.md) (разделы 3, 4, 5, 9). Здесь — как шаблон их реализует.

## Порядок интерцепторов

Каждый вызов (unary и поток) проходит по порядку:

1. **`x-request-id`** — из metadata или новый; значение неожиданной формы (не `[A-Za-z0-9._-]{1,128}`) заменяется.
2. **Логи и метрики** — после обработчика (см. «Наблюдаемость»).
3. **Перехват паники** — голый `INTERNAL`, в журнале безопасное событие без payload и стека (go-grpc v0.1.0).
4. **Сервисный токен** `x-service-authorization` → имя вызывающего. Нет или неверный — голый `UNAUTHENTICATED`.
5. **Таблица доступа** (`AccessPolicy()` в `internal/app/wire.go`, копия таблицы grpc-api.md 3.3). Метода нет в таблице или вызывающего нет в правиле — голый `PERMISSION_DENIED`, строка в лог уровня `ERROR` и метрика `finassist_grpc_denied_total{cause="caller_not_allowed"}` (на неё — алерт). Сервер **не стартует**, если у зарегистрированного метода нет правила.
6. **Подсказки `bank-bff`** — `x-client-ip`, `x-client-version`, `x-read-primary` берутся только от `bank-bff`; от других вызывающих игнорируются.
7. **Токен пользователя** `authorization` — по модели метода:

   | Модель | `authorization` | Пользователь в обработчике |
   |---|---|---|
   | `ModelUser` | обязателен; нет — `INVALID_TOKEN` | `reqctx.UserID(ctx)` = `sub` |
   | `ModelMixed` | есть — проверяется; нет — системный вызов | `UserID` или поле `user_id` запроса |
   | `ModelSystem` | игнорируется | поле `user_id` запроса |
   | `ModelNoUser` | игнорируется | нет (вход: `AuthService`, кроме `Logout` — у него `ModelUser`) |

   Ошибки проверки — из реестра (`INVALID_TOKEN`, `TOKEN_EXPIRED`, `TOKEN_REVOKED`), они доходят до клиента. Токен, принятый верификатором без `sub`, — тоже `INVALID_TOKEN`: пользовательский вызов не превращается в системный.
8. **Tombstone** — удалённый пользователь в вызове от его имени получает `TOKEN_REVOKED`. Исключение — точное правило `DeleteAccount` с `AllowDeleted`: повтор разрешён с проверенным пользовательским токеном и сервисным ACL; wildcard и модели кроме `ModelUser` с этим флагом запрещены при старте. Сбой проверки — голый `UNAVAILABLE` (обработчик ещё не выполнялся, повтор безопасен). Системные вызовы для удалённого пользователя обрабатывает сам обработчик: OK без действий (grpc-api.md 6.1).
9. **protovalidate** — нарушения аннотаций `.proto` → `VALIDATION_ERROR` с `BadRequest` по полям; `reason` нарушения: `required` → `REQUIRED`, формат (`pattern`, `email`, `uuid`, `prefix`, `defined_only`, …) → `INVALID_FORMAT`, остальное (длины, диапазоны, размеры, CEL) → `OUT_OF_RANGE`. В потоках проверяется каждое входящее сообщение.

10. **Нормализация ошибки обработчика** — вызывающему уходит только статус: обёрнутый `fmt.Errorf("…: %w", finerr.NotFound())` разворачивается до самого статуса (текст обёртки не уходит), ошибка без статуса (`pgx`, `io`, своя) становится голым `INTERNAL`, `context.Canceled` / `DeadlineExceeded` — голыми `CANCELLED` / `DEADLINE_EXCEEDED`. Без этого grpc-go отправил бы `UNKNOWN` с `err.Error()` в `status.message`. Исходная ошибка не попадает в журнал: go-grpc оставляет безопасные code/reason/event без raw error, DSN и секретов.

Голые коды без `ErrorInfo` (шаги 3–5, 8, 10) — внутренние отказы, `bank-bff` отдаёт по ним 500 (grpc-api.md 5.1, правило 3). Строит их только `bare()` в `go-grpc/server/status.go`; остальные ошибки — конструкторы `finerr`, обход запрещён линтером (`forbidigo`, `.golangci.yml`).

Health (`grpc.health.v1`) и reflection (только `ENV=dev`) открыты без токенов.

## Идентичность

Идентичность собирает `identity.NewAssembly` из go-identity v0.2.0 (FIN-325): её
верификаторы и источник исходящего токена подключены к прежним интерфейсам ServiceVerifier,
UserVerifier и TokenSource. Таблица доступа и reqctx остаются у сервера; вторая цепочка
идентичности не устанавливается. Ошибка access JWT, включая истечение, — INVALID_TOKEN.

- AUTH_MODE=insecure-dev разрешён только при ENV=dev; AUTH_MODE=verify при ENV=dev не
  стартует (библиотека строит проверенную идентичность только для stage и prod).
- ENV=prod: ServiceAccount Kubernetes — projected finassist-internal токен
  (`SERVICE_TOKEN_FILE`) для исходящих вызовов; JWKS API-сервера по HTTPS с CA из
  `AUTH_KUBERNETES_CA_FILE` и отдельным API-server-audience токеном
  (`AUTH_KUBERNETES_API_TOKEN_FILE`, другой файл). Namespace `AUTH_KUBERNETES_NAMESPACE`,
  audience finassist-internal, issuer `AUTH_KUBERNETES_ISSUER`. `AUTH_STAGE_*` в prod
  запрещены. Ошибка ключей не включает dev-режим.
- ENV=stage: короткие Ed25519 JWT сервисов — свой ключ `AUTH_STAGE_SIGNING_KEY_FILE`,
  общий issuer `AUTH_STAGE_ISSUER` и доверенный публичный JWKS
  `AUTH_STAGE_PUBLIC_KEYS_FILE` (kid — имя сервиса); разбирает его библиотека.
- Прежние `SERVICE_AUTH_MODE`, `SERVICE_TOKEN_ISSUER`, `STAGE_*`, `KUBERNETES_*` больше
  не читаются: заданное непустое значение — ошибка старта с именем замены
  ([config.md](config.md), «Переход на AUTH_*»).
- Issuer и audience входящих access-токенов — те же `JWT_ISSUER`/`JWT_AUDIENCE`, которыми
  user подписывает (у потребителей — `AUTH_USER_ISSUER`/`AUTH_USER_AUDIENCE`).
- Пользовательские ключи берутся из локального public JWKS signing.Keys (`UserKeys`
  сборки); bootstrap не вызывает собственный GetSigningKeys. Обновление обоих кэшей
  ключей — одно задание `identity-keys` (`Assembly.Run`) в общем worker lifecycle;
  `Assembly.Close` — при остановке.
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
- Остановка по `SIGTERM`: HTTP `/readyz` (`ops.Readiness.Drain`, необратимо до конца процесса) и gRPC health одновременно перестают сообщать готовность → пауза `SHUTDOWN_DELAY` (вызовы ещё принимаются, `/healthz` отвечает 200) → `GracefulStop` не дольше `SHUTDOWN_TIMEOUT` → фоновые задания → пулы БД (FIN-289).

## Клиенты других сервисов

`client.Dial(cfg.Peers["<name>"], client.Options{...})` (`github.com/fin-assist/go-grpc/client`):

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
в finassist_grpc_denied_total. С go-identity v0.2.0 добавлен и
finassist_identity_jwks_skipped_keys{kind,reason} — число пропущенных неподдерживаемых
ключей в текущем снимке (kind user/service). Идентификаторы пользователей, ключи и токены в метки не идут.


- **Логи** — `slog`, JSON (text в dev), в каждой строке `service`, `request_id`, `caller`, `user_id` из контекста (методы `*Context`). Строка на вызов: `method`, `code`, `reason`, `duration_ms`, безопасное событие без текста исходной ошибки обработчика. На `debug` — metadata и запрос: `authorization`, `x-service-authorization`, `idempotency-key`, ключи и поля с `token` в имени маскируются, поля `bytes` (файл выписки, ключ) очищаются независимо от имени.
- **Метрики** (`/metrics` на `HTTP_ADDR`): `grpc_server_handled_total{method,code,reason}`, `grpc_server_handling_seconds{method}`, `finassist_grpc_denied_total{method,caller,cause}`, `grpc_client_handled_total{peer,method,code}`, `grpc_client_handling_seconds{peer,method}`, метрики Go и процесса. **Без `user_id` и других неограниченных значений в метках.**

Лимиты FIN-219: `finassist_rate_limit_configured_limit{scope}`,
`finassist_rate_limit_window_seconds{scope}`, `finassist_rate_limit_checks_total{scope,result}`,
`finassist_rate_limit_consumed_total{scope}`, `finassist_rate_limit_refunded_total{scope}`,
`finassist_rate_limit_window_utilization_ratio{scope}`
(histogram). Семь фиксированных scope, result — allowed/denied/error. Настройки публикуются
сразу; списания и заполнение — только после коммита через Receipt.Committed. Смысл
показателей, границы применения и выражения алертов — [limits.md](limits.md).

- **Проверки**: `/healthz` — процесс жив, `/readyz` — БД отвечает и процесс не останавливается; то же в gRPC health. Работает и под systemd без проб Kubernetes.

## Фоновые задания

phone-notifications использует одно отдельное primary-соединение LISTEN на процесс.
phone-cleanup раз в минуту стирает истёкшие результаты, освобождает номера и удаляет
старые заявки, сохраняя живые refresh-сессии. Обрыв LISTEN закрывает ожидающие Watch;
worker пересоздаёт соединение, клиент возобновляет поток с тем же секретом.
finassist_phone_verification_provider_duplicate_total считает отказы провайдера по
дублю при локально свободном номере; без меток и ПДн. consumed/refunded публикуются
после commit и различают резервирование попытки и его доказанную компенсацию.
Метрики finassist_phone_watch_active_streams, finassist_phone_watch_max_streams,
finassist_phone_watch_listener_ready и finassist_phone_watch_rejected_total{reason}
показывают заполнение, предел и недоступность. reason — только capacity/not_ready,
других меток нет. Настройки и выражения алертов — [verification.md](verification.md).

`outbox` доставляет `user.timezone_changed` и `user.deleted` через go-outbox v0.1.0, на отдельном primary-пуле.
Метрики: `finassist_outbox_rows{target}`, `finassist_outbox_oldest_age_seconds{target}`,
`finassist_outbox_delivery_errors_total{target}`; target ограничен настройкой получателей,
пустой строкой до fanout и unconfigured. Содержимое событий и user_id метками не становятся.
Обязателен внешний алерт на возраст > 3600 секунд для всех target; включение в infra
учитывается отдельно. Сборка, остановка, повторы и тесты — [profile.md](profile.md), [deletion.md](deletion.md).
Темы объединены по target: правило возраста покрывает в том числе PurgeUserData.

Register/Refresh подключают `finassist_idempotency_requests_total{endpoint,outcome}`:
endpoint — два полных имени RPC, outcome — new/replay/conflict/in_progress. Это решения
библиотеки до commit, а не число закоммиченных эффектов. Секреты и ключи метками не
становятся. Рост conflict/in_progress и ошибки auth-cleanup отслеживаются внешними
правилами мониторинга; параметры порога задаются в infra. Детали — [auth.md](auth.md).

`github.com/fin-assist/go-worker`: `worker.Run(ctx, worker.Options{Logger: log, Clock: worker.SystemClock{}, RestartDelay: 5*time.Second}, jobs...)` — задание с ошибкой или паникой перезапускается через 5 с; `worker.Every(name, interval, log, worker.SystemClock{}, fn)` — периодическое. Задания работают в каждом поде; единственность — блокировкой или `FOR UPDATE SKIP LOCKED`, как в outbox. Задания возвращает `wire()`.

## Zvonok и номера пула (FIN-139/216)

Секрет public_key уходит в multipart POST confirm, а при документированных GET —
в query по отдельному решению владельца. Клиент фиксирует HTTPS endpoint, запрещает
редиректы и удаляет URL/текст ответа из ошибок. Никаких debug-логов HTTP-тел.
Постбэк вынесен на отдельный listener; query с ct_phone не попадает в логи сервиса.
Ingress также обязан отключить/маскировать query. Схема и границы — [zvonok.md](zvonok.md).

| Метрика | Значение и ограниченные метки |
|---|---|
| `finassist_zvonok_requests_total{operation,outcome}` | phones/calls_by_phone / phones/confirm / phones/call_by_id; ok / rejected / rate_limited / unavailable / capacity_unavailable. ok здесь означает HTTP 200, не подтверждение номера |
| `finassist_zvonok_request_duration_seconds{operation}` | время операции вместе с ожиданием общего API gate |
| `finassist_zvonok_postbacks_total{result}` | accepted / foreign_ip / unknown_call / unconfirmed / unavailable / invalid; accepted может быть повтором или поздним событием без изменения |
| `finassist_phone_provider_checks_total{source,result}` | source postback / fallback; result confirmed / unchanged / mismatch / unconfirmed / unavailable / capacity |
| `finassist_phone_provider_poll_interval_seconds`, `finassist_phone_provider_poll_capacity`, `finassist_phone_provider_poll_active` | действующие настройки интервала/ёмкости и число заявок в coalescing-карте |
| `finassist_phone_callback_outcomes_24h{callback_number,outcome}` | gauges заявок, созданных за последние 24 ч: pending / confirmed / expired; публичные номера ограничены 256 значениями на процесс, остальные other |
| `finassist_phone_callback_overflow_requests_24h` | заявки в other, включая недетализированные группы; рост означает потерю детализации пула |

Личные телефоны, call_id, токены, IP и ключи в метках запрещены. callback_number —
исключение только для публичных номеров из authenticated allowed_phones_for_call.
Глобальные rolling gauges читают одну primary: **не суммировать реплики**. Сравнение
конверсии отдельного номера — confirmed / (confirmed + expired), pending из знаменателя
исключается. При небольшом числе заявок отношение ещё не показатель качества номера.

Примеры правил для внешнего мониторинга (подключение в infra отдельно):

```promql
# Provider API quota/rejection: investigate errors and capacity rather than paid retries.
sum(increase(finassist_zvonok_requests_total{outcome=~"rate_limited|capacity_unavailable"}[5m])) > 0
# Source-IP refusals and overflow of the fallback coalescing map.
sum(increase(finassist_zvonok_postbacks_total{result="foreign_ip"}[5m])) > 0
sum(increase(finassist_phone_provider_checks_total{result="capacity"}[5m])) > 0
max(finassist_phone_callback_overflow_requests_24h) > 0
```

Собственный контроль баланса и соответствующие метрики/алерты отложены в
[FIN-233](https://finassist.youtrack.cloud/issue/FIN-233); пока используются уведомления Zvonok.
Платные запросы тестами не выполняются. FIN-81 остаётся условием включения prod.

## Фейк и защита prod (FIN-122)

`PHONE_PROVIDER=fake` работает только в dev/stage и использует общий Confirm.
В prod перед любыми эффектами Confirm служебные телефоны +7000000xxxx отклоняются
при любом провайдере. `finassist_phone_test_confirmation_rejected_total` — счётчик
таких попыток без меток, включая откатившиеся; сопровождается ERROR без ПДн.
Готовое правило `UserSyntheticPhoneConfirmationInProd`:
[deploy/prometheus/phone.rules.yml](../deploy/prometheus/phone.rules.yml).
Его загрузка в vmalert/Prometheus и маршрутизация Alertmanager выполняются при
развёртывании; в текущем infra мониторинга пока нет.
Правило `counter > 0` сохраняет сигнал до перезапуска процесса.
Общие gauges пула также допускают два фиксированных синтетических номера fake.
Локальный запуск и поведение клиента — [fake-phone.md](fake-phone.md).
