<script setup lang="ts">
const problems = [
  {
    title: 'kubectl exec на свой страх и риск',
    text: 'Подключение к pod, ручной cat, tail, journalctl. Каждая команда — потенциальный rm.',
    icon: 'terminal',
  },
  {
    title: 'Ansible playbook’и для диагностики',
    text: 'Долго писать, страшно запускать, версионируется отдельно от ситуации.',
    icon: 'playbook',
  },
  {
    title: 'sosreport и must-gather',
    text: 'Тонна данных, разбираешь сам ночью, половина не нужна.',
    icon: 'archive',
  },
]

const howSteps = [
  { n: 1, title: 'Вы вводите цель', text: 'Свободным текстом: «почему упал nginx на edge-03»' },
  { n: 2, title: 'LLM предлагает шаг', text: 'Один tool_use за раз с понятным rationale' },
  { n: 3, title: 'Вы аппрувите', text: 'Approve, edit JSON, skip или end — решение за вами' },
  { n: 4, title: 'Агент собирает данные', text: 'Read-only коллектор на целевом хосте' },
  { n: 5, title: 'LLM анализирует', text: 'Новый шаг или finding с доказательствами, цикл' },
]

const collectors = [
  'system_info', 'dns_resolve', 'net_listen', 'net_connect',
  'net_ifaces', 'systemd_units', 'journal_tail', 'process_list',
  'file_read', 'disk_usage', 'package_query',
]

const features = [
  { title: '11 read-only коллекторов', text: 'Сетка, DNS, systemd, journal, файлы, процессы, диски. Расширяется релизом агента.' },
  { title: 'Step-by-step approval', text: 'Approval card: approve, edit params, skip, end, inject hypothesis.' },
  { title: 'Findings с доказательствами', text: 'Каждый finding ссылается на task_id. Pin / ignore. Экспорт в Markdown.' },
  { title: 'Парк хостов', text: 'Enroll через bootstrap token, mTLS из коробки, labels, статусы.' },
  { title: 'Audit-лог', text: 'Каждое действие оператора и каждое решение модели — в журнале.' },
  { title: 'Любой OpenAI-совместимый LLM', text: 'OpenRouter по умолчанию. vLLM, LiteLLM, raw OpenAI — всё подойдёт.' },
]

const useCases = [
  { title: 'Cron на k8s падает по ночам', text: 'Натравите reconops на namespace и получите причину к утру.' },
  { title: 'Prometheus-агент отвалился на 3 хостах', text: 'Найти, что у них общего — быстрее, чем читать три journalctl руками.' },
  { title: 'DNS «иногда» не резолвится', text: 'Пройти стек resolv → systemd-resolved → network.' },
  { title: 'Onboarding нового SRE', text: 'Investigation превращается в пошаговый туториал «как мы дебажим».' },
  { title: 'Post-mortem после инцидента', text: 'Финальный экспорт в Markdown — готовый раздел отчёта.' },
]

const faq = [
  { q: 'Можно ли запретить ИИ некоторые коллекторы?', a: 'Да, через config whitelist на хабе.' },
  { q: 'Какие LLM поддерживаются?', a: 'Любой OpenAI-совместимый chat/completions с function calling. По умолчанию OpenRouter с claude-sonnet-4.5.' },
  { q: 'Что уходит во внешний LLM?', a: 'Только сжатые описания результатов (~500–2000 токенов). Сырые артефакты остаются в хабе.' },
  { q: 'Можно ли работать с air-gapped инфрой?', a: 'Да — поднимите локальный vLLM или LiteLLM как backend.' },
  { q: 'А Windows?', a: 'Нет. Только Linux.' },
  { q: 'Как обновить агента?', a: 'Через пакетный менеджер или замена бинарника + restart unit.' },
]
</script>

<template>
  <div id="top">
    <StructuredData :faq="faq" />

    <!-- HERO -->
    <section class="relative section overflow-hidden">
      <div
        class="pointer-events-none absolute inset-0 opacity-60"
        aria-hidden="true"
      >
        <div
          class="absolute left-1/2 top-0 h-[520px] w-[900px] -translate-x-1/2 rounded-full blur-3xl"
          style="background: radial-gradient(closest-side, rgba(122,162,255,0.18), transparent 70%);"
        />
      </div>
      <div
        class="container-x relative grid gap-12 md:grid-cols-2 md:items-center"
      >
        <div>
          <span class="chip mb-6">
            <span class="h-1.5 w-1.5 rounded-full bg-accent-2" />
            Open source · Self-hosted · OpenAI-совместимый LLM
          </span>
          <h1 class="font-display text-4xl md:text-display">
            ИИ-расследования инцидентов на Linux-парке —
            <span class="bg-grad-aurora bg-clip-text text-transparent">
              под вашим контролем
            </span>
          </h1>
          <p class="mt-5 max-w-xl text-lg text-fg-1">
            LLM предлагает следующий шаг — вы аппрувите. Каждый коллектор
            read-only по конструкции: инструмент физически не может выполнить
            команду, которой нет в системе.
          </p>
          <div class="mt-8 flex flex-wrap gap-3">
            <a href="#quickstart" class="btn-primary">Quickstart · 5 минут</a>
            <a href="#docs" class="btn-ghost">Документация</a>
          </div>
        </div>
        <HeroDemo />
      </div>
    </section>

    <!-- PROBLEM -->
    <section
      id="problem"
      class="section bg-bg-1"
      aria-labelledby="problem-title"
    >
      <div class="container-x">
        <div class="heading-eyebrow">Знакомая ситуация</div>
        <h2 id="problem-title" class="max-w-2xl font-display text-h2">
          Дебаг инцидента — это либо страшно, либо долго
        </h2>
        <div class="mt-10 grid gap-5 md:grid-cols-3">
          <div
            v-for="p in problems"
            :key="p.title"
            class="card card-interactive"
          >
            <div
              class="mb-4 inline-flex h-11 w-11 items-center justify-center rounded-lg bg-grad-warm/20"
              style="background: linear-gradient(135deg, rgba(229,181,103,0.18), rgba(224,108,117,0.18));"
            >
              <!-- Terminal + zachrk rm -->
              <svg
                v-if="p.icon === 'terminal'"
                viewBox="0 0 24 24"
                fill="none"
                class="h-6 w-6"
                aria-hidden="true"
              >
                <rect
                  x="2.5"
                  y="4"
                  width="19"
                  height="16"
                  rx="2.5"
                  stroke="#e5b567"
                  stroke-width="1.5"
                />
                <path
                  d="M6 9l3 3-3 3"
                  stroke="#e5b567"
                  stroke-width="1.5"
                  stroke-linecap="round"
                  stroke-linejoin="round"
                />
                <path
                  d="M11 15h6"
                  stroke="#e5b567"
                  stroke-width="1.5"
                  stroke-linecap="round"
                />
                <path
                  d="M4 18l16-12"
                  stroke="#e06c75"
                  stroke-width="1.5"
                  stroke-linecap="round"
                />
              </svg>

              <!-- Playbook + pesochnie chasy -->
              <svg
                v-else-if="p.icon === 'playbook'"
                viewBox="0 0 24 24"
                fill="none"
                class="h-6 w-6"
                aria-hidden="true"
              >
                <path
                  d="M5 4h10a2 2 0 012 2v14H7a2 2 0 01-2-2V4z"
                  stroke="#e5b567"
                  stroke-width="1.5"
                  stroke-linejoin="round"
                />
                <path
                  d="M5 4v14a2 2 0 002 2h10"
                  stroke="#e5b567"
                  stroke-width="1.5"
                  stroke-linejoin="round"
                />
                <circle cx="16" cy="15" r="4" fill="#0a0b0d" stroke="#e06c75" stroke-width="1.3" />
                <path
                  d="M14.5 13.5h3m-3 3h3M15 13.5l2 3m-2 0l2-3"
                  stroke="#e06c75"
                  stroke-width="1"
                  stroke-linecap="round"
                />
              </svg>

              <!-- Archive / sosreport -->
              <svg
                v-else
                viewBox="0 0 24 24"
                fill="none"
                class="h-6 w-6"
                aria-hidden="true"
              >
                <rect x="3" y="5" width="18" height="4" rx="1" stroke="#e5b567" stroke-width="1.5" />
                <path d="M5 9v10a1 1 0 001 1h12a1 1 0 001-1V9" stroke="#e5b567" stroke-width="1.5" stroke-linejoin="round" />
                <path d="M10 13h4" stroke="#e5b567" stroke-width="1.5" stroke-linecap="round" />
                <circle cx="17" cy="17" r="3" stroke="#e06c75" stroke-width="1.3" />
                <path d="M19.2 19.2L21 21" stroke="#e06c75" stroke-width="1.3" stroke-linecap="round" />
              </svg>
            </div>
            <h3 class="font-display text-lg">{{ p.title }}</h3>
            <p class="mt-2 text-sm text-fg-1">{{ p.text }}</p>
          </div>
        </div>
      </div>
    </section>

    <!-- SOLUTION -->
    <section class="section">
      <div class="container-x">
        <div
          class="relative overflow-hidden rounded-3xl border border-accent-1/20 bg-bg-1 p-8 md:p-12"
        >
          <div
            class="pointer-events-none absolute -right-20 -top-20 h-72 w-72 rounded-full bg-grad-aurora opacity-20 blur-3xl"
            aria-hidden="true"
          />
          <div class="heading-eyebrow">Решение</div>
          <h2 class="max-w-3xl font-display text-h2">
            reconops — расследование под вашим контролем
          </h2>
          <p class="mt-4 max-w-3xl text-lg text-fg-1">
            LLM (Claude или любая OpenAI-совместимая модель) ходит по парку
            через read-only коллекторы. Каждый шаг — карточка для оператора,
            которую он аппрувит. На выходе — структурированный post-mortem с
            findings и ссылками на реальные наблюдения.
            <span class="text-fg-0">
              Read-only гарантирован конструктивно, не «политикой».
            </span>
          </p>
        </div>
      </div>
    </section>

    <!-- HOW IT WORKS -->
    <section id="how" class="section bg-bg-1">
      <div class="container-x">
        <div class="heading-eyebrow">Как это работает</div>
        <h2 class="font-display text-h2">Один цикл расследования</h2>
        <div class="mt-10 grid gap-4 md:grid-cols-5">
          <div
            v-for="s in howSteps"
            :key="s.n"
            class="card card-interactive relative"
          >
            <div class="font-mono text-xs text-accent-1">шаг {{ s.n }}</div>
            <h3 class="mt-2 font-display text-lg">{{ s.title }}</h3>
            <p class="mt-2 text-sm text-fg-1">{{ s.text }}</p>
          </div>
        </div>
        <p class="mt-6 text-sm text-fg-2">
          Шаги 2–5 повторяются до <code class="font-mono">mark_done</code>.
          Оператор может в любой момент закончить расследование или подсказать
          гипотезу.
        </p>
      </div>
    </section>

    <!-- FEATURES -->
    <section id="features" class="section">
      <div class="container-x">
        <div class="heading-eyebrow">Возможности</div>
        <h2 class="font-display text-h2">Что внутри</h2>
        <div class="mt-10 grid gap-5 md:grid-cols-3">
          <div
            v-for="f in features"
            :key="f.title"
            class="card card-interactive"
          >
            <h3 class="font-display text-lg">{{ f.title }}</h3>
            <p class="mt-2 text-sm text-fg-1">{{ f.text }}</p>
          </div>
        </div>

        <div class="mt-10 rounded-2xl border border-white/10 bg-bg-1 p-6">
          <div class="mb-3 text-sm text-fg-2">
            Встроенные коллекторы (compiled-in, расширяются релизом):
          </div>
          <div class="flex flex-wrap gap-2">
            <span v-for="c in collectors" :key="c" class="chip font-mono">
              {{ c }}
            </span>
          </div>
        </div>
      </div>
    </section>

    <!-- SAFETY -->
    <section id="safety" class="relative section overflow-hidden">
      <div
        class="pointer-events-none absolute inset-0"
        aria-hidden="true"
        style="background: radial-gradient(ellipse at 50% 0%, rgba(78,201,164,0.10), transparent 60%);"
      />
      <div class="container-x relative">
        <div class="heading-eyebrow" style="color: #4ec9a4;">
          Почему это безопасно
        </div>
        <h2 class="max-w-3xl font-display text-h2">
          Read-only по конструкции, не по политике
        </h2>
        <p class="mt-4 max-w-2xl text-fg-1">
          Пять слоёв enforcement. Если один пропустит destructive команду —
          она упрётся в следующий.
        </p>

        <div class="mt-12">
          <SafetyLayers />
        </div>

        <p
          class="mt-12 border-l-2 border-accent-2 pl-4 font-display text-h3 text-fg-0"
        >
          ИИ не может выполнить команду, которой нет в системе.
        </p>
      </div>
    </section>

    <!-- TECH / QUICKSTART -->
    <section id="quickstart" class="section bg-bg-1">
      <div class="container-x grid gap-10 md:grid-cols-2">
        <div>
          <div class="heading-eyebrow">Развёртывание</div>
          <h2 class="font-display text-h2">
            Один бинарник на хост, один на хаб
          </h2>
          <ul class="mt-6 space-y-3 text-fg-1">
            <li class="flex gap-3">
              <span class="mt-2 h-1.5 w-1.5 shrink-0 rounded-full bg-accent-1" />
              Single Go binary (~28 MB, без CGO, без зависимостей).
            </li>
            <li class="flex gap-3">
              <span class="mt-2 h-1.5 w-1.5 shrink-0 rounded-full bg-accent-1" />
              SQLite хранилище в хабе. Никакого Postgres-кластера.
            </li>
            <li class="flex gap-3">
              <span class="mt-2 h-1.5 w-1.5 shrink-0 rounded-full bg-accent-1" />
              mTLS между хабом и агентом обязателен. Сертификаты выдаёт хаб.
            </li>
            <li class="flex gap-3">
              <span class="mt-2 h-1.5 w-1.5 shrink-0 rounded-full bg-accent-1" />
              Self-hosted: ваши данные не покидают периметра, только сжатые
              рассуждения уходят в LLM-провайдера.
            </li>
            <li class="flex gap-3">
              <span class="mt-2 h-1.5 w-1.5 shrink-0 rounded-full bg-accent-1" />
              Systemd-юниты, nginx config, install runbook — в комплекте.
            </li>
          </ul>
        </div>

        <div
          class="overflow-hidden rounded-2xl border border-white/10 bg-[#0d0e11] shadow-xl"
        >
          <div
            class="flex items-center gap-2 border-b border-white/5 px-4 py-2.5 font-mono text-xs text-fg-2"
          >
            <span class="h-2.5 w-2.5 rounded-full bg-[#e06c75]" />
            <span class="h-2.5 w-2.5 rounded-full bg-[#e5b567]" />
            <span class="h-2.5 w-2.5 rounded-full bg-[#4ec9a4]" />
            <span class="ml-2">bash · quickstart</span>
          </div>
          <pre class="overflow-x-auto p-5 font-mono text-[13px] leading-relaxed"><code><span class="text-fg-2"># 1. поднять хаб и фронт-nginx</span>
<span class="text-accent-2">$</span> <span class="text-accent-1">git</span> <span class="text-fg-0">clone</span> <span class="text-accent-4">https://github.com/reconops/reconops.git</span>
<span class="text-accent-2">$</span> <span class="text-fg-0">cd</span> <span class="text-accent-4">reconops</span> <span class="text-fg-2">&&</span> <span class="text-accent-1">docker</span> <span class="text-fg-0">compose up -d</span>

<span class="text-fg-2"># 2. выпустить bootstrap-токен для хоста</span>
<span class="text-accent-2">$</span> <span class="text-accent-1">docker</span> <span class="text-fg-0">compose exec hub recon-hub gen-token</span> \
    <span class="text-accent-3">--agent-id</span> <span class="text-accent-4">edge-03</span>

<span class="text-fg-2"># 3. установить агента на целевой хост</span>
<span class="text-accent-2">$</span> <span class="text-accent-1">curl</span> <span class="text-accent-3">-sSL</span> <span class="text-accent-4">https://reconops.ru/install.sh</span> <span class="text-fg-2">|</span> <span class="text-accent-1">sh</span>

<span class="text-fg-2"># 4. открыть UI</span>
<span class="text-accent-2">$</span> <span class="text-accent-1">open</span> <span class="text-accent-4">https://localhost/investigations/new</span></code></pre>
          <div
            class="flex items-center justify-between border-t border-white/5 px-4 py-2.5 text-xs text-fg-2"
          >
            <span class="font-mono">5 минут · от clone до первого findings</span>
            <a href="#docs" class="text-accent-1 hover:underline">
              Полный install runbook →
            </a>
          </div>
        </div>
      </div>
    </section>

    <!-- USE CASES -->
    <section class="section">
      <div class="container-x">
        <div class="heading-eyebrow">Сценарии</div>
        <h2 class="font-display text-h2">Когда это полезно</h2>
        <div class="mt-10 grid gap-5 md:grid-cols-2 lg:grid-cols-3">
          <div
            v-for="u in useCases"
            :key="u.title"
            class="card card-interactive"
          >
            <h3 class="font-display text-lg">{{ u.title }}</h3>
            <p class="mt-2 text-sm text-fg-1">{{ u.text }}</p>
          </div>
        </div>
      </div>
    </section>

    <!-- FAQ -->
    <section id="faq" class="section bg-bg-2">
      <div class="container-x">
        <div class="heading-eyebrow">FAQ</div>
        <h2 class="font-display text-h2">Частые вопросы</h2>
        <div class="mx-auto mt-10 max-w-2xl divide-y divide-white/10">
          <details v-for="item in faq" :key="item.q" class="group py-4">
            <summary
              class="flex cursor-pointer list-none items-center justify-between gap-4 text-fg-0"
            >
              <span class="font-display text-lg">{{ item.q }}</span>
              <span
                class="text-accent-1 transition group-open:rotate-45"
                aria-hidden="true"
                >+</span
              >
            </summary>
            <p class="mt-3 text-sm text-fg-1">{{ item.a }}</p>
          </details>
        </div>

        <p class="mx-auto mt-12 max-w-2xl text-center text-fg-1">
          Заинтересовало? Напишите:
          <a
            href="mailto:hello@reconops.ru"
            class="text-accent-1 hover:underline"
            >hello@reconops.ru</a
          >
        </p>
      </div>
    </section>
  </div>
</template>
