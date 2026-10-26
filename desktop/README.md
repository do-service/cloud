# Десктоп-клиент

Клиент синхронизации (фаза 2, [docs/desktop-client.md](../docs/desktop-client.md)). Окно (E14-1, E14-2): подключения, «Добавить облако» (вход в браузере), обзор облака с галочками и статусами; значок в строке меню с состояниями (E14-3).

```
cmd/cloud-desktop/   приложение Wails: окно + трей; движки всех подключений — в этом процессе
cmd/cloudsync/       CLI: `login [-dir] [-no-browser] <server>` (+ папка, маркер, state.db), `select`/`unselect`/`selection`,
                     `sync [-once|-watch] [-confirm-deletes] [-limit-up/-limit-down КиБ/с]`, `pause`/`resume`, `status`,
                     `problems`, `logout`, `accounts`, `move-folder <папка>`, `watch <dir>`
internal/engine/     движок: зеркало (tree + changes, 410 → ресинк), трёхсторонняя сверка (`MakePlan` — чистая функция),
                     исполнитель через журнал ops, выборочная синхронизация, защита от массового удаления
internal/api/        клиент сервера: /sync/*, узлы с If-Match, скачивание с Range/If-Range, tus с докачкой, ретраи
internal/signin/     вход в браузере (loopback + PKCE), токен в keyring (иначе файл 0600), список аккаунтов
internal/testserver/ настоящий сервер ../backend на одноразовой базе для интеграционных тестов
internal/app/        всё окно без Wails (тесты с CGO_ENABLED=0): Manager (движок на подключение, вход, удаление),
                     API окна /app/v1, прокси /api с токеном устройства, состояние и значки трея
internal/connection/ подключение вне движка, общее для CLI и приложения: Setup после входа, Open под блокировкой, Remove
internal/state/      реестр подключений (connections.json) и state.db подключения: SQLite (modernc), журнал ops
internal/localfs/    скан, file id, имена (golden-векторы с сервером), игнор, атомарная запись, маркер .cloudsync
internal/watch/      FSEvents (darwin && cgo), inotify (linux), на остальных — ErrUnsupported; Debounce → грязные пути
frontend/            Vite + React + Mantine, алиас @web → ../../frontend/src
```

## Запуск

```sh
cd desktop/frontend && npm ci && cd ..
make build                       # фронтенд → cmd/cloud-desktop/dist, bin/cloud-desktop, bin/cloudsync
./bin/cloud-desktop             # «Добавить облако» → адрес → вход в браузере
make dev                         # окно из Vite (:5174) с горячей перезагрузкой, /api — через прокси приложения
make lint test                   # gofmt, go vet (в т. ч. с CGO_ENABLED=0), tsc; go test с FSEvents
```

Тесты `internal/api` и `internal/signin` собирают `../backend/cmd/server` и запускают его на одноразовой базе `cloud_desktop_*` в postgres из compose (`docker compose up -d postgres`; другой — `TEST_POSTGRES_URL`, любая база на нём). Без postgres эти тесты пропускаются.

Окно:

- API окна — `/app/v1/*` (`internal/app/handler.go`) в том же `AssetOptions.Handler`, что и ассеты, обычным `fetch` (`frontend/src/appApi.ts`), а не генерируемые биндинги Wails: без `wails3 generate` и JS-рантайма беты, ошибки `{"code","message"}` как у сервера, тесты без cgo.
- `/api/*` проксируется на сервер **показанного** подключения (`PUT /app/v1/active`) с `Authorization: Bearer <токен устройства>`; токен в WebView не попадает, cookie и `Set-Cookie` вырезаются. Кэш запросов обзора — свой на подключение (ключи веб-кода не содержат сервер).
- Движок каждого подключения работает в процессе приложения под блокировкой `<state>/<id>/lock` (flock / LockFileEx, `state.LockConnection`); второй экземпляр приложения или `cloudsync sync/select/unselect/selection` на том же подключении получает отказ («открыто в другом процессе»), `pause`/`resume`/`status` работают — это флаг в `state.db`.
- Галочки и статусы — `engine.Marks`/`Usage` (`internal/engine/browse.go`) по зеркалу, правилам, base, `ops` и проблемам; `SelectNode`/`UnselectNode` вызываются параллельно с `Run` (после `Prepare`), `Wake` запускает проход сразу.
- «Доступно мне» в корне — строка с id `~shared` (как в движке); внутри — `/shared-with-me`.
- Папка: при добавлении — `~/Cloud/<хост>` или выбранная («Выбрать…», `Options.PickFolder`; для проверок `CLOUD_PICK_FOLDER=<путь>` отвечает за диалог). Непустая папка подключается с файлами: её верхние имена — в `adopted` (state.db), такое не загружается и не отмечается само (`planner.leftAlone`), а при отметке папки совпадающее по sha256 становится base без передачи, отличающееся — конфликтной копией; «Отметить папки, которые уже есть на диске» — `engine.Adoptable`. Маркер `.cloudsync` чужого подключения или другого аккаунта — отказ, ушедшего подключения того же аккаунта — заменяется.
- «Сменить папку…» (`connection.MoveFolder`, `cloudsync move-folder`): только в новую или пустую папку (кроме `.DS_Store`) на том же диске, одним `rename`; намерение сначала пишется в реестр (`movingTo`), прерванный перенос завершает или откатывает следующий `connection.Open`.
- Отключение облака: отзыв устройства на сервере (`force` — только локально, если сервер недоступен), запись реестра, каталог состояния и маркер `.cloudsync` удаляются, файлы остаются.

Для ручной проверки с временным `HOME`: `CLOUD_TOKEN_STORE=file` (токен в `tokens.json` 0600, не в Keychain — `security` пишет в связку ключей пользователя мимо `$HOME`), `CLOUD_SIGNIN_NO_BROWSER=1` (адрес входа — в stderr, открыть другим браузером, например agent-browser). Снимок WKWebView скрытого окна (экран выключен, нет «Записи экрана») показывает устаревшую картинку: CSS-переходы и таймеры в скрытом окне стоят — перед снимком отключать `transition`.

## Сборка для macOS

```sh
cd desktop/frontend && npm ci && cd ..
make app                         # bin/Облако.app: universal (arm64 + x86_64), внутри и cloudsync
make dmg                         # bin/Cloud-<версия>-macos-universal.dmg (+ .sha256): приложение и ссылка на /Applications
make dmg VERSION=1.2.3           # версия явно; по умолчанию — из последнего тега v* (git describe)
make icon                        # перерисовать build/darwin/icons.icns (build/darwin/icon.go → sips → iconutil)
```

- Две сборки с cgo (`GOARCH=arm64`, `amd64`) склеиваются `lipo`; macOS 13+ (`MACOSX_DEPLOYMENT_TARGET`, `LSMinimumSystemVersion`). Версия — в `Info.plist` (`CFBundleShortVersionString` — только `x.y.z`) и в `api.Version` (`-ldflags -X`), её видит сервер в списке устройств.
- `Info.plist` — `build/darwin/Info.plist` (`com.do-service.cloud`, «Облако», `LSUIElement` — без значка в Dock). Значок — облако из трея на синем фоне, нарисован `icon.go`: логотипа в репозитории нет.
- `cloudsync` — `Облако.app/Contents/MacOS/cloudsync`, можно запускать оттуда или сделать ссылку в `PATH`.
- CI: `.github/workflows/desktop.yml`, job `macos` на `macos-15` — DMG артефактом `desktop-macos` на каждый push в `main`, PR, ручной запуск (`gh workflow run desktop.yml --ref <ветка>`) и тег `v*`; на тег — ещё и в релиз GitHub.

Подписи Developer ID и нотаризации пока нет, только ad-hoc (`codesign -s -`, без неё arm64 не запускает бинарник). Gatekeeper поэтому не даёт открыть скачанное приложение двойным щелчком: после копирования в «Программы» — правый щелчок → «Открыть» → «Открыть» (на macOS 15+ — «Системные настройки» → «Конфиденциальность и безопасность» → «Всё равно открыть») или снять карантин:

```sh
xattr -dr com.apple.quarantine "/Applications/Облако.app"
```

## Wails

Закреплён **`github.com/wailsapp/wails/v3 v3.0.0-beta.34`** (тег от 2026-10-20, последний на 2026-10-25). Причины:

- трей есть только в v3 (в v2 его нет), а v3 пока только в бете;
- беты выходят почти каждый день (beta.31–34 за три дня) и ломают API без предупреждения, поэтому версия — точная, обновлять осознанно, с ручной проверкой окна и трея на macOS.

CLI `wails3` не нужен: сборка — обычный `go build` (`-tags production` — ассеты только из бинарника, без dev-сервера), `.app` и DMG — `make app`/`make dmg` (lipo, codesign, hdiutil), по образцу его Taskfile для darwin. Если понадобится для установщиков Windows/Linux (E15-2, E15-3) — ставить той же версией: `go install github.com/wailsapp/wails/v3/cmd/wails3@v3.0.0-beta.34`.

Что выяснилось в спайке:

- `application.InvokeAsync` до `app.Run()` падает с nil pointer (платформенное приложение ещё не создано) — всё, что трогает меню из горутин, запускать после `events.Common.ApplicationStarted`.
- Wails просит в cgo `-mmacosx-version-min=10.13`, Go 1.27 и SDK — 13+; без `MACOSX_DEPLOYMENT_TARGET`/`CGO_*FLAGS=13.0` (они в Makefile) линковщик сыплет предупреждениями.
- На Linux `pkg/application` без cgo не компилируется, поэтому CI (`desktop:lint`, `desktop:test`) проверяет только пакеты без Wails — список строится по `go list -deps`, `internal/app` и ниже должны оставаться без импорта Wails.
- Без тега `production` `AssetFileServerFS` берёт ассеты с `FRONTEND_DEVSERVER_URL`, если она задана (так работает `make dev`).
- Dev-прокси Wails ходит на `localhost` по IPv4, а Vite на `localhost` в macOS слушает только `::1` — окно пустое без ошибок; поэтому Vite на `127.0.0.1`.
- `screencapture` без разрешения «Запись экрана» не снимает окно; для проверок проще снимок самого WKWebView (`takeSnapshotWithConfiguration`), а трей — через System Events (Accessibility).

## Движок и `cloudsync`

```sh
cloudsync login http://localhost:8090        # браузер → «Разрешить»; папка ~/Cloud/localhost-8090
cloudsync select /Работа                     # при первом запуске не отмечено ничего
cloudsync sync -watch                        # или -once; status, problems — что происходит
```

Скорость и устойчивость (E13-10):

- Долговечность локальных изменений — пачками (`localfs.Syncer`, групповой коммит, инвариант — в `localfs/durable.go`): файл — `fsync` и общий на пачку `F_BARRIERFSYNC` перед переименованием (полузаписанный файл не заменит целый), папки — один `F_FULLFSYNC` на устройство перед записью результата в `state.db` (отмеченное сделанным есть на диске и после отключения питания). На Linux — `fsync` файла и папки, на Windows — как было. Писатели `state.db` одного процесса стоят в очереди в Go, а не в busy-обработчике SQLite. 1000 мелких файлов с testserver: скачивание ≈14 с → ≈4 с; загрузка ≈14–15 с упирается в сервер (`fsync` на macOS у сервера, запущенного не в Docker), сам API без движка — ≈12,6 мс на файл.
- Большие передачи (от `LargeSize`) идут в фоне, через проходы: мелкие и новые изменения не ждут; операции на путях выше/ниже идущей передачи откладываются до её конца.
- `Pause`/`Resume` (флаг в `state.db`, `cloudsync pause`/`resume`, работающий `sync -watch` подхватывает за секунду): передачи обрываются, их операции снова в очереди, докачка — с места.
- Офлайн: бэкофф 1 → 30 с с джиттером, между попытками — один `Ping` (`/sync/info`) вместо прохода, локальное изменение будит сразу; `Status().Offline`, проблема `offline` («нет сети» в `cloudsync status`).
- Простаивая, движок ждёт изменений сервера long-poll'ом (`wait`=55 с, не больше `maxWait` из `/sync/info`); локальное изменение его обрывает. Сервер, отвечающий сразу, спрашивается не чаще `PollInterval` (5 с).
- 423 — повтор каждые `LockBackoff`, проблема через 30 мин; 507/квота — проблема и пауза 10 мин без повторов.

Общие папки (E13-9, `internal/engine/shared.go`):

- То, чем поделились другие (`/sync/roots`), лежит в `Доступно мне/<имя>` в корне папки; имена — как в `/sync/roots` и WebDAV («Проект (Иван)», если имена повторяются). Своя папка с таким же именем в корне облака сдвигает виртуальную в «Доступно мне (2)». В зеркале «Доступно мне» — узел `~shared` только для чтения: создать файл прямо в нём — проблема `readonly`, не загрузка.
- Выбор: `cloudsync select "/Доступно мне/Проект"` (или `/Доступно мне` — все, и будущие). Отметка `/` общие папки не включает, новая общая папка не отмечена; `selection` и `status` показывают общие папки, их права и отметку.
- `rootsChanged` → список корней и деревья всех общих папок заново (права могли смениться где угодно). Курсор с курсорами владельцев клиент только передаёт.
- Только чтение: правка — конфликтная копия рядом (не загружается, проблема `readonly`), серверная версия возвращается; удаление и переименование отменяются (файл снова скачивается или переименовывается обратно); новый файл — проблема `readonly`. На запись — как свои папки; квота — владельца (507 → проблема `quota`).
- Между деревьями (своё ↔ общая папка, общая ↔ общая) сервер не перемещает (409 `cross_owner_move`): перенос — загрузка в новом месте и удаление в старом (из папки только для чтения — копия); в папку без записи — перенос отменяется. Удалить или переименовать саму общую папку или «Доступно мне» нельзя — вернутся; убрать с диска — снять отметку.
- Отзыв доступа, удаление владельцем: синхронизированное убирается с диска, а несохранённые правки и новые файлы переезжают в корень папки под именем конфликтной копии и загружаются в своё облако (проблема `rescued`). Права «запись → чтение»: правки — конфликтные копии, как выше.
- Файлы в папках только для чтения на диске не помечаются read-only (атрибут мешал бы удалению и переименованию на Windows и требовал бы смены при повышении прав).

План (`internal/engine/plan*.go`) — чистая функция от base/local/remote: позиция и содержимое решаются отдельно, за проход у узла не больше одной операции, зависящее от результата других — на следующем проходе. Тесты: клетки таблицы сверки (`plan_test.go`), движок против фейкового сервера с крашами на каждом виде операции (`engine_test.go`), stateful property-тест (`property_test.go`: `go test ./internal/engine -run SyncProperty -rapid.checks=1000 -rapid.seed=N`), интеграция с настоящим сервером, включая `kill -9` посреди передачи 200 МБ и посреди пачки мелких скачиваний (`integration_test.go`, `kill_test.go`; 10 000 файлов — `ENGINE_BIG=1`); пауза, офлайн, long-poll, 423/507, фоновые передачи — `control_test.go`; общие папки — `shared_test.go` (фейковый сервер с владельцами, правами и квотами), `integration_shared_test.go`, второй владелец в property-тесте. В CI (`desktop:test`) интеграционные тесты идут на сервисе postgres.

## Состояние

Каталог состояния — `state.BaseDir()`: `~/Library/Application Support/Cloud`, `%LOCALAPPDATA%\Cloud`, `$XDG_STATE_HOME/cloud`. В нём `connections.json` (подключения: id, адрес, пользователь, папка; папки не вкладываются) и `<id>/state.db` каждого подключения. Схема — `internal/state/migrations/` (свой раннер на `PRAGMA user_version`, только вверх; база от более новой версии клиента не открывается). WAL, `synchronous=FULL`, запись — `BEGIN IMMEDIATE`. Журнал: `db.Intend(op)` → действие → `db.Complete(id, …)` (база и удаление op одной транзакцией); после `kill -9` база целая (тест `TestKillMidTransaction`).

## Имена

`localfs.NormalizeName` — копия серверной `fs.NormalizeName`; обе сверяются с `backend/internal/fs/testdata/name_vectors.json`. Новый случай: добавить `note` и `in` в файл, `cd backend && go test ./internal/fs -run NameVectors -update`, проверить diff; тест клиента подхватит его сам.

## Вотчер

На Windows вотчера пока нет (`ErrUnsupported`, движок живёт ресканами): fsnotify открывает дескриптор на каждую папку, а открытый дескриптор подпапки не даёт переименовать родителя; нужен один рекурсивный ReadDirectoryChangesW на корень. inotify (Linux) — через fsnotify, по наблюдению на каждую папку; переполнение очереди → `MustRescan` корня, нехватка `fs.inotify.max_user_watches` при старте → ошибка `New`.

## FSEvents

Своя cgo-обёртка (~250 строк), не `github.com/fsnotify/fsevents`: у того последний релиз v0.2.0 от мая 2024, и он не отдаёт file id. Поток с `FileEvents | WatchRoot | UseExtendedData` на dispatch-очереди: пути, id событий, inode, старт с сохранённого id (`Options.Since`) с маркером `HistoryDone`. События — подсказки: ОС склеивает их в окне задержки (создан и удалён — одно событие с обоими флагами), переименование приходит двумя событиями с одним inode, в истории порядок id не строго возрастает; при `MustRescan` — пересканировать.
