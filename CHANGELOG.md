# История изменений

## [Unreleased]

### Изменено

- FIN-255: добавлены integration-сценарии записи логов в файл с единым reqctx и развёртывания signing keys с атомарной заменой файла, удержанием предыдущих публичных ключей и отказом небезопасных файлов.

- FIN-255: общие server/client/reqctx, runner миграций и supervisor заданий подключены через go-grpc/go-migrate/go-worker v0.1.0; lifecycle пулов — через go-postgres v0.2.0. Identity, ACL, tombstones, SQL, границы auth/outbox-транзакций и shutdown сохранены; библиотечные копии удалены. Тесты используют публичный migratetest и service-owned PostgreSQL 18. Логи transport/worker содержат безопасные причины без raw ошибок и panic payload.

- FIN-240: транзакции переведены на go-postgres v0.1.1; ожидаемые нарушения БД обрабатываются адаптером всей операции при запросе и COMMIT. Участники auth-транзакции явно используют её выделенный пул, чужой транзакционный контекст отклоняется.
