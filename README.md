# fleeting-plugin-yandex

[Fleeting plugin](https://docs.gitlab.com/runner/executors/docker_autoscaler/) для GitLab Runner (executor-ы
`docker-autoscaler` и `instance`), который создаёт и удаляет виртуальные машины в
[Yandex Compute Cloud](https://yandex.cloud/ru/services/compute) под нагрузку CI-джобов. Поддерживаются Linux (SSH)
и Windows (WinRM, статический пароль).

## Как это работает

Плагин реализует интерфейс `provider.InstanceGroup` из `gitlab.com/gitlab-org/fleeting/fleeting` и запускается
GitLab Runner-ом как отдельный процесс. С облаком работает через официальный
[go-sdk v2](https://github.com/yandex-cloud/go-sdk); весь доступ к SDK спрятан за узким интерфейсом в
`internal/yandexapi`.

Instance Groups не используются: плагин сам создаёт и удаляет отдельные ВМ. Свои машины он узнаёт по label
`fleeting-group=<name>` (плюс информационный `managed-by=fleeting-plugin-yandex`) внутри каталога `folder_id`.
Имя ВМ — `<name>-<8 hex>`, но принадлежность группе определяет именно label, поэтому отдельный каталог под раннер
не обязателен — достаточно уникального `name`. Не снимайте label руками: машина выпадет из группы и будет биллиться,
пока её не удалят вручную.

Публичный адрес (`nat = true`) эфемерный и освобождается вместе с ВМ, загрузочный диск создаётся с `auto_delete` —
после `Decrease` в каталоге ничего не остаётся.

## Сервисный аккаунт

Плагину нужен сервисный аккаунт с ролями на каталог:

| роль | зачем |
|---|---|
| `compute.editor` | создавать, читать и удалять ВМ и диски |
| `vpc.user` | подключать ВМ к подсети и группам безопасности |
| `vpc.publicAdmin` | только при `nat = true` — выдавать публичный адрес |
| `iam.serviceAccounts.user` | только при `service_account_id` — привязывать аккаунт к ВМ |
| `compute.images.user` на каталог с образом | только если образ лежит в другом каталоге |

```shell
yc iam service-account create --name gitlab-fleeting
yc resource-manager folder add-access-binding <folder_id> \
  --role compute.editor --subject serviceAccount:<sa_id>
yc resource-manager folder add-access-binding <folder_id> \
  --role vpc.user --subject serviceAccount:<sa_id>
yc iam key create --service-account-name gitlab-fleeting --output yc-key.json
```

`yc-key.json` (authorized key) положите на хост раннера с правами `0600` и укажите в `service_account_key_file`
либо в переменной окружения `YC_SERVICE_ACCOUNT_KEY_FILE` (она важнее конфига). SDK сам меняет ключ на IAM-токен и
обновляет его.

Если ключ не задан, плагин берёт сервисный аккаунт, привязанный к ВМ, на которой он запущен (через metadata) — это
режим для раннер-менеджера, живущего внутри Yandex Cloud. Вне облака без ключа первый же запрос упадёт с ошибкой
авторизации.

## Сборка

```shell
go build -o fleeting-plugin-yandex ./cmd/fleeting-plugin-yandex
```

## Деплой

На каждый тег `X.Y.Z` GitHub Actions собирает бинари под `linux`/`darwin` × `amd64`/`arm64`, а для Linux ещё и
пакеты `deb`/`rpm`, и прикладывает всё к
[релизу](https://github.com/AlexeySetevoi/yandex-fleeting-plugin/releases).

Пакет кладёт бинарь в `/usr/bin/fleeting-plugin-yandex`, поэтому в конфиге раннера достаточно имени:
`plugin = "fleeting-plugin-yandex"`.

```shell
# Debian/Ubuntu
curl -fsSLO "https://github.com/AlexeySetevoi/yandex-fleeting-plugin/releases/download/<версия>/fleeting-plugin-yandex_<версия>_amd64.deb"
sudo dpkg -i fleeting-plugin-yandex_<версия>_amd64.deb

# RHEL/Rocky/Alma
sudo rpm -Uvh "https://github.com/AlexeySetevoi/yandex-fleeting-plugin/releases/download/<версия>/fleeting-plugin-yandex-<версия>-1.x86_64.rpm"
```

Для arm64 — `_arm64.deb` и `.aarch64.rpm`. Пока репозиторий приватный, скачивание требует токен
(`gh release download <версия> -p '*.deb'`).

Либо просто бинарь:

```shell
curl -fsSL -o fleeting-plugin-yandex \
  "https://github.com/AlexeySetevoi/yandex-fleeting-plugin/releases/download/<версия>/fleeting-plugin-yandex-linux-amd64"
chmod +x fleeting-plugin-yandex
```

### Проверка подлинности

Ключей GPG у проекта нет — используется то, что даёт сам GitHub:

- **аттестация происхождения** (artifact attestation, Sigstore): на каждый файл релиза — бинари, `deb`, `rpm`,
  `SHA256SUMS` — workflow выпускает подпись, привязанную к репозиторию, workflow, коммиту и тегу;
- **неизменяемые релизы**: тег и файлы после публикации подменить нельзя, GitHub сам выпускает аттестацию релиза;
- `SHA256SUMS` — для проверки целостности без `gh`.

```shell
gh attestation verify fleeting-plugin-yandex_<версия>_amd64.deb -R AlexeySetevoi/yandex-fleeting-plugin
gh release verify <версия> -R AlexeySetevoi/yandex-fleeting-plugin
gh release verify-asset <версия> fleeting-plugin-yandex_<версия>_amd64.deb -R AlexeySetevoi/yandex-fleeting-plugin
sha256sum -c SHA256SUMS --ignore-missing
```

Нужен свежий `gh` с [cli.github.com](https://cli.github.com/): в 2.46 из репозитория Ubuntu команд `attestation` и
`release verify` ещё нет.

Это не GPG-подпись внутри пакета: `rpm -K`, `dnf` и `apt` её не видят, проверка — только через `gh`.

1. Поставить пакет либо положить бинарь на хост, где крутится `gitlab-runner` (например,
   `/etc/gitlab-runner/plugins/fleeting-plugin-yandex`, `root:root`, `0755`).
2. Указать в `plugin` секции `[runners.autoscaler]` имя (для пакета) или полный путь (для бинаря).
3. `systemctl restart gitlab-runner` — новый бинарь плагина подхватывается только при рестарте раннера. Правки
   самого `config.toml` раннер перечитывает сам.

## Конфигурация (`plugin_config`)

| поле | обязательное | описание |
|---|---|---|
| `name` | да | имя группы: префикс имён ВМ и значение label. `^[a-z][-a-z0-9]*$`, до 54 символов |
| `folder_id` | да | каталог, в котором создаются ВМ |
| `service_account_key_file` | нет | путь к authorized key; см. «Сервисный аккаунт» |
| `zone` | да* | зона доступности, например `ru-central1-a` |
| `subnet_id` | да* | подсеть **в этой же зоне** |
| `platform_id` | нет | платформа, по умолчанию `standard-v3` |
| `placements` | да* | список `{zone, subnet_id, platform_id}` вместо трёх полей выше, см. ниже |
| `cores` | да | число vCPU |
| `memory_gb` | да | RAM в ГБ, можно дробное (`0.5`) |
| `core_fraction` | нет | гарантированная доля vCPU в %, по умолчанию `100` |
| `gpus` | нет | число GPU (нужна GPU-платформа) |
| `image_id` | да** | ID образа |
| `image_family` | да** | семейство образа; последний образ ищется при каждом создании ВМ |
| `image_folder_id` | нет | каталог семейства, по умолчанию `standard-images` (публичные образы) |
| `disk_type` | нет | тип загрузочного диска, по умолчанию `network-ssd` |
| `disk_size_gb` | да | размер загрузочного диска, не меньше минимального для образа |
| `nat` | нет | выдать публичный адрес |
| `security_group_ids` | нет | группы безопасности интерфейса |
| `service_account_id` | нет | сервисный аккаунт, привязываемый к ВМ (например, для pull из Container Registry) |
| `preemptible` | нет | прерываемые ВМ, см. ниже |
| `labels` | нет | дополнительные labels; `fleeting-group` и `managed-by` зарезервированы |
| `metadata` | нет | дополнительные ключи metadata; `ssh-keys` и `user-data` зарезервированы |
| `user_data` / `user_data_file` | нет | cloud-init (Linux) или `#ps1`-скрипт (Windows); файл читается один раз при старте |

\* либо `zone` + `subnet_id` (+ `platform_id`), либо `placements`. \*\* ровно одно из двух.

Набор допустимых сочетаний `cores` / `memory_gb` / `core_fraction` зависит от платформы — см.
[уровни производительности](https://yandex.cloud/ru/docs/compute/concepts/performance-levels). Недопустимое
сочетание API отклонит при создании ВМ, ошибка будет в логе раннера.

## Фолбэк по зонам и платформам (`placements`)

```toml
[runners.autoscaler.plugin_config]
  # ...
  placements = [
    { zone = "ru-central1-a", subnet_id = "e9b..." },
    { zone = "ru-central1-b", subnet_id = "e2l...", platform_id = "standard-v2" },
  ]
```

Для каждой новой ВМ варианты пробуются по порядку. К следующему плагин переходит только если облако ответило
`RESOURCE_EXHAUSTED` — не хватило ресурсов зоны/платформы или квоты; любая другая ошибка (права, неверный образ,
несуществующая подсеть) от смены зоны не лечится и возвращается сразу. Нехватка ресурсов зоны приходит не в ответе
на запрос, а в результате операции создания, поэтому `Increase` дожидается операций (до 5 ВМ создаются параллельно).

Что проверено на живом облаке:

- превышение квоты (`The limit on maximum number of cores has exceeded`) приходит как `RESOURCE_EXHAUSTED` сразу в
  ответе на запрос. Фолбэк на неё сработает, но не поможет: квота общая на все зоны облака, следующий вариант упадёт
  с той же ошибкой;
- платформа, которой нет в зоне (`platform "standard-v1" is unavailable in zone "ru-central1-d"`), — это
  `FAILED_PRECONDITION`, то есть ошибка конфигурации: перехода к следующему варианту не будет, ошибка видна в логе
  раннера при каждой попытке.

Настоящую нехватку ресурсов зоны по заказу не воспроизвести, поэтому её код вживую не подтверждён. Если облако
ответит на неё не `RESOURCE_EXHAUSTED`, код будет виден в ошибке `Increase`, а условие перехода задаётся в одном
месте — `mapError` в `internal/yandexapi/sdk.go`.

## Пример: Linux-раннер по SSH

```toml
[[runners]]
  name = "yandex-docker-autoscaler"
  executor = "docker-autoscaler"

  [runners.docker]
    image = "alpine:latest"

  [runners.autoscaler]
    plugin = "/etc/gitlab-runner/plugins/fleeting-plugin-yandex"

    capacity_per_instance = 1
    max_use_count = 10
    max_instances = 10
    instance_ready_command = "cloud-init status --wait || test $? -eq 2"

    [runners.autoscaler.plugin_config]
      name                     = "ci-linux"
      folder_id                = "b1g..."
      service_account_key_file = "/etc/gitlab-runner/yc-key.json"
      zone                     = "ru-central1-a"
      subnet_id                = "e9b..."
      cores                    = 4
      memory_gb                = 8
      image_family             = "ubuntu-2404-lts"
      disk_size_gb             = 50
      nat                      = true
      user_data_file           = "/etc/gitlab-runner/docker-cloud-init.yaml"

    [runners.autoscaler.connector_config]
      username = "ubuntu"
      protocol = "ssh"
      use_external_addr = true

    [[runners.autoscaler.policy]]
      idle_count = 1
      idle_time  = "20m0s"
```

SSH-ключ: плагин при старте генерирует пару ed25519 и кладёт публичную часть в metadata `ssh-keys` каждой ВМ в виде
`<username>:<ключ>` — cloud-init образа создаёт этого пользователя с sudo. Вход под `root` на стандартных образах
закрыт, поэтому `username` по умолчанию `ubuntu`. Свой ключ — `use_static_credentials = true` + `key_path` в
`connector_config`: плагин возьмёт из него публичную часть.

Включённый на уровне организации OS Login этому не мешает (проверено вживую): он действует только на ВМ с metadata
`enable-oslogin=true`, а плагин её не ставит. Не добавляйте этот ключ в `metadata` — ключ из `ssh-keys` перестанет
приниматься.

`use_external_addr = true` нужен, когда раннер-менеджер снаружи облака и ходит на ВМ по публичному адресу (тогда
обязателен и `nat = true`). Если менеджер в той же сети VPC — уберите оба: подключение пойдёт по внутреннему адресу,
публичные адреса не тратятся.

`instance_ready_command` обязателен, если Docker ставится через cloud-init: ВМ переходит в `RUNNING` раньше, чем
cloud-init закончит работу, и без проверки первая джоба упадёт на отсутствующем Docker.

### Docker через cloud-init

```yaml
#cloud-config
package_update: true
packages:
  - docker.io
runcmd:
  - usermod -aG docker ubuntu
  - systemctl enable --now docker
```

Быстрее и надёжнее — собрать свой образ с уже установленным Docker (Packer) и указать его через `image_id` или
`image_family` + `image_folder_id`.

## Приватная сеть: воркеры без публичных адресов

Если раннер-менеджер живёт в той же сети VPC (или ходит в неё по VPN), публичные адреса воркерам не нужны: уберите
`nat` из `plugin_config` и `use_external_addr` из `connector_config` — подключение пойдёт по внутреннему адресу.
Чтобы воркеры при этом ходили наружу (`apt`, `docker pull`, клонирование репозиториев), подсети нужен NAT-шлюз:

```shell
yc vpc gateway create --name ci-runner-nat
yc vpc route-table create --name ci-runner-nat --network-name <сеть> \
  --route destination=0.0.0.0/0,gateway-id=<gateway_id>
yc vpc subnet update <подсеть> --route-table-id <route_table_id>
```

Таблица маршрутов привязывается к каждой подсети отдельно — при `placements` в нескольких зонах не забудьте про все.

Менеджеру внутри облака ключ не нужен: привяжите сервисный аккаунт к его ВМ и не задавайте
`service_account_key_file` — плагин возьмёт токен из metadata-сервиса.

Схема проверена вживую: smoke-тест с ВМ внутри сети, без ключа и без `nat`, вход на воркер по внутреннему адресу,
`curl ifconfig.me` с воркера возвращает адрес NAT-шлюза. Менеджеру снаружи облака (без VPN) эта схема недоступна —
ему нужны `nat = true` и `use_external_addr = true`.

## Прерываемые ВМ (`preemptible`)

Прерываемые ВМ в разы дешевле, но облако может остановить их в любой момент и обязательно останавливает через
24 часа. Остановленная машина получает статус `STOPPED`; плагин отдаёт для неё состояние `timeout`, и taskscaler
удаляет её и при необходимости создаёт новую. Джоба, выполнявшаяся на прерванной машине, упадёт — для таких раннеров
имеет смысл `retry` в `.gitlab-ci.yml` и небольшой `max_use_count`.

По той же причине не останавливайте ВМ группы руками — плагин машины только создаёт и удаляет, остановленная будет
снесена.

## Пример: Windows-раннер по WinRM

Compute API не генерирует и не отдаёт пароль администратора, поэтому для WinRM обязателен
`use_static_credentials = true` — без него плагин не стартует. Пароль должен оказаться в ВМ одним из двух способов:

- зашит в ваш образ (вместе с включённым WinRM);
- задаётся при первом запуске через `user_data` — если в образе есть агент, исполняющий user-data (cloudbase-init):

```powershell
#ps1
net user Administrator "S3cret-Passw0rd"
Enable-PSRemoting -Force
Set-Item WSMan:\localhost\Service\Auth\Basic $true
Set-Item WSMan:\localhost\Service\AllowUnencrypted $true
netsh advfirewall firewall add rule name="WinRM" dir=in action=allow protocol=TCP localport=5985
```

```toml
    [runners.autoscaler.plugin_config]
      name           = "ci-windows"
      folder_id      = "b1g..."
      service_account_key_file = "/etc/gitlab-runner/yc-key.json"
      zone           = "ru-central1-a"
      subnet_id      = "e9b..."
      cores          = 8
      memory_gb      = 16
      image_id       = "fd8..."          # свой Windows-образ
      disk_size_gb   = 100
      nat            = true
      user_data_file = "/etc/gitlab-runner/windows-user-data.ps1"

    [runners.autoscaler.connector_config]
      protocol = "winrm"
      username = "Administrator"
      password = "S3cret-Passw0rd"
      use_static_credentials = true
      use_external_addr = true
```

Пароль в user-data виден всем, у кого есть доступ к metadata ВМ и к каталогу; WinRM по HTTP — только внутри
доверенной сети или за группой безопасности, пускающей порт 5985 лишь с адреса раннер-менеджера. Windows-образов в
маркетплейсе Yandex Cloud сейчас нет — нужен собственный образ с вашей лицензией.

## Расписание: тёплая машина в рабочие часы

Сколько машин держать в простое, решает не плагин, а сам GitLab Runner через `[[runners.autoscaler.policy]]` с
полями `periods` (unix-cron) и `timezone`:

```toml
# базовая политика: ночью и в выходные скейлимся до нуля
[[runners.autoscaler.policy]]
  idle_count = 0
  idle_time  = "20m0s"

# будни 9:00–17:59 МСК: одна машина всегда тёплая
[[runners.autoscaler.policy]]
  periods    = ["* 9-17 * * mon-fri"]
  timezone   = "Europe/Moscow"
  idle_count = 1
  idle_time  = "10h0m0s"
```

Применяется последний совпавший блок. **Грабли:** taskscaler отсчитывает `idle_time` от создания ВМ, а не от её
готовности. Если образ разворачивается долго (Windows), а `idle_time` сопоставим с этим временем, тёплая машина будет
сноситься и пересоздаваться по кругу. В «тёплом» блоке ставьте `idle_time` больше самого окна — вечером машину удалит
базовая политика. С `preemptible` тёплая машина всё равно проживёт не дольше 24 часов.

## CI/CD

`.github/workflows/ci.yml`: `fmt`/`vet`/`test` (с `-race`, покрытие — в summary джобы) на каждый пуш в `main` и на
pull request, кросс-сборка (`linux/darwin` × `amd64/arm64`) и `deb`/`rpm` для Linux через
[nfpm](https://nfpm.goreleaser.com/) (`nfpm.yaml`), на тег `X.Y.Z` — GitHub Release с бинарями и пакетами.

Выпуск версии: `git tag 1.0.0 && git push origin 1.0.0`. Создавать релиз руками в интерфейсе не нужно и нельзя:
релизы неизменяемые (immutable releases), workflow публикует релиз сразу со всеми файлами, а к уже опубликованному
файлы не добавить.

## Suspend/Resume

Не поддерживаются: `Decrease` всегда удаляет ВМ.

## Разработка

```shell
go build ./...
go vet ./...
go test ./... -count=1 -race
```

Тесты работают без доступа к облаку:

- `provider.InstanceGroup` — на фейковой реализации `yandexapi.Compute`: отбор своих ВМ по label, сборка запроса
  (labels, metadata, размеры, значения по умолчанию), фолбэк по `placements`, частичный успех `Increase`, `Decrease`
  с уже удалённой ВМ, ошибки `Init`;
- `internal/yandexapi` — через настоящий go-sdk к локальному gRPC-серверу с фейковыми Instance/Image/Operation-сервисами
  (аналог httptest): пагинация, передача IAM-токена, содержимое `CreateInstanceRequest`, ожидание операции, ошибка в
  результате операции, отмена по контексту;
- валидация конфига и маппинг статусов, включая сверку с enum из API — новый статус в SDK уронит тест, а не будет
  молча пропускаться в `Update`.

Покрытие: `go test ./... -coverprofile=coverage.out && go tool cover -func=coverage.out`.

### Смоук-тест на реальном каталоге

Фейковый сервер не знает, как ведёт себя настоящее облако (коды ошибок при нехватке ресурсов, время до `RUNNING`,
работа `ssh-keys` на конкретном образе) — это проверяет `cmd/smoketest`: Init →
Increase(1) → ожидание `RUNNING` → ConnectInfo → реальный вход по SSH/WinRM тем же пакетом `fleeting/connector`,
что использует GitLab Runner → Decrease → Shutdown. Создаёт настоящую ВМ (и удаляет её, если не задан `-keep`).

```shell
YC_SERVICE_ACCOUNT_KEY_FILE=yc-key.json go run ./cmd/smoketest \
  -folder-id=b1g... -zone=ru-central1-a -subnet-id=e9b... \
  -image-family=ubuntu-2404-lts -nat
```

Реальные коды ошибок удобно смотреть одиночными прогонами с заведомо невыполнимым запросом — ВМ при этом не
создаётся:

```shell
... -cores=80 -memory-gb=80 -core-fraction=100        # сверх квоты -> RESOURCE_EXHAUSTED
... -zone=ru-central1-d -subnet-id=... -platform-id=standard-v1   # платформы нет в зоне -> FAILED_PRECONDITION
```

Остальные флаги — `go run ./cmd/smoketest -h`.
