# Руководство администратора

Как поставить облако на свой сервер, спрятать его за HTTPS, подключить редактор документов и вход через OIDC, делать бэкапы и обновляться.

Облако — это один контейнер `cloud` (Go-бинарник со встроенным веб-интерфейсом) и PostgreSQL. Рядом по умолчанию стоит OnlyOffice — онлайн-редактор документов. Всё поднимается через `docker compose` из этого репозитория.

Содержание:

1. [Установка](#1-установка)
2. [Reverse proxy и HTTPS](#2-reverse-proxy-и-https)
3. [Редакторы документов](#3-редакторы-документов)
4. [Вход через OIDC](#4-вход-через-oidc)
5. [Бэкап и восстановление](#5-бэкап-и-восстановление)
6. [Обновление](#6-обновление)

## 1. Установка

### Требования к серверу

- Linux (x86_64 или arm64) с Docker Engine и Docker Compose v2.24 или новее (`docker compose version`).
- Память: само облако в простое занимает меньше 150 МБ. OnlyOffice — около 0,8 ГБ в простое и 1,3 ГБ с одним открытым документом. Минимум с редактором — 2 vCPU и 4 ГБ RAM, без редактора — 2 vCPU и 2 ГБ. На 2 000 пользователей рассчитана одна машина 8 vCPU / 16 ГБ вместе с PostgreSQL.
- Диск: место под файлы пользователей, их версии и корзину. Образ OnlyOffice весит около 3 ГБ. Каталог данных должен быть одной файловой системой: новые версии и корзина работают через `rename`. На XFS и Btrfs копирование файлов почти бесплатное (reflink).
- Два DNS-имени, если нужен редактор: например, `cloud.example.ru` для облака и `office.example.ru` для OnlyOffice. Открытые порты 80 и 443.

### Первый запуск

```sh
git clone <адрес репозитория> /opt/cloud
cd /opt/cloud
```

Создайте два файла рядом с `docker-compose.yml`. Оба в `.gitignore`.

`.env` читает сам `docker compose`. Здесь — всё, что упомянуто в `docker-compose.yml` как `${ПЕРЕМЕННАЯ}`:

```sh
cat > .env <<EOF
COMPOSE_PROJECT_NAME=cloud
PUBLIC_BASE_URL=https://cloud.example.ru
WOPI_EDITOR_URL=https://office.example.ru
POSTGRES_PASSWORD=$(openssl rand -hex 24)
ADMIN_USERNAME=admin
ADMIN_PASSWORD=$(openssl rand -hex 12)
EOF
```

`cloud.env` передаётся в контейнер `cloud` целиком. Здесь — секреты, OIDC и остальные настройки:

```sh
cat > cloud.env <<EOF
SECRET_KEY=$(openssl rand -base64 32)
EOF
chmod 600 .env cloud.env
```

Запустите:

```sh
docker compose up -d --build
docker compose ps        # cloud, postgres, onlyoffice — healthy
grep ADMIN_PASSWORD .env # пароль первого администратора
```

Сборка образа занимает несколько минут. Миграции базы применяются при старте контейнера `cloud`.

Облако слушает `127.0.0.1:8090`, OnlyOffice — `127.0.0.1:9981`. Снаружи их не видно: доступ даёт reverse proxy, см. [раздел 2](#2-reverse-proxy-и-https). Проверить, что облако живо, можно и без него: `curl http://127.0.0.1:8090/readyz`.

Правила, о которые легко споткнуться:

- **Переменная из `environment:` в `docker-compose.yml` берётся только из `.env` или окружения shell.** Если положить её в `cloud.env`, она молча не подействует: пустое значение из `environment:` перекрывает `env_file`. В таблице ниже у каждой переменной указано, в какой файл её писать.
- **Первый администратор** создаётся из `ADMIN_USERNAME` и `ADMIN_PASSWORD` (пароль — от 8 символов) только на пустой базе. Когда в базе есть хоть один администратор, эти переменные игнорируются. Пароль можно сменить в интерфейсе, а строку `ADMIN_PASSWORD` потом удалить из `.env`. Если переменные не заданы, облако стартует без администратора и пишет об этом в лог; задайте их и выполните `docker compose up -d` ещё раз.
- **`POSTGRES_PASSWORD` задаётся до первого запуска.** PostgreSQL применяет его только при создании пустого тома. Используйте hex-строку: пароль подставляется в URL базы, и символы `@ : / ?` его сломают.
- **Имя проекта compose определяет имена томов** (`cloud_cloud_data`, `cloud_postgres_data`). По умолчанию это имя каталога. Задайте `COMPOSE_PROJECT_NAME` в `.env`, как в примере, чтобы переименование каталога не подключило пустые тома.
- **Порты на хосте:** `CLOUD_PORT` (8090) и `ONLYOFFICE_PORT` (9981). PostgreSQL опубликован на `127.0.0.1:55433` для разработки; наружу он не виден.
- После правки `.env` или `cloud.env` выполните `docker compose up -d`: compose пересоздаст изменившиеся контейнеры. `docker compose restart` новые значения не подхватывает.

### Переменные окружения

Колонка «Файл» говорит, куда писать переменную при запуске через compose. «По умолчанию» — значение в compose; если в коде оно другое, указано оба. Неверное значение не даёт облаку стартовать, ошибка — в `docker compose logs cloud`.

**Только для compose** (подставляются в `docker-compose.yml`):

| Переменная | Файл | По умолчанию | Назначение |
|---|---|---|---|
| `COMPOSE_PROJECT_NAME` | `.env` | имя каталога | Префикс имён контейнеров, сети и томов |
| `CLOUD_PORT` | `.env` | `8090` | Порт облака на `127.0.0.1` хоста |
| `ONLYOFFICE_PORT` | `.env` | `9981` | Порт OnlyOffice на `127.0.0.1` хоста |
| `COLLABORA_PORT` | `.env` | `9980` | Порт Collabora на `127.0.0.1` хоста (профиль `collabora`) |
| `POSTGRES_USER` / `POSTGRES_PASSWORD` / `POSTGRES_DB` | `.env` | `cloud` / `cloud` / `cloud` | Учётная запись и база PostgreSQL; из них же собирается `DATABASE_URL` |

**Облако** (`backend/internal/config`):

| Переменная | Файл | По умолчанию | Назначение |
|---|---|---|---|
| `PUBLIC_BASE_URL` | `.env` | `http://localhost:8090` (в коде `http://localhost:5173`) | Адрес, по которому браузер открывает облако. От него зависят ссылки, адрес возврата OIDC, флаг `Secure` у cookie и HSTS (при `https://`) |
| `DATABASE_URL` | — | собирается из `POSTGRES_*` | Строка подключения к PostgreSQL. Compose задаёт её сам |
| `DATA_DIR` | — | `/data` (в коде `./data`) | Каталог файлов внутри контейнера, том `cloud_data` |
| `ADMIN_USERNAME` / `ADMIN_PASSWORD` | `.env` | пусто | Первый администратор, только на пустой базе. Задаются вместе |
| `SECRET_KEY` | `cloud.env` | пусто | 32 случайных байта в base64 (`openssl rand -base64 32`). Шифрует секреты в базе, см. ниже |
| `TRUSTED_PROXIES` | `.env` | пусто | Адреса или CIDR прокси через запятую, которым облако верит в `X-Forwarded-For`. См. [раздел 2](#trusted_proxies-настоящий-ip-клиента) |
| `DEFAULT_QUOTA` | `.env` | пусто (без лимита) | Квота новых учётных записей: `10GiB`, `500MB` (единицы двоичные) или число байт. Администратор меняет её каждому в «Админке» |
| `TRASH_RETENTION` | `.env` | `720h` | Сколько удалённое лежит в корзине. `0` — пока пользователь не очистит сам |
| `VERSIONS_MAX_COUNT` | `.env` | `50` | Сколько прошлых версий хранит файл. `0` — без ограничения |
| `VERSIONS_MAX_AGE` | `.env` | `0` | Удалять версии старше этого срока (`2160h`). `0` — без ограничения |
| `AUDIT_RETENTION` | `.env` | `8760h` | Сколько хранится журнал аудита. `0` — всегда |
| `SYNC_JOURNAL_RETENTION` | `.env` | `720h` | Сколько хранится журнал изменений для программ синхронизации — столько клиент может быть офлайн без полной пересверки. `0` — всегда |
| `WOPI_DISCOVERY_URL` | `.env` | `http://onlyoffice/hosting/discovery` (в коде пусто) | Discovery редактора, как его видит облако. Пустое значение выключает редактор |
| `WOPI_HOST_URL` | `.env` | `http://cloud:8080` (в коде — `PUBLIC_BASE_URL`) | Адрес облака, по которому сервер редактора скачивает и сохраняет файлы |
| `WOPI_EDITOR_URL` | `.env` | `http://localhost:9981` (в коде пусто) | Origin редактора для браузера (`https://office.example.ru`, без пути). Пустое значение — брать адреса из discovery как есть (нужно для Collabora) |
| `SESSION_TTL` | `cloud.env` | `720h` | Срок сессии после последнего действия, не меньше `1h` |
| `LOG_LEVEL` | `cloud.env` | `info` | `debug`, `info`, `warn`, `error` |
| `LOG_FORMAT` | `cloud.env` | `json` | `json` или `text` |
| `DB_MAX_CONNS` / `DB_MIN_CONNS` | `cloud.env` | `20` / `2` | Размер пула соединений с PostgreSQL |
| `DB_STATEMENT_TIMEOUT` | `cloud.env` | `30s` | Предел на один SQL-запрос. `0` — без предела |
| `ROLE` | `cloud.env` | `all` | `all` — API и фоновые задачи в одном процессе; `api` и `worker` — разделить их по контейнерам (пример в комментарии в `docker-compose.yml`) |
| `HTTP_ADDR` | — | `:8080` | Адрес HTTP внутри контейнера. Задан в образе, в compose не меняйте |
| `METRICS_ADDR` | — | `:9090` | Prometheus `/metrics` и `/healthz` внутри сети compose (порт не опубликован, авторизации нет). Пустая строка выключает |
| `OIDC_PROVIDERS` и `OIDC_<SLUG>_*` | `cloud.env` | пусто | Провайдеры входа, см. [раздел 4](#4-вход-через-oidc) |

### Где лежат данные

| Том | Что в нём |
|---|---|
| `cloud_postgres_data` | База: дерево файлов, пользователи, права, ссылки, журнал |
| `cloud_cloud_data` (`/data` в контейнере) | Содержимое файлов |
| `cloud_onlyoffice_data` | Ключ подписи WOPI-запросов OnlyOffice |
| `cloud_collabora_proof` | Ключ подписи Collabora (если включена) |

Имена даны для `COMPOSE_PROJECT_NAME=cloud`. На диске хоста тома лежат в `/var/lib/docker/volumes/<имя>/_data`.

Внутри `/data` файлы хранятся «как есть». Их можно прочитать и без приложения:

```
files/{user_id}/Документы/Отчёт.docx   текущие версии, повторяют дерево папок
versions/{node_id}/{version_id}       прошлые версии
trash/{node_id}/…                     корзина
uploads/                              незавершённые загрузки
previews/{blob_id}/{s|l}.webp         миниатюры
```

Не меняйте файлы в `/data` в обход облака: база о правках не узнает.

### SECRET_KEY

Ключ шифрует два вида секретов в базе:

- `client_secret` OIDC-провайдеров. Без ключа провайдер с секретом не настроить: облако не стартует.
- Токены публичных ссылок. Без ключа ссылки не создаются: интерфейс пишет «Публичные ссылки выключены: на сервере не задан SECRET_KEY».

Что будет, если ключ потерян или заменён:

- Секреты OIDC восстановятся сами: облако при каждом старте заново шифрует их из `cloud.env`.
- Существующие публичные ссылки продолжат открываться. Но у файлов с такими ссылками владелец не сможет их ни посмотреть, ни удалить: список ссылок отвечает ошибкой 500. Отозвать такую ссылку можно только удалением самого файла.

Храните ключ отдельно от бэкапа базы: вместе они раскрывают секреты.

## 2. Reverse proxy и HTTPS

Облако не принимает TLS само: перед ним ставится nginx или Caddy. Что важно для прокси:

- **Не буферизовать загрузки и не ограничивать их размер.** Подробно, с настройками для каждого прокси, — в подразделе [«Буферизация загрузок»](#буферизация-загрузок).
- **WebSocket для редактора.** Без него документ не откроется.
- **Заголовки `X-Forwarded-For`, `X-Forwarded-Proto`, `X-Forwarded-Host` и исходный `Host`.**

WebDAV (`/dav/files/{логин}/` и `/remote.php/dav/files/{логин}/`) и публичные ссылки (`/s/…`) идут через те же правила: отдельные `location` не нужны.

### Caddy в том же compose

Caddy сам получает сертификаты Let's Encrypt. Добавьте рядом с `docker-compose.yml` два файла. Это ваши файлы, `git pull` их не трогает.

`docker-compose.override.yml` (compose читает его автоматически):

```yaml
services:
  caddy:
    image: caddy:2-alpine
    restart: unless-stopped
    ports:
      - "80:80"
      - "443:443"
      - "443:443/udp"
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy_data:/data
      - caddy_config:/config

volumes:
  caddy_data:
  caddy_config:
```

`Caddyfile`:

```
cloud.example.ru {
	reverse_proxy cloud:8080
}

office.example.ru {
	reverse_proxy onlyoffice:80
}
```

Настраивать буферизацию, размер тела и таймауты в Caddy не нужно (см. [«Буферизация загрузок»](#буферизация-загрузок)). WebSocket он проксирует сам. Заголовки `X-Forwarded-*` тоже ставит сам, а пришедший от клиента `X-Forwarded-For` отбрасывает.

Запуск: `docker compose up -d`. Дальше задайте `TRUSTED_PROXIES` (см. ниже).

### nginx на хосте

Сертификат — например, из certbot. Облако и OnlyOffice доступны nginx на `127.0.0.1:8090` и `127.0.0.1:9981`. Если nginx тоже работает в контейнере в сети compose, замените адреса на `cloud:8080` и `onlyoffice:80`.

`/etc/nginx/conf.d/cloud.conf`:

```nginx
# Upgrade только для WebSocket (редактор); обычные запросы держат
# соединение с облаком открытым.
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      '';
}

upstream cloud_app {
    server 127.0.0.1:8090;
    keepalive 16;
}

upstream cloud_office {
    server 127.0.0.1:9981;
}

server {
    listen 80;
    server_name cloud.example.ru office.example.ru;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl;
    http2 on;    # nginx старше 1.25.1: "listen 443 ssl http2;"
    server_name cloud.example.ru;

    ssl_certificate     /etc/letsencrypt/live/cloud.example.ru/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/cloud.example.ru/privkey.pem;

    # Загрузки и скачивания без буфера и без лимита размера:
    # см. «Буферизация загрузок».
    client_max_body_size 0;
    proxy_request_buffering off;
    proxy_buffering off;
    proxy_http_version 1.1;
    proxy_read_timeout 1h;
    proxy_send_timeout 1h;

    location / {
        proxy_pass http://cloud_app;
        proxy_set_header Host $http_host;
        proxy_set_header Connection "";
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Host $http_host;
    }
}

server {
    listen 443 ssl;
    http2 on;
    server_name office.example.ru;

    ssl_certificate     /etc/letsencrypt/live/office.example.ru/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/office.example.ru/privkey.pem;

    client_max_body_size 100m;
    proxy_read_timeout 1h;

    location / {
        proxy_pass http://cloud_office;
        proxy_http_version 1.1;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_set_header Host $http_host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header X-Forwarded-Host $http_host;
    }
}
```

Проверка и применение: `nginx -t && systemctl reload nginx`.

### Буферизация загрузок

Нужно ли что-то настраивать, зависит от прокси:

| Прокси | Что настроить |
|---|---|
| nginx | Обязательно: `client_max_body_size 0` и `proxy_request_buffering off`. Желательно: `proxy_buffering off`, `proxy_http_version 1.1` (до nginx 1.29.7), таймауты |
| Caddy | Ничего: тела запросов и ответов идут потоком, лимита размера и таймаутов чтения нет |
| Traefik | Не подключать middleware `buffering`; поднять `readTimeout` у entryPoint |
| Apache | `LimitRequestBody 0`, `ProxyTimeout` |

**Через что идут большие тела:**

- Загрузка из браузера — протокол tus. Файл уходит кусками по 64 МБ (`PATCH /api/v1/uploads/{id}`), один кусок — один запрос.
- Другие tus-клиенты (и `curl`) могут отправить весь файл одним `PATCH`.
- WebDAV-клиенты (Проводник, Finder, rclone) отправляют весь файл одним `PUT`.
- Скачивание файла и ZIP папки — один длинный ответ.

**nginx.** Каждая директива из конфига выше и что будет без неё:

| Директива | По умолчанию в nginx | Зачем | Без неё |
|---|---|---|---|
| `client_max_body_size 0` | `1m` | Снимает лимит размера тела | Любой кусок tus и любой `PUT` больше 1 МБ получает `413 Request Entity Too Large`. Загрузка из браузера не идёт совсем |
| `proxy_request_buffering off` | `on` | Тело уходит в облако по мере получения | nginx сначала принимает всё тело к себе — во временный файл в `client_body_temp_path`, — и только потом отправляет облаку. Нужен диск на размер всех одновременных загрузок. Прогресс у клиента доходит до 100%, а потом запрос висит, пока nginx пересылает тело. Отказ облака (например, нет прав или места у WebDAV `PUT`) клиент узнает, только когда отправит файл целиком |
| `proxy_buffering off` | `on` | Ответ уходит клиенту по мере получения | Медленному клиенту nginx скачивает ответ во временный файл, до 1 ГБ на каждое скачивание (`proxy_max_temp_file_size`). Скачивание работает, но занимает диск прокси |
| `proxy_http_version 1.1` | `1.0` до nginx 1.29.7, дальше `1.1` | Keepalive к облаку; тело с `Transfer-Encoding: chunked` не буферизуется | На HTTP/1.0 тело без `Content-Length` nginx буферизует, даже при `proxy_request_buffering off` |
| `proxy_read_timeout 1h`, `proxy_send_timeout 1h` | `60s` | Запас на паузы | Таймауты nginx считают паузу между двумя операциями чтения или записи, а не всю передачу. Ровная загрузка в лимит 60 с укладывается. Обрыв будет, если клиент или облако молчат дольше минуты |

`client_body_timeout` (60 с, тоже пауза между чтениями) менять не нужно.

**Caddy.** По документации `reverse_proxy` тела запросов не буферизуются, пока не задан `request_buffers`; ответы — пока не задан `response_buffers`. Таймаутов `read_timeout` и `write_timeout` к облаку по умолчанию нет. `flush_interval` для скачиваний не нужен.

**Traefik.** Тела не буферизуются, пока к маршруту не подключён middleware `buffering`; лимит размера тела задаёт тоже он (`maxRequestBodyBytes`, по умолчанию без лимита). Но у entryPoint есть `transport.respondingTimeouts.readTimeout`, по умолчанию `60s`, и это предел на чтение всего запроса вместе с телом. Загрузка одного куска или `PUT` дольше минуты оборвётся. Задайте `readTimeout: 0` (без предела) или большое значение, например `--entryPoints.websecure.transport.respondingTimeouts.readTimeout=0`. В доверенных адресах облака (`TRUSTED_PROXIES`) — адрес Traefik.

**Apache (`mod_proxy_http`).** С версии 2.4.54 `LimitRequestBody` по умолчанию — 1 ГиБ (раньше без лимита). WebDAV `PUT` файла больше 1 ГиБ получит 413. Задайте `LimitRequestBody 0`. `ProxyTimeout` по умолчанию равен `Timeout` (60 с) — поставьте `ProxyTimeout 3600`.

Конфиги Traefik и Apache в этом руководстве не проверялись. Значения по умолчанию взяты из их официальной документации.

### TRUSTED_PROXIES: настоящий IP клиента

Без этой переменной облако считает клиентом сам прокси. Тогда в журнале аудита у всех один адрес, а лимиты попыток входа бьют по всем пользователям сразу.

`TRUSTED_PROXIES` — это адреса, от которых облако принимает `X-Forwarded-For`. Облако читает цепочку справа налево до первого недоверенного адреса. Поэтому подделанный клиентом заголовок не помогает: его значение стоит левее.

Запросы от прокси приходят в контейнер из сети compose:

- от Caddy в compose — с адреса контейнера Caddy;
- от nginx на хосте — с адреса шлюза этой сети.

Оба адреса входят в подсеть сети compose. Узнайте её и запишите в `.env`:

```sh
docker network inspect cloud_default -f '{{(index .IPAM.Config 0).Subnet}}'
# например, 172.18.0.0/16
echo 'TRUSTED_PROXIES=172.18.0.0/16' >> .env
docker compose up -d
```

Подсеть может поменяться, если сеть пересоздать (`docker compose down`). После этого сверьте её снова.

Проверка: войдите в облако и откройте «Админка» → «Журнал». В строке входа должен стоять ваш внешний IP, а не адрес из подсети Docker.

### Что ещё стоит знать

- В логах прокси остаются токены. В адресах редактора — `access_token` WOPI (живёт около 10 часов), в адресах публичных ссылок — их токен. Ограничьте доступ к логам и срок их хранения.
- HSTS включается сам, когда `PUBLIC_BASE_URL` начинается с `https://`.

## 3. Редакторы документов

Облако открывает документы во встроенном редакторе по протоколу WOPI. Схема одна для обоих редакторов:

- облако читает discovery редактора по внутреннему адресу (`WOPI_DISCOVERY_URL`);
- браузер открывает редактор по публичному адресу (`WOPI_EDITOR_URL` или адрес из discovery);
- сервер редактора ходит к облаку за файлом по внутреннему адресу (`WOPI_HOST_URL`, `http://cloud:8080`).

Публичный адрес редактора облако добавляет в CSP (`frame-src`, `form-action`). Если он не совпадает с тем, что видит браузер, iframe редактора останется пустым.

### OnlyOffice (по умолчанию)

OnlyOffice Docs Community 10.0 уже есть в `docker-compose.yml`. Нужно только поставить его за прокси на своё имя и сказать облаку, где его видит браузер:

```sh
# .env
WOPI_EDITOR_URL=https://office.example.ru
```

Конфиги прокси для `office.example.ru` — в [разделе 2](#2-reverse-proxy-и-https). OnlyOffice собирает свои адреса из `Host` и `X-Forwarded-Proto`, поэтому прокси должен их передавать.

Что уже настроено в compose и почему:

- `WOPI_ENABLED=true` — в образе WOPI выключен.
- `ALLOW_PRIVATE_IP_ADDRESS=true` — иначе OnlyOffice не ходит на адреса внутренней сети, а `cloud` — как раз такой.
- `IP_FILTER_RULES=allow cloud` — WOPI-запросы уходят только на хосты, названные в правилах. Правило `*` не считается. Если вы меняете хост в `WOPI_HOST_URL`, его нужно назвать здесь же (через `docker-compose.override.yml`).
- Том `onlyoffice_data` хранит ключ подписи. Без тома каждое пересоздание контейнера даёт новый ключ.
- `deploy/onlyoffice/local-production-linux.json` включает автосохранение.

Когда появляется новая версия файла:

- при закрытии документа последним участником (через несколько секунд);
- раз в 5 минут, пока в документе есть правки.

Кнопка «Сохранить» в OnlyOffice новую версию не создаёт.

После пересоздания контейнера (`up -d` после обновления, `down`/`up`) OnlyOffice 1–2 минуты генерирует шрифты и один раз перезапускается сам. Документы, открытые в это время, падают с «Неизвестная ошибка» — откройте их заново.

### Collabora (по желанию)

Collabora Online включается профилем `collabora`. Вместо OnlyOffice в `.env`:

```sh
WOPI_DISCOVERY_URL=http://collabora:9980/hosting/discovery
WOPI_EDITOR_URL=
```

`WOPI_EDITOR_URL` должен быть именно пустым: Collabora сама пишет в discovery публичный адрес. В compose адреса Collabora рассчитаны на `localhost`, поэтому для своего домена переопределите параметры в `docker-compose.override.yml`:

```yaml
services:
  collabora:
    environment:
      extra_params: >-
        --o:ssl.enable=false --o:ssl.termination=true
        --o:server_name=office.example.ru
        --o:net.frame_ancestors=cloud.example.ru
```

- `ssl.termination=true` — TLS снимает прокси, а Collabora строит адреса с `https://` и `wss://`.
- `server_name` — имя, по которому её открывает браузер.
- `net.frame_ancestors` — кому можно встраивать редактор в iframe.

В прокси для `office.example.ru` укажите `collabora:9980` (Caddy) или `127.0.0.1:9980` (nginx на хосте) вместо OnlyOffice. Запуск:

```sh
docker compose --profile collabora up -d
docker compose stop onlyoffice   # если OnlyOffice больше не нужен
```

Что делает compose:

- Сервис `collabora-key` один раз создаёт ключ подписи WOPI в томе `collabora_proof`. В образе Collabora нет shell, поэтому ключ делает отдельный контейнер. Без ключа Collabora не подписывает запросы.
- `aliasgroup1` равен `WOPI_HOST_URL` (`http://cloud:8080`): Collabora открывает файлы только с хостов из этого списка.

В Collabora новая версия появляется по кнопке сохранения в редакторе и при закрытии документа.

### Если редактор не работает

| Признак | Что проверить |
|---|---|
| В меню файла нет «Редактировать» или «Админка» → «Система» → «Онлайн-редактор» показывает «Не отвечает» | `docker compose logs cloud \| grep wopi`. Облако должно достучаться до `WOPI_DISCOVERY_URL` из своего контейнера. Пустой `WOPI_DISCOVERY_URL` выключает редактор |
| Iframe пустой, в консоли браузера ошибка CSP `frame-src` | `WOPI_EDITOR_URL` должен совпадать с адресом редактора в браузере: схема, имя, порт. После правки — `docker compose up -d` |
| OnlyOffice: «Ошибка загрузки документа» | `IP_FILTER_RULES` должен называть хост из `WOPI_HOST_URL`. Доступность облака из контейнера редактора: `docker compose exec onlyoffice curl -s http://cloud:8080/readyz` |
| Редактор открывается, но сразу «соединение прервано» | Прокси не пропускает WebSocket: нужны заголовки `Upgrade` и `Connection` (nginx) |
| «Неизвестная ошибка» в первые минуты после пересоздания OnlyOffice | Генерация шрифтов, подождите 1–2 минуты |
| В логе облака `wopi: request with a bad proof` | Редактор сменил ключ подписи (например, пропал его том), а облако держит старый discovery до часа. `docker compose restart cloud` |
| Collabora не открывает документ, в логе `Low disk space` | Collabora отказывается работать, когда на диске почти нет места |

## 4. Вход через OIDC

Кроме локальных учётных записей облако умеет входить через любые OIDC-провайдеры: Keycloak, Authentik, GitLab, Google, Яндекс ID и другие. Провайдеров может быть несколько. На странице входа на каждый будет своя кнопка.

### Настройка у провайдера

Создайте у провайдера клиента (приложение):

- тип — confidential (с секретом) или public (только PKCE);
- Authorization Code flow, PKCE S256;
- адрес возврата (redirect URI): `https://cloud.example.ru/api/v1/auth/oidc/<slug>/callback`.

`<slug>` — короткое имя провайдера, которое вы выберете ниже. Адрес строится из `PUBLIC_BASE_URL`.

### Настройка облака

Провайдеры задаются в `cloud.env`. `SECRET_KEY` обязателен, если у клиента есть секрет.

```sh
OIDC_PROVIDERS=kc,gitlab

OIDC_KC_NAME=Вход через Keycloak
OIDC_KC_ISSUER=https://sso.example.ru/realms/company
OIDC_KC_CLIENT_ID=cloud
OIDC_KC_CLIENT_SECRET=…

OIDC_GITLAB_NAME=GitLab
OIDC_GITLAB_ISSUER=https://gitlab.example.ru
OIDC_GITLAB_CLIENT_ID=…
OIDC_GITLAB_CLIENT_SECRET=…
```

| Переменная | По умолчанию | Назначение |
|---|---|---|
| `OIDC_PROVIDERS` | пусто | Slug-и через запятую: строчные латинские буквы, цифры и дефис, до 32 символов. Slug входит в адрес возврата и в имена переменных (дефис → `_`, буквы заглавные) |
| `OIDC_<SLUG>_ISSUER` | — | Issuer провайдера, обязателен |
| `OIDC_<SLUG>_CLIENT_ID` | — | ID клиента, обязателен |
| `OIDC_<SLUG>_CLIENT_SECRET` | пусто | Секрет клиента; пусто — public-клиент с PKCE |
| `OIDC_<SLUG>_NAME` | slug | Надпись на кнопке входа |
| `OIDC_<SLUG>_SCOPES` | `openid profile email` | Scopes через пробел или запятую; `openid` обязателен |
| `OIDC_<SLUG>_AUTO_CREATE` | `true` | Создавать учётную запись при первом входе. `false` — пускать только уже связанных |
| `OIDC_<SLUG>_LINK_BY_EMAIL` | `false` | Привязывать первый вход к существующей учётной записи с тем же email. Срабатывает, только если провайдер подтвердил email. Включайте, только если провайдер не даёт пользователям ставить чужой email |

Примените: `docker compose up -d`. В логе появится `OIDC provider enabled`.

Как это работает:

- Логин новой учётной записи берётся из `preferred_username` или из email. При совпадении добавляется `-2`, `-3` и так далее.
- Провайдер, убранный из `OIDC_PROVIDERS`, выключается, но не удаляется. Не переименовывайте slug: учётные записи, связанные со старым, потеряют вход.
- Issuer должен открываться и из контейнера `cloud`, и из браузера, по одному и тому же адресу. Облако проверяет, что issuer в ответе провайдера совпадает с заданным.
- Вход по паролю остаётся включённым всегда.
- WebDAV-клиентам у OIDC-пользователей нужен пароль приложения: «Настройки» → «Пароли приложений».

### Администратор при входе через OIDC

Пользователь из OIDC никогда не становится администратором сам. Порядок такой:

1. Установите облако с `ADMIN_USERNAME` и `ADMIN_PASSWORD`, как в [разделе 1](#1-установка).
2. Войдите через OIDC своей рабочей учётной записью. Она создастся обычным пользователем.
3. Войдите локальным администратором и в «Админке» сделайте эту учётную запись администратором.
4. По желанию заблокируйте локального администратора. Последнего активного администратора облако заблокировать не даст.

### Пример: Keycloak

1. В realm создайте клиента: Client ID `cloud`, Client authentication — On, Standard flow — On. Valid redirect URIs — `https://cloud.example.ru/api/v1/auth/oidc/kc/callback`. В Advanced → «Proof Key for Code Exchange Code Challenge Method» выберите `S256`.
2. Скопируйте секрет с вкладки Credentials.
3. В `cloud.env`:

   ```sh
   OIDC_PROVIDERS=kc
   OIDC_KC_NAME=Keycloak
   OIDC_KC_ISSUER=https://sso.example.ru/realms/company
   OIDC_KC_CLIENT_ID=cloud
   OIDC_KC_CLIENT_SECRET=<секрет>
   ```

4. `docker compose up -d`, на странице входа появится кнопка «Keycloak».

Готовый realm для опытов — `deploy/keycloak/realm-cloud.json` (клиент `cloud` / `kc-client-secret`, пользователь `ivan` / `ivanpass`). Он рассчитан на облако на `localhost:8090`: `docker compose --profile e2e up -d keycloak`.

## 5. Бэкап и восстановление

Бэкап — это две части, которые должны соответствовать друг другу: дамп базы и архив каталога файлов. Плюс `.env` и `cloud.env` с `SECRET_KEY`.

### Бэкап

Остановите облако на время копирования. Тогда база и файлы будут из одного момента. PostgreSQL и редактор продолжают работать.

```sh
cd /opt/cloud
mkdir -p backup && D=$(date +%F)
docker compose stop cloud
docker compose exec -T postgres pg_dump -U cloud -Fc cloud > backup/cloud-$D.dump
docker run --rm --volumes-from $(docker compose ps -aq cloud) -v "$PWD/backup":/backup \
  alpine tar -C /data -czf /backup/data-$D.tar.gz .
docker compose start cloud
cp .env cloud.env backup/
```

- Если меняли `POSTGRES_USER` или `POSTGRES_DB`, подставьте их в `pg_dump`.
- `--volumes-from` подключает том с файлами, не угадывая его имя.
- Унесите `backup/` с сервера. `.env` и `cloud.env` храните отдельно от дампа: в них `SECRET_KEY` и пароли.
- Тома редакторов (`onlyoffice_data`, `collabora_proof`) копировать не обязательно. Без них редактор создаст новый ключ подписи.

Почему не копировать на ходу. Пока облако работает, файлы переезжают: в версии, в корзину, на место загруженных. Если файл переехал между дампом и архивом, в бэкапе будет запись в базе без содержимого. Без остановки бэкап согласован, только если делать снимок тома (LVM, ZFS, Btrfs) в одну секунду с `pg_dump`.

### Восстановление на чистый сервер

Берите ту же версию облака, что была при бэкапе, или новее. Новее можно: миграции применятся при старте.

```sh
git clone <адрес репозитория> /opt/cloud && cd /opt/cloud
cp /backup/.env /backup/cloud.env .        # тот же SECRET_KEY и POSTGRES_PASSWORD
docker compose up -d --wait postgres
docker compose exec -T postgres pg_restore -U cloud -d cloud --no-owner < /backup/cloud-2026-10-25.dump
docker compose create cloud
docker run --rm --volumes-from $(docker compose ps -aq cloud) -v /backup:/backup:ro \
  alpine tar -C /data -xzf /backup/data-2026-10-25.tar.gz
docker compose up -d
```

- Базу восстанавливайте до первого запуска `cloud`. Облако, запущенное на пустой базе, создаёт таблицы, и `pg_restore` поверх них упадёт. Тогда начните заново: `docker compose down -v` удалит пустые тома.
- `tar` от root сохраняет владельца файлов (uid 10001, под ним работает облако).
- После запуска проверьте: вход, несколько файлов, их версии, публичную ссылку.

### При чём тут SECRET_KEY

Дамп содержит секреты, зашифрованные `SECRET_KEY`: токены публичных ссылок и секреты OIDC. Восстанавливайте с тем же ключом. С другим ключом ссылки откроются, но владельцы не смогут их посмотреть и удалить (см. [SECRET_KEY](#secret_key)).

## 6. Обновление

1. Сделайте бэкап (раздел 5). Миграции базы необратимы: откатиться можно только восстановлением из бэкапа вместе со старой версией.
2. Получите новую версию и пересоберите:

   ```sh
   cd /opt/cloud
   git pull                 # или git fetch --tags && git checkout <версия>
   docker compose up -d --build
   ```

3. Проверьте, что всё поднялось:

   ```sh
   docker compose ps
   docker compose logs cloud | grep -E 'goose|error'
   ```

   Миграции применяются при старте, в логе их видно по строкам `goose`.

Что ещё учесть:

- Версии образов PostgreSQL, OnlyOffice и Collabora закреплены в `docker-compose.yml`. Новые подтянутся при `up -d`, если обновление их поменяло.
- После обновления OnlyOffice контейнер пересоздаётся и 1–2 минуты генерирует шрифты.
- Смена мажорной версии PostgreSQL (например, 18 → 19) несовместима с файлами старой базы. Сделайте `pg_dump`, поднимите новую версию на пустом томе и восстановите дамп, как в разделе 5.
- `docker-compose.override.yml`, `Caddyfile`, `.env` и `cloud.env` — ваши файлы, `git pull` их не меняет.
