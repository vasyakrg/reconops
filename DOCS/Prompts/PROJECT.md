# Recon — распределённая диагностическая система с ИИ-исследователем

**Статус:** design document, версия 0.1
**Дата:** 17 апреля 2026
**Рабочее название:** *Recon* (от reconnaissance — разведка). Название рабочее, меняется по вкусу.

---

## 1. Обзор и цели

### 1.1. Назначение

Recon — система для централизованного сбора диагностических данных с распределённого парка Linux-хостов и их анализа с помощью LLM-исследователя. Пользователь не ходит по машинам, а формулирует инцидент как цель для Recon: *«разберись, почему на k8s-кластере перестали запускаться cronjobs»*. Система пошагово проводит расследование, опрашивая агентов, накапливая findings, и выдаёт итоговый отчёт.

Ключевое отличие от оркестраторов (Ansible, Salt): Recon **строго read-only**. Он только наблюдает, никогда не модифицирует. Это делает его безопасным на критичных средах и устраняет страх запускать его в проде.

### 1.2. Ключевые свойства

- **Read-only по построению.** У агента в API нет ни одного глагола, способного изменить состояние системы. Это свойство протокола, а не политика.
- **Структурированные данные.** Каждый коллектор возвращает типизированный JSON по известной схеме.
- **Агентный LLM-слой.** Claude исследует инцидент пошагово, используя коллекторы как tools.
- **Step-by-step под контролем человека.** Каждый tool call LLM проходит через одобрение оператора.
- **On-demand.** Никакой непрерывной телеметрии; результат = снапшот по запросу.
- **Минимум зависимостей.** Один бинарь сервера + один бинарь агента, деплой за 10 минут.

### 1.3. Что Recon НЕ делает

- Не исполняет команды, меняющие состояние системы. Нет `Exec`, `Run`, `Write`, `Delete`.
- Не собирает метрики непрерывно (для time-series — Prometheus).
- Не заменяет `kubectl`, `sosreport`, `must-gather` — он их дополняет.
- Не управляет конфигурацией (Ansible/Salt/Chef).
- Не хранит память между расследованиями (в MVP).

### 1.4. Целевой масштаб (MVP)

- До ~50 агентов на один hub (первая цель — 10).
- Одна активная investigation на пользователя.
- Один пользователь-оператор (multi-user — после MVP).
- Claude API внешний; локальные модели — вне MVP.

### 1.5. Глоссарий

| Термин | Значение |
|--------|----------|
| **Hub** | Серверный компонент: gRPC-endpoint для агентов, web-UI, SQLite, хранилище артефактов, investigator |
| **Agent** | Клиентский бинарь на целевом хосте |
| **Collector** | Модуль в агенте, выполняющий один тип read-only наблюдения |
| **Run** | Прогон набора коллекторов на наборе хостов в момент времени |
| **Investigation** | LLM-сессия расследования инцидента; управляет Run'ами изнутри |
| **Finding** | Структурированный вывод, сформулированный LLM на основе собранных данных |
| **Hint** | Предварительное «подозрение», эмитимое коллектором (не LLM), нижний уровень эвристик |
| **Step** | Один шаг investigation — предложение tool_call'а от Claude + решение оператора |

---

## 2. Архитектурная схема

```
┌────────────────────────────────────────────────────────────┐
│                         Browser                            │
│  (Web UI: HTMX + Go templates + SSE)                       │
└────────────────────┬───────────────────────────────────────┘
                     │ HTTPS
┌────────────────────▼───────────────────────────────────────┐
│                          HUB                               │
│  ┌─────────────┐  ┌──────────────┐  ┌─────────────────┐    │
│  │  Web layer  │  │ Investigator │  │  gRPC server    │    │
│  │  (HTMX/SSE) │──│  (LLM loop)  │──│  (for agents)   │    │
│  └──────┬──────┘  └──────┬───────┘  └────────┬────────┘    │
│         └────────────────┼───────────────────┘             │
│  ┌──────────────────────▼──────────────────────────────┐   │
│  │         Store: SQLite + FS (artifacts)              │   │
│  └─────────────────────────────────────────────────────┘   │
│  ┌───────────────────────┬────────────────────────────┐    │
│  │   LLM client (OpenAI-compat → OpenRouter etc.)     │    │
│  └────────────────────────────────────────────────────┘    │
└────────────────────┬───────────────────────────────────────┘
                     │ gRPC + mTLS  (long-lived bidi streams)
          ┌──────────┼──────────┐
          │          │          │
    ┌─────▼────┐ ┌───▼────┐ ┌───▼────┐
    │ Agent 1  │ │Agent 2 │ │Agent N │
    │ (Host A) │ │(Host B)│ │(Host X)│
    └──────────┘ └────────┘ └────────┘
```

### 2.1. Основные потоки данных

**Регистрация агента.** Агент при первом запуске читает bootstrap-токен и CA hub'а из конфига, делает POST `/enroll`, получает клиентский сертификат. Дальше — только mTLS, токен больше не нужен.

**Постоянный канал.** Агент открывает bidirectional stream `Hub/Connect`. Шлёт `Hello` с лейблами и манифестами коллекторов, потом периодический `Heartbeat`. Сервер через тот же стрим пушит `CollectRequest`.

**Investigation flow.** Оператор создаёт investigation с целью. Hub начинает диалог с Claude, отдавая tool-schemas. Claude предлагает tool call → hub сохраняет как `pending` → оператор одобряет → hub вызывает `collect()` → агент исполняет коллектор → результат возвращается → `tool_result` отдаётся Claude → цикл продолжается до `mark_done`.

**Artifact streaming.** Большие дампы (journal, iptables-save) не умещаются в одно сообщение: агент стримит их как `ArtifactChunk`, hub пишет на диск, в БД сохраняет путь.

---

## 3. Agent

### 3.1. Назначение

Агент — статический Go-бинарь, который подключается к hub'у по mTLS, анонсирует свои коллекторы и лейблы, принимает запросы на сбор данных, исполняет коллекторы и возвращает структурированные результаты.

### 3.2. Конфигурация

`/etc/recon/agent.yaml`:

```yaml
hub:
  endpoint: hub.example.com:9443
  ca_cert:  /etc/recon/hub-ca.pem
  cert:     /etc/recon/agent.pem
  key:      /etc/recon/agent.key
identity:
  id: node-3.cluster.local
  labels:
    env: prod
    role: k8s-master
    dc: msk-1
runtime:
  max_concurrent_collectors: 4
  artifact_dir: /var/lib/recon/artifacts
  default_timeout: 30s
```

Плюс **auto-facts**, вычисляемые на старте: `os`, `os_version`, `kernel`, `hostname`, `primary_ip`, `cpu_count`, `ram_gb`. Они автоматически попадают в лейблы.

### 3.3. Контракт коллектора

```go
package collect

type Collector interface {
    Manifest() Manifest
    Run(ctx context.Context, p Params) (Result, error)
}

type Manifest struct {
    Name         string         // "net_listen"
    Version      string         // "1.0.0"
    Category     string         // "network"
    Description  string         // human-readable, используется LLM
    Reads        []string       // что коллектор читает: "/proc/net/tcp", "ss -tulpn"
    Requires     []Capability   // SUDO_SS, CAP_DAC_READ_SEARCH, CAP_SYSLOG (kernel ring)
    ParamsSchema []ParamSpec    // для UI и LLM
    OutputType   reflect.Type   // для генерации JSON schema
}

type Result struct {
    Data      any          // сериализуется в JSON
    Artifacts []Artifact   // опционально, большие файлы
    Hints     []Hint       // предварительные подозрения
    Stderr    string       // если коллектор вызывал внешнюю команду
    ExitCode  int
}

type Hint struct {
    Severity string       // info | warn | error
    Code     string       // "service.inactive"
    Message  string
    Evidence any
}
```

### 3.4. Read-only: пять слоёв защиты

1. **Протокол.** В proto-схеме gRPC есть только `Collect`. Ни одного глагола для изменения. Невозможно выразить деструктив через API.
2. **Каталог.** Коллекторы вкомпилированы в бинарь. Нет механизма подгрузки кода извне. Новый коллектор = новый релиз агента. Дифференциальная методология инвестигатора (§7.7) намеренно остаётся на уровне рассуждения и не добавляет коллектор — она опирается на уже существующую logtriage-кластеризацию, поэтому новый релиз агента не нужен.
3. **Exec gateway.** Все вызовы внешних команд проходят через `internal/agent/exec/readonly.go` с whitelist бинарников и whitelist'ом форм аргументов. Попытка вызвать что-то вне списка → panic на старте.
4. **ОС-уровень.** Systemd-юнит запускает агента под пользователем `recon` с минимальными capabilities. Для `journalctl`, `ss`, `iptables -L` — точечные sudoers-записи с конкретными аргументами, без подстановок.
5. **CI-проверка.** Go-линтер запрещает в пакете `collectors/` импорт `os.Remove`, `os.OpenFile` с WRITE-флагами, `syscall.Unlink`, `exec.Command` минуя gateway.

Каждый слой удерживает свойство независимо. Чтобы произошёл деструктив, должны упасть все пять.

---

## 4. Hub

### 4.1. Подсистемы

- **gRPC API** для агентов: регистрация, стрим `Connect`, приём результатов и артефактов.
- **HTTP API + Web UI** для оператора.
- **Investigator** — сервис, ведущий диалог с Claude и планирующий tool calls.
- **Store** — SQLite для метаданных + filesystem для артефактов.
- **Auth** — bootstrap-токены, mTLS-сертификаты, аудит.

### 4.2. Конфигурация

`/etc/recon/hub.yaml`:

```yaml
server:
  grpc_addr: :9443
  http_addr: :8080
  tls_cert: /etc/recon/hub.pem
  tls_key:  /etc/recon/hub.key
  client_ca: /etc/recon/clients-ca.pem
storage:
  db_path: /var/lib/recon/recon.db
  artifact_dir: /var/lib/recon/artifacts
  retention_days: 30
llm:
  base_url: https://openrouter.ai/api/v1     # any OpenAI-compatible endpoint
  model: anthropic/claude-sonnet-4.5
  api_key_env: RECON_LLM_API_KEY
  http_referer: https://recon.example.com    # optional, OpenRouter ranking
  x_title: Recon                             # optional, OpenRouter ranking
  max_steps_per_investigation: 40
  max_tokens_per_investigation: 500000
auth:
  bootstrap_tokens_file: /etc/recon/bootstrap.tokens
  admin_users: [vasyansk]
```

### 4.3. Самостоятельный хостинг агента (self-hosted distribution)

По умолчанию агент распространяется через **GitHub Releases** (install-скрипт,
self-update, бейдж «устарел»). Чтобы хаб работал **без доступа к GitHub**,
включается режим self-hosted — хаб сам раздаёт агента.

- **Сборка в образ.** Dockerfile кросс-компилирует агента под `amd64` и `arm64`,
  пакует `recon-agent-linux-<arch>.tar.gz` (та же раскладка, что у `make
  dist-agent`: `recon-agent-linux-<arch>/bin/recon-agent` + `deploy/systemd` +
  `deploy/sudoers`) и `checksums.txt` в невольюмный путь
  `/usr/local/share/recon/releases`. Агент версионируется тем же LDFLAGS, что и
  хаб, — раздаваемая версия совпадает с запущенным хабом.
- **Раздача.** Публичные (без auth, как `/install/agent.sh`) маршруты:
  - `GET /releases/latest/download/<asset>` и `GET /releases/download/<tag>/<asset>`
    — тарболлы и `checksums.txt` из baked-каталога (`filepath.Clean` +
    prefix-guard, whitelist арки, неизвестное → 404);
  - `GET /releases/latest` — JSON в форме GitHub Releases API
    (`{tag_name, draft:false, prerelease:false, assets:[{name,
    browser_download_url}]}`), `tag_name` = версия baked-агента, ссылки активов
    указывают на сам хаб (от `install.external_url` либо из запроса). Это
    позволяет self-updater'у агента переиспользовать GitHub-JSON-парсер.
- **Переключатель.** `install.self_hosted: true` (env `RECON_INSTALL_SELF_HOSTED`).
  В этом режиме install-one-liner и `update.repo_url` агента указывают на базу
  хаба (`external_url` + `/releases/...`); `release_repo_url` можно не задавать.
  GitHub-путь остаётся выбираемым (`self_hosted: false`).
- **Self-updater агента.** Источник релиза определяется автоматически по
  `update.repo_url`: GitHub-URL → GitHub Releases API; любой другой `http(s)`-URL
  трактуется как база хаба → `<base>/releases/latest`. Весь путь
  скачивание → проверка SHA256 по `checksums.txt` → атомарная замена бинаря
  переиспользуется без изменений.
- **Бейдж «устарел».** В self-hosted источником «последней версии» служит
  версия baked-агента (без `api.github.com`). Требование: `RECON_VERSION` должен
  быть валидным semver — не-semver-тег (включая дефолт `docker`) схлопывается в
  `0.0.0` в `version.Outdated` и тихо отключает сравнение версий.
- **Инварианты.** Read-only сохранён (§3.4): хаб только *раздаёт* read-артефакты,
  никаких мутирующих verb/коллекторов; проверка SHA256 всех загрузок сохранена —
  хаб отдаёт настоящий `checksums.txt`.

---

## 5. Протокол агент ↔ hub (proto sketch)

```proto
syntax = "proto3";
package recon.v1;

service Hub {
  rpc Enroll(EnrollRequest) returns (EnrollResponse);
  rpc Connect(stream AgentMsg) returns (stream HubMsg);
}

message EnrollRequest {
  string bootstrap_token = 1;
  string agent_id        = 2;
  bytes  csr_pem         = 3;
}
message EnrollResponse {
  bytes client_cert_pem = 1;
  bytes hub_ca_pem      = 2;
}

message AgentMsg {
  oneof payload {
    Hello         hello     = 1;
    Heartbeat     heartbeat = 2;
    CollectResult result    = 3;
    ArtifactChunk artifact  = 4;
  }
}

message HubMsg {
  oneof payload {
    CollectRequest collect = 1;
    CancelRequest  cancel  = 2;
    ConfigUpdate   config  = 3;
  }
}

message Hello {
  string agent_id                    = 1;
  string version                     = 2;
  map<string,string> labels          = 3;
  repeated CollectorManifest collectors = 4;
}

message CollectRequest {
  string request_id              = 1;
  string collector               = 2;
  map<string,string> params      = 3;
  int32  timeout_seconds         = 4;
}

message CollectResult {
  string request_id               = 1;
  Status status                   = 2;
  bytes  data_json                = 3;
  string error                    = 4;
  repeated string artifact_refs   = 5;
  repeated Hint hints             = 6;
  string stderr                   = 7;
  int32  exit_code                = 8;
  int64  duration_ms              = 9;
}

enum Status {
  STATUS_UNSPECIFIED = 0;
  STATUS_OK          = 1;
  STATUS_ERROR       = 2;
  STATUS_TIMEOUT     = 3;
  STATUS_CANCELED    = 4;
}

message ArtifactChunk {
  string request_id  = 1;
  string artifact_id = 2;
  string name        = 3;   // "journalctl.kubelet.txt"
  string mime        = 4;
  int64  offset      = 5;
  bytes  data        = 6;
  bool   last        = 7;
}
```

**Важное свойство протокола:** среди всех сообщений нет ни одного, которое могло бы изменить состояние агента. Это оформлено как часть контракта и проверяемо глазами.

---

## 6. Модель данных

```sql
-- Хосты (агенты)
CREATE TABLE hosts (
  id                TEXT PRIMARY KEY,
  agent_version     TEXT,
  labels_json       TEXT NOT NULL,
  facts_json        TEXT NOT NULL,
  cert_fingerprint  TEXT NOT NULL,
  first_seen_at     DATETIME NOT NULL,
  last_seen_at      DATETIME NOT NULL,
  status            TEXT NOT NULL          -- online | offline | degraded
);

CREATE TABLE collector_manifests (
  host_id        TEXT NOT NULL,
  name           TEXT NOT NULL,
  version        TEXT NOT NULL,
  manifest_json  TEXT NOT NULL,
  PRIMARY KEY (host_id, name)
);

-- Прогоны и задачи
CREATE TABLE runs (
  id                TEXT PRIMARY KEY,
  investigation_id  TEXT,                  -- null для ручных runs
  name              TEXT,
  selector_json     TEXT,
  created_by        TEXT,
  created_at        DATETIME NOT NULL,
  finished_at       DATETIME,
  status            TEXT NOT NULL
);

CREATE TABLE tasks (
  id            TEXT PRIMARY KEY,
  run_id        TEXT NOT NULL,
  host_id       TEXT NOT NULL,
  collector     TEXT NOT NULL,
  params_json   TEXT,
  status        TEXT NOT NULL,
  started_at    DATETIME,
  finished_at   DATETIME,
  duration_ms   INTEGER,
  error         TEXT
);

CREATE TABLE results (
  task_id       TEXT PRIMARY KEY,
  data_json     TEXT,
  hints_json    TEXT,
  stderr        TEXT,
  exit_code     INTEGER,
  artifact_dir  TEXT                       -- путь в FS
);

-- Расследования
CREATE TABLE investigations (
  id                TEXT PRIMARY KEY,
  goal              TEXT NOT NULL,
  status            TEXT NOT NULL,         -- active | waiting | done | aborted
  created_by        TEXT NOT NULL,
  created_at        DATETIME NOT NULL,
  updated_at        DATETIME NOT NULL,
  model             TEXT NOT NULL,
  total_tokens      INTEGER DEFAULT 0,
  total_tool_calls  INTEGER DEFAULT 0,
  summary           TEXT                   -- mark_done result
);

CREATE TABLE messages (
  id                TEXT PRIMARY KEY,
  investigation_id  TEXT NOT NULL,
  role              TEXT NOT NULL,         -- user | assistant | tool_result | system_note
  content           TEXT NOT NULL,         -- json
  timestamp         DATETIME NOT NULL
);

CREATE TABLE tool_calls (
  id                TEXT PRIMARY KEY,
  investigation_id  TEXT NOT NULL,
  tool              TEXT NOT NULL,
  input_json        TEXT NOT NULL,
  rationale         TEXT,
  status            TEXT NOT NULL,         -- pending | approved | edited | skipped | executed | failed
  decided_by        TEXT,
  task_id           TEXT,                  -- ссылка на созданный task (если executed)
  created_at        DATETIME NOT NULL,
  decided_at        DATETIME
);

CREATE TABLE findings (
  id                TEXT PRIMARY KEY,
  investigation_id  TEXT NOT NULL,
  severity          TEXT NOT NULL,
  code              TEXT NOT NULL,
  message           TEXT NOT NULL,
  evidence_json     TEXT,
  pinned            BOOLEAN DEFAULT 0,
  ignored           BOOLEAN DEFAULT 0,
  created_at        DATETIME NOT NULL
);

CREATE TABLE audit (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  ts            DATETIME NOT NULL,
  actor         TEXT NOT NULL,
  action        TEXT NOT NULL,
  details_json  TEXT
);
```

---

## 7. Investigator — агентный loop с Claude

### 7.1. Концепт

Investigator — сервис внутри hub'а, который ведёт диалог с LLM через OpenAI-compatible chat/completions endpoint (бекенд по умолчанию — OpenRouter, см. §10) в режиме function-calling tools. Он:

- хранит state investigation'а (messages, tool_calls, findings);
- экспортирует коллекторы как tools для Claude (на основе их манифестов);
- получает от Claude предложения tool calls;
- **не исполняет их автоматически** — ждёт решения оператора;
- после одобрения — исполняет, получает результат, отдаёт обратно Claude;
- продолжает до `mark_done` или принудительной остановки.

### 7.2. Step-by-step loop

```
┌──────────────────────────────────────────────────────────┐
│ 1. Operator: создаёт investigation (goal)                │
└───────────────────────┬──────────────────────────────────┘
                        ▼
┌──────────────────────────────────────────────────────────┐
│ 2. Hub: system prompt + tools, вызов Claude              │
└───────────────────────┬──────────────────────────────────┘
                        ▼
┌──────────────────────────────────────────────────────────┐
│ 3. Claude возвращает:                                    │
│     • thinking / rationale                               │
│     • ОДИН tool_call (function-calling)                  │
└───────────────────────┬──────────────────────────────────┘
                        ▼
┌──────────────────────────────────────────────────────────┐
│ 4. Hub сохраняет tool_call как `pending`, стримит в UI   │
└───────────────────────┬──────────────────────────────────┘
                        ▼
┌──────────────────────────────────────────────────────────┐
│ 5. Operator решает:                                      │
│     ├ Approve         → execute                          │
│     ├ Edit params     → execute с новыми параметрами     │
│     ├ Skip            → synthetic tool_result, continue  │
│     ├ Hypothesis      → discard, inject OPERATOR block   │
│     ├ Ignore finding  → discard, inject system_note      │
│     └ End             → force mark_done                  │
└───────────────────────┬──────────────────────────────────┘
                        ▼
┌──────────────────────────────────────────────────────────┐
│ 6. Hub добавляет tool_result (или note) в messages,      │
│    вызывает Claude снова                                 │
└───────────────────────┬──────────────────────────────────┘
                        │
                        └───────── loop к шагу 3 ────────────┐
                                                             │
                                   until mark_done ◀─────────┘
```

### 7.3. Tool-set для Claude

**Discovery (дешёвые tools для ориентации):**

| Tool | Назначение |
|------|-----------|
| `list_hosts(selector?)` | Массив хостов с лейблами и статусом |
| `list_collectors(category?)` | Каталог коллекторов |
| `describe_collector(name)` | Полный манифест с output schema |

**Action (основные tools):**

| Tool | Назначение |
|------|-----------|
| `collect(host_id, collector, params, timeout_seconds)` | Один сбор на один хост |
| `collect_batch(host_ids[], collector, params, timeout)` | Параллельно на нескольких хостах |
| `search_artifact(task_id, artifact_name, pattern, context_lines)` | Grep по большому артефакту без загрузки в контекст |
| `compare_across_hosts(task_ids[])` | Структурированный дифф результатов между хостами |
| `get_full_result(task_id)` | Полный structured result, если summary недостаточно |

**Investigation (мета-tools):**

| Tool | Назначение |
|------|-----------|
| `add_finding(severity, code, message, evidence_refs[])` | Зафиксировать вывод в memo |
| `ask_operator(question, context?)` | Поставить investigation в `waiting`, ждать ответа |
| `mark_done(summary)` | Финализировать investigation |

### 7.4. Управление контекстом

Сырые данные коллекторов могут быть мегабайтные. Решение — **трёхуровневое представление**:

1. **Summary в messages** (всегда). При каждом `collect` Claude получает компактное JSON-резюме: ключевые поля, severity флаги, размер артефакта. Цель — уложиться в 500-2000 токенов на результат.
2. **Full structured data.** Доступен через tool `get_full_result(task_id)` по явному запросу.
3. **Raw artifacts.** Только через `search_artifact`. Claude никогда не загружает весь дамп.

**Compaction.** Когда контекст приближается к 150K токенов, hub вызывает Claude вспомогательным промптом «сожми старую часть расследования в state». Старые messages помечаются `archived`, в активном контексте остаётся сжатый state + последние N шагов.

**Кластеризация логов (`artifact_index`).** Слой summary/raw для лог-артефактов проходит через `internal/hub/logtriage`: строки схлопываются в кластеры `(template, severity)` с числом вхождений и диапазоном строк (`first_line`/`last_line`), и этот `artifact_index` отдаётся модели рядом с резюме. Именно он выносит на поверхность **низкочастотные outlier-кластеры** (редкие, но диагностически важные) рядом с доминирующим шумом — основа дифференциальной методологии (§7.7). Поэтому отдельный «коллектор аномалий» не нужен: кластеризация уже есть (§3.4). Замечание о бюджете: на одно-хостовом пути большой `artifact_index` может быть схлопнут до заголовочного (самого громкого) кластера с флагом `_index_truncated`; полный набор кластеров достаётся через `get_full_result(task_id)` — поэтому перечисление классов в §7.7 идёт по полному индексу, а не по схлопнутому заголовку.

### 7.5. Hypothesis и Ignore — структурные вмешательства

**Hypothesis** — форма в UI:

```
OPERATOR HYPOTHESIS  [priority: HIGH]
Claim: возможно, проблема в истёкшем серте kube-apiserver
Expected evidence: check expiration date of apiserver.crt
Instruction: verify before continuing other branches
```

Вставляется как user message. Текущее pending-предложение Claude отбрасывается. Claude обязан перепланировать (правило в system prompt).

**Ignore finding** — клик по карточке finding'а в memo. В БД находка помечается `ignored=true`. Следующий запрос к Claude содержит system-note:

```
OPERATOR ACTIONS (since last turn):
- Finding F-3 "CoreDNS pod restarted 12 times" marked IGNORED.
  Do not investigate this direction further.
```

### 7.6. Бюджеты и лимиты

| Параметр | Значение по умолчанию | Назначение |
|----------|----------------------|------------|
| `max_steps` | 40 | Всего tool_call'ов в investigation |
| `max_tokens` | 500 000 | Суммарно токенов на investigation |
| `max_parallel_collects` | 10 | Коллекторов одновременно в `collect_batch` |
| `broad_selector_threshold` | 5 | Селектор покрывает > N хостов → confirmation |
| Per-agent rate limit | 30/min | Коллекторов в минуту на один агент |

### 7.7. Дифференциальная методология и coverage gate

Реальный инцидент (`inv_a00000000002`): хост завис и пропал из сети, первопричина — зависший контроллер NIC. Инвестигатор её упустил: он заякорился на самом громком кластере логов (21k строк `tpm_try_transmit` storm), закрылся выводом «TPM storm / inconclusive» и вызвал `mark_done` — редкие NIC/link-flap-кластеры были прямо в `artifact_index`, но остались непросмотренными (139k токенов за 13 шагов). Сбой был в **рассуждении, не в инструментах**.

**Методология (rule 14 в system prompt).** Из симптома инвестигатор сначала перечисляет небольшой набор *классов* первопричин (cpu-lockup, OOM, storage/IO, network/NIC, kernel-panic, hardware/MCE, watchdog, power/thermal — это **примеры**, не хардкод-список) и для каждого называет самое дешёвое отличающее наблюдение. Самый громкий кластер — один кандидат, а не вывод по умолчанию; редкие outlier-кластеры заслуживают внимания именно потому, что редки. Сначала breadth-first проход по существующему logtriage-индексу, и только потом узкие чтения. При флаге `_index_truncated` перечисление идёт по полному набору кластеров через `get_full_result(task_id)` (§7.4). Непокрытый high-prior класс — это открытый `evidence_gap`: `mark_done` и объявление находки load-bearing блокируются, пока он стоит; покрытие проверяется **до** load-bearing-находки, а не после (правило 9 после находки отключает пробы).

**Coverage gate (hub-side backstop).** Помимо промпта, hub перехватывает первый `mark_done` (`internal/hub/investigator/coverage_gate.go`): если расследование заканчивается, когда был предъявлен мультикластерный `artifact_index`, но модель просмотрела меньше двух различных регионов лога (`search_artifact` / `get_full_result`), `mark_done` один раз отклоняется с подсказкой-репланом. Это **грубый breadth-прокси, а не покрытие по классам**: структурного состояния класса в истории нет (`MemoryKindHypothesis` без точек записи), точная дисциплина по классам живёт в промпте. Гейт срабатывает **строго один раз** (маркер в истории сообщений предотвращает бесконечную блокировку), уважает anti-loop-бюджет, отключается при директивах оператора (`OPERATOR FINALIZE` / `HYPOTHESIS` / `RESUME`), и его отбой аддитивен — `mark_done` штатно резолвится своим `function_call_output` (никаких висящих tool_call). Отклонённый `mark_done` становится новейшим executed-инструментом, поэтому post-finding-локдаун (правило 9) снимается на один реплан-ход и подсказка «проверь непокрытый класс» становится выполнимой.

**Второй инцидент того же класса (`inv_a00000000003` → `inv_a00000000005`): сбой в синтезе, а не в широте.** Тот же хост (`host-x`), тот же симптом «не отвечает по сети + IPMI завис». Здесь модель **нашла** нужный сигнал (`igb eno1 Link Down/Up`) и сама записала, что он «прямо соответствует "не отвечает по сети"», — а затем **отбросила** его: «NIC events объясняют только сетевую недоступность, не полный freeze». Она заякорилась на слове оператора «**завис**» (механизм-гипотеза, не наблюдение), сожгла ~25 шагов и +2M операторских токенов в боковой ветке про границу boot-окна, и закрылась **отрицательным** выводом `not_kernel_hang`, не объяснив первичный симптом. Coverage gate (§выше) не сработал: модель просмотрела 22 региона — breadth-прокси был удовлетворён. Правильную первопричину (`network.eno1_lacp_member_link_flap`) модель нашла лишь после явного хинта оператора. Выводы универсальны (не про сеть): сбой в **ранжировании/объяснении**, плюс anti-tunnel-vision и stale-priors. Доработки:

- **Rule 15 (symptom-anchoring).** На первом ходу (и после каждого редиректа оператора) модель формулирует инцидент как список **наблюдаемых симптомов**, явно отделённых от слов-механизмов («завис»/«freeze»/«crash» — это гипотезы о причине, не наблюдения). Дифференциал (rule 14) ранжирует классы по тому, как они объясняют **первичный** симптом; подтверждённое наблюдение, совпадающее с симптомом, нельзя отбросить лишь потому, что оно не объясняет предполагаемый механизм — иначе его фиксируют как открытый `evidence_gap` и закрывают до `mark_done`.
- **Explanatory-adequacy gate (`explanation_gate.go`).** Hub-side backstop в дополнение к coverage gate: перед **уверенным** закрытием (`confidence` confirmed|likely) один раз отбивает `mark_done` с self-critique-подсказкой — переформулировать первичный симптом, объяснить его причинно, и перечислить наблюдения, совпадающие с симптомом, но не принятые как причина. Где coverage gate меряет **широту** drilling, этот — **адекватность синтеза**. One-time, отключается на `OPERATOR FINALIZE`/`HYPOTHESIS`/`RESUME`, гуманные закрытия (speculative/inconclusive) не трогает.
- **Схема `mark_done` ужесточена.** Обязательны `symptoms` (≥1), `confidence` (confirmed|likely|speculative|inconclusive), `root_cause_explains` (какой симптом объясняет вывод; не требуется при inconclusive) и `where_to_look_next` (требуется, если confidence ≠ confirmed). Вывод «только исключил X», не объясняющий первичный симптом, честно помечается `inconclusive`.
- **Differential re-rank checkpoint (`loop.go`).** Каждые `investigator.rerank_interval_steps` пробных вызовов без load-bearing-находки (по умолчанию 8; 0 отключает) — одноразовый system_note «шаг назад, переранжируй дифференциал»; ограничен (≤3 на расследование), снимается после load-bearing-находки, не делает собственных LLM/tool-вызовов.
- **Priors fencing (`priors.go`).** Дайджест прайоров теперь показывает **инцидент** каждого прайора рядом с выводом, а заголовок поднят до MUST: вывод прошлого расследования — гипотеза из **другого** инцидента, его нельзя присваивать как причину текущего без независимой перепроверки по симптомам ЭТОГО инцидента. (В `inv_a00000000005` дайджест прайора прямо подсовывал ошибочный «TPM storm».)

**Без нового коллектора и без релиза агента.** Методология опирается на уже существующую logtriage-кластеризацию (§7.4) и `artifact_index`; контракты read-only, one-tool-per-turn и evidence-first не затронуты (§3.4). Это model-generated дифференциал, ортогональный и подчинённый оператор-директиве `OPERATOR HYPOTHESIS`.

Регрессии: детерминированные тесты гейтов, схемы, чекпоинта и прайоров (`coverage_gate_test.go`, `explanation_gate` + схема в `diagnosis_quality_test.go`, `differential_regression_test.go` — реплей реального инцидента, `prompt_test.go`) — в CI; нондетерминированный офлайн-eval на живой модели (`differential_eval_test.go`, build-tag `eval`) — периодическая ручная проверка, не CI-гейт.

### 7.8. Автономный прогон в рамках бюджета шагов/токенов

Поверх per-investigation тумблера `auto_approve` (§7.6, безлимитный) оператор может **армировать ограниченный автономный прогон**: задать бюджет «+N шагов / +M токенов», в рамках которого инвестигатор сам аппрувит пробные tool_call'ы **без подтверждения** на каждом шаге, а по исчерпании бюджета **встаёт на паузу для ревью** (не abort) и разоружается. Это прямой ответ на инцидент выше, где оператор вручную 6 раз доливал бюджет, аппручивая каждый шаг.

- **Состояние** (миграция `0020_autonomous_run.sql`): абсолютные цели `auto_run_until_steps` / `auto_run_until_tokens` (0/0 = не армировано). Независимы от `auto_approve` — разоружение чистое, безлимитный тумблер не остаётся включённым.
- **Loop** (`StartAutonomousRun`): армирование одновременно **выдаёт** тот же бюджет как global-headroom (`ExtendBudget`), чтобы global-cap-пауза не сработала раньше автономной цели; `shouldAutoApprove` аппрувит пробы, пока `totals < target`; по достижении — `step()` ставит `paused` с сообщением `AUTONOMOUS PAUSE` и разоружает. Re-arm из paused возобновляет.
- **Терминальный carve-out сохранён.** `mark_done` под автономным прогоном по-прежнему ждёт оператора (если не было `OPERATOR FINALIZE`) — «расследуй сам, вывод покажи мне». Read-only-инвариант не затронут: меняется только КТО аппрувит те же read-only пробы.
- **UI** (`investigation_detail.html`): в шапке — контрол «▶ run autonomously» (поля steps/tokens) и «⏸ take over» (разоружить); пока армировано, безлимитный «⚡ auto» скрыт. Re-arm доступен в paused-карточке. Эндпоинты: `POST /investigations/autonomous` (web) и `POST /api/v1/investigations/{id}/autonomous` (JSON).

---

## 8. Web UI

### 8.1. Экраны

1. **Dashboard** — последние investigations, статус hub'а, счётчик агентов.
2. **Hosts** — таблица агентов с лейблами, статусом, версией.
3. **Host detail** — панели по категориям коллекторов с кнопкой «refresh».
4. **New investigation** — форма с полем `goal` и опциональными подсказками.
5. **Investigation** — главный рабочий экран (см. 8.2).
6. **Runs** — история прогонов (в том числе ручных, вне investigation).
7. **Audit** — журнал действий.
8. **Settings** — bootstrap-токены, модель LLM, retention, админы.

### 8.2. Investigation view

Трёхколоночный layout:

**Левая колонка — Chat:**
- Реплики оператора.
- Сообщения Claude с раскрываемым `thinking`.
- Поле ввода (для hypothesis или свободных реплик).

**Центральная колонка — Actions timeline:**
- Хронология tool_calls.
- **Текущий pending** в верхней части, подсвеченный «waiting for your decision».
- Карточка pending: название tool'а, input JSON, rationale, estimated tokens, кнопки **Approve / Edit / Skip / Hypothesis / End**.
- Исполненные calls сворачиваются, раскрываются по клику.
- У каждого исполненного call: input, duration, result summary, полный JSON (по раскрытию), ссылки на артефакты.

**Правая колонка — Memo:**
- Findings: pinned сверху, затем active, внизу ignored.
- На каждом: severity badge, code, message, evidence refs (клик → прокрутить timeline).
- Кнопки `pin` и `ignore`.

**Нижняя панель:**
- Прогресс-бар (steps used / budget).
- Tokens used / budget.
- Duration.
- Кнопки **Export (md), Abort, Fork**.

### 8.3. Streaming

Для живого обновления используется **SSE** (HTMX умеет из коробки). Типы событий:

- `thinking-chunk`
- `tool-proposed`
- `tool-executed`
- `finding-added`
- `message`
- `state-change`
- `done`

---

## 9. Безопасность

### 9.1. mTLS

Все соединения агентов с hub'ом — mTLS. Hub имеет серверный серт (Let's Encrypt или приватный CA). Агенты — клиентские серты, выданные hub'ом при enroll.

### 9.2. Bootstrap

Оператор в UI генерирует одноразовый токен с TTL (по умолчанию 24h). Копирует в конфиг агента. Агент при старте с токеном делает `Enroll`, присылая CSR. Hub проверяет токен, подписывает сертификат своей приватной CA, возвращает. Дальше токен не нужен.

### 9.3. Read-only

См. 3.4 — пять слоёв защиты.

### 9.4. Аудит

В таблицу `audit` попадают:
- enroll / revoke агента;
- создание / абортинг investigation;
- одобрение / редактирование tool_call;
- pin / ignore finding;
- изменения конфигурации.

Лог append-only, экспортируется в JSON.

### 9.5. Изоляция данных при вызове LLM

В system prompt Claude'у передаётся только:
- список tool'ов и schemas;
- роль и правила;
- goal от оператора;
- history messages этой investigation.

**Не передаются:** API-ключи, чужие investigations, содержимое `hub.yaml`.

Опциональный `sanitize` mode для будущих версий: маскировать hostname'ы, IP, e-mail перед отправкой в LLM. В MVP нет.

---

## 10. Технологический стек

- **Язык:** Go 1.22+
- **gRPC:** `google.golang.org/grpc`, `protoc-gen-go`
- **DB:** SQLite через `modernc.org/sqlite` (pure Go, без CGO)
- **Web server:** stdlib `net/http`
- **Templates:** `html/template` (или `templ`, если захочется)
- **Web client:** HTMX + Alpine.js + Tailwind (из CDN, минимум сборки)
- **LLM:** **OpenAI-compatible** chat/completions API (function-calling tools). Тонкий собственный HTTP-клиент (`internal/hub/llm`), без vendor SDK. Бекенд по умолчанию — **OpenRouter** (`https://openrouter.ai/api/v1`), модель по умолчанию `anthropic/claude-sonnet-4.5`. Любой OpenAI-совместимый endpoint работает (vLLM, LiteLLM, raw OpenAI). Все настройки внешние:
  - env: `RECON_LLM_BASE_URL`, `RECON_LLM_MODEL`, `RECON_LLM_API_KEY`
  - override: `hub.yaml.llm.{base_url, model, api_key_env, http_referer, x_title}`
  
  **Отклонения от Anthropic Messages API:** `tool_choice: "required"` вместо `{"type":"any"}`; extended thinking (`budget_tokens`) НЕ используется — не портируется между провайдерами. Качество диагностики на сложных кейсах теоретически ниже, чем с Claude через native API; если потребуется — вернуться к Anthropic SDK прицельно для этой возможности.
- **Observability:** structured logs (`slog`), опционально Prometheus на `/metrics`. Уровень логирования обоих бинарей (hub + agent) задаётся env `RECON_LOG_LEVEL` (`debug|info|warn|error`, default `info`) через общий `internal/common/logging` — `debug` поднимает per-decision auto-approve trail (`[FIX:auto-approve]`) на проде без пересборки (в Debug-сайтах нет секретов/PII). Operator-facing timestamps в web-UI рендерятся в таймзоне браузера (cookie `tz`, встроенный `time/tzdata`); все machine-facing метки (export, JSON API, SSE, notebook, LLM) остаются UTC/RFC3339 (инвариант three-tier / §7).
- **Packaging:** один бинарь hub, один бинарь agent; systemd units; tar + deb/rpm (пост-MVP)

---

## 11. Структура репозитория

```
recon/
├── cmd/
│   ├── hub/main.go
│   └── agent/main.go
├── internal/
│   ├── proto/
│   │   ├── recon.proto
│   │   └── *.pb.go  (generated)
│   ├── hub/
│   │   ├── api/           # gRPC server (для агентов)
│   │   ├── web/           # HTTP + templates + SSE
│   │   ├── store/         # SQLite + миграции + artifacts
│   │   ├── runner/        # исполнение runs
│   │   ├── investigator/  # LLM-loop, tool-routing
│   │   ├── llm/           # OpenAI-compat HTTP client (default: OpenRouter)
│   │   └── auth/          # bootstrap, mTLS, audit
│   ├── agent/
│   │   ├── conn/          # gRPC-клиент с reconnect/backoff
│   │   ├── collect/       # Collector interface, registry
│   │   ├── collectors/
│   │   │   ├── system/
│   │   │   ├── systemd/
│   │   │   ├── net/
│   │   │   ├── files/
│   │   │   └── process/
│   │   └── exec/          # readonly gateway
│   └── common/
│       ├── ids/
│       ├── labels/
│       └── version/
├── web/
│   ├── templates/
│   └── static/
├── deploy/
│   ├── systemd/
│   ├── nginx/
│   └── docs/
├── docs/
│   ├── design.md          # этот файл
│   ├── prompts.md         # следующий артефакт
│   └── collectors/
├── Makefile
└── go.mod
```

---

## 12. Коллекторы первой волны (MVP)

| Name | Category | Описание | Требует |
|------|----------|----------|---------|
| `system_info` | system | uname, distro, uptime, load, RAM, CPU | — |
| `systemd_units` | systemd | список юнитов со статусами (фильтр) | — |
| `journal_tail` | systemd | journalctl: юнит / kernel ring (`-k`) / previous boot (`-b -1`) / окно (`--since`/`--until`) / `-p` / `-g`; полный вывод — в артефакт | SUDO_JOURNALCTL; CAP_SYSLOG для `-k` на хостах с `kernel.dmesg_restrict=1` |
| `net_ifaces` | network | ip addr / ip link / ip route / ARP | — |
| `net_listen` | network | ss -tulpn, распарсен в таблицу | SUDO_SS |
| `net_connect` | network | TCP/ICMP-чек до списка эндпоинтов | — |
| `dns_resolve` | network | резолв имён через системный resolver | — |
| `process_list` | process | снапшот /proc, топ по cpu/ram | — |
| `file_read` | files | чтение окна файла из whitelist (head / `offset` / `from_end` / `tail_lines`), full-file sha256 для head; окно — в searchable-артефакт, инлайн только метаданные | CAP_DAC_READ_SEARCH (опц.) |
| `log_search` | files | grep RE2 по файлам whitelist **in-process на хосте** (path/path_glob, `since`/`until`, `from_end`); мегабайты не попадают в контекст; symlink-guard на каждый матч | CAP_DAC_READ_SEARCH (опц.) |
| `dmesg` | system | kernel ring buffer с абсолютными ISO-таймстемпами (`--time-format=iso`); grep — in-process | CAP_SYSLOG (для `kernel.dmesg_restrict=1`) |
| `disk_usage` | system | df, inodes, du по whitelist | — |

**Вторая волна (после MVP):** `k8s_certs`, `k8s_kubelet`, `iptables_dump`, `crictl_ps`, `etcd_health`, `cert_expiry`, `env_vars`.

---

## 13. Роадмап

### Неделя 1 — скелет
- Proto-контракт, кодогенерация.
- Hub: gRPC-endpoint, регистрация/enroll, SQLite миграции.
- Agent: conn с reconnect, один коллектор `system_info`.
- Web-UI: страница Hosts.
- mTLS и bootstrap-токены (упрощённо).

### Неделя 2 — фундамент коллекторов
- Collector framework + registry + readonly gateway.
- 5-6 коллекторов первой волны.
- Run-модель, страница Run detail, базовый рендер результатов.

### Неделя 3 — investigator MVP
- LLM-клиент (OpenAI-compat), function-calling loop.
- Tool schemas из манифестов коллекторов.
- Step-by-step execution с Approve / Skip.
- Investigation view (упрощённый).

### Неделя 4 — investigator расширенный
- Hypothesis и Ignore.
- Memo с findings.
- SSE-стриминг в UI.
- Compaction контекста.
- Markdown-экспорт.

### Неделя 5 — полировка MVP
- Остальные коллекторы первой волны.
- Audit log в UI.
- Edit-and-rerun для tool_call'ов.
- Бюджеты и broad-selector confirmation.
- Deployment docs, systemd units.

### После MVP (не входит)
- K8s-специфичные коллекторы (вторая волна).
- Локальный LLM через Ollama.
- Sanitize mode.
- Мультипользовательский режим.
- Долгосрочная память между investigations.
- Scheduled investigations и алёрт-правила.

---

## 14. Открытые вопросы и риски

**Качество LLM-расследования** критически зависит от качества описаний коллекторов и их output schemas. Это главный вектор работы на этапах 3-4. Плохое описание → Claude дёргает не тот tool.

**Latency step-by-step.** Каждый шаг = RTT до Claude + время оператора. 10-шаговая investigation ≈ 2-5 минут. Для MVP ок; если станет проблемой — добавить «batch approve» (одобрить следующие N шагов вперёд).

**Context bloat.** Даже с компактными summary 40 шагов могут вылезти за окно. Compaction решает, но его качество — отдельный промпт-инженерный сюжет.

**Ошибки LLM в target selector'ах.** Claude может ошибочно выбрать не тот хост. Митигация — broad-selector confirmation + явное отображение `agent_id` в summary tool_result.

**Cost.** Claude Sonnet — основной. Одна investigation ориентировочно $0.10–$1.00 в зависимости от глубины. В UI виден счётчик.

**Устойчивость агента.** Агент не должен падать. Паники коллекторов оборачиваются в `STATUS_ERROR`. Крэш бинаря недопустим — fuzzing входов приветствуется.

---

*Документ живой. Изменения через PR в `docs/design.md`.*