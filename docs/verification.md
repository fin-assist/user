# Создание заявки, общее подтверждение и Watch

FIN-219 реализует StartPhoneVerification, WatchPhoneVerification и общий сценарий
подтверждения по api.md 1.1–1.2 и grpc-api.md 7.1. FIN-139/216 подключают Zvonok V1,
постбэк с проверкой по API, fallback открытого Watch и сохранение выбранного номера.
По умолчанию провайдер выключен: создание даёт INTERNAL без списания лимита.
`PHONE_PROVIDER=zvonok` разрешён только в dev/stage до защиты бюджета FIN-81.
[Zvonok и ограничения активации](zvonok.md).
Runtime-фейк FIN-122 использует этот же Confirm, шифрование, pg_notify и Watch.
Его автоподтверждение входит в транзакцию завершения Start; настройки, тестовый
диапазон, повторы и защита prod — в [fake-phone.md](fake-phone.md).

## Создание и восстановление (FIN-364)

Нормативный источник — contracts/gen/go v0.7.0, REST 1.1–1.3 и gRPC 7.1.
Start получает отдельные `age_confirmed`, `pd_consent_accepted`, `pd_consent_version`.
После ключа (replay/409) проверяются лимиты и занятость (429), формат/14+/границы
версии (400), согласие (403), текущая версия PERSONAL_DATA (422). Прямой RPC
подчиняется тем же проверкам. Неверный телефон проходит IP-лимиты без создания
произвольного phone-counter. Структурные REST-проверки выполняет BFF до RPC.

Одна primary-транзакция сохраняет ключ/хеш, четыре исходных окна, claim и отдельный
`phone_consent_receipt`: phone, доверенный IP, версия, фактические UTC-времена
согласия и подтверждения 14+. Receipt не принадлежит найденному по телефону
аккаунту до проверенного владения. Версия документа — FK на неизменяемый документ;
старый текст/URL нельзя перезаписывать или удалять пока существуют доказательства.
Ошибка/отказ COMMIT запрещает внешний вызов. Network никогда не выполняется в Run.

`PHONE_VERIFICATION_UNKNOWN_TIMEOUT=5m` — локальный timeout неизвестного исхода,
Go duration 1s..5m, проверяется при startup. При приёме сохраняется абсолютный
unknown_deadline; конфигурация, restart и replay его не пересчитывают. Отдельная
транзакция атомарно записывает first_dispatch_at и dispatch_not_after=unknown_deadline.
Только победитель первой записи вправе сделать один Create до границы, с ограниченным
контекстом; crash после marker допускает только read-only Recover. Нет повторного
confirm даже при потерянном ответе. Recover без достаточного status/pool остаётся
unknown. Общий бюджет завершения 10s; конкретный HTTP timeout у адаптера 3s.

До unknown_deadline новый ключ получает обычный cached duplicate429 до
ceilMinute(deadline), с однократным списанием phone/IP; replay не списывает, чистый
limiter429 не кэшируется/не списывает. Исходный key продолжает свою попытку до лимитов
и собственной занятости. Если read-only сверка безопасна, unknown INTERNAL_ERROR
несёт явный RequestRetryInfo true, после срока — false.

На unknown_deadline claim закрывается и номер освобождается без возврата квоты.
Это принятый владельцем риск поздней внешней заявки/дубля, не удалённая отмена.
Поздний ответ/постбэк не восстанавливает attempt и не выдаёт старые токены. Старый
key в обычном окне 24h от первого приёма получает terminal INTERNAL_ERROR false
(другое тело —409), без нового confirm/receipt/списания. Рабочая строка и архив
могут жить дольше lookup окна; после него UUID может стать fresh с новыми gates.
Успешный библиотечный cache ограничивается исходным key deadline, не completion+24h.

Проверенный pending candidate связывается атомарно по (provider, configured campaign,
call_id), уникально среди live и сохранённых receipts. Проверяются phone/campaign,
status, distinct E.164 pool 1..1000, provider.created внутри допустимого dispatch/recv
интервала и положительный Duration остатка на commit. Подтверждение прежнего terminal
звонка не наследуется. Подробнее о Zvonok ε=1s и authenticated GET — [zvonok.md](zvonok.md).
Хранятся once declared=provider.created+5m, public expires_at=declared−ε и
known_busy_until=declared+ε. Public deadline консервативен; expires_in — floor
max(0, remainder), поэтому 0 возможен при положительном остатке <1s. Fake использует
ε=0 через те же интерфейсы/gates, core не выбирает правила по providerName.

Проверенный active NONTERMINAL старый/чужой ID создаёт самостоятельный phone+busy_until
факт без ownership/proof. Original dispatched key C — terminal: cached429 с явным
false до future ceilMinute(busy_until), затем terminal500false до конца key window.
Новый key до busy получает DB429 без HTTP. Terminal/expired ID не даёт fake fence;
недоверенный ответ остаётся unknown. Bare duplicate имеет конечный fallback +5m.
После полученного429 клиент ждёт retry_at и создаёт новый key для нового намерения;
transport retry потерянного ответа сохраняет прежний. Predispatch busy keys после
retry_at могут выполняться заново для совместимости.

Доказанный первый отказ без созданной заявки закрывает original attempt; только
ReleaseIfCurrent разрешает атомарный возврат исходных окон. Сам key terminal false,
повторное намерение использует новый key. Внешние mismatch учитывает существующая
finassist_phone_verification_provider_duplicate_total без персональных labels;
обычные DB429 в неё не входят.

## Подтверждение

verification.Service.Confirm принимает Proof только после проверки статуса и номера
через API провайдера. Сам по себе входящий постбэк доказательством не является.
Проверяются provider/call_id/phone, соответствие строке заявки и локальный срок
`created_at <= now < expires_at`. Часы и генератор секретов внедряются.

Одна primary-транзакция узнаёт пользователя по телефону без блокировки заявки.
Если пользователь уже существует, порядок блокировок — Guard tombstone → app_user
→ phone_verification. Новый номер блокирует только заявку. Подтверждение для
удалённого пользователя не создаёт сессию. Повтор/опоздание не выпускает новых токенов.

Существующий пользователь получает access JWT и отдельную refresh-сессию через тот же
генератор пар и репозиторий auth, что регистрация/refresh. Новый получает случайный
256-битный registration_token; в строке хранится его SHA-256. Полный результат
зашифрован. Результат, сессия и pg_notify('phone_verification', id) фиксируются вместе:
откат не выдаёт уведомления и не оставляет частичной сессии. В payload — только
внутренний ID; телефона и секретов нет. Внешнего RPC внутри транзакции нет.

## Срок, повторная доставка и блокировки чтения

Watch доступен только bank-bff без пользовательского access JWT. Каждая попытка
сначала коммитит IP-лимит (60/мин по умолчанию), затем проверяет verification_token.
Неизвестный, закрытый, уже истёкший pending либо verified старше 15 минут —
INVALID_TOKEN до первого сообщения. Уже открытый pending завершается событием expired
при наступлении срока; проверка состояния снова читает primary перед терминальным ответом.

Первое сообщение — текущий снимок. Verified закрывает поток после результата:
либо is_new_user=true + registration_token, либо is_new_user=false + tokens.
Повтор 15 минут с verified_at отдаёт тот же результат, даже после expires_at заявки.
Первый Register/Refresh закрывает заявку и стирает ciphertext; последующий Watch
получает INVALID_TOKEN. Старые refresh-звенья сохраняются по правилам auth.

Снимок берёт краткую транзакцию и FOR SHARE заявки. Для результата существующего
пользователя Guard берётся до блокировки заявки, проверяя удаление. Если pending
успел подтвердиться и получить владельца между чтениями, транзакция откатывается,
и чтение повторяется с правильным Guard. Открытый поток транзакцию не удерживает.
Хеш входного секрета дополнительно сравнивается constant-time с хешем строки.

## Слушатель и ограничения ресурсов

На экземпляр — одно отдельное primary-соединение LISTEN, общее для всех Watch.
Подписка по ID регистрируется перед повторным чтением снимка: подтверждение между
первым чтением и подпиской не теряется. Уведомления сливаются в ограниченный канал;
чтение primary — источник истины. PHONE_WATCH_RESYNC_INTERVAL (5 с по умолчанию,
положительный, не больше 30 с) дополнительно перечитывает состояние, например после
удаления заявки без NOTIFY. На pending действует граница expires_at + 10 с;
немедленный replay verified не получает уже прошедший срок pending-потока.

PHONE_WATCH_MAX_STREAMS ограничивает одновременно ожидающие подписки на экземпляр,
по умолчанию 1024 (1–100000). Это отдельный предел, не 60 попыток/мин на IP.
Насыщение/недоступный слушатель до сообщения дают голый INTERNAL; секретные данные
не выдаются, попытка IP уже учтена. Отмена освобождает подписку. Обрыв LISTEN закрывает
все ожидающие потоки без выдуманного verified/expired; worker повторяет задание и
переподключается. Клиент возобновляет SSE по обычному backoff с тем же секретом.

## Шифрование и эксплуатация

Нужен **PHONE_RESULT_KEY_FILE**: 32 raw-байта AES-256, обычный файл 0600 (credential systemd — 0400 или 0440, [config.md](config.md)), общий для
реплик, отдельный от ключа идемпотентности и ключа подписи JWT. Совпадение AES-ключей
запрещает старт, даже если файлы разные. PHONE_RESULT_KEY_ID (default primary,
1–64 символа A-Z/a-z/0-9/._-) хранится с ciphertext. AES-GCM использует случайный nonce,
AAD связывает ID и хеш verification_token; перестановка ciphertext между заявками
отвергается. Повреждённый результат/неизвестный key ID дают внутренний отказ без токенов.

Первый вариант поддерживает один ключ результата. При плановой замене приостановите
создание новых заявок и дождитесь завершения всех 15-минутных результатов; до этого
сохраняйте прежние ключ/ID на всех репликах. Замена ключа раньше сделает остаточные
результаты нечитаемыми и потребует нового входа. Ротация с одновременным чтением
нескольких ключей здесь не заявляется. make dev-key создаёт отдельный локальный ключ
без перезаписи; compose/make run/systemd используют его. В образ helper не входит.

## Очистка и конкуренция

phone-cleanup работает каждую минуту, batch≤1000, общий deadline5s. Отдельные транзакции
стирают истёкшие ciphertext, освобождают unknown holders и active holders после
known_busy_until, чистят истёкшие known-busy факты и key/hash после обычного24h окна;
session-only стадия отвязывает старые ссылки, следующая удаляет working rows и архивы.
Claim также освобождает срок атомарно, без ожидания job. Completion проверяет open,
unknown_deadline и public remainder прямо в UPDATE. Квота при cleanup не возвращается.

Abandoned receipt хранится ровно до attempt_end+24h; для unknown end — первоначальный
unknown_deadline. Подтверждённый existing account связывается при Confirm до выдачи
токенов; новый — атомарно в Register. Linked receipt живёт до удаления/отзыва аккаунта,
затем 3 календарных года. Удаление аккаунта не каскадирует evidence, первоначальные
времена не меняются. Сессии/operational requests/кэши удаляются обычным путём, proof
не открывает auth и не удерживает phone. На cutoff удаляются receipt и provider bindings,
PII/hash/key/token/ciphertext связанных старых working rows; другие live данные и квоты
не затрагиваются. Архивный key не продлевает lookup. Отдельные права доступа описаны
в [schema.md](schema.md); клиентских методов чтения evidence нет.

Миграция000005 добавляет receipts/binding/known-busy/metadata, без CHECK/triggers/functions.
Она инвалидирует старые pending и ещё выдаваемые verified результаты без evidence.
Уже выданные до cutover persisted refresh/access sessions сохраняются, даже если первый
Refresh ещё не выполнялся. Старые Watch/registration tokens не возобновляют результат.
Rollback схемы не восстанавливает эти секреты; upgrade/downgrade требует остановленного
приёма phone Start, а несовместимый downgrade возможен только после проверки retained
provider-ID коллизий разных campaigns. Конкурентные completion/link/cleanup сериализуются
request locks; Guard→user→request и отдельная session-only стадия сохраняются.

## Метрики и проверки

finassist_phone_watch_active_streams и finassist_phone_watch_max_streams — текущее
число и предел ожидающих подписок. finassist_phone_watch_listener_ready — 0/1.
finassist_phone_watch_rejected_total{reason} — отказы capacity/not_ready. Никаких
ID, телефонов, IP или секретов в метках нет. Счётчики IP/настройки окон — [limits.md](limits.md).
Примеры внешних правил, пороги/выдержку задаёт infra:

```promql
finassist_phone_watch_active_streams / finassist_phone_watch_max_streams > 0.8
increase(finassist_phone_watch_rejected_total{reason="capacity"}[5m]) > 0
finassist_phone_watch_listener_ready == 0
```

Метрики/документация не означают, что уведомления уже развёрнуты.

Unit покрывают сроки/повторы, ошибки зависимостей, гонку снимка/подписки, освобождение,
маппинг и криптографическую привязку. PostgreSQL проверяет primary при пустой replica,
commit/rollback NOTIFY, единственную сессию при конкурентном подтверждении, очистку
занятых сессий и переподключение LISTEN. API-тесты проходят полный сервер:
новый/существующий пользователь, два экземпляра, Register/Refresh, replay, истечение,
лимит до секрета, чужой вызывающий, ёмкость/отмена, повреждённый результат, отказ БД,
обрыв LISTEN, resync после удаления и гонка Confirm/DeleteAccount. Продовый провайдер
и сквозной стенд этим этапом не подтверждаются.

Start проверяется на трёх уровнях: валидация ответа провайдера/границ времени и
сценарий на фейках; PostgreSQL — claim/busy, CAS, rollback, исходные окна компенсации,
удержание ссылок живого кэша; API — полный вход нового/существующего пользователя,
повтор/конфликт/конкурентные ключи, лимит перед busy, потеря ответа и настоящий рестарт
сервиса с новой сборкой адаптера, отказ/дубль провайдера, отмена клиента после резерва
и удаление аккаунта во время внешнего вызова без воскрешения данных.

## Итоговая сверка 13 RPC FIN-219

Все проверки ниже вызывают настоящий gRPC через internal/app/apitest, с таблицей
доступа, интерцепторами и PostgreSQL. Полные альтернативы/ошибки остаются в тестовых
файлах и профильных документах; таблица указывает основной набор сценариев.

| RPC | API-тесты |
| --- | --- |
| StartPhoneVerification | start_test.go: полный вход, replay/conflict/busy/лимит, restart, отказ/дубль, отмена/удаление |
| WatchPhoneVerification | verification_test.go: новый/существующий, повтор 15 мин, истечение, capacity, LISTEN/resync, rollback |
| Register | auth_test.go: atomic replay, ошибки согласий, одноразовость, конкурентные заявки одного номера |
| RefreshTokens | auth_test.go: replay/reuse, конкурентная ротация, лимит до секрета, удалённый владелец |
| Logout | TestAPIAuthAPILogoutRequiresAccessTokenAndOwner (access-токен, чужой refresh, повтор, удалённый пользователь), TestAPILogoutSignedJWTRejectsForgedAndExpiredAccess (поддельный/просроченный JWT в AuthVerify), TestAPIAuthAPIRefreshReplayReuseAndLogout, TestAPIAuthAPIAccessTableErrorsAndDeletedReplay |
| GetConsentDocument | TestDocumentAPI: тип/версия, публичный ПДн и защищённый assistant, отсутствие документа |
| GetProfile | TestProfileAPISettingsAndIsolation, TestProfileAPIAccessDeletionAndFailures |
| UpdateProfile | TestProfileAPISettingsAndIsolation: пустое/частичное, неверные значения, timezone/outbox/version |
| GrantAssistantConsent | TestProfileAPIConsentAndSystemCheck: актуальная/устаревшая версия, повтор |
| RevokeAssistantConsent | TestProfileAPIConsentAndSystemCheck: отзыв/повтор, немедленное чтение primary |
| DeleteAccount | deletion_test.go/deletion_signed_test.go: повтор, старый JWT, rollback, конкуренция, отсутствие воскрешения |
| GetAssistantConsentStatus | TestProfileAPIConsentAndSystemCheck, TestProfileAPIAccessDeletionAndFailures |
| GetSigningKeys | TestSigningKeysAPI, TestStageSignedIdentityAPI: ключи, подпись/ACL, ошибки идентичности |

Надёжность соседних доставок проверяют outbox_test.go и deletion_outbox_test.go:
реальные задания, bufconn-получатели, недоступность/медленный ответ, независимые
получатели, рестарт и повтор. Подпись/перекрытие ключей также проверяет internal/signing.
Runtime-провайдеры, публикация утверждённых документов, infra и работающий стенд
не подменяются этими тестами и остаются самостоятельными условиями запуска.

Down000005 под exclusive table lock отказывает при любом сохранённом receipt; откат пустой схемы проходит up/down/up. После ошибки source dirty проверяется оператором, evidence не удаляется.
