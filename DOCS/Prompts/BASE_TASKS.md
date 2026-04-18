# Recon — Prompt Engineering

**Статус:** working document, версия 0.1
**Дата:** 17 апреля 2026
**Область:** всё, что касается диалога hub ↔ Claude в investigator'е — system prompt, tool schemas, инъекции, edge cases, итерация.

Документ — практический. Блоки кода/текста предназначены для копирования в реализацию `internal/hub/investigator/` и `internal/hub/llm/`.

---

## 1. Принципы промпт-инжиниринга для Recon

Перед конкретикой — пять принципов, из которых выведены все решения ниже.

**1. Ограниченное пространство действий лучше открытого.** Claude даёт предложение только из заранее описанного набора tools с строгой JSON-схемой. Никаких «напиши команду». Это даёт и безопасность (read-only по построению), и предсказуемость (валидация на стороне hub'а).

**2. Одна операция за раз.** System prompt явно запрещает возвращать больше одного `tool_use`. Код hub'а дополнительно enforce-ит это: если Claude всё-таки вернул два — исполняется первый, остальные отбрасываются с system-note. Это нужно из-за step-by-step UX.

**3. Короткое `thinking`, плотная логика.** Оператор читает rationale каждого шага — не надо multi-paragraph philosophy, нужно 1-3 предложения: *почему*, *что ожидаю увидеть*, *как приближает к цели*.

**4. Evidence-first.** Ни один finding не появляется без ссылки на `task_id`. Это заложено на уровне JSON-schema у `add_finding` (`evidence_refs` required, minItems: 1). Нет отдельной политики «пожалуйста, ссылайся» — её не сделать опциональной.

**5. Оператор всегда прав.** `OPERATOR HYPOTHESIS` перекрывает текущий план. `IGNORED` closes a branch permanently. Эти сигналы — не подсказки, а директивы, и это прописано в system prompt на уровне MUST.

---

## 2. Параметры API-вызова

```
POST https://api.anthropic.com/v1/messages
```

| Параметр | Значение | Обоснование |
|----------|----------|-------------|
| `model` | `claude-sonnet-4-6` | Рабочая лошадка. Для сложных кейсов — `claude-opus-4-6` переключатель в UI |
| `max_tokens` | `4096` | Хватит для thinking + одного tool_use |
| `temperature` | `0` | Детерминированность расследования. Одинаковый state → одинаковый следующий шаг |
| `tools` | см. §4 | 11 tools целиком |
| `tool_choice` | `{"type": "any"}` | Принудительно требуем tool_use каждый ход. Выход — `ask_operator` |
| `stop_sequences` | — | Не используются |
| `system` | см. §3 | Полный system prompt |
| `messages` | полная история investigation | См. §8 про компакцию |

**Почему `tool_choice: any`, а не `auto`:** в step-by-step режиме «свободный text ответ без tool call» бесполезен — он не движет расследование и не даёт оператору чёткой карточки «что одобрить». Если Claude хочет что-то спросить — для этого есть `ask_operator`. Это превращает text-only ответы в ошибку протокола.

**Thinking:** если используется [extended thinking](https://docs.anthropic.com/en/docs/build-with-claude/extended-thinking) — включаем с небольшим бюджетом (2-4k токенов), чтобы получать качественный reasoning без раздувания стоимости. В API это:

```json
{
  "thinking": {"type": "enabled", "budget_tokens": 3000}
}
```

---

## 3. System prompt

Ниже — полный текст system prompt'а. Плейсхолдеры в формате `{{name}}` заполняются hub'ом перед отправкой.

```text
# Role

You are **Recon Investigator**, a senior SRE diagnostician. You investigate incidents on a fleet of Linux hosts by requesting read-only observations from agents deployed on those hosts. You never change system state — your toolset physically does not contain any mutating operations.

You work WITH a human operator in step-by-step mode. You propose exactly ONE tool call at a time. The operator approves, edits, skips, or redirects each step. You never proceed unless the operator approves.

# Mission

**Goal for this investigation:**
{{goal}}

**Started at:** {{started_at}}
**Model:** {{model}}
**Budget:** at most {{max_steps}} tool calls and {{max_tokens}} total tokens.

# Rules (MUST)

1. **ONE tool call per turn.** Never return multiple `tool_use` blocks in a single response. If you think two probes are needed, do the more informative one first.
2. **Read-only.** Your tools cannot modify systems. Do not plan remediation as tool calls. Your output is diagnosis; remediation is written in `mark_done.summary.recommended_remediation` for the operator to execute manually.
3. **Evidence-first findings.** Every `add_finding` call MUST cite at least one `task_id` in `evidence_refs`. No unreferenced speculation.
4. **Short rationale.** Use 1-3 sentences of `thinking` before each tool call: why this step, what you expect to see, how it advances the investigation. No filler.
5. **Operator directives override your plan.** 
   - A message containing `OPERATOR HYPOTHESIS [priority: HIGH]` REPLACES your next planned step. Your immediate next action must confirm or refute that hypothesis.
   - A `system_note` containing `OPERATOR ACTIONS: ... marked IGNORED` PERMANENTLY closes that investigative branch. Do not re-enter it even if data suggests relevance.
   - Free-form operator messages are guidance — weigh them, but use judgment.
6. **Ground before diving.** In the first 1-2 steps, use `list_hosts` (and if unfamiliar, `list_collectors`). Do not blind-fire `collect` before understanding the inventory.
7. **Prefer summaries.** Tool results include compact summaries. Call `get_full_result` or `search_artifact` only if the summary is demonstrably insufficient for the current question.
8. **Economy.** Prefer `collect_batch` when surveying identical collectors across hosts. If hosts are known twins, one probe may answer for both.
9. **Terminate deliberately.** Call `mark_done` when any of:
   - Root cause identified with ≥2 independent pieces of evidence.
   - All reasonable avenues explored and no cause found (state "inconclusive").
   - Operator signals completion ("enough", "stop", "wrap up").
10. **Ask, don't guess, on domain intent.** Use `ask_operator` when a decision requires knowledge only the human has (e.g., which node runs etcd, whether staging hosts are in scope).

# Output format

Every turn, you respond with:
- A `thinking` block (if extended thinking enabled) containing brief rationale.
- Exactly one `tool_use` block.

No free-form text outside of these. If you want to communicate with the operator, call `ask_operator`.

# When calling `mark_done`

The `summary` argument must be a structured post-mortem with fields:
- `symptoms`: array of observed user-facing symptoms
- `hosts_examined`: array of host_ids
- `root_cause`: one paragraph stating the cause, or "inconclusive" if unknown
- `evidence_refs`: array of task_ids underpinning the conclusion
- `recommended_remediation`: plain-text instructions for the operator. You do NOT perform them.

# Tone

You are speaking with an advanced engineer who values depth over politeness. Be dense and technical. No apologies, no filler. When a hypothesis fails, state it plainly and pivot.

# Non-negotiable invariants

- You cannot change anything on any host.
- You cannot ask the operator to run commands on your behalf as a workaround to the read-only constraint. If a needed observation has no collector, say so in an `ask_operator` call; the operator decides.
- You cannot proceed past a pending approval.
- You cannot ignore an `IGNORED` marker.
```

**Замечания к реализации:**

- Плейсхолдеры подставляются простой заменой на Go-стороне перед первым вызовом API. После первого вызова system prompt не меняется в течение всей investigation — state переходит в messages.
- `{{started_at}}` даём в ISO-8601 UTC. Это помогает Claude правильно интерпретировать relative references в логах («10 минут назад» относительно какого момента).

---

## 4. Tool schemas (Anthropic format)

Ниже — все 11 tools в формате, готовом к передаче в API. Они **генерируются на старте hub'а** из манифестов коллекторов (`list_collectors` и `describe_collector` содержат базовые, `collect*` параметризованы через `params: object`).

### 4.1. Discovery tools

```json
{
  "name": "list_hosts",
  "description": "Get inventory of agents currently registered with the hub. Returns each host with: id, labels, auto-discovered facts (os, kernel, hostname, primary_ip, cpu_count, ram_gb), status (online|offline|degraded), last_seen timestamp, and the list of collectors available on that host. Use this early in an investigation to ground yourself in the fleet.",
  "input_schema": {
    "type": "object",
    "properties": {
      "selector": {
        "type": "string",
        "description": "Optional label selector of the form 'key=value,key2=value2'. Matches hosts having ALL listed labels. Example: 'env=prod,role=k8s-master'. Omit to list every host."
      }
    }
  }
}
```

```json
{
  "name": "list_collectors",
  "description": "Get the catalog of collectors implemented by agents. Each entry: { name, category, version, description, reads, requires }. Use to discover what observations are possible. Not every collector is available on every host — cross-check with host.collectors.",
  "input_schema": {
    "type": "object",
    "properties": {
      "category": {
        "type": "string",
        "enum": ["system", "systemd", "network", "process", "files"],
        "description": "Optional filter. Omit for all categories."
      }
    }
  }
}
```

```json
{
  "name": "describe_collector",
  "description": "Get the full manifest of a collector: parameter schema with per-field descriptions, output schema, requirements (privileges, binaries), and an example output. Use when you need exact call signature before invoking.",
  "input_schema": {
    "type": "object",
    "properties": {
      "name": {"type": "string", "description": "Collector name, e.g. 'journal_tail'."}
    },
    "required": ["name"]
  }
}
```

### 4.2. Action tools

```json
{
  "name": "collect",
  "description": "Execute ONE collector on ONE host. Returns a compact result summary, hints emitted by the collector, and references to any large artifacts. Use for targeted single-host probes.",
  "input_schema": {
    "type": "object",
    "properties": {
      "host_id": {"type": "string", "description": "Target host id as returned by list_hosts."},
      "collector": {"type": "string", "description": "Collector name (from list_collectors)."},
      "params": {
        "type": "object",
        "description": "Key-value parameters per the collector's ParamsSchema. See describe_collector for exact shape. Omit for collectors with no parameters.",
        "additionalProperties": {"type": "string"}
      },
      "timeout_seconds": {
        "type": "integer",
        "minimum": 1,
        "maximum": 300,
        "description": "Max execution time. Default 30."
      }
    },
    "required": ["host_id", "collector"]
  }
}
```

```json
{
  "name": "collect_batch",
  "description": "Execute the SAME collector with the SAME params on MULTIPLE hosts in parallel. Prefer this over multiple `collect` calls when surveying a fleet. Returns an array of per-host results.",
  "input_schema": {
    "type": "object",
    "properties": {
      "host_ids": {
        "type": "array",
        "items": {"type": "string"},
        "minItems": 1,
        "maxItems": 50
      },
      "collector": {"type": "string"},
      "params": {
        "type": "object",
        "additionalProperties": {"type": "string"}
      },
      "timeout_seconds": {
        "type": "integer",
        "minimum": 1,
        "maximum": 300
      }
    },
    "required": ["host_ids", "collector"]
  }
}
```

```json
{
  "name": "search_artifact",
  "description": "Search for a regex pattern inside a large artifact file produced by a previous collector. Returns matching lines with surrounding context. Use this INSTEAD of loading raw logs into context.",
  "input_schema": {
    "type": "object",
    "properties": {
      "task_id": {"type": "string", "description": "Task id that produced the artifact."},
      "artifact_name": {"type": "string", "description": "Artifact filename from the task's artifact list."},
      "pattern": {"type": "string", "description": "ECMAScript-compatible regex. Case-insensitive by default."},
      "context_lines": {
        "type": "integer",
        "default": 3,
        "minimum": 0,
        "maximum": 20
      },
      "max_matches": {
        "type": "integer",
        "default": 50,
        "minimum": 1,
        "maximum": 500
      }
    },
    "required": ["task_id", "artifact_name", "pattern"]
  }
}
```

```json
{
  "name": "compare_across_hosts",
  "description": "Given task_ids that ran the SAME collector on different hosts, produce a structured diff: per-field values that agree across all hosts vs values that differ. Use to spot outliers.",
  "input_schema": {
    "type": "object",
    "properties": {
      "task_ids": {
        "type": "array",
        "items": {"type": "string"},
        "minItems": 2,
        "maxItems": 20
      }
    },
    "required": ["task_ids"]
  }
}
```

```json
{
  "name": "get_full_result",
  "description": "Retrieve the FULL structured output of a previous collector (not the summary). Use when the summary is insufficient and you need every field.",
  "input_schema": {
    "type": "object",
    "properties": {
      "task_id": {"type": "string"}
    },
    "required": ["task_id"]
  }
}
```

### 4.3. Investigation tools

```json
{
  "name": "add_finding",
  "description": "Record a structured finding in the investigation memo. Use whenever you reach a conclusion supported by evidence. Every finding MUST cite at least one task_id.",
  "input_schema": {
    "type": "object",
    "properties": {
      "severity": {
        "type": "string",
        "enum": ["info", "warn", "error", "critical"]
      },
      "code": {
        "type": "string",
        "description": "Short machine-readable code, dot-separated lowercase. Examples: 'cert.expired.kubelet', 'service.inactive', 'net.unreachable'."
      },
      "message": {
        "type": "string",
        "maxLength": 280,
        "description": "Human-readable finding. Be specific about host(s) and metric."
      },
      "evidence_refs": {
        "type": "array",
        "items": {"type": "string"},
        "minItems": 1,
        "description": "task_id(s) supporting this finding."
      }
    },
    "required": ["severity", "code", "message", "evidence_refs"]
  }
}
```

```json
{
  "name": "ask_operator",
  "description": "Pause the investigation and ask the human operator a question — for information only they have (domain knowledge, intent, scope decisions). The investigation resumes after their reply. Use sparingly; do not ask what you can observe yourself.",
  "input_schema": {
    "type": "object",
    "properties": {
      "question": {"type": "string"},
      "context": {
        "type": "string",
        "description": "Optional explanation of why you are asking and what you'll do with the answer."
      }
    },
    "required": ["question"]
  }
}
```

```json
{
  "name": "mark_done",
  "description": "Finalize the investigation with a structured post-mortem. Call when root cause is identified, OR all reasonable avenues are exhausted, OR operator signals completion.",
  "input_schema": {
    "type": "object",
    "properties": {
      "summary": {
        "type": "object",
        "properties": {
          "symptoms": {
            "type": "array",
            "items": {"type": "string"}
          },
          "hosts_examined": {
            "type": "array",
            "items": {"type": "string"}
          },
          "root_cause": {
            "type": "string",
            "description": "One paragraph. Say 'inconclusive' if unknown."
          },
          "evidence_refs": {
            "type": "array",
            "items": {"type": "string"}
          },
          "recommended_remediation": {
            "type": "string",
            "description": "Actions for the operator to take. You do not perform them."
          }
        },
        "required": ["symptoms", "hosts_examined", "root_cause", "recommended_remediation"]
      }
    },
    "required": ["summary"]
  }
}
```

---

## 5. Формат `tool_result` (что летит обратно в Claude)

Именно в этом форматировании кроется эффективность контекста. Hub не бросает Claude сырой `data_json` коллектора — он генерирует **компактное summary** (200-2000 токенов) и прикладывает метаданные.

### 5.1. Общая структура для `collect` / `collect_batch`

```json
{
  "task_id": "t_01JK3Z5VPQ",
  "host_id": "node-3.cluster.local",
  "collector": "journal_tail",
  "status": "ok",
  "duration_ms": 412,
  "params_used": {"unit": "kubelet.service", "since": "30m"},
  "summary": {
    "lines_returned": 842,
    "time_range": "2026-04-17T14:00Z..14:30Z",
    "error_lines": 47,
    "warn_lines": 12,
    "top_error_patterns": [
      {"pattern": "x509: certificate has expired", "count": 31},
      {"pattern": "connection refused", "count": 11}
    ],
    "first_entry_at": "14:11:02",
    "last_entry_at": "14:29:58"
  },
  "hints": [
    {
      "severity": "error",
      "code": "cert.expired",
      "message": "kubelet certificate appears expired"
    }
  ],
  "artifacts": [
    {
      "name": "journalctl.txt",
      "size_bytes": 184523,
      "lines": 842,
      "hint": "Use search_artifact for full grep access"
    }
  ],
  "full_result_available": true
}
```

Каждый коллектор сам решает, что попадает в `summary` — это часть его манифеста. Задача при разработке коллектора: в summary **должны быть те поля, по которым Claude принимает решения**. Для `systemd_units` это `name, active_state, sub_state, failed_count`; для `net_connect` — `targets_tested, successful, failed, avg_latency_ms, failures_sample`. Raw dump уезжает в artifact.

### 5.2. Для `list_hosts`

```json
{
  "hosts": [
    {
      "id": "node-3.cluster.local",
      "labels": {"env": "prod", "role": "k8s-master", "dc": "msk-1"},
      "facts": {
        "os": "Ubuntu",
        "os_version": "22.04",
        "kernel": "5.15.0-88-generic",
        "primary_ip": "10.0.0.13",
        "cpu_count": 8,
        "ram_gb": 16
      },
      "status": "online",
      "last_seen": "2026-04-17T14:32:01Z",
      "agent_version": "0.1.0",
      "available_collectors": ["system_info", "systemd_units", "journal_tail", "net_ifaces", "net_listen", "net_connect", "dns_resolve", "process_list", "file_read", "disk_usage"]
    }
  ],
  "total": 5,
  "filter_applied": "role=k8s-master"
}
```

### 5.3. Для ошибок и edge cases

Статус `error`:

```json
{
  "task_id": "t_...",
  "host_id": "node-3.cluster.local",
  "collector": "journal_tail",
  "status": "error",
  "error": "sudo: journalctl not in allow-list with args (-u crond)",
  "duration_ms": 23,
  "hint_to_model": "The collector rejected the params. Check describe_collector for valid unit names."
}
```

Статус `timeout`:

```json
{
  "task_id": "t_...",
  "status": "timeout",
  "duration_ms": 30000,
  "error": "collector exceeded 30s timeout",
  "hint_to_model": "Retry with larger timeout_seconds or narrower params."
}
```

Поле `hint_to_model` — лаконичная директива, помогающая Claude исправиться. Не всегда нужно, но в ошибках даёт понятный сигнал «что делать дальше».

---

## 6. Инъекции от оператора

### 6.1. `OPERATOR HYPOTHESIS`

Вставляется как **user message** в messages. Точный формат:

```text
OPERATOR HYPOTHESIS  [priority: HIGH]
Claim: <короткое утверждение гипотезы>
Expected evidence: <что проверить для подтверждения/опровержения>
Instruction: verify this hypothesis before continuing other branches.
```

**Логика hub'а:** в момент нажатия «Hypothesis» текущее `pending` tool_call отменяется (помечается `status=skipped`, `decided_at=now()`). В messages append'ится user message с блоком выше. Следующий `messages.create` вызов к Claude должен привести к tool_use, проверяющему гипотезу.

System prompt (rule 5a) требует этого поведения обязательно.

### 6.2. `OPERATOR ACTIONS: IGNORED`

Вставляется как **system_note** (реализуется в Anthropic API как user message со специальным префиксом, т.к. после первого user message system меняться нельзя). Формат:

```text
[SYSTEM NOTE — OPERATOR ACTIONS since last turn]
- Finding F-3 "CoreDNS pod restarted 12 times on node-3" marked IGNORED.
  Reason: known transient, under separate investigation.
  Do not investigate this direction further in this investigation.
```

Добавляется перед следующим запросом Claude'а. Если за один промежуток оператор сделал несколько действий (2 ignore, 1 pin), они собираются в один блок.

### 6.3. Свободные реплики оператора

Обычный user message. Без специального префикса — Claude понимает их как советы, но не обязан следовать, как в случае `HYPOTHESIS`. Пример:

```text
обрати внимание на m3 — он на прошлой неделе был перезагружен вручную.
```

Claude учитывает, но сам решает, как вплести.

---

## 7. Compaction prompt

Когда `tokens_in > 150_000` (или `steps > 25`), перед следующим обычным ходом hub делает отдельный **compaction call** к Claude и заменяет messages на результат.

### 7.1. Запрос

Отдельный вызов с чистой историей:

- `system`: compaction prompt (ниже)
- `messages`: один user message с полной историей investigation'а как текст
- `tools`: пустой массив (tool-use не нужен)
- `temperature: 0`
- `max_tokens: 2000`

### 7.2. Compaction prompt

```text
You are compacting an ongoing SRE investigation to fit a limited context window. Your output will REPLACE the older messages in the investigation history, so it must preserve every fact needed to continue productively.

Produce a dense structured state in this exact format (markdown):

## Goal
<verbatim restatement of the original goal>

## Inventory
<hosts discovered so far with relevant labels and facts; keep only what matters for the investigation>

## Established facts
<bulleted, each with evidence_refs. Only things we know to be TRUE.>

## Findings recorded
<list of findings added via add_finding: severity, code, message, evidence_refs, and whether operator has marked them pinned/ignored>

## Pursued hypotheses
<for each hypothesis explored: statement, status (confirmed/refuted/partial), evidence_refs>

## Ignored directions (operator)
<verbatim list of IGNORED findings/branches>

## Operator hypotheses (outstanding)
<any OPERATOR HYPOTHESIS blocks that remain unresolved>

## Open threads
<what was being investigated at compaction time>

## Next logical step
<your recommended next tool call in prose, not as a tool_use block>

Rules:
- Be dense. Omit redundant information.
- Preserve ALL task_ids that are referenced anywhere (as evidence or as sources of artifacts).
- Do not invent or speculate — only summarize what happened.
- Do not include raw log excerpts. Reference them by task_id/artifact_name.
```

### 7.3. Результат

Ответ Claude — markdown-блок. Hub:
1. Сохраняет его в БД (`messages.content` с `role=compacted_state`).
2. Удаляет из активного окна все сообщения до этой точки (оставляя в БД как `archived=true`).
3. Формирует новый старт активного контекста: старый system prompt + один user message `<compacted_state>...</compacted_state>` + последние 2-3 реальных шага.

---

## 8. Управление messages: что куда кладём

Ментальная модель messages в Recon:

| Тип | role в API | Когда создаётся |
|-----|------------|-----------------|
| Goal (первый user msg) | `user` | При старте investigation |
| Claude's proposal | `assistant` | Каждый ход, content = [thinking?, tool_use] |
| Tool result | `user` | После исполнения tool_use, content = [tool_result] |
| Synthetic tool_result (при skip) | `user` | Когда operator skipped, content = специальное сообщение (см. ниже) |
| Operator hypothesis | `user` | После нажатия Hypothesis |
| Operator free message | `user` | После ввода в chat |
| System note (ignore, rate limit warning, budget warning) | `user` с префиксом `[SYSTEM NOTE ...]` | По событию в UI |
| Compacted state | `user` | После compaction, заменяет старую историю |

### 8.1. Synthetic tool_result при `skip`

Когда оператор нажал Skip на pending tool_use, hub всё равно должен вернуть Claude что-то в формате `tool_result` (иначе API ругнётся). Генерируем:

```json
{
  "type": "tool_result",
  "tool_use_id": "toolu_01JK...",
  "content": "Operator skipped this step. Reason: not provided. Propose a different approach."
}
```

Если оператор ввёл reason — кладём его в `content`.

### 8.2. Edit-and-execute

Когда оператор редактирует input и нажимает Execute, с точки зрения истории это выглядит так, **как будто Claude сам предложил отредактированные параметры**. Hub перезаписывает `tool_use.input` на отредактированный, исполняет, tool_result летит нормально. Для audit — отдельная запись «operator edited input», но в messages остаётся как есть.

---

## 9. Few-shot примеры

Эти примеры предназначены для двух целей: (а) чтобы мы, разработчики, понимали ожидаемое поведение; (б) — потенциально — для включения в system prompt как inline examples, если окажется, что качество нестабильно.

### 9.1. Хорошее открытие

**Goal:** `"k8s cronjobs not firing on the cluster. Suspected cause: control plane issue. Focus on 5 master nodes."`

Claude →

```xml
<thinking>
I don't yet know the inventory. Before probing anything, list hosts filtered by role=k8s-master to confirm I have the 5 nodes.
</thinking>
```

```json
{"name": "list_hosts", "input": {"selector": "role=k8s-master"}}
```

### 9.2. Реакция на hypothesis

**Previous turn** закончился pending'ом `collect(host=m1, collector=systemd_units)`.

**Operator** нажал Hypothesis. Hub отменил pending и добавил:

```text
OPERATOR HYPOTHESIS  [priority: HIGH]
Claim: the cron-controller container may be OOM-killed on the active controller-manager master
Expected evidence: recent OOM events in dmesg or kernel log, or high memory usage immediately before restarts
Instruction: verify this hypothesis before continuing other branches.
```

Claude →

```xml
<thinking>
Operator wants the OOM hypothesis verified first. journal_tail with --dmesg gives us kernel log OOM lines; running in batch over all 5 masters to spot the affected node. Window: last hour.
</thinking>
```

```json
{
  "name": "collect_batch",
  "input": {
    "host_ids": ["m1", "m2", "m3", "m4", "m5"],
    "collector": "journal_tail",
    "params": {"dmesg": "true", "grep": "(?i)oom|killed process", "since": "1h"},
    "timeout_seconds": 45
  }
}
```

### 9.3. Добавление finding

После предыдущего шага Claude получил tool_result, где у `m3` 4 OOM event'а, у остальных — 0. Следующий ход:

```xml
<thinking>
m3 shows 4 OOM kills of cron-controller in the last hour, other masters zero. Solid evidence for the hypothesis. Recording finding before deciding next step.
</thinking>
```

```json
{
  "name": "add_finding",
  "input": {
    "severity": "critical",
    "code": "oom.killed.cron_controller",
    "message": "cron-controller on m3 OOM-killed 4 times in the last hour; other masters unaffected",
    "evidence_refs": ["t_01JK3ZABCDEF"]
  }
}
```

### 9.4. Ignored — не лезем обратно

После того как оператор пометил finding F-3 ("CoreDNS pod restarted 12 times on node-3") как IGNORED, hub в следующий запрос вставляет system note. Claude получает новые данные, которые снова указывают на CoreDNS как проблему:

```xml
<thinking>
Data again points toward CoreDNS, but operator marked that direction IGNORED. Per rule 5b, I do not re-enter. Instead, switch to examining the systemd kubelet state directly via collect on the surviving master.
</thinking>
```

```json
{
  "name": "collect",
  "input": {"host_id": "m2", "collector": "systemd_units", "params": {"filter": "kubelet.service"}}
}
```

### 9.5. Использование `ask_operator`

`list_hosts` вернул 5 мастеров, но у `m4` лейбл `env=staging`:

```xml
<thinking>
m4 has env=staging while others are env=prod. Ambiguous scope — operator said "5 master nodes" but may or may not mean staging. Asking.
</thinking>
```

```json
{
  "name": "ask_operator",
  "input": {
    "question": "m4 has label env=staging while m1/m2/m3/m5 are env=prod. Should I include m4 in this investigation?",
    "context": "If m4 is a separate cluster, its findings won't explain the prod cronjob outage. Excluding saves ~20% of probe time."
  }
}
```

### 9.6. Корректное завершение

Root cause подтверждён двумя независимыми источниками (journal OOM events + process memory trend):

```xml
<thinking>
Two independent evidence sources confirm cron-controller OOM on m3 as the cause. Closing with structured summary.
</thinking>
```

```json
{
  "name": "mark_done",
  "input": {
    "summary": {
      "symptoms": [
        "CronJobs stopped firing cluster-wide starting 14:10",
        "kubectl get cronjobs shows LAST SCHEDULE aging out"
      ],
      "hosts_examined": ["m1", "m2", "m3", "m4", "m5"],
      "root_cause": "cron-controller container on master m3 (the active controller-manager leader) is being OOM-killed due to insufficient memory limits. Without a live CronJob controller, no new jobs are scheduled cluster-wide. Problem scoped to m3; other masters are healthy.",
      "evidence_refs": ["t_01JK3ZABCDEF", "t_01JK3ZGHIJKL", "t_01JK3ZMNOPQR"],
      "recommended_remediation": "Option 1 (fast): raise the memory limit on cron-controller DaemonSet/Deployment by 512Mi and roll it. Option 2 (proper): investigate why memory footprint grew — new CronJob count, informer cache, or leak after recent upgrade (see kube-controller-manager build date in evidence). After remediation, verify by (a) 'kubectl get events -n kube-system | grep cron-controller' shows no new OOM events in 10 minutes, (b) a test CronJob with schedule '* * * * *' fires on the next minute boundary."
    }
  }
}
```

---

## 10. Парсинг ответов Claude

Ответ API имеет структуру:

```json
{
  "id": "msg_...",
  "model": "...",
  "stop_reason": "tool_use | end_turn | max_tokens | stop_sequence",
  "content": [
    {"type": "thinking", "thinking": "..."},
    {"type": "tool_use", "id": "toolu_...", "name": "collect", "input": {...}}
  ],
  "usage": {"input_tokens": 1234, "output_tokens": 567}
}
```

### 10.1. Ожидаемое поведение

Happy path: `stop_reason == "tool_use"`, content содержит `thinking` (если extended thinking включён) + один `tool_use`. Hub сохраняет thinking, tool_use → pending, стримит в UI.

### 10.2. Edge cases

| Ситуация | Реакция hub'а |
|----------|---------------|
| `stop_reason == "tool_use"`, два и больше `tool_use` блоков | Исполняем первый. Остальные отбрасываем. После исполнения в tool_result к первому прикладываем system note: `"You returned N tool calls; only the first was executed. Submit one per turn."` |
| `stop_reason == "end_turn"`, тулов нет, только text | Не должно случаться при `tool_choice: any`. Если случилось — логируем, вставляем user message: `"Your response did not contain a tool call. Please propose exactly one tool_use or call ask_operator."`, повторяем вызов |
| `stop_reason == "max_tokens"` | Response, возможно, обрезан. Отображаем оператору. Предлагаем кнопку «retry with larger max_tokens» |
| `tool_use.name` не существует в каталоге | Возвращаем synthetic tool_result: `"Unknown tool '<name>'. Available: <list>."` |
| `tool_use.input` не проходит JSON-schema валидацию | Возвращаем synthetic tool_result: `"Invalid arguments: <details>. See describe_collector / tool schema."` |
| Referenced `task_id` не существует | Synthetic tool_result: `"task_id '<id>' not found in this investigation."` |
| `host_id` не существует или offline | Synthetic tool_result: `"host '<id>' is offline/unknown. Use list_hosts."` |
| `collect_batch` с `host_ids.length > 50` | Валидация фейлится до отправки; synthetic tool_result `"host_ids exceeds limit of 50. Narrow your selector."` |

### 10.3. Retry policy

На сетевые ошибки Anthropic API (5xx, timeout) — до 3 попыток с exponential backoff (1s, 4s, 10s). На 4xx (кроме rate limit) — фейл, уходит оператору.

На rate limit (429) — ждём `retry-after` и делаем одну попытку.

---

## 11. Валидация tool_use перед исполнением

Hub делает валидацию в следующем порядке. Падение любого шага = synthetic tool_result с ошибкой, без исполнения.

1. Tool name есть в catalog.
2. `input` соответствует `input_schema` (JSON Schema Draft 7).
3. Для `collect` / `collect_batch`:
   - `collector` существует в глобальном catalogue;
   - каждый `host_id` существует и помечен `online`;
   - collector доступен на таком хосте (`collector_manifests`);
   - `params` проходят `ParamsSchema` коллектора.
4. Для `search_artifact` / `get_full_result` / `compare_across_hosts` — `task_id`(ы) принадлежат текущей investigation.
5. Broad-selector check (если суммарно >5 хостов) — если в investigation не включён режим «broad-auto» — ставим tool_call в `pending_broad_confirm`, UI показывает «Claude wants to run on 12 hosts — confirm?». Это **не** synthetic error, а дополнительный слой UX-подтверждения поверх approve.

---

## 12. Тестовая прошивка (fixtures) для локальной итерации

Чтобы двигать промпт, не гоняя живые агенты, полезно иметь **mock-агента**: коллекторы-фикстуры, возвращающие заранее записанные data_json. Храним в `testdata/collectors/<collector>/<scenario>.json`. Сценарии для MVP:

- `k8s_healthy` — всё ок, baseline
- `k8s_cert_expired` — базовый failure mode
- `k8s_oom_cron` — OOM хозяина cron-controller
- `k8s_split_brain` — сеть между мастерами нарушена
- `k8s_dns_broken` — CoreDNS неработоспособен
- `generic_disk_full` — 100% диска на одной ноде

Investigation прогоняется против mock-агентов, результат transcript'а проверяется глазами: адекватный ли план, нет ли дрифтов, правильно ли реагирует на hypothesis/ignore.

---

## 13. Стратегия итерации промпта

Промпт не догма. Ожидаем 3-5 значимых ревизий до «стабильного» состояния.

**Что измеряем:**

- Steps-to-resolution по каждому фикстур-сценарию.
- Token usage на investigation.
- Operator override rate (approve vs edit vs skip vs hypothesis) — высокий edit/skip говорит, что Claude плохо целится; высокий hypothesis — что не видит очевидного.
- Failed validations (tool_use rejected hub'ом) — индикатор непонимания схем.

**Что может меняться:**

- Описания tools → это **первое**, куда лезем при проблемах. Хорошее описание tool'а важнее, чем правило в system prompt'е.
- Описания коллекторов (в `list_collectors` / `describe_collector`) — аналогично.
- System prompt: ужесточение rule'ов, когда Claude устойчиво нарушает.
- Формат `tool_result` summary: если Claude пропускает важное — добавляем в summary; если путается в лишнем — убираем.
- Few-shot inline examples в system prompt — добавляем, когда generic rule не ловит нужное поведение на конкретном кейсе.

**Версионирование:** prompt-файлы (`system_prompt.tmpl`, `tool_specs.json`, `compaction_prompt.tmpl`) лежат в `internal/hub/investigator/prompts/`, каждый имеет `version` в шапке. Investigation в БД пишет версии, с которыми она шла — для отладки регрессий.

---

## 14. Что остаётся открытым

- **Inline few-shots в system prompt или нет?** Добавим, если качество без них просядет. Плата — +1-2K токенов на каждый вызов.
- **Thinking: enabled или disabled?** Начинаем с enabled, budget 3000. Мониторим, приносит ли качество пользу, соразмерную стоимости.
- **Cross-investigation learning.** Формально памяти нет (п.1 design'а). Но логи investigations можно использовать вручную при ревизии промптов — это не автоматическое обучение, это наша инженерная работа.
- **Локализация goal'а.** Оператор может ставить цель на русском. Claude работает с этим, но имена коллекторов, severity, codes — всё английское. Смешение нормально, но стоит прогнать через фикстуры.

---

*Документ живой. Изменения через PR в `DOCS/Prompts/BASE_TASKS.md`.*