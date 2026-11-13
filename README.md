# magnet

`magnet` находит и сохраняет `.torrent`-файлы по magnet-ссылкам через BitTorrent DHT.
Содержимое торрента не скачивается — инструмент загружает только метаданные, то есть сам
torrent-файл.

Программа собрана как один бинарник и умеет работать в двух режимах:

1. **Разовый запуск** — `magnet fetch` ищет метаданные, сохраняет файл и завершается.
2. **Демон** — `magnet daemon` принимает ссылки по HTTP API и ищет их в фоне. Для демона
   есть CLI-клиент (`magnet add`, `magnet ls`, `magnet wait`, `magnet get` и т.д.).

Дополнительно собирается отдельный лёгкий бинарник `magnet-web` с локальным
web-интерфейсом. Он может подключиться к запущенному демону или сам поднять встроенный
движок — без Node.js, WebView и CGO.

## Возможности

- Поиск метаданных через DHT; трекеры можно отключить (`--no-trackers`).
- Пакетная обработка ссылок из аргументов, файлов (`@links.txt`) и stdin (`-`).
- Демон с очередью, несколькими воркерами и сохранением состояния в bbolt.
- Автоматические повторные попытки с настраиваемым таймаутом и задержкой.
- Восстановление очереди после перезапуска демона.
- HTTP/JSON API, скачивание готового `.torrent` через API.
- Локальный web-интерфейс в браузере: добавление ссылок, список заданий, отмена, повтор,
  удаление и скачивание `.torrent`.
- Необязательный API-токен. По умолчанию демон слушает только `127.0.0.1`.
- JSON-вывод для сценариев (`--json`).

## Сборка

Нужен Go 1.23+.

```sh
make build          # bin/magnet и bin/magnet-web
# или по отдельности
go build -o magnet ./cmd/magnet
go build -o magnet-web ./cmd/magnet-web
```

Установка в `/usr/local/bin`:

```sh
sudo make install
# или
sudo install -m 0755 bin/magnet bin/magnet-web /usr/local/bin/
```

Проверка:

```sh
make vet
make test
make race
```

### Кросс-сборка под Linux, Windows и macOS

```sh
make build-all       # все платформы
make build-linux     # Linux amd64 + arm64
make build-windows   # Windows amd64 + arm64
make build-darwin    # macOS amd64 + arm64
```

Готовые файлы складываются в `dist/` вместе с `SHA256SUMS`:

```text
dist/magnet-dev-linux-amd64
dist/magnet-web-dev-linux-amd64
dist/magnet-dev-linux-arm64
dist/magnet-web-dev-linux-arm64
dist/magnet-dev-windows-amd64.exe
dist/magnet-web-dev-windows-amd64.exe
dist/magnet-dev-windows-arm64.exe
dist/magnet-web-dev-windows-arm64.exe
dist/magnet-dev-darwin-amd64
dist/magnet-web-dev-darwin-amd64
dist/magnet-dev-darwin-arm64
dist/magnet-web-dev-darwin-arm64
dist/LICENSE
dist/THIRD_PARTY_NOTICES.md
dist/SHA256SUMS
```

Версия по умолчанию берётся из `git describe`; её можно задать явно:

```sh
make build-all VERSION=v1.0.0
```

Кросс-сборка выполняется с `CGO_ENABLED=0`, поэтому не нужен C-компилятор и
соответствующие toolchain'ы. Torrent-клиент в этом режиме использует pure-Go реализацию uTP;
DHT, TCP и загрузка метаданных работают полноценно.

## Режим 1: разовое скачивание

```sh
magnet fetch 'magnet:?xt=urn:btih:43519d14444904f6015e1a0d92018068fb0a49fa'
```

В stdout будет выведен путь к сохранённому файлу, прогресс и ошибки — в stderr.

```sh
# В каталог downloads/, имя файла берётся из метаданных торрента
magnet fetch -o downloads/ 'magnet:?xt=urn:btih:...'

# Явное имя файла (путь заканчивается на .torrent)
magnet fetch -o downloads/ubuntu.torrent 'magnet:?xt=urn:btih:...'

# Несколько ссылок, файл со ссылками и stdin
magnet fetch -o downloads/ \
  'magnet:?xt=urn:btih:...' \
  @links.txt \
  - < more-links.txt

# Без трекеров, только DHT
magnet fetch --no-trackers -o downloads/ 'magnet:?xt=urn:btih:...'

# Для скриптов
magnet fetch --json -o downloads/ 'magnet:?xt=urn:btih:...'
```

Параметры `fetch`:

| Параметр | По умолчанию | Описание |
|---|---|---|
| `-o, --out` | `.` | Каталог или путь к `.torrent`. Путь без `.torrent` считается каталогом. |
| `-t, --timeout` | `15m` | Таймаут поиска одной ссылки. `0` — без ограничения. |
| `-f, --force` | `false` | Перезаписать существующий `.torrent`. |
| `--port` | `0` | UDP/TCP-порт DHT и peer-соединений; `0` — случайный. |
| `--no-trackers` | `false` | Не использовать трекеры, искать только через DHT. |
| `--json` | `false` | Вывести массив результатов в JSON. |
| `-q, --quiet` | `false` | Не выводить прогресс. |
| `--debug` | `false` | Подробный лог torrent-клиента. |

Коды возврата: `0` — успех, `1` — хотя бы одна ссылка не обработана, `2` — ошибка
аргументов, `130` — прерывание по Ctrl+C.

## Режим 2: демон и CLI-клиент

### Запуск демона

```sh
magnet daemon
```

По умолчанию:

- HTTP API: `127.0.0.1:8787`;
- состояние, база заданий и torrent-файлы: `~/.local/state/magnet`
  (или `$XDG_STATE_HOME/magnet`);
- 4 одновременных поиска;
- одна попытка — до 15 минут, затем повтор через 30 секунд; число попыток не ограничено.

Пример с явными параметрами:

```sh
magnet daemon \
  --listen 127.0.0.1:8787 \
  --data-dir /var/lib/magnet \
  --workers 8 \
  --attempt-timeout 20m \
  --retry-delay 1m \
  --max-attempts 0
```

Параметры демона:

| Параметр | По умолчанию | Описание |
|---|---|---|
| `--listen` | `127.0.0.1:8787` | Адрес HTTP API. |
| `--data-dir` | `$MAGNET_DATA_DIR` или `$XDG_STATE_HOME/magnet` | Каталог состояния, БД, torrent-файлов и кеша. |
| `--api-token` | `$MAGNET_API_TOKEN` | Токен API. Если задан, нужен для всех `/v1/*`. |
| `--workers` | `4` | Число параллельных поисков. |
| `--attempt-timeout` | `15m` | Таймаут одной попытки; `0` — без ограничения. |
| `--retry-delay` | `30s` | Пауза перед повтором. |
| `--max-attempts` | `0` | Максимум попыток на ссылку; `0` — без ограничения. |
| `--port` | `0` | UDP/TCP-порт DHT и peer-соединений. |
| `--no-trackers` | `false` | Искать только через DHT. |
| `--log-level` | `info` | `debug`, `info`, `warn`, `error`. |
| `--log-format` | `text` | `text` или `json`. |

Внутри `--data-dir` создаются:

- `jobs.db` — база заданий bbolt;
- `torrents/<info-hash>.torrent` — найденные torrent-файлы;
- `cache/` — служебный каталог torrent-клиента.

### CLI-клиент

Клиентские команды используют один и тот же бинарник. Адрес демона задаётся
`--server`/`-s` или переменной `MAGNET_SERVER`, токен — `--token` или
`MAGNET_API_TOKEN`. Флаги можно указывать как до, так и после magnet-ссылок и ID заданий.

```sh
# Поставить ссылки в очередь
magnet add 'magnet:?xt=urn:btih:...'
magnet add @links.txt
cat links.txt | magnet add -

# Список заданий
magnet ls
magnet ls --state searching
magnet ls --all

# Подробности
magnet show <id>

# Дождаться завершения и получить путь к файлу
magnet wait <id>
magnet wait --timeout 6h <id1> <id2>

# Скачать готовые torrent-файлы
magnet get -o downloads/ <id>
magnet get -o downloads/ <id1> <id2>

# Управление заданиями
magnet cancel <id>
magnet retry <id>
magnet rm <id>              # убрать из списка, файл оставить
magnet rm --purge <id>      # убрать задание и файл

# Состояние демона
magnet stats
magnet ping
```

Те же команды доступны в явной форме:

```sh
magnet client add ...
magnet client ls
magnet client get ...
```

Для машинной обработки почти все клиентские команды поддерживают `--json`.
`magnet add` при массовой загрузке обрабатывает некорректные ссылки по одной и не блокирует
остальные; код возврата будет `1`, если хотя бы одна ссылка отклонена.

### Состояния задания

| Состояние | Значение |
|---|---|
| `queued` | Ждёт воркера или паузы перед повтором. |
| `searching` | Идёт поиск метаданных. |
| `found` | `.torrent` найден и сохранён. |
| `failed` | Исчерпаны попытки (`--max-attempts`). |
| `canceled` | Задание отменено пользователем. |

При ошибке или таймауте задание возвращается в очередь; причина видна в поле `error`.
После перезапуска демона задания из `searching` снова становятся в очередь, а найденные
задания сохраняются. Если сохранённый torrent-файл пропал, задание автоматически уходит на
повторный поиск.

## Web UI

`magnet-web` — отдельный лёгкий бинарник с локальным web-интерфейсом. Он не требует Node.js,
WebView, CGO и системных библиотек нативного GUI: HTML/CSS/JS встроены в исполняемый файл.

```sh
./bin/magnet-web
```

По умолчанию web-интерфейс:

1. проверяет, запущен ли демон на `http://127.0.0.1:8787`;
2. если демон найден — подключается к нему;
3. если нет — запускает встроенный движок и использует тот же каталог данных,
   что и `magnet daemon`;
4. поднимает локальный web-интерфейс на случайном loopback-порту и открывает браузер.

Адрес интерфейса печатается в stdout. Если браузер открыть не удалось, откройте этот адрес
вручную.

```sh
# Всегда использовать встроенный движок
./bin/magnet-web --embedded

# Подключиться к существующему демону
./bin/magnet-web --server http://127.0.0.1:8787

# Демон с токеном
./bin/magnet-web --server http://127.0.0.1:8787 --api-token secret

# Не открывать браузер, выбрать порт явно
./bin/magnet-web --no-browser --listen 127.0.0.1:9090

# Встроенный движок с отдельным каталогом данных
./bin/magnet-web --embedded --data-dir ~/.local/state/magnet-web
```

Параметры:

| Параметр | По умолчанию | Описание |
|---|---|---|
| `--server URL` | автоопределение | Подключиться к демону вместо встроенного движка. |
| `--api-token` | `$MAGNET_API_TOKEN` | Токен для внешнего демона. |
| `--embedded` | `false` | Всегда запускать встроенный движок. |
| `--listen` | `127.0.0.1:0` | Адрес локального web-интерфейса. |
| `--data-dir` | `$MAGNET_DATA_DIR` / XDG | Каталог данных встроенного движка. |
| `--workers` | `4` | Число параллельных поисков. |
| `--attempt-timeout` | `15m` | Таймаут одной попытки; `0` — без ограничения. |
| `--retry-delay` | `30s` | Пауза перед повтором. |
| `--max-attempts` | `0` | Максимум попыток; `0` — без ограничения. |
| `--port` | `0` | UDP/TCP-порт DHT и peer-соединений. |
| `--no-trackers` | `false` | Искать только через DHT. |
| `--no-browser` | `false` | Не открывать браузер автоматически. |
| `--log-level` | `info` | `debug`, `info`, `warn`, `error`. |

В интерфейсе можно:

- добавлять одну или несколько magnet-ссылок;
- фильтровать задания по состоянию;
- видеть имя, info hash, попытки, размер и текст ошибки;
- отменять поиск, повторять задания и удалять их;
- скачивать готовый `.torrent`;
- опционально удалять вместе с заданием сохранённый torrent-файл.

Скачивание `.torrent` идёт через браузер, поэтому место сохранения выбирает браузер
(обычно папка загрузок). Для точного выбора пути используйте CLI `magnet get` или API демона.

Встроенный движок и демон используют одну и ту же bbolt-базу, поэтому одновременно работать
с одним `--data-dir` они не могут. Если демон запущен не на стандартном порту, укажите его
через `--server`.

Параметры `--data-dir`, `--workers`, `--attempt-timeout`, `--retry-delay`, `--max-attempts`,
`--port` и `--no-trackers` относятся только к встроенному движку; при подключении к демону
используются его настройки.

`magnet-web` не имеет собственной аутентификации и по умолчанию слушает только `127.0.0.1`.
Для удалённого доступа используйте SSH-туннель или reverse proxy с аутентификацией; привязка
к не-loopback-адресу выводит предупреждение. Изменяющие запросы требуют служебный заголовок
и проверяются на same-origin, что защищает от простого CSRF из браузера.

## HTTP API

Базовый URL по умолчанию: `http://127.0.0.1:8787`.

Если задан `--api-token`, передавайте его так:

```http
Authorization: Bearer <token>
```

или:

```http
X-API-Token: <token>
```

`GET /healthz` всегда доступен без токена.

| Метод и путь | Описание |
|---|---|
| `GET /healthz` | Проверка живости. |
| `GET /v1/stats` | Счётчики заданий и uptime. |
| `POST /v1/jobs` | Добавить одну или несколько ссылок. |
| `GET /v1/jobs` | Список заданий: `state`, `limit` (1..1000), `offset`. |
| `GET /v1/jobs/{id}` | Одно задание. |
| `GET /v1/jobs/{id}/torrent` | Скачать готовый `.torrent`. |
| `POST /v1/jobs/{id}/cancel` | Отменить задание. |
| `POST /v1/jobs/{id}/retry` | Повторить `failed`/`canceled`. |
| `DELETE /v1/jobs/{id}` | Удалить задание; `?purge=true` — вместе с файлом. |

### Примеры API

Добавить одну ссылку:

```sh
curl -sS -X POST http://127.0.0.1:8787/v1/jobs \
  -H 'Content-Type: application/json' \
  -d '{"magnet":"magnet:?xt=urn:btih:43519d14444904f6015e1a0d92018068fb0a49fa"}'
```

Добавить пакет:

```sh
curl -sS -X POST http://127.0.0.1:8787/v1/jobs \
  -H 'Content-Type: application/json' \
  -d '{"magnets":[
        "magnet:?xt=urn:btih:43519d14444904f6015e1a0d92018068fb0a49fa",
        "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567"
      ]}'
```

Пакет валидируется целиком: если хотя бы одна ссылка некорректна, возвращается `400` и
ни одна ссылка не добавляется. CLI-клиент `magnet add` проверяет ссылки локально и
отправляет в API только валидные, поэтому одна опечатка не блокирует массовый импорт.

Ответ на добавление:

```json
{
  "jobs": [
    {
      "id": "43519d14444904f6015e1a0d92018068fb0a49fa",
      "magnet": "magnet:?xt=urn:btih:...",
      "info_hash": "43519d14444904f6015e1a0d92018068fb0a49fa",
      "state": "queued",
      "attempts": 0,
      "created_at": "2026-01-01T12:00:00Z",
      "updated_at": "2026-01-01T12:00:00Z"
    }
  ],
  "created": 1,
  "existing": 0
}
```

Список:

```sh
curl -sS 'http://127.0.0.1:8787/v1/jobs?state=found&limit=50'
```

Скачать torrent:

```sh
curl -sSJO http://127.0.0.1:8787/v1/jobs/<id>/torrent
```

Ошибки API имеют единый формат:

```json
{"error":{"code":"not_found","message":"job not found"}}
```

## Systemd

Пример unit-файла `/etc/systemd/system/magnet.service`:

```ini
[Unit]
Description=magnet DHT metadata daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=magnet
Group=magnet
StateDirectory=magnet
ExecStart=/usr/local/bin/magnet daemon \
    --listen 127.0.0.1:8787 \
    --data-dir /var/lib/magnet
Restart=on-failure
RestartSec=5s
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true

[Install]
WantedBy=multi-user.target
```

```sh
sudo useradd --system --home /var/lib/magnet --shell /usr/sbin/nologin magnet
sudo systemctl daemon-reload
sudo systemctl enable --now magnet
sudo -u magnet /usr/local/bin/magnet stats
```

Если демон слушает не loopback-адрес, обязательно задайте `--api-token` или положите токен
в `Environment=MAGNET_API_TOKEN=...` / `EnvironmentFile=`.

API не терминирует TLS. Для доступа по сети используйте SSH-туннель, VPN или reverse proxy
с HTTPS; иначе токен и magnet-ссылки передаются открытым текстом.

## Как это работает

1. Magnet-ссылка разбирается, из неё извлекается v1 info hash (`btih`, hex или base32).
2. Metadata fetcher добавляет ссылку в torrent-клиент anacrolix/torrent.
3. Клиент ищет пиров через DHT (и трекеры, если они не отключены), обменивается extended
   handshake и получает info-словарь.
4. Info-словарь вместе с announce-list кодируется в полноценный bencode `.torrent`.
5. Разовый режим сохраняет файл и завершается. Демон сохраняет файл в
   `torrents/<info-hash>.torrent`, обновляет задание в bbolt и освобождает воркер.

Демон не запрашивает куски файлов и не участвует в раздаче, поэтому на диск попадает только
torrent-файл.

Ограничения:

- поддерживаются magnet-ссылки с v1 info hash (`urn:btih:`); v2-only (`urn:btmh:`) пока
  не поддерживаются;
- для DHT нужен исходящий UDP и желательно входящий UDP/TCP на выбранном порту;
- если у торрента нет доступных пиров, метаданные получить нельзя — задание будет
  повторяться согласно настройкам, пока его не отменят;
- приватные торренты без DHT/трекеров найти нельзя.

## Лицензия

Проект распространяется под лицензией **MIT**. Полный текст — в файле
[LICENSE](LICENSE).

Сторонние зависимости сохраняют свои лицензии. Список использованных модулей,
их версий, copyright-уведомлений и тексты лицензий находятся в
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md). В частности, компоненты
`anacrolix/torrent`, `anacrolix/dht` и ряд вспомогательных модулей `anacrolix`
распространяются под MPL-2.0; их исходный код не изменялся, а ссылки на исходники
и версии перечислены в notices.
