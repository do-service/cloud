# Архитектура: Self-hosted файловое облако — черновик v0.1

**Дата:** 2026-10-22 · **Основа:** [prd.md](prd.md) v0.2

Решения, на которые опирается документ: бэкенд на Go, PostgreSQL для метаданных, файлы хранятся как есть (Q4), синхронизация через WebDAV, редакторы через WOPI, лицензия Apache 2.0, интерфейс только на русском с задатком на i18n (Q10).

**Соглашения из проекта chat (приняты при старте фазы 0):** монорепо `backend/` + `frontend/`; модуль `github.com/do-service/cloud/backend`; stdlib `net/http`; pgx + goose (миграции встроены через `embed`); **SQL пишется руками в `repo.go` пакета, без sqlc**; конфиг только из env; фронтенд — React 19 + Mantine + TanStack Query + react-router; GitLab CI с `gofmt`, `go vet`, `tsc -b`. Сборка фронтенда копируется в `backend/internal/webui/dist` и встраивается в бинарник.

> Библиотеки ниже — **кандидаты**. Перед фазой 0 проверить актуальные версии, лицензию (совместимость с Apache 2.0) и активность поддержки.

---

## 1. Общая схема

```
                ┌────────────── Браузер (SPA) ──────────────┐
                │  файлы · шаринг · админка · iframe редактора │
                └───────┬───────────────┬──────────────┬──────┘
                        │ REST /api/v1   │ tus /uploads │ /s/{token}
WebDAV-клиенты ──/dav──►│                │              │
                ┌───────▼────────────────▼──────────────▼──────┐
                │                 cloud (Go, один бинарник)      │
                │  http ─► auth ─► access ─► fs-service ─► storage│
                │                    ▲          │                 │
                │                    │          └─► jobs (очередь)│
                │  /wopi ◄───────────┼──────────────────────────┐ │
                └────────────────────┼──────────────────────────┼─┘
                          ┌──────────┴───┐   ┌──────────────┐   │
                          │  PostgreSQL  │   │ ФС или S3    │   │
                          └──────────────┘   └──────────────┘   │
                ┌────────────────────────────────────┐          │
                │ Collabora / OnlyOffice (WOPI-клиент)├──────────┘
                └────────────────────────────────────┘
```

Один процесс обслуживает всё: API, WebDAV, WOPI, публичные ссылки, фоновые задачи. Флаги `--role=api|worker|all` позволяют позже разнести воркеры. **Состояние в памяти процесса не хранится** (сессии, блокировки, очередь — в Postgres). Так задел на горизонтальное масштабирование (P2) появляется без переписывания.

## 2. Модули (Go-пакеты)

| Пакет | Ответственность | Зависит от |
|-------|-----------------|------------|
| `cmd/cloud` | Точка входа, конфиг, сборка зависимостей, миграции при старте | всё |
| `internal/config` | env + YAML, валидация при старте | — |
| `internal/db` | pgx-пул, транзакции, сгенерированные sqlc-запросы, миграции | — |
| `internal/auth` | Сессии, локальные пароли (Argon2id), OIDC-провайдеры, app passwords, токены WOPI и ссылок | db |
| `internal/access` | **Единственная** точка проверки прав: `Can(principal, node, action)` | db |
| `internal/fs` | Доменная логика дерева: create/move/copy/delete/restore, версии, корзина, квоты, ETag | db, storage, access |
| `internal/storage` | Интерфейс `Backend` + реализации `localfs` и `s3` | — |
| `internal/upload` | tus-обработчик, сборка, проверка хэша, финализация через `fs` | fs, storage |
| `internal/webdav` | WebDAV-сервер поверх `fs` | fs, auth |
| `internal/wopi` | WOPI-хост, discovery редакторов, блокировки | fs, auth |
| `internal/share` | Шаринг пользователям и группам, публичные ссылки | db, access, fs |
| `internal/preview` | Генерация превью (воркер) | storage, jobs |
| `internal/jobs` | Очередь фоновых задач в Postgres | db |
| `internal/api` | REST-хендлеры, DTO, ошибки | все сервисы |
| `internal/audit` | Журнал действий | db |
| `internal/sync` | API sync-клиентов (`/api/v1/sync/*`): корни, дерево, изменения из журнала | fs, share |
| `web/` | SPA, встраивается в бинарник через `embed.FS` | — |

**Правило:** хендлеры (`api`, `webdav`, `wopi`, публичные ссылки) не ходят в `db` и `storage` напрямую, только через `fs` и `share`. Проверка прав происходит внутри сервисов, поэтому новый протокол не может её «забыть». Это ответ на главный security-риск из PRD.

**Журнал изменений (E11-1):** каждый путь записи в `nodes` внутри `fs` (создание, замена содержимого, перемещение и переименование, корзина, восстановление) вызывает `journal()` из `fs/repo.go` последним запросом своей транзакции — строка «узел X изменился в seq N» в `sync_journal` для sync-клиентов (`docs/desktop-client.md`). Поддерево перемещённой или удалённой папки не журналируется — только она сама; purge тоже. Seq берётся из строки `sync_heads` владельца, её блокировка держится до commit, поэтому seq монотонен в порядке commit. Новая запись в `nodes` без `journal()` — ошибка: её ловит тест «воспроизведение» (`fs/journal_test.go`).

**API синхронизации (E11-2):** `internal/sync` только переводит HTTP в `fs` (`SyncView`, `SyncTree`, `Journal`, `Get`) и `share` (`SharedWithMe`, `Entries` — имена общих корней, общие с WebDAV). Курсор — base64 JSON `{v, own, a, s: {владелец: seq}, r}`, где `r` — отпечаток долей, выданных пользователю (узел и права): `rootsChanged` поднимается, если сдвинулся `access_seq` или отпечаток. Журнал читается до головы, прочитанной заранее (всё до неё уже закоммичено), поэтому курсор не перепрыгивает поздние строки. Чужие деревья: строки с `scopes && <узлы, расшаренные пользователю>` (`access.Granted`, включая удалённые в корзину), затем текущая видимость через `fs.Get` — невидимое теперь становится tombstone. Изменения дедуплицируются по узлу; живые идут раньше tombstone, папка — раньше вложенного; `subtree: true` — у восстановленной папки и у перемещённой папки в чужом дереве (могла въехать в видимую область). `parentId` — `null` у корня и у расшаренного узла, чья папка пользователю не видна.

**Видимость (E11-5):** `fs.TouchAccess` поднимает `access_seq` получателей последним запросом той же транзакции, что меняет доступ: `share.Grant/Update/Revoke` (получатель — пользователь или участники группы), `SetMember` (если строка реально добавлена или удалена), `DeleteGroup` (участники, прочитанные до каскада), purge (`fs/trash.go`: получатели долей в удаляемом поддереве — сюда же попадает удаление аккаунта владельца через `DeleteOwner`). Строки `sync_heads` блокируются по возрастанию id и последними, как в `journal()`. Ссылки (`links`) пользователей не касаются и `access_seq` не трогают.

**Long-poll (E11-4):** `?wait=N` (0–55 с). Если изменений нет, запрос регистрируется в `sync.Hub` и ждёт на канале, не держа соединения с БД; цикл хаба раз в секунду читает `sync_heads` пользователей, которых ждут (`fs.SyncHeads`), и будит тех, у кого `tree_seq` своего или чужого (из курсора) дерева или свой `access_seq` ушёл дальше известного. Проснувшийся перечитывает журнал; изменения не по его `scopes` — снова ждёт. Пробуждение с джиттером `min(2 с, 5 мс × число разбуженных)`: одиночное изменение — сразу, сотни ожидающих расходятся на 2 с. Таблица — единственный сигнал, поэтому работает на нескольких API-нодах. При остановке (контекст сервера отменён) ожидающие сразу получают пустой ответ с курсором. Метрика `cloud_sync_waiters`.

**Хранение журнала (E11-6):** задача `sync_journal_retention` удаляет строки старше `SYNC_JOURNAL_RETENTION` (720h, `0` — хранить всегда) пачками по 500 и поднимает `sync_heads.min_seq` до последнего удалённого seq — курсор старее получает 410 `cursor_expired`. `/sync/info` отдаёт этот срок в `journalRetention`.

### Кандидаты на библиотеки
| Задача | Кандидат |
|--------|----------|
| HTTP-роутинг | стандартный `net/http` (паттерны Go 1.22+) или `chi` |
| Postgres | `jackc/pgx/v5` + `sqlc` (типобезопасные запросы без ORM) |
| Миграции | `pressly/goose` |
| Очередь задач | `riverqueue/river` (поверх Postgres) |
| OIDC | `coreos/go-oidc/v3` + `golang.org/x/oauth2` |
| tus | `tus/tusd/v2` как библиотека (готовые клиенты: tus-js-client, Uppy) |
| WebDAV | `golang.org/x/net/webdav` как основа; хендлер форкаем в `internal/webdav` ради кастомных свойств (ETag папок, file id), LOCK в БД и N+1 в PROPFIND — см. [webdav-spike.md](webdav-spike.md) |
| S3 | `aws-sdk-go-v2` или `minio-go` |
| Превью | утилита `vips` (libvips 8.16 с libheif и poppler) в дочернем процессе, без cgo — см. §10 |
| Метрики | `prometheus/client_golang` |
| Логи | `log/slog` |

## 3. Модель данных (PostgreSQL)

```sql
users          (id uuid pk, username citext unique, email citext, display_name,
                password_hash null, is_admin bool, status, quota_bytes null,
                used_bytes bigint, created_at)
identities     (id, user_id fk, provider_id fk, subject text, email, last_login_at, unique(provider_id, subject))
auth_providers (id, slug unique, type 'oidc', issuer, client_id, client_secret_enc,
                scopes text[], enabled, auto_create bool, link_by_email bool,
                from_env bool)   -- строки из OIDC_* перезаписываются при старте
sessions       (id_hash pk, user_id, created_at, expires_at, ip, user_agent)
app_passwords  (id, user_id, name, token_hash unique, last_used_at, created_at)
devices        (id, user_id fk cascade, name, platform macos|windows|linux, client_version,
                token_hash unique, created_at, last_used_at, last_ip)  -- E12, программа синхронизации
device_codes   (code_hash pk, user_id fk cascade, name, platform, client_version,
                code_challenge, redirect_uri, expires_at)  -- E12-2, одноразовый код входа на 120 с
groups         (id, name unique);  group_members (group_id, user_id)

nodes (
  id          uuid pk,
  owner_id    uuid fk users,      -- владелец дерева, квота считается ему
  parent_id   uuid fk nodes null, -- null = корень пользователя
  name        text not null,
  name_key    text collate name_order not null, -- нормализованное имя (NFC + lower) для уникальности и сортировки
  kind        'file' | 'dir',
  size        bigint,             -- для dir: суммарный размер поддерева
  mime        text,
  blob_id     uuid null,          -- текущее содержимое (для file)
  content_hash bytea,             -- SHA-256 текущей версии
  etag        text not null,      -- меняется при изменении; у dir — при изменении любого потомка
  mtime       timestamptz,
  modified_by uuid null,          -- кто записал текущее содержимое; при замене уходит в versions.author_id (null — неизвестно или ссылка)
  trashed_at  timestamptz null,
  trash_root  bool default false, -- узел, который удалили явно (видим в корзине)
  created_at, updated_at
)
unique (parent_id, name_key) where trashed_at is null
index (parent_id, kind, name_key, id)           -- листинг: папки сначала, курсор по имени
index (parent_id, kind, mtime, id), (parent_id, kind, size, id)  -- папки первыми при любой сортировке
gin   (owner_id, name_key gin_trgm_ops)           -- R5 поиск по подстроке в дереве пользователя (pg_trgm + btree_gin)

blobs    (id uuid pk, backend, storage_key text, size, hash, created_at, refcount int)
versions (id, node_id fk, blob_id fk, size, hash, author_id, created_at, replaced_at, label null)  -- created_at = mtime содержимого (может прислать клиент), хранение VERSIONS_* считается от replaced_at

shares (id, node_id fk, grantee_type 'user'|'group', grantee_id,
        perms smallint,            -- битовая маска read|write|reshare|delete
        created_by, created_at, unique(node_id, grantee_type, grantee_id))
links  (id, node_id, token_hash unique, mode 'view'|'download'|'upload',
        password_hash null, expires_at null, created_by, created_at, download_count)

locks  (node_id pk, kind 'wopi'|'webdav', token unique, owner_id, deep, owner_info, expires_at)
uploads(id pk, user_id, target_parent_id, name, size, offset, backend_upload_id,
        status, created_at, expires_at)
audit_log (id bigserial, at, actor_id null, ip, action, node_id null, details jsonb)

sync_heads   (user_id pk fk users, tree_seq, min_seq, access_seq)  -- голова журнала дерева пользователя
sync_journal (owner_id fk users, seq, node_id,  -- без FK: строка переживает purge
              op smallint,                      -- подсказка: create|content|move|trash|restore
              scopes uuid[] null,               -- расшаренные узлы на старой и новой цепочке
              device_id null, at, pk(owner_id, seq))
index (at)                                      -- хранение по возрасту
```

### Почему так
- **Adjacency list (`parent_id`)**, а не materialized path. Перемещение папки — это `UPDATE` одной строки, без переписывания путей у 100 000 потомков (R1: < 1 с). Предков для проверки прав и ETag получаем рекурсивным CTE; глубина реальных деревьев — десятки уровней, это дёшево.
- **Collation `name_order` (ICU `und-u-kn-true`)** у `name_key`: «файл2» идёт раньше «файл10», кириллица и «ё» стоят по алфавиту, порядок одинаков на любом хосте. libc-collation по умолчанию в musl (образ alpine) сравнивает байты, а в glibc — нет, и обновление glibc может испортить индекс. При мажорном обновлении ICU Postgres предупредит о версии collation — нужен `REINDEX`.
- **Курсорная пагинация** по `(kind, ключ сортировки, id)`: стабильна при вставках и не деградирует на 50 000 элементах, в отличие от OFFSET (G2). Папки идут первыми при любой сортировке и направлении: страница читается двумя диапазонами индекса — сначала папки после курсора, затем файлы, — без сортировки в памяти.
- **`blobs` отдельно от `nodes`.** Версии, корзина и копирование ссылаются на blob, не копируя байты, где это позволяет бэкенд (§4).
- **ETag папок** обновляется одним запросом: рекурсивный CTE по предкам → `UPDATE ... SET etag = new_etag()`. Число строк равно глубине, а не размеру дерева. Это та же стратегия, что у sync-клиентов Nextcloud и ownCloud: клиент спускается только в поддеревья с изменённым ETag.
- **`size` у папок** обновляется так же — по цепочке предков в той же транзакции. Это бесплатно даёт размер папки в UI и `quota-used-bytes` в WebDAV.
- **Блокировка цепочки предков** (E1-4): каждая запись блокирует `FOR UPDATE` свой узел и всех предков (при move — обе цепочки) в порядке `id`, поэтому дедлоков между операциями нет, а дельта `size` всегда попадает на актуальных предков. Цена — записи одного пользователя сериализуются на его корне (риск A3). Порядок записи: метаданные в транзакции → операция в storage последней → commit; при ошибке commit операция в storage откатывается.

## 4. Хранение содержимого («файлы как есть»)

Интерфейс (`internal/storage`, реализован в E1-2; контракт — `storagetest.Run`, общий для всех бэкендов):

```go
type Backend interface {
    Layout() Layout                     // Tree (localfs: текущая версия по пути) | Flat (S3: blobs/{blob_id})
    Put(ctx, key, r io.Reader, size int64) (PutResult{Size, SHA256}, error) // атомарно, хэш на лету
    Get(ctx, key, rng *Range) (io.ReadCloser, error)
    Stat(ctx, key) (Info, error)
    Mkdir(ctx, key) error               // S3: no-op
    Move(ctx, from, to) error           // ФС: rename(2) файла или папки; не перезаписывает
    Copy(ctx, from, to) error           // только файл; ФС: copy_file_range (reflink — E1-6)
    Delete(ctx, key) error              // файл или поддерево; отсутствующий ключ — не ошибка
}
```

Мультипарт для tus (`NewUpload`/`WritePart`/`Complete`/`Abort`) добавим в E3-2 вместе с tusd: у него свой DataStore, стык с ним определит форму интерфейса.

### Локальная ФС: человекочитаемое дерево
```
data/
  files/{user_id}/Документы/Отчёт.docx      ← текущие версии, повторяют дерево
  versions/{node_id}/{version_id}           ← прошлые версии
  trash/{node_id}/...                       ← удалённое, по id корня удаления
  uploads/{upload_id}                       ← незавершённые загрузки
  previews/{blob_id}/{size}.webp
```
- `blobs.storage_key` для текущей версии — это путь в `files/`. **Он меняется при move и rename**, поэтому fs-сервис в одной операции делает `rename(2)` на диске и `UPDATE` ключей поддерева. Для папки достаточно одного `rename(2)` каталога, но ключи потомков в БД нужно переписать. Чтобы не трогать 100 000 строк, **ключ blob текущей версии на ФС не хранится, а вычисляется** из пути узла (корень пользователя + цепочка имён). В `blobs.storage_key` пишется только ключ для версий, корзины и S3.
- **Новая версия:** загрузка пишется в `uploads/`, затем старое содержимое переносится `rename` в `versions/{node_id}/…`, а новое — `rename` на место. Оба шага атомарны и O(1) на одной ФС; `data/` обязан быть одной файловой системой (проверка при старте).
- **Корзина:** `rename` поддерева в `trash/{node_id}`, в БД ставится `trashed_at`. Восстановление — обратный `rename`.
- **Копирование:** `copy_file_range` / reflink на Btrfs и XFS (почти бесплатно), иначе обычная копия. Копия папки — фоновая задача с прогрессом, если в ней больше N файлов.
- **Имена:** ограничения ФС (255 байт, запрещённые символы) валидируются на уровне `fs`. Регистр на Linux значим, но уникальность в БД — по `name_key` (без учёта регистра), чтобы WebDAV-клиенты на macOS и Windows не сломались.
- **Ограничение v1:** правки файлов напрямую на диске в обход приложения не поддерживаются. Метаданные разойдутся, исправит команда `scan` (P1, Q11).

### S3: ключи по id
- Ключ: `blobs/{blob_id}`. Перемещение и переименование меняют только метаданные (rename в S3 — это copy+delete, с путями-ключами перемещение было бы O(N)).
- «Как есть» означает: объект — это файл целиком, без разбиения на блоки. Его можно скачать любым S3-клиентом, а имя восстанавливается из БД. В метаданных объекта храним `x-amz-meta-path` на момент записи — так можно восстановить данные даже без БД.
- Версия — это ещё один объект; корзина — только флаг в БД.

## 5. Загрузка (R2)

1. Клиент (tus-js-client или Uppy) делает `POST /api/v1/uploads` с `Upload-Length` и метаданными: имя, родитель, ожидаемый хэш, если есть.
2. Сервер проверяет права на запись в родителя и **резервирует квоту** (`used_bytes + reserved ≤ quota`), создаёт строку `uploads`.
3. `PATCH`-запросы дописывают данные; смещение хранится в БД, поэтому докачка переживает перезапуск сервера.
4. Параллельная загрузка: расширение tus `concatenation` — клиент грузит части как отдельные partial uploads, сервер склеивает. На S3 части идут напрямую в Multipart Upload, без промежуточного диска.
5. Финализация: проверка хэша → транзакция в `fs` (создать узел или новую версию, обновить ETag и size предков, снять резерв квоты) → задача на превью.
6. Брошенные загрузки удаляет периодическая задача.

Реализация (E3-2): tusd v2 как библиотека с `filestore` в `data/uploads/tus` (та же ФС, что `files/`, поэтому готовый файл встаёт на место одним `rename`), пакет `internal/upload` — только связка с tusd, решения принимает `fs` (`ReserveUpload`, `FinishUpload`). Резерв — строка `uploads` до финализации, срок 7 дней; квота проверяется под блокировкой строки пользователя. sha256 считается на лету, пока PATCH пишет данные: обёртка над `filestore` (`upload/hashstore.go`) держит состояние хэша в файле `{id}.sha256` рядом с данными (смещение + состояние `sha256`, запись через временный файл и `rename`), и финализация (`fs.FinishUploadHashed`) проверяет только размер. Если состояние не совпадает со смещением данных (сбой между записью чанка и сохранением, файл пропал, записан до перезагрузки машины), хэш при финализации считается повторным чтением, как раньше (~1,6 ГБ/с). Загрузки без `Upload-Length` отклоняются: квоту не на что резервировать.

**Производительность (G1):** без буферизации всего файла в памяти; `io.CopyBuffer` с буфером 1–4 МБ; на Linux `sendfile` и `splice` при раздаче. За reverse proxy выключить буферизацию запросов (`proxy_request_buffering off` в nginx) — это попадёт в документацию.

## 6. Права доступа (R9, R10)

```go
type Principal struct { UserID uuid.UUID; Groups []uuid.UUID; Link *LinkGrant; AppPassword bool }
func (s *Access) Can(ctx, p Principal, nodeID uuid.UUID, a Action) (bool, error)
```
- Владелец дерева — все права.
- Иначе одним рекурсивным CTE от узла вверх собираем `shares` по пути для пользователя и его групп и берём объединение прав. Права наследуются вниз.
- Ссылка ограничена своим поддеревом и режимом.
- «Доступно мне» — это список `shares` на пользователя и его группы; получатель видит расшаренный узел как виртуальный корень. В WebDAV — виртуальная папка `/dav/files/{user}/Доступно мне/…` (E7-4).
- Кэш результатов `Can` на время запроса; межзапросного кэша в v1 нет, поэтому отзыв доступа действует немедленно.
- Тестовая матрица: владелец, user-share, group-share, вложенные шары, ссылка, отзыв — по каждому протоколу (API, WebDAV, WOPI, ссылка). **Сделано в E6-5:** `TestSecurityMatrix` (`backend/cmd/server/security_test.go`) собирает сервер той же функцией `newApp`, что и `main`, и проверяет ~350 клеток «кто × протокол × действие» с точным статусом; таблица — [security-matrix.md](security-matrix.md), генерируется флагом `-update` и сверяется с поведением.

Реализация (E6-1…E6-3): права — маска `read|write|reshare` (`access.Perm`); `write` покрывает создание, переименование, перемещение, удаление, загрузку и восстановление версий; `reshare` — делиться дальше не шире своих прав, менять и отзывать только не более широкие шары. Владелец проверяется сравнением `owner_id` без запроса. Без `read` — 404, с `read` без нужного права — 403 `forbidden`. Переименовать, переместить или удалить узел — это изменение его папки, поэтому получатель не может так поступить с самой общей папкой. Перемещение между деревьями разных владельцев запрещено (`cross_owner_move`), загруженное получателем занимает квоту владельца, удалённое им попадает в корзину владельца. Поиск — пока только по своему дереву. «Доступно мне» показывает верхние общие узлы; хлебные крошки получателя начинаются с общей папки. API: `GET/POST /nodes/{id}/shares`, `PATCH/DELETE /shares/{id}` (получатель может удалить свой шар — «выйти»), `GET /shared-with-me`, `GET /directory?q=`, `/admin/groups` (только админ).

## 7. Аутентификация (R13, R14)

| Канал | Механизм |
|-------|----------|
| Веб-UI | Cookie-сессия (`__Host-session`, `HttpOnly`, `Secure`, `SameSite=Lax`), в БД хранится SHA-256 токена; срок 30 дней со скольжением. CSRF — `http.CrossOriginProtection` (Sec-Fetch-Site / Origin) на всём публичном хендлере, без токенов: так же сделано в chat, а WebDAV и tus (без Origin) проходят |
| Локальный вход | Argon2id, rate limit по IP и логину |
| OIDC | Начатый не на хосте `PUBLIC_BASE_URL` вход (например, `127.0.0.1` вместо `localhost`) сначала перенаправляется туда (`?canonical=1`, один раз): провайдер возвращает браузер на `PUBLIC_BASE_URL`, и cookie потока должна стоять там же |
| OIDC | Authorization Code + PKCE (S256), `state` и `nonce` в короткоживущей cookie флоу; N провайдеров в `auth_providers` (из `OIDC_*` env при старте, позже — из админки), `client_secret` зашифрован AES-256-GCM ключом `SECRET_KEY`; discovery ленивый, недоступный IdP не мешает старту. JIT-создание (`auto_create`, логин из `preferred_username` или email, при коллизии `-2`…); связь по email выключена по умолчанию, включается `link_by_email` и срабатывает только при `email_verified=true` (Q7). В `users.email` пишется только подтверждённый email |
| WebDAV | Basic auth с **паролем приложения** (у OIDC-пользователей пароля нет): 25 символов base32 (~125 бит), генерирует сервер и показывает один раз; в БД — SHA-256 (не Argon2: клиент шлёт пароль с каждым запросом, а перебирать 125 бит бессмысленно), `last_used_at` — не чаще раза в час. Rate limit — только на неудачные попытки по IP. Обычный пароль аккаунта через Basic не принимается |
| Программа синхронизации (E12) | `Authorization: Bearer cld_…` — токен устройства (`devices`, `auth.Devices`): 32 случайных байта, в БД SHA-256, `last_used_at`/`last_ip` — не чаще раза в час, до 20 устройств на пользователя (`device_limit`). Принимает тот же `auth.Middleware`, что и cookie (Bearer важнее cookie; неверный токен — аноним, т.е. 401), и проверяет `users.status` на каждом запросе. Область ограничена в одном месте (`deviceMayUse`): `/api/v1/admin/*`, `/api/v1/me/app-passwords*`, `/api/v1/me/devices*` — 403 `device_forbidden`, кроме `DELETE /api/v1/me/devices/current` (выход клиента, отзывает свой токен). `GET /me/devices` и `DELETE /me/devices/{id}` — только из веба (`/settings` → «Устройства»); администратор — `GET /admin/users/{id}/devices` и `DELETE /admin/users/{id}/devices/{device}` (диалог пользователя в `/admin`, аудит `device.revoke` с владельцем в подробностях). Id устройства уходит в `fs.WithDevice` → `sync_journal.device_id`. CSRF не нужен: Bearer не прикладывается браузером сам, а `CrossOriginProtection` пропускает запросы без Origin/Sec-Fetch-Site. WebDAV токен устройства не принимает (только Basic + пароль приложения). Аудит `device.revoke`, `device.signout` |
| Вход программы синхронизации (E12-2) | RFC 8252 + PKCE S256. Клиент слушает `http://127.0.0.1:<порт>/…` и открывает в системном браузере `/devices/authorize?redirect_uri=&code_challenge=&code_challenge_method=S256&state=&name=&platform=&client_version=` — страница SPA за `RequireAuth` (не вошёл — `/login?next=` с полным query, OIDC несёт `next` в cookie потока). Страница проверяет параметры (неверные — ошибка без редиректа) и показывает устройство, аккаунт и сервер. «Разрешить» — `POST /devices/authorize` (только сессия: устройство — 403 `device_forbidden` через `deviceMayUse`; CSRF — `CrossOriginProtection`) → `{redirect: "<redirect_uri>?code=…&state=…"}`; «Отказать» — тот же запрос с `deny: true` → `?error=access_denied&state=…` (строит сервер по тем же правилам). `redirect_uri` — только `http://127.0.0.1:<1024–65535>/<путь>` или `http://[::1]:…`, без `localhost`, query и fragment, в каноническом виде. Код — 32 случайных байта, в `device_codes` его SHA-256, 120 с; при согласии проверяется лимит устройств. `POST /devices/token` (без входа) `{code, codeVerifier, redirectUri}` удаляет код первым же запросом (повтор и подбор верификатора бесполезны), сверяет verifier и `redirect_uri` за постоянное время, проверяет, что аккаунт не заблокирован, и только тогда создаёт устройство (`Devices.Create`) → `{token, device, user}`; любая неудача — 400 `invalid_grant`, лимит — 409 `device_limit`. Rate limit по IP (10 сразу, потом 1 в 6 с). Cookie не читается, поэтому CSRF ему не нужен; клиент без Origin проходит `CrossOriginProtection`. Просроченные коды удаляются при каждом обмене. Аудит `device.authorize` (имя, платформа, версия клиента) |
| WOPI | `access_token` (подписанный, TTL ~10 ч по рекомендации WOPI, привязан к user, file и perms) |
| Публичная ссылка | Токен в URL (≥ 128 бит); после ввода пароля — короткоживущая cookie на ссылку |

### Заголовки и прокси (E10-2)

- Каждый ответ: `Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'self'; frame-ancestors 'self'; frame-src 'self' <редактор>; form-action 'self' <редактор>` — скрипты только из своих файлов (в сборке нет встроенных скриптов и `eval`), inline-стили нужны Mantine. Адрес редактора — origin из discovery (грузится при старте в фоне, `Discovery.Warm`). Скачивания заменяют CSP на `sandbox`. Ещё `nosniff`, `Referrer-Policy: same-origin`, `Permissions-Policy` (камера, микрофон, геолокация, платежи, USB выключены), `Cross-Origin-Opener-Policy: same-origin`, `Strict-Transport-Security` (год) — только если `PUBLIC_BASE_URL` на https.
- `TRUSTED_PROXIES` (CIDR через запятую, по умолчанию пусто): запросу от такого адреса `RemoteAddr` заменяется на клиента из `X-Forwarded-For` — справа налево до первого недоверенного адреса, подделка слева не помогает. Самый внешний middleware, поэтому журнал, лимиты и сессии видят клиента без правок в коде.
- Лимиты попыток: вход (IP 20 → раз в 3 с, логин 5 → раз в 30 с), неудачный Basic (IP), пароль публичной ссылки (ссылка + IP). Токены ссылок и WOPI — 256 бит, отдельные лимиты не нужны.
- Прокси перед облаком: передавать `X-Forwarded-For`, не буферизовать тела (загрузки многогигабайтные), снять лимит размера тела. nginx: `proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for; proxy_request_buffering off; proxy_buffering off; client_max_body_size 0;`. Caddy: `reverse_proxy cloud:8080` — тела идут потоком и так, заголовок ставит сам (замер в E10-3). Конфиги — `docs/admin-guide.md`. Collabora — отдельный хост или путь с WebSocket.

## 8. WebDAV (R11)

- Endpoint: `/dav/files/{username}/…`. Алиас `/remote.php/dav/files/{username}/…` — для клиентов, ожидающих Nextcloud-пути: стоит дёшево, а мобильным приложениям помогает.
- Свойства: `getetag` (у папок тоже — меняется с любым изменением внутри), `getlastmodified`, `getcontentlength`, `getcontenttype` (из БД, файл не читается), `resourcetype`, `displayname`, `supportedlock`, `quota-used-bytes` (размер папки), `quota-available-bytes` (остаток квоты; без квоты не отдаётся), `oc:fileid` (id узла) и `oc:size`. Мёртвые свойства (PROPPATCH: даты Windows, данные Finder) — таблица `dav_props`, до 64 на узел по 16 КБ.
- `PROPFIND Depth: infinity` запрещён (403) — дорого и не нужно клиентам. Depth 1 строится из листинга страницами по 1000, ответ стримится: число запросов не зависит от числа детей (50 000 элементов — ~0,3 с).
- LOCK хранится в таблице `locks`, общей с WOPI: файл, открытый в редакторе, нельзя перезаписать через WebDAV, и наоборот. Только эксклюзивные (shared — 501), одна на узел, токен `opaquelocktoken:<uuid>`, срок не больше часа (`Infinite` — тоже час; клиенты продлевают). **Проверяет `fs` в транзакции каждой записи** (`checkLocks`): узел и глубокие блокировки предков, для изменения состава папки — сама папка, для перемещения и удаления папки — всё внутри. Пройти может только владелец блокировки, предъявив токен (`fs.WithLockTokens`; WebDAV берёт токены из заголовка `If`). Веб-API и tus получают 423 `locked`. `lockdiscovery` — одним запросом на PROPFIND.
- Большие PUT стримятся в `uploads/` и финализируются так же, как tus.
- Общие папки (E7-4, `webdav/shared.go`): в корне — виртуальная папка «Доступно мне» с верхними узлами из `share.SharedWithMe`; одинаковые имена — «Имя (Владелец)», дальше «(2)», порядок по дате шаринга. Если у пользователя есть своя папка с таким именем, виртуальная становится «Доступно мне (2)». ETag виртуальной папки — хэш ETag записей. Создать, удалить или переместить саму папку или запись в ней нельзя (403; отказаться от доступа — в вебе), ниже записи — обычные пути, права проверяет `fs`. MOVE между владельцами — копия и удаление в корзину. `oc:permissions` — буквы ownCloud (S, R, G, W/CK, DNV).
- Реализация (E7-1, `internal/webdav`): путь разрешается в узел одним запросом (`fs.Lookup`, без учёта регистра), дальше всё по id через `fs`. Чужой логин в пути — 404. PUT — `fs.PutFile`: новый файл или новая версия, `If-Match`/`If-None-Match` проверяются под блокировкой папки в транзакции, квота — до и после стриминга (507). DELETE и перезапись при MOVE/COPY (`Overwrite: T`) отправляют старое в корзину. COPY — `fs.Copy`, синхронно до 10 000 узлов (больше — 507 до E1-6). XML — стандартный `encoding/xml` на разбор, ответ пишется руками с префиксом `D:` (Mini-Redirector не понимает default namespace); из библиотеки взяты разбор `If` и подтверждение блокировок.
- **Матрица совместимости в CI (E7-5):** litmus и rclone автоматически — job `backend:webdav`, локально `make test-webdav`, известные падения litmus в скрипте; Windows, macOS Finder, Linux и мобильные клиенты — ручной чек-лист перед релизом. Всё — в [webdav-clients.md](webdav-clients.md).

## 9. WOPI (R12)

```
Браузер ─ GET /api/v1/files/{id}/edit ─► cloud: проверка прав, выдача access_token,
                                          URL редактора из discovery
Браузер ─ iframe (POST access_token) ──► Collabora / OnlyOffice
Редактор ─ GET  /wopi/files/{id}?access_token=… ─► CheckFileInfo
Редактор ─ GET  /wopi/files/{id}/contents        ─► GetFile
Редактор ─ POST /wopi/files/{id} (X-WOPI-Override: LOCK|UNLOCK|REFRESH_LOCK|PUT_RELATIVE)
Редактор ─ POST /wopi/files/{id}/contents        ─► PutFile → новая версия
```
- Discovery (`/hosting/discovery`) редактора кэшируется, оттуда берутся URL действий по расширению и публичный ключ для проверки `X-WOPI-Proof`.
- `CheckFileInfo` возвращает `UserCanWrite` по правам из `access`, `Version` = etag, `UserFriendlyName`, `SupportsLocks=true` и т. д.
- Сохранение из редактора создаёт версию с автором — пользователем токена.
- Редактор должен достучаться до `/wopi` по сети. В docker-compose — внутренняя сеть; в документации — настройка `aliasgroup` (Collabora) или `WOPI`-режима (OnlyOffice).
- Реализация (E8-1…E8-3, `internal/wopi`): включается `WOPI_DISCOVERY_URL`; `WOPI_HOST_URL` — адрес облака для сервера редактора (WOPISrc и URL в подписи), по умолчанию `PUBLIC_BASE_URL`. Discovery кэшируется на час (при недоступности — последняя копия); действия `edit`, иначе `view`/`view_comment`. Токен — 32 байта, в `wopi_tokens` SHA-256, 10 ч, на один файл; права проверяет `fs` от имени пользователя токена на каждом запросе. `X-WOPI-Proof` проверяется текущим и старым ключом, окно 20 мин; плохая подпись — 500. PutFile — `fs.WriteFile` (право записи на сам файл, новая версия); без блокировки сохранение проходит (Collabora сохраняет и без неё), при чужой — 409 и `X-WOPI-Lock`; `X-COOL-WOPI-Timestamp` не совпал — 409 `COOLStatusCode 1010`. WOPI-блокировка — строка редактора в `locks` (вид `wopi`, токен `wopi:{node}:{строка}`, 30 мин), её держит сессия документа, а не пользователь: сохранить может любой соавтор с этой строкой. PutRelativeFile — рядом со свободным именем или по точному имени (имена в UTF-7). API для веба: `GET /api/v1/editor` (расширения), `POST /api/v1/nodes/{id}/edit` (URL, токен, срок), `POST /api/v1/nodes/{id|root}/documents` (пустой документ, имя занято — «(2)»). Веб (E9-9): `/edit/:id` — токен уходит формой POST во фрейм (не в URL), `closebutton=1`, `UI_Close` из фрейма (только с origin редактора) возвращает в папку; двойной клик открывает в редакторе только офисные форматы, картинки, PDF и текст скачиваются как раньше. Collabora (опционально, профиль `collabora`): `collabora/code`, ключ подписи создаёт одноразовый сервис `collabora-key` в томе `collabora_proof`; включить — `WOPI_DISCOVERY_URL=http://collabora:9980/hosting/discovery WOPI_EDITOR_URL= docker compose --profile collabora up -d`, адрес для браузера задаёт сам Collabora (`server_name`).
- OnlyOffice (E8-4) — редактор по умолчанию: `onlyoffice/documentserver:10.0.0` (Community, AGPL), сервис `onlyoffice` на `localhost:9981`, у облака `WOPI_DISCOVERY_URL=http://onlyoffice/hosting/discovery`, `WOPI_EDITOR_URL=http://localhost:9981`. В образе: `WOPI_ENABLED=true`; `ALLOW_PRIVATE_IP_ADDRESS=true` (иначе фильтр запросов не пускает к `cloud` во внутренней сети); `IP_FILTER_RULES=allow cloud` — WOPI-запросы уходят только на хосты, явно названные в правилах (`wopi.requireIpFilterRule`, правило `*` не считается). Ключ подписи генерируется при первом старте в `/var/www/onlyoffice/Data` — том `onlyoffice_data`. JWT образа к WOPI не относится (это его собственный API). Адрес: OnlyOffice собирает `urlsrc` в discovery из `Host`/`X-Forwarded-Host` запроса, поэтому облако, читающее discovery как `http://onlyoffice`, заменяет origin всех URL действий на `WOPI_EDITOR_URL` (общая настройка, не только для OnlyOffice; только origin, без пути) — он же попадает в CSP `frame-src`/`form-action`. Отличия от Collabora: кнопка «Сохранить» фиксирует правки только в сессии редактора, `PutFile` (новая версия) приходит при закрытии документа последним участником (через ~5 с, `savetimeoutdelay`) и, пока документ открыт, раз в 5 минут, если были правки: `autoAssembly` включён файлом `deploy/onlyoffice/local-production-linux.json` (node-config читает его после `local.json`, который скрипт старта образа пересоздаёт; переменной окружения нет). Без правок автосохранение версий не создаёт; закрытие после автосохранения даёт ещё одну версию — OnlyOffice заново упаковывает docx, байты другие; блокировку OnlyOffice берёт всегда (`LOCK` при открытии на редактирование, `UNLOCK` после сохранения), поэтому запись в файл в это время извне — 423; кнопка закрытия появляется только при `ClosePostMessage: true` в CheckFileInfo (шлёт тот же `UI_Close`); пустой файл (E8-5) OnlyOffice сам заменяет шаблоном через `PutFile` при первом открытии (отсюда версия размера 0); `PutFile` несёт `X-LOOL-WOPI-Timestamp` — не проверяем: файл всё время под блокировкой, а повторные сохранения в одной сессии могли бы дать ложный конфликт; имя при переименовании — UTF-7, как у Collabora; `doc` и `ppt` только просмотр (их редактирование у OnlyOffice — через действие `convert`, его не используем), ODF и `xls` редактируются. Docservice после создания контейнера перегенерирует шрифты и перезапускается (~1–2 мин) — открытые в это время документы падают с «Неизвестная ошибка». Память: ~0,8 ГБ в простое, ~1,3 ГБ с одним открытым документом. Лимиты Community (к Q3 PRD): `info.json` образа сообщает `hasLicense: false` и `connections: 2147483647`, в коде сервера (`_checkLicenseAuth`) число подключений сравнивается именно с этим значением — лимита подключений нет; вместо него ограничитель по памяти переводит новые сессии в просмотр. Прежние «20 подключений» в 10.0 не подтвердились, нагрузкой не проверено.

## 9a. Администрирование и аудит (E9-11, E10-1)

- Журнал `audit_log` (`internal/audit`): время, кто (id и имя — имя остаётся после удаления пользователя), IP, действие, объект (id и имя на тот момент), подробности JSON. Без внешних ключей. Пишется синхронно из хендлеров; ошибка записи — в лог, запрос не падает. События: вход (пароль, OIDC), неудачный вход (с логином, без пароля), выход, пароли приложений, отзыв устройства и выход на нём, удаление в корзину (веб и WebDAV), окончательное удаление и очистка корзины, шаринг, ссылки (без токена), действия админа с пользователями и группами. Хранение `AUDIT_RETENTION` (365 дней), чистит задача `audit_retention`.
- API админа (`internal/admin`, только `is_admin`): `/api/v1/admin/users` (список с поиском, создание, PATCH имени/квоты/админа/статуса/пароля, DELETE), `/admin/audit` (фильтр по действию или префиксу `admin.`, по пользователю), `/admin/system` (версия, провайдеры, редактор, квота по умолчанию, сроки хранения). Нельзя заблокировать, разжаловать или удалить себя (`own_account`) и последнего активного админа (`last_admin`, строки админов блокируются). Блокировка и смена пароля завершают сессии.
- Удаление пользователя: статус `deleting` (как блокировка, сессии сброшены), затем задача river `user_delete` — `fs.DeleteOwner` (блокировки и выданные ему шары, загрузки, корзина, каждый верхний узел через `Trash`+`Purge`, корень, каталог на диске; повтор безопасен) и удаление строки.
- Квота новых аккаунтов — `DEFAULT_QUOTA` (первый админ при установке — без ограничения).
- Веб (`/admin`, пункт «Админка» только для админов, вкладка в `?tab=`): «Пользователи» (поиск, место и квота, создание, изменение только изменённых полей, удаление с вводом логина; себе нельзя снять админа, заблокировать и удалить), «Группы» (создание, переименование, удаление, участники через поиск), «Журнал» (фильтр по области), «Система» (провайдеры, редактор, правила — только просмотр).

## 10. Фоновые задачи и превью

Очередь в Postgres (river): превью, очистка корзины и версий по политике, удаление брошенных загрузок, пересчёт квот, копирование больших папок.

Реализация (E5-1): пакет `internal/jobs`, схема river ведётся его же миграциями (`rivermigrate` при старте, после goose), роль процесса — `ROLE=all|api|worker` (`api` только ставит задачи, `worker` не открывает публичный порт; `/healthz` и `/metrics` — на порту метрик). Периодические задачи выполняет лидер, которого выбирает river. Метрики `cloud_jobs_total{kind,outcome}` и `cloud_job_duration_seconds{kind}`. Задачи (раз в час и при старте): `upload_cleanup` — просроченные загрузки, данные tus без строки `uploads` и staging-файлы старше суток; `trash_retention` — окончательное удаление из корзины старше `TRASH_RETENTION` (по умолчанию 720h, 0 — выключено); `version_prune` — версии сверх `VERSIONS_MAX_COUNT` (по умолчанию 50) или старше `VERSIONS_MAX_AGE` (по умолчанию без срока), с возвратом места в квоту; `sync_journal_retention` — строки журнала синхронизации старше `SYNC_JOURNAL_RETENTION` (720h). Выключенная политика задачу не планирует.

**Копирование (E1-6).** `fs.Copy` копирует содержимое через `storage.Copy` (в localfs — `copy_file_range`: reflink на btrfs/XFS, копирование в ядре на ext4) под shared-блокировкой пути, хэш и размер берёт у исходного блоба — без перечитывания. Папка копируется по узлам, недоделанная копия удаляется. API: операция `copy` в `POST /nodes/batch`, занятое имя → «имя (2)». Больше 1000 узлов или 1 ГБ (`CopyWork` считает узлы с `LIMIT`) — задача river `copy` без лимита размера, в ответе `job`; статус `GET /api/v1/jobs/{id}` (`running`/`done`/`failed` с кодом ошибки; только свои). Неустранимая ошибка (права, квота) отменяет задачу с кодом, прочие повторяются. WebDAV COPY синхронный, до 10 000 узлов. Папка 5 ГБ в Docker Desktop — 12 с.

**Превью (E5-2, `internal/preview`).** Делает утилита `vips thumbnail_source` в дочернем процессе: содержимое на stdin, WebP на stdout, без увеличения, с поворотом по EXIF, у PDF — первая страница. Бинарник остаётся статическим (без cgo), битый файл роняет или вешает только дочерний процесс (таймаут 30 с), одновременно — не больше числа ядер. В образе `vips-tools vips-heif vips-poppler font-dejavu` (~65 МБ); без `vips` в PATH превью выключены. Генерация — при первом запросе `GET /nodes/{id}/preview?size=s|l` (256 и 2048 px), не в очереди: миниатюры видны сразу, а массовая загрузка не порождает превью, которые никто не откроет. Хранятся в `previews/{blob_id}/{size}.webp`, строка в `previews` (ok или неудача — неудача не повторяется), удаляются вместе с блобом. Форматы: JPEG, PNG, WebP, GIF, BMP, HEIC/HEIF, TIFF, PDF, до 200 МБ. С `?v=<etag>` кэшируются браузером на год. Для ссылок — `/s/{token}/api/nodes/{id}/preview`. Просмотр PDF и картинок — `/content?inline=1` (только безопасные типы, `inline` с именем файла).

## 11. API (REST, `/api/v1`)

| Метод и путь | Назначение |
|--------------|------------|
| `GET  /nodes/{id}/children?cursor=&sort=&order=&limit=` | Листинг (R3); `{id}` = `root` — корень текущего пользователя |
| `GET  /nodes/{id}` (с `path` для хлебных крошек) · `PATCH /nodes/{id}` (rename/move) · `DELETE /nodes/{id}` | Узел: `{id, parentId, name, kind, size, mime?, etag, sha256?, mtime, permissions}`; `parentId` — `null` у корня и у верхнего узла общего или ссылки (там, где ответ это знает: `GET /nodes/{id}`, «Доступно мне»), `sha256` — hex содержимого, только у файлов. `PATCH` и `DELETE` с `If-Match` (список ETag, кавычки и `W/` допустимы, `*` — любой) выполняются, только если ETag узла совпал — проверка в транзакции `fs` под блокировкой цепочки; иначе 412 `precondition_failed`. ETag файла меняется только с содержимым (не при переименовании/перемещении), папки — с любым изменением внутри. В `batch` условий нет |
| `POST /nodes/{id}/children` (mkdir) · `POST /nodes/{id}/copy` | Создание папки, копия |
| `POST /nodes/batch` | Групповые move, copy, delete: каждая операция в своей транзакции, ответ — результат по каждой в порядке запроса; до 1000 операций. Есть move и delete (в корзину), copy — E1-6 |
| `GET  /nodes/{id}/content` (Range) · `GET /nodes/{id}/archive` (ZIP) | Скачивание (R4): всегда `attachment` (inline с нашего origin — stored XSS), один диапазон на запрос, ETag/`If-Range`; ZIP потоковый, без сжатия, файлы от 4 ГиБ — в два прохода ради CRC в локальном заголовке (иначе macOS не распакует ZIP64) |
| `POST /uploads` + tus `PATCH/HEAD/DELETE /uploads/{id}` | Загрузка (R2): метаданные `filename`, `parentId` (`root`), `sha256`, `onConflict` (`rename` по умолчанию, `fail` или `replace`), `mtime` (RFC 3339, от 1970 до «сейчас + 24 ч», иначе 400; хранится как mtime файла вместо времени сервера). Для синхронизации (E11-3): `nodeId` — новая версия этого файла по id, где бы он ни был к концу загрузки (`filename`, `parentId`, `onConflict` игнорируются; нужна запись на сам файл, по ссылке — 400), `ifMatch` — ETag базы (только с `nodeId`), проверяется при создании и ещё раз при финализации под блокировкой; расхождение — 412 `precondition_failed`, на финализации загрузка удаляется вместе с резервом квоты. Условия хранятся в `uploads` (`node_id`, `if_match`, `mtime`). Ответ на последний PATCH несёт `Upload-Node-Id` и `Upload-Node-ETag` (новый ETag узла, без кавычек) |
| `GET  /search?q=&limit=&cursor=` | Поиск (R5): подстрока в имени без учёта регистра, от 3 символов, по имени с курсором, у результата — путь папки |
| `GET  /nodes/{id}/versions` · `GET /versions/{id}/content` · `POST /versions/{id}/restore` | Версии (R7): загрузка с `onConflict=replace` переносит текущее содержимое в `versions/{node_id}/{version_id}`; восстановление копирует версию на место текущей, а текущую делает версией — история только растёт; версии в квоте |
| `GET  /trash` · `POST /trash/{id}/restore` · `DELETE /trash/{id}` · `DELETE /trash` · `DELETE /nodes/{id}` | Корзина (R8): восстановление в исходную папку, если она жива, иначе в корень; при занятом имени — «имя (2)». Удалённое остаётся в квоте до окончательного удаления |
| `GET/POST/DELETE /nodes/{id}/shares` · `GET /shared-with-me` | Шаринг (R9) |
| `GET/POST/PATCH/DELETE /nodes/{id}/links` | Ссылки (R10) |
| `GET  /nodes/{id}/edit` | Запуск редактора (R12) |
| `/auth/*` · `/me` · `/me/app-passwords` | Вход, профиль |
| `POST /devices/authorize` (сессия) | Согласие на вход программы синхронизации: `{redirectUri, codeChallenge, codeChallengeMethod: "S256", state, name, platform, clientVersion, deny?}` → `{redirect}` (E12-2) |
| `POST /devices/token` (без входа) | `{code, codeVerifier, redirectUri}` → `{token, device, user}`; 400 `invalid_grant`, 409 `device_limit`, 429 |
| `GET  /sync/info` (без входа) | `{apiVersion: 1, serverVersion, minClientVersion, journalRetention, maxWait: 55}`, секунды |
| `GET  /sync/roots` | Свой корень (`kind: own`) и общие (`shared`, имена как в «Доступно мне»), `owner`, `permissions`, `etag`; курсор, взятый до листинга |
| `GET  /sync/tree?root=` | NDJSON: `{cursor}`, затем узлы `{id, parentId, name, kind, size, etag, sha256, mtime, permissions}` — корень первым, папка раньше содержимого; один снимок `REPEATABLE READ READ ONLY`, курсор из него же; сбой посреди — оборванный ответ |
| `GET  /sync/changes?cursor=&limit=1000&wait=` | `{changes: [{id, owner, deleted, node?, subtree?}], cursor, more, rootsChanged}`; 400 на плохой курсор, 410 `cursor_expired`; `wait` — long-poll до N с (≤ 55), см. E11-4 |
| `/admin/users`, `/admin/groups`, `/admin/providers`, `/admin/settings` | Админка (R15) |

Публичные ссылки: `/s/{token}` (страница), `/s/{token}/api/...` (листинг, скачивание, загрузка в режиме upload).

Реализация ссылок (E6-4): токен — 32 случайных байта; в БД — его SHA-256 для поиска и он же, зашифрованный `SECRET_KEY` (`SecretBox`), чтобы владелец мог скопировать ссылку снова; без `SECRET_KEY` ссылки не создаются (`links_disabled`). Ссылка — субъект `access` (`link:{id}`): `view`/`download` дают чтение, `upload` — только запись (нельзя листать). `view` отдаёт inline только безопасные типы (изображения кроме SVG, PDF, текст, видео, аудио), без ZIP; `download` — `attachment` и ZIP. Пароль — Argon2id; после ввода — cookie `link_session` (12 ч, Path `/s/{token}/`, на сервере хэш); попытки ограничены по ссылке и IP. Загрузка по ссылке — тот же tusd под `/s/{token}/api/uploads/` (Location переписывается), всегда «сохранить оба», квота владельца, продолжить может только эта ссылка (`uploads.link_id`). Просроченная, удалённая или ведущая в корзину ссылка — одинаковый 404. API владельца: `GET/POST /nodes/{id}/links`, `PATCH/DELETE /links/{id}`. Ошибки — единый формат `{"code": "...", "message": "..."}` (RFC 9457 problem+json — вариант).

## 12. Фронтенд

Рекомендация на Q8 — **React + TypeScript + Vite**, TanStack Query (кэш и инвалидация), TanStack Virtual (списки на 50 000 строк), Uppy или tus-js-client для загрузки. Сборка встраивается в Go-бинарник через `embed`. Аргумент — самая большая экосистема и привычный стек; Svelte дал бы меньший бандл, но для облака это не узкое место.

## 13. Эксплуатация

- `docker-compose.yml`: `cloud`, `postgres`, `onlyoffice` (опционально `collabora`, профиль `collabora`), пример с Caddy для TLS.
- Конфиг: только env (`DATABASE_URL`, `DATA_DIR`, …); секреты и OIDC-провайдеры — в необязательном `cloud.env` (`env_file` в compose). Секреты провайдеров в БД шифруются ключом `SECRET_KEY`.
- `/healthz` (живость), `/readyz` (БД и хранилище доступны), `/metrics` (Prometheus, по отдельному порту или токену).
- Бэкап: `pg_dump` + копия `data/`. При «файлах как есть» данные читаемы и без приложения.

## 14. Бенчмарки (с фазы 0)

| Сценарий | Инструмент | Цель из PRD |
|----------|------------|-------------|
| Загрузка 10 ГБ по tus, 1 Гбит/с | свой Go-клиент или k6 | ≥ 90% канала |
| Листинг папки на 50 000 файлов | k6 | p95 < 200 мс |
| PROPFIND Depth 1 на 10 000 файлов | k6 | сравнение с Nextcloud |
| RAM в простое и под нагрузкой 50 пользователей | cAdvisor или `docker stats` | < 150 МБ в простое |
| «Рабочий день» на 400 одновременных пользователей, 8 vCPU / 16 ГБ | k6 | p95 листинга < 300 мс, 0 ошибок 5xx |
| Поиск на 20 млн узлов | k6 | p95 < 300 мс |

Стенд: 2 vCPU / 2 ГБ, Nextcloud последней версии с рекомендованным тюнингом (Redis, APCu, opcache) — честное сравнение.

## 15. Риски и открытые технические вопросы

| # | Риск или вопрос | Митигация |
|---|-----------------|-----------|
| A1 | `x/net/webdav` не покрывает кастомные свойства и LOCK в БД | **Проверено в E0-9** ([webdav-spike.md](webdav-spike.md)): ядро проходит litmus и rclone, но нет `ctx` в `LockSystem`, ETag папок, условного PUT и маппинга ошибок, а PROPFIND делает N+1. В E7-1 форкнули: свой хендлер поверх `fs`, из библиотеки — только `if.go` (E7-3 убрал и `MemLS`, зависимость от `x/net/webdav` снята); XML-слой библиотеки оказался неэкспортируемым (`internal/xml`), он свой на `encoding/xml`. litmus: basic, copymove, http — 100%, props 29/30, locks 33/34 (shared-блокировки не поддерживаются) |
| A2 | Вычисляемый ключ ФС: путь меняется, пока файл читается или пишется | **Сделано в E1-5:** писатели блокируют цепочку строк предков (E1-4); `Move`/`Rename` дополнительно берут эксклюзивный advisory lock на узел, а `fs.Open` вычисляет путь и открывает файл под shared advisory locks на всю цепочку и отпускает их до отдачи потока (дескриптор переживает rename). Advisory, а не `FOR SHARE`: чтение не пишет в строку корня |
| A3 | Обновление ETag и size предков — горячая строка у корня при массовой загрузке | Батчинг обновлений предков на финализации пачки; замер в бенчмарке |
| A4 | Регистр имён: Linux различает, клиенты macOS и Windows — нет | Уникальность по `name_key`; на диске — имя как ввёл пользователь |
| A5 | cgo для превью усложняет кросс-сборку | **Снят в E5-2:** превью делает утилита `vips` в отдельном процессе, бинарник без cgo |
| A6 | Целевой масштаб (2 000 пользователей, ~10 млн узлов): таблица `nodes` и trigram-индекс — порядка единиц–десятков ГБ, обновления ETag предков от ~400 активных пользователей | Бенчмарк E1-8 на 20 млн узлов с фазы 0; рекомендованный сайзинг в документации (8 vCPU / 16 ГБ, Postgres на SSD); при упоре в одну ноду — разнести `api` и `worker` (роли уже есть) |
