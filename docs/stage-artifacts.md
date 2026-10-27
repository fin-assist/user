# Артефакты для systemd-stage (FIN-148)

Job `image` извлекает исполняемый файл из **того же собранного образа**, который прошёл smoke-check.
Повторной компиляции для stage нет. `VERSION` сервисного бинаря — полный SHA исходников.
Артефакт `stage-user-SHA-linux-amd64` содержит бинарь, `manifest.json` и `SHA256SUMS`;
у assistant отдельный `stage-fake-llm-SHA-linux-amd64` из `Dockerfile.fake-llm`.
Manifest фиксирует репозиторий, SHA, Actions run/attempt, content ID образа, платформу,
SHA-256 бинаря и версии модулей fin-assist из go.mod. Контрольная сумма подтверждает целостность,
но не заменяет доверенный источник Actions.

## Получение и проверка

Для выкладки выбирать только **успешный полный CI** события `push` в `main` нужного репозитория.
Артефакты PR предназначены для проверки изменений; зелёный job `image` сам по себе не означает,
что остальные проверки CI прошли. Нужны успешные `go`, `lint`, `image` актуального SHA.

```bash
gh run view RUN_ID --repo fin-assist/user --json event,headBranch,headSha,conclusion,jobs
gh run download RUN_ID --repo fin-assist/user --name stage-user-SHA-linux-amd64 --dir ./user-SHA
(cd ./user-SHA && sha256sum --check SHA256SUMS)
```

Сверить SHA, repository, run_id/attempt, platform и зависимости в manifest с выбранным run;
не выполнять скачанный бинарь до этой проверки. Набор пяти сервисов и fake-llm фиксируется
отдельно в deployment manifest infra: одинаковая версия contracts необходима, но сама по себе
не доказывает совместимость. Её подтверждает совместный запуск и API-приёмка FIN-362.
Срок хранения artifact — 30 дней; нужный для отката проверенный набор до истечения срока
сохраняется в закрытом хранилище оператора. Автоматической выкладки нет.

Это stage-часть FIN-148. Публикация GHCR, SBOM/сканирование, политика хранения образов и
production-доставка через PR infra остаются отдельной невыполненной частью исходной задачи.
