# Study Monitoring

Локальный сборщик учебных источников на Go. Он получает выбранные письма Gmail и Yandex IMAP, сообщения Telegram, страницы учебных платформ, расписание, задания GitHub и файлы ручного inbox. Изменения сохраняются в очередь до подтверждения смыслового разбора.

Публичный репозиторий содержит общий код, тесты и страницы OAuth-приложения. В нём нет личной конфигурации, писем, чатов, учебных работ, аккаунтов или секретов. История личного рабочего пространства в этот репозиторий не импортируется.

## Сборка и проверка

Нужны Go 1.27 и macOS: helper учётных данных использует macOS Keychain.

```bash
make check
mkdir -p .study_state/bin
go build -trimpath -buildvcs=false -o .study_state/bin/study-keychain ./cmd/study-keychain
go build -trimpath -buildvcs=false -o .study_state/bin/study-monitor ./cmd/study-monitor
cp config.example.toml study_data.local.toml
```

Отредактируйте локальную конфигурацию. Токены и пароли храните в Keychain. JSON клиента OAuth должен оставаться в `.study_state/credentials/`. Helper нельзя пересобирать при каждом запуске: его стабильная идентичность нужна для доступа к Keychain без окон подтверждения.

```bash
.study_state/bin/study-monitor auth gmail --client-json /absolute/path/to/oauth-client.json --config study_data.local.toml
.study_state/bin/study-monitor sync gmail --apply --config study_data.local.toml
.study_state/bin/study-monitor brief --config study_data.local.toml
```

Для Gmail используются узкие поисковые запросы и разрешение `gmail.readonly`. Приложение не отправляет и не удаляет письма через Gmail API. Для постоянного фонового доступа OAuth-приложение должно иметь статус **In production**: External/Testing ограничивает авторизацию и refresh token семью днями. После перехода получите новый токен и проверьте сбор из свежего процесса. См. [правила Google OAuth](https://developers.google.com/identity/protocols/oauth2#expiration).

## Адаптеры и границы

Telegram требует установленного [telegram-harvest](https://github.com/chupakobra6/telegram-harvest), явного имени и ID аккаунта, разрешённых чатов в обоих проектах. Личная идентичность не задана в коде по умолчанию. Для учебных платформ используется отдельный [hse-lms-harvest](https://github.com/chupakobra6/hse-lms-harvest). Эти внешние адаптеры и их личные настройки в репозиторий не входят. Полный сбор требует настройки всех нужных источников; для проверки одного подключения используйте адресный `sync <source>`.

Сбор и смысловой разбор выполняются отдельно. `brief` читает готовое состояние; `queue prepare` создаёт шаблон квитанции, `queue ack --receipt <path>` подтверждает только разобранные события. Требования преподавателя и сообщения студентов имеют разный статус. Сохранённый срок внутренней проверки не является сроком сдачи работы.

## Страницы приложения

[Приложение](https://chupakobra6.github.io/study-automation/) · [Конфиденциальность](https://chupakobra6.github.io/study-automation/privacy.html) · [Условия](https://chupakobra6.github.io/study-automation/terms.html).

GitHub Pages публикует `docs/` из ветки `main`. Эти страницы описывают доступ к данным и не содержат учебных материалов. По вопросам используйте [issues](https://github.com/chupakobra6/study-automation/issues); не прикладывайте письма, токены или персональные данные.

## Безопасная публикация

`.gitignore` разрешает только код Go в `cmd/` и `internal/`, Go module files, Makefile, этот README, шаблон конфигурации и страницы `docs/`. Остальные файлы игнорируются по умолчанию. Личный промпт, launchd-настройки и журналы остаются локальными. Перед push проверьте список подготовленных файлов и сканируйте секреты; игнорирование не защищает от явного `git add -f`.
