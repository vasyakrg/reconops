# DESIGN_HUB — дизайн-бриф для web UI Recon Hub

> Этот документ — задание для Claude (или другого дизайнера). Цель — получить
> связную визуальную систему и HTML/CSS прототипы ключевых экранов `recon-hub`.
> Перед любой работой **обязательно** прочитать `PROJECT.md` §3, §4.3 и
> `BASE_TASKS.md` §2–§3 — они описывают модель взаимодействия оператора с LLM,
> которая и формирует UI.

---

## 0. TL;DR для дизайнера

Recon Hub — это **operator cockpit во время инцидента на Linux-парке**.
Не дашборд, не админка, не marketing-sass. Оператор сидит перед большим
монитором, читает план LLM-исследователя, аппрувит каждый шаг по одному,
видит findings со ссылками на evidence, при желании — ломает план своей
гипотезой. Всё read-only по конструкции: UI физически не может сломать
прод, и это должно **читаться визуально** — никаких красных destructive-
кнопок, никакой агрессии.

Эстетика: **Linear / Vercel dark** — нейтральный графит, тонкие бордеры,
один акцентный цвет, density выше среднего, монопропорциональный шрифт
для data. Интерфейс **живой** — реальное время через SSE, пульсы на
статусах, плавное появление новых строк timeline, прогресс-бары на
бюджетах токенов и шагов.

---

## 1. Контекст продукта (в одном абзаце)

Recon — система диагностики Linux-инфраструктуры, управляемая LLM. Агент
на каждом хосте публикует read-only коллекторы (`dns_resolve`,
`net_listen`, `systemd_units`, `journal_tail` и т.д.). Оператор в UI
открывает **Investigation** — формулирует цель словами («почему крон-джобы
падают на prod k8s»), и LLM-исследователь строит план step-by-step:
каждый шаг — это один `tool_use` (вызов коллектора), который оператор
видит как карточку и **аппрувит вручную**. По итогу — структурированный
post-mortem с findings, каждый findings — с ссылкой на `task_id`
реального наблюдения. См. `README.md` и `DOCS/Prompts/PROJECT.md` §1–§3.

## 2. Кто пользователь

- **SRE / devops** в разгар инцидента. Усталый, под давлением, смотрит на
  24–32" монитор в тёмной комнате в 3 часа ночи.
- Читает JSON свободно, любит `kubectl`, `k9s`, `btop`, Grafana.
- Не ждёт, что ему всё объяснят визуально, — но ценит когда информация
  *плотно* подана и ничего не спрятано под лишние клики.
- Работает в одной сессии по 1–8 часов. Глазам должно быть комфортно.

## 3. Технологический стек (жёсткий)

Из `CLAUDE.md` репозитория:

- Server-rendered Go `html/template` + **HTMX** (полу-SPA без JS-фреймворка)
- **Alpine.js** для мелкой интерактивности (dropdowns, tabs)
- **Tailwind CSS через CDN** — сборки нет, только готовые утилиты базовой
  стилевой карты (никакого `tailwind.config.js` с кастомными цветами —
  вместо них `<style>` с CSS custom properties)
- **SSE** (`EventSource`) для real-time (см. `investigation_detail.html`)
- Mono-binary: все ассеты в `internal/hub/web/templates/` + `web/static/`

Из этого следует: **никаких React-компонентов, Figma-only мокапов,
Storybook, Vite.** Всё, что рисуется, должно быть выражаемо через обычный
HTML + Tailwind-классы + небольшой CSS-слой в `<style>` / `static/hub.css`.

## 4. Принципы дизайна

1. **Operator-first, не AI-first.** Не чат-бот. LLM — инструмент, оператор
   — субъект. Pending-карточка = «модель предложила, ты решаешь».
   Кнопки аппрува всегда заметнее, чем сообщение LLM.

2. **Read-only visual language.** Никаких ярко-красных destructive
   action buttons. Primary действия — `Approve`, `Skip`, `End`. Красный
   зарезервирован **только** под severity=error в findings и статус
   `offline`/`failed`. Система не должна выглядеть опасной — потому что
   она и не опасна (5 слоёв read-only enforcement, см. PROJECT.md §3.4).

3. **Density > воздух.** Цель — видеть pending tool call, последние 5
   шагов timeline, список findings, бюджеты и статус хоста на одном
   экране без скролла на 1440×900+. Ориентир density: k9s, Linear,
   Datadog, GitHub file tree. Не Notion, не Stripe marketing.

4. **Honest motion.** Анимация — индикатор события, не украшение.
   Движение допустимо когда:
   - прилетел SSE (новая строка в timeline → плавный fade-in за 200мс)
   - изменился статус хоста (пульс 800мс на badge)
   - LLM думает (дыхание pending-карточки: subtle opacity oscillation)
   - меняются счётчики токенов/шагов (плавная интерполяция числа)

   **Запрещено:** бесконечные shimmer-скелетоны, фоновые gradient-
   animations, параллакс, page transitions, hover-эффекты на карточках
   сильнее `brightness(1.05)`.

5. **JSON — first-class citizen.** Половина контента — это evidence в
   JSON. Нужен отдельный компонент `<pre class="json">` с:
   - синтаксической подсветкой (ключи / строки / числа / null)
   - fold/unfold вложенных объектов
   - `copy` кнопкой в правом верхнем углу
   - line-numbers опционально
   - гарантированной моноширинкой на любом браузере (fallback стек)

6. **Desktop only.** Не адаптируем под мобилки и планшеты. Минимум
   1280px ширины. Основная сетка строится под 1440–2560px. Допустимо
   явно ломать layout < 1280 с предупреждением «open on desktop».

---

## 5. Эстетика и референсы

**Направление:** Linear / Vercel dark dev-tool.

**Изучить и впитать (не копировать):**

- **Linear** (linear.app, dark mode) — цветовая температура, акцентная
  кнопка, таблицы issues, плотность списков.
- **Vercel dashboard** — структура nav, карточки проектов, типографика.
- **Raycast Pro** — сочетание mono/sans, мелкие spacing, status dots.
- **Grafana 10 dark** — density таблиц, semantic колоры, живые панели.
- **Sentry** (dark) — issue detail view — особенно для
  `investigation_detail`.
- **GitHub Actions job view** — как показывать timeline шагов с
  результатами ниже.
- **k9s / lazygit** — «ощущение консоли», mono density.

**Не брать за основу:**

- Stripe/Tailwind UI marketing dark (слишком воздушно, декоративно).
- Supabase/Neon landing (много градиентов).
- Любой glassmorphism, neon, cyberpunk.
- Admin-Boilerplate дашборды с огромными круговыми диаграммами.

---

## 6. Визуальная система

### 6.1. Цветовая палитра (starter — дизайнеру докрутить)

Использовать CSS custom properties, не Tailwind-custom-цвета (чтобы
жить на CDN без сборки). Предлагаемая база:

```css
:root {
  /* Neutral scale (background → foreground) */
  --bg-0:        #0a0b0d;   /* deep canvas */
  --bg-1:        #111215;   /* panels */
  --bg-2:        #171a1f;   /* elevated cards */
  --bg-3:        #1d2026;   /* hover, stripes */
  --border:      #2a2e36;   /* thin dividers */
  --border-hi:   #3a3f4a;   /* focused, active */

  --fg-0:        #e6e8eb;   /* primary text */
  --fg-1:        #a8adb5;   /* secondary text */
  --fg-2:        #6b7079;   /* muted, timestamps */
  --fg-3:        #4a4e57;   /* disabled, placeholders */

  /* Single accent — activity, primary action, live indicator */
  --accent:      #7aa2ff;   /* muted blue, не кислотный */
  --accent-hi:   #9fbaff;
  --accent-dim:  #3a4a73;

  /* Semantic (status / severity) */
  --ok:          #4ec9a4;   /* online, success */
  --warn:        #e5b567;   /* degraded, severity=warn */
  --err:         #e06c75;   /* offline, severity=error */
  --info:        #7aa2ff;   /* running, info */
  --pending:     #c3a6e0;   /* pending approval — выделяется из остальных */
}
```

Контрастность: `--fg-0` на `--bg-0` ≥ 13:1 (AAA), `--fg-1` на `--bg-1`
≥ 7:1. Semantic-цвета — не менее 4.5:1 на любом фоне, где используются.

### 6.2. Типографика

- **Sans (UI chrome):** `Inter`, `Geist Sans`, `-apple-system`, system-ui
  fallback. Основной размер 13–14px (не 16 — плотность).
- **Mono (data, IDs, JSON, tool names, commands):** `JetBrains Mono`,
  `Geist Mono`, `ui-monospace`, `Menlo`. Размер 12–13px.
- **Features:** `font-variant-numeric: tabular-nums` обязательно для
  любых колонок со счётчиками/временем.
- **Размеры (type scale):**
  - `text-xs` 11px — timestamps, помощь, small labels
  - `text-sm` 13px — основной UI text
  - `text-base` 14px — body
  - `text-lg` 16px — section titles
  - `text-xl` 20px — page title
  - `text-2xl` 24px — рабочий максимум (только investigation goal)

### 6.3. Spacing & сетка

- Базовая единица 4px. Gaps 4/8/12/16/24/32.
- Контейнер страницы **не ограничиваем по max-width** (это не блог).
  Full-bleed, с внутренними padding 24–32px.
- Sidebar nav слева, фиксированная ширина 220px, в ней — разделы.
  Альтернатива: top-nav в одну строку, если экономим вертикаль
  (investigation view съедает её всю).

### 6.4. Компонентные токены

- **Borders:** `1px solid var(--border)`. Почти никогда не `2px`.
- **Border-radius:** 6px для карточек, 4px для кнопок/инпутов, 3px для
  бейджей. Никаких `rounded-full` пилюль.
- **Elevation:** тенями не злоупотребляем (в тёмной теме плохо
  читаются). Вместо — `--bg-1/2/3` разная яркость.
- **Focus ring:** `0 0 0 2px var(--accent-dim)`, обязательно для
  a11y.

---

## 7. Motion guidelines

### 7.1. Что анимируем

| Событие                                      | Анимация                                 | Длительность | Easing      |
|----------------------------------------------|------------------------------------------|--------------|-------------|
| SSE: новая строка в timeline                 | fade-in + slide-down 4px                 | 200ms        | ease-out    |
| SSE: изменился статус карточки               | flash-highlight фона → `--bg-3`          | 800ms        | ease-out    |
| Host/agent status change                     | pulse на status dot (2 цикла)            | 1400ms       | ease-in-out |
| LLM «думает» (pending, ждём ответа)          | breathing opacity 0.7↔1.0                | 2200ms loop  | ease-in-out |
| Счётчики токенов/шагов                       | numeric tween                            | 400ms        | ease-out    |
| Progress bar (steps/tokens budget)           | width transition                         | 400ms        | ease-out    |
| Expand `<details>` с JSON                    | height auto + content fade-in            | 150ms        | ease-out    |
| Approve button click → карточка уезжает      | fade-out + collapse                      | 250ms        | ease-in     |
| New finding появляется                       | fade-in + 2px slide + 600ms акцент-glow  | 600ms        | ease-out    |

### 7.2. Что НЕ анимируем

- Переходы между страницами (обычный reload / HTMX swap без transitions).
- Hover на таблицах — максимум `background: var(--bg-3)` без transition.
- Кнопки — без lift/scale эффектов.
- Никогда `animation: pulse` на всей карточке (только на статус-точке).

### 7.3. Как SSE интегрируется с UI

Текущий паттерн (см. `investigation_detail.html`): EventSource слушает
`state-change`, при расхождении с baseline — **перезагружает страницу**.
Это грубо. В редизайне:

1. Hub посылает SSE events **с типом и payload-ом** (не только state
   hash):
   - `tool_call:new` — новый pending шаг
   - `tool_call:result` — пришёл результат
   - `finding:added` — новый finding
   - `budget:update` — изменились счётчики
   - `status:change` — изменился статус investigation/host
2. Клиент через HTMX-SSE extension (или Alpine) **patch**-ит нужный
   фрагмент страницы (`hx-swap-oob` / Alpine store), без полного reload.
3. Анимация появляется **на вставке** нового фрагмента.
4. Fallback: если клиент упал — через 10 сек мягкий toast «connection
   lost, refresh?»

Этот переход от «reload on change» к «patch on event» — часть дизайн-
задачи: компоненты должны быть готовы к точечным обновлениям.

---

## 8. Экраны к проработке

Читать текущие шаблоны перед редизайном:
`internal/hub/web/templates/*.html`. Они рабочие, но выглядят как HTML
1999-го — это baseline, не target.

### 8.1. Layout / навигация (`layout.html`) — **priority P0**

- Sidebar слева: логотип «Recon» + версия, секции:
  - `Hosts` (иконка сервера)
  - `Collectors` (иконка пазла)
  - `Runs` (иконка play)
  - `Investigations` (иконка лупы) — **подсветить счётчиком активных**
  - `Audit` (иконка свитка)
  - `Settings` (иконка шестерёнки)
- Внизу sidebar: пользователь + logout, индикатор подключения hub→agents
  (N/M online, зелёная точка).
- Основная область — full-bleed, страница сама управляет layout.

### 8.2. Investigations list (`investigations_list.html`) — **P0**

Таблица с колонками: `ID` (короткий хэш, mono) · `Goal` (truncate) ·
`Status` (badge) · `Steps` · `Tokens` · `Findings` (с разбивкой по
severity, микро-бары) · `Created` · `Updated`. Фильтры вверху:
`status in [active, done, aborted]`, `host`, `date`. Строка кликабельная
целиком.

### 8.3. Investigation detail (`investigation_detail.html`) — **P0 HERO**

**Главный экран. На него тратить больше всего дизайн-времени.**

Предлагаемый layout (3 колонки на широком мониторе, стек на узком):

```
┌─ Header bar ────────────────────────────────────────────────────┐
│ [ID] [Goal truncate…] [status badge] [model] [budgets: steps/tokens bars] [actions: refresh / export / abort] │
├──────────────────────────┬─────────────────────┬────────────────┤
│ TIMELINE (60%)           │ FINDINGS (25%)      │ CONTEXT (15%)  │
│                          │                     │                │
│ ▸ step 1 system_info …   │ 📌 error CERT_EXP   │ Messages: 14   │
│   ▸ rationale            │   /etc/kube…crt     │ Tool calls: 9  │
│   ▸ compact result       │ ⚠ warn DNS_FLAKY    │ Artifacts: 12  │
│ ▸ step 2 dns_resolve…    │   resolv.conf line3 │                │
│                          │                     │ Operator       │
│ ━━━ PENDING ━━━          │                     │ hypothesis     │
│ ╔════════════════════╗   │                     │ [inject form]  │
│ ║ [tool] net_listen  ║   │                     │                │
│ ║ rationale: …       ║   │                     │                │
│ ║ input { … }  [edit]║   │                     │                │
│ ║ [Approve] [Edit]   ║   │                     │                │
│ ║ [Skip]   [End]     ║   │                     │                │
│ ╚════════════════════╝   │                     │                │
└──────────────────────────┴─────────────────────┴────────────────┘
```

**Pending tool call card** — сердце интерфейса. Требования:
- Чётко выделена рамкой акцентного цвета (не жёлтой «warning»-заливкой
  как сейчас — это не warning, это решение).
- Название инструмента — mono, крупно.
- Rationale — italic, `--fg-1`.
- Input JSON — inline editor (monaco-lite либо `<textarea>` с mono и
  автоформатом через простой JS-prettifier).
- 4 кнопки: Approve (primary accent) · Edit & Approve (secondary) ·
  Skip (ghost) · End investigation (ghost с тонкой красной рамкой,
  НЕ заливка).
- «Дыхание» opacity пока ждём SSE-тика.

**Timeline** — вертикальный поток шагов, каждый шаг — карточка с:
`[seq][status dot][tool name mono] [rationale]`, разворачивается в
input + compact result (не raw JSON! — использовать 3-tier context:
summary → full result через клик «full» → raw artifact через отдельное
окно). Цветовая маркировка status dot: pending (accent), approved
(info), executed (ok), skipped (muted), error (err).

**Findings panel** — компактный список. Pinned наверху (📌). Ignored
внизу с opacity 0.4. Клик по finding → подсвечивает соответствующие
шаги в timeline (scroll + glow).

**Operator hypothesis** — не `<details>` как сейчас, а отдельный
призыв-to-action в правой колонке: «Направить модель?». При раскрытии
— inline форма на 3 поля.

### 8.4. Hosts list (`hosts.html`) + Host detail (`host_detail.html`) — **P1**

List: таблица с status dot (online/offline/degraded), ID, labels как
chip-строка, agent version, last seen (с `time-ago` и tooltip-абсолют).
Detail: шапка с host info, затем табы `Overview` / `Recent runs` /
`Recent findings` / `Labels`. На Overview — метрики last heartbeat,
enrolled date, collector catalog версия.

### 8.5. Runs list + Run detail — **P1**

Run = батч одного коллектора по набору хостов. В detail — таблица
host × collector × status × duration × result с фильтрами по статусу.

### 8.6. Collectors (`collectors.html`) — **P2**

Каталог доступных коллекторов: имя, краткое описание, версия,
input-schema (раскрываемый JSON). Это справочник, не интерактив.

### 8.7. Audit (`audit.html`) — **P2**

Хронологическая лента событий: кто, что, когда. Фильтры по actor/action.

### 8.8. Settings + Login — **P2**

Settings — форма LLM endpoint / model / api key env-var name + статус
«ключ доступен да/нет», retention policy, auth toggle. Login — минимум:
центрированная карточка, лого, user/pass.

---

## 9. Ключевые компоненты (атомы/молекулы для дизайн-системы)

1. `StatusDot` — 8px круг, цвет от semantic, опционально пульсирует.
2. `Badge` — 3px radius, 11px mono, варианты: ok / warn / err / info /
   pending / neutral.
3. `Tag` (labels) — chip-строка, `key=value` в mono, клик = фильтр.
4. `JsonBlock` — `<pre>` с подсветкой, copy, fold, truncate preview.
5. `KeyValueGrid` — для метаданных вверху detail-страниц.
6. `ProgressBar` — тонкий (4px), с секциями для multi-budget
   (tokens: prompt | completion | headroom).
7. `TimelineItem` — карточка шага, expandable.
8. `PendingCard` — см. выше, уникальный компонент.
9. `DataTable` — плотная таблица с hover row, sticky header, sortable
   columns, pagination в footer.
10. `Toast` — ephemeral уведомления (SSE события), нижний правый угол.
11. `DiffView` — для `ConfigUpdate` и «approve with edits» (было/стало).
12. `CopyButton` — универсальная, с «copied!» toast.

## 10. Anti-patterns (не делать)

- Большие круговые диаграммы «80% коллекторов работает».
- Breadcrumbs глубже 2 уровней — у нас плоская навигация.
- Модальные окна для основного потока (approve нельзя модалкой — закроет
  контекст timeline).
- Иконки-только кнопки без tooltip для критичных действий.
- Emoji как affordance (📌 и 🚫 в findings — OK как маркеры состояния,
  но не в кнопках).
- Любые gradient-backgrounds, glassmorphism, `backdrop-filter: blur`.
- Скругления > 8px.
- Шрифт > 24px (кроме одного H1 на login-экране если нужен).
- Toast в centre-top блокирующий контент.

## 11. A11y и частные требования

- Все интерактивные элементы — клавиатурно доступны. `Tab`-порядок:
  sidebar → header → timeline → pending card → findings.
- `Approve` pending card должен триггериться `Enter` когда фокус на
  карточке; `Esc` закрывает expand.
- Все цвета статусов дублируются формой/иконкой (colorblind mode).
- Motion-respect: `@media (prefers-reduced-motion: reduce)` отключает
  pulse/breath/tween, оставляет только мгновенные переходы.

---

## 12. Что нужно сдать (deliverables)

1. **`DESIGN_SYSTEM.md`** в `DOCS/Design/` — финальные токены (палитра,
   spacing, type scale, motion table), список компонентов с описанием
   состояний и edge cases.
2. **`web/static/hub.css`** — CSS-файл с custom properties, компонентными
   классами, motion keyframes. Работает поверх Tailwind CDN.
3. **HTML-прототипы в `DOCS/Design/prototypes/`** (статические, без Go-
   шаблонных тегов) для экранов:
   - `layout.html` (sidebar + пример контента)
   - `investigations_list.html`
   - `investigation_detail.html` (**обязательно с pending card,
     timeline, findings panel**)
   - `hosts.html` + `host_detail.html`
   - `run_detail.html`
   - `login.html`
   Прототипы должны открываться двойным кликом в браузере и выглядеть
   финально (с seed-данными захардкоженными).
4. **Motion spec** — отдельный раздел в `DESIGN_SYSTEM.md` либо
   демо-страница `DOCS/Design/prototypes/motion.html`, где каждый тип
   анимации триггерится кнопкой и его можно посмотреть изолированно.
5. **SSE-integration notes** — как именно фрагменты страницы должны
   быть размечены (`hx-swap-oob`, Alpine stores) чтобы принимать
   точечные обновления без reload. Без кода Go-handler'а — только
   клиентская часть.

**Что НЕ надо сдавать:**
- Figma-файлы (у нас нет Figma — HTML-прототипы самодостаточны).
- React-компоненты.
- Иконочный набор с нуля — взять `lucide` inline SVG (CDN или копипаст
  в шаблон), оно совместимо с Tailwind.
- Мобильную версию.

---

## 13. Обязательное чтение перед работой

1. `CLAUDE.md` (корень) — стек, инварианты, стиль.
2. `README.md` — зачем вообще Recon.
3. `DOCS/Prompts/PROJECT.md` §3 (протокол), §4.3 (Web UI), §5 (SQLite),
   §7.4 (контекст LLM).
4. `DOCS/Prompts/BASE_TASKS.md` §2 (tool schemas), §3 (system prompt —
   понимать, ЧТО модель будет предлагать в pending card).
5. `internal/hub/web/templates/*.html` — текущий baseline (чтобы знать,
   от чего отталкиваемся; не копировать).
6. `internal/hub/web/server.go` — какие handlers рендерят какие
   шаблоны, какие данные приходят в view-model.

## 14. Критерии приёмки

- Investigation detail экран на 1920×1080 вмещает: pending card + 5
  последних шагов timeline + 3 findings + бюджеты + hypothesis form —
  без вертикального скролла.
- SRE впервые видит интерфейс и за 30 секунд понимает: (а) что есть
  pending решение от модели, (б) что он может его одобрить/отредактировать/
  отклонить, (в) где посмотреть evidence.
- Все цвета semantic однозначно читаются при дальтонизме (проверяется
  симулятором, напр. Sim Daltonism).
- Нет ни одного элемента, предполагающего write-действие на хосте.
  `Abort investigation` и `End` — единственные «негативные» действия,
  визуально спокойные.
- CSS работает поверх Tailwind CDN без сборочного pipeline: прототипы
  открываются из файловой системы и рендерятся корректно.
- `prefers-reduced-motion: reduce` отключает всю motion.

---

## 15. Открытые вопросы для дизайнера

Эти вопросы можно (и нужно) задавать в процессе, а не замораживать
заранее:

- Нужен ли mini-sparkline на host row (последние 24ч heartbeat) — или
  это overkill для MVP?
- Показывать ли «chat-like» представление messages LLM (с avatar-ами)
  или оставить табличный timeline? Голосую за табличный — это не чат,
  это лог.
- Где селектор «на каком хосте запустить investigation» — на dedicated
  странице создания или модалкой из hosts list?
- Как визуализировать Operator Hypothesis в timeline после inject? Это
  прерывание плана — должно быть **очень** заметно.
- Стоит ли иметь command palette (Cmd+K) для навигации между host/run/
  investigation по ID? Для power users — да, но это P2.

---

*Любые правки к этому брифу сохраняем в git вместе с design system —
дизайн эволюционирует вместе с продуктом.*
