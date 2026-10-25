# История изменений

## [Unreleased]

### Несовместимо

- FIN-325: идентичность собирает `identity.NewAssembly` из go-identity v0.2.0; режим сервисной идентичности выбирает `ENV` (prod — ServiceAccount Kubernetes, stage — JWT из файлов ключей), `AUTH_MODE=verify` при `ENV=dev` не запускается. Переменные переименованы в стандартный набор `AUTH_*`: `SERVICE_TOKEN_ISSUER` → `AUTH_KUBERNETES_ISSUER`/`AUTH_STAGE_ISSUER`, `KUBERNETES_JWKS_URL`/`KUBERNETES_CA_FILE`/`KUBERNETES_TOKEN_FILE` → `AUTH_KUBERNETES_JWKS_URL`/`AUTH_KUBERNETES_CA_FILE`/`AUTH_KUBERNETES_API_TOKEN_FILE`, `STAGE_SIGNING_KEY_FILE`/`STAGE_PUBLIC_KEYS_FILE` → `AUTH_STAGE_*`; добавлена `AUTH_KUBERNETES_NAMESPACE`, `SERVICE_AUTH_MODE` удалена. Старое имя с непустым значением — ошибка запуска с именем замены; окружение стенда меняется вместе с выкладкой (`deploy/systemd/user.service`, таблица перехода — `docs/config.md`). Issuer/audience входящих access-токенов — по-прежнему `JWT_ISSUER`/`JWT_AUDIENCE`. Новая метрика библиотеки `finassist_identity_jwks_skipped_keys`.

### Исправлено

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
