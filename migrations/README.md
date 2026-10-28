Migrations of the service: `NNN_name.up.sql` + `NNN_name.down.sql`, numbered from `000001`.
Rules — `docs/migrations.md`.

FIN-364 применяется четырьмя шагами до service version 8:

- 000005 создаёт независимые receipts/bindings/known-busy и dispatch/deadline поля.
  Nullable FK рабочей таблицы добавляется NOT VALID: новые записи проверяются сразу,
  проверка старых строк не удерживает блокировку этого шага. Старые pending/results
  инвалидируются; уже выданные session chains сохраняются.
- 000006 отдельно VALIDATE CONSTRAINT receipt FK. Down — SELECT 1: валидация
  монотонна, FK удаляется вместе с колонкой при down000005.
- 000007 строит triple-key UNIQUE INDEX CONCURRENTLY отдельной SQL-командой.
  Прежний UNIQUE(provider,provider_call_id) продолжает действовать.
- 000008 атомарно заменяет прежний constraint на UNIQUE USING INDEX для
  (provider,campaign,provider_call_id), сохраняя canonical имя runtime error mapping.
  Единственный локальный squawk-ignore ban-drop-constraint обоснован уже готовым
  replacement index; глобальные проверки не ослаблены.

При cutover остановить phone Start на всех старых репликах и применить все шаги до
нового runtime; downgrade к repeat-confirm recovery недопустим. Down000008 и
down000005 под exclusive receipt-table lock отказывают при любом сохранённом proof.
До offline downgrade также исключить коллизии provider/call ID между campaigns:
прежний UNIQUE не допускает их. Down000008 восстанавливает standalone triple-key
index000007 перед прежним constraint, поэтому down(8)→up(8) работает без повтора000007.
Down000007 — одна DROP INDEX CONCURRENTLY команда. Down000005 не меняет уникальность
и не восстанавливает старые секреты. Неудачный CLI down оставляет dirty source;
оператор проверяет схему перед force, evidence не удаляется ради отката.

Receipt не удаляется каскадом аккаунта/working request; только собственный retention
удаляет provider bindings и известную занятость. CHECKs/triggers/functions/extensions
не добавляются. Миграции000001–000004 не меняются; новые шаги проверяются Squawk FIN-372.
