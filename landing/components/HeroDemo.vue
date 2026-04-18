<script setup lang="ts">
import { nextTick, onBeforeUnmount, onMounted, ref } from 'vue'

type Entry =
  | { ts: string; kind: 'goal'; text: string }
  | { ts: string; kind: 'plan'; tool: string; rationale: string }
  | { ts: string; kind: 'approve'; tool: string }
  | { ts: string; kind: 'result'; tool: string; text: string }
  | { ts: string; kind: 'finding'; text: string }
  | { ts: string; kind: 'sys'; text: string }

const entries = ref<Entry[]>([])
const logRef = ref<HTMLElement | null>(null)
let timer: ReturnType<typeof setTimeout> | null = null
let cancelled = false

function ts(seconds: number): string {
  const m = String(Math.floor(seconds / 60)).padStart(2, '0')
  const s = String(seconds % 60).padStart(2, '0')
  return `00:${m}:${s}`
}

const script: Array<{ delay: number; entry: Entry }> = [
  { delay: 400, entry: { ts: ts(0), kind: 'sys', text: 'investigation #42 · goal set' } },
  { delay: 600, entry: { ts: ts(1), kind: 'goal', text: 'расследовать почему упал nginx на edge-03' } },
  { delay: 1400, entry: { ts: ts(3), kind: 'plan', tool: 'systemd_units', rationale: 'проверить статус nginx.service' } },
  { delay: 1200, entry: { ts: ts(4), kind: 'approve', tool: 'systemd_units' } },
  { delay: 1000, entry: { ts: ts(5), kind: 'result', tool: 'systemd_units', text: 'nginx.service — inactive (failed) since 03:12' } },
  { delay: 1400, entry: { ts: ts(7), kind: 'plan', tool: 'journal_tail', rationale: 'смотрю последние 200 строк журнала' } },
  { delay: 1200, entry: { ts: ts(8), kind: 'approve', tool: 'journal_tail' } },
  { delay: 1000, entry: { ts: ts(10), kind: 'result', tool: 'journal_tail', text: 'Out of memory: killed process 2318 (nginx)' } },
  { delay: 1400, entry: { ts: ts(12), kind: 'finding', text: 'nginx OOM-killed — memory pressure от worker #3' } },
]

function scrollToEnd() {
  nextTick(() => {
    const el = logRef.value
    if (el) el.scrollTop = el.scrollHeight
  })
}

function run(i = 0) {
  if (cancelled) return
  if (i >= script.length) {
    timer = setTimeout(() => {
      entries.value = []
      run(0)
    }, 3500)
    return
  }
  timer = setTimeout(() => {
    entries.value = [...entries.value, script[i].entry]
    scrollToEnd()
    run(i + 1)
  }, script[i].delay)
}

onMounted(() => {
  const reduce =
    typeof window !== 'undefined' &&
    window.matchMedia('(prefers-reduced-motion: reduce)').matches
  if (reduce) {
    entries.value = script.map((s) => s.entry)
    return
  }
  run(0)
})

onBeforeUnmount(() => {
  cancelled = true
  if (timer) clearTimeout(timer)
})
</script>

<template>
  <div
    class="relative flex h-[520px] w-full flex-col overflow-hidden rounded-2xl border border-white/10 bg-bg-1 shadow-2xl"
  >
    <!-- Window chrome -->
    <div
      class="flex shrink-0 items-center gap-2 border-b border-white/5 px-4 py-2.5 text-xs"
    >
      <span class="h-2.5 w-2.5 rounded-full bg-[#e06c75]" />
      <span class="h-2.5 w-2.5 rounded-full bg-[#e5b567]" />
      <span class="h-2.5 w-2.5 rounded-full bg-[#4ec9a4]" />
      <span class="ml-2 font-mono text-fg-2">
        reconops · investigation #42
      </span>
      <span class="ml-auto flex items-center gap-1.5 font-mono text-accent-2">
        <span class="relative flex h-2 w-2">
          <span
            class="absolute inline-flex h-full w-full animate-ping rounded-full bg-accent-2 opacity-60"
          />
          <span class="relative inline-flex h-2 w-2 rounded-full bg-accent-2" />
        </span>
        live
      </span>
    </div>

    <!-- Log body -->
    <div
      ref="logRef"
      class="flex-1 overflow-y-auto px-4 py-3 font-mono text-[12.5px] leading-relaxed"
    >
      <div v-for="(e, i) in entries" :key="i" class="fade-in mb-1.5">
        <template v-if="e.kind === 'sys'">
          <span class="text-fg-2">[{{ e.ts }}]</span>
          <span class="ml-2 text-fg-2">▸ {{ e.text }}</span>
        </template>

        <template v-else-if="e.kind === 'goal'">
          <span class="text-fg-2">[{{ e.ts }}]</span>
          <span class="ml-2 text-accent-1">goal</span>
          <span class="ml-2 text-fg-0">{{ e.text }}</span>
        </template>

        <template v-else-if="e.kind === 'plan'">
          <div class="rounded-md border border-accent-3/30 bg-accent-3/5 px-3 py-2">
            <div class="flex items-center gap-2">
              <span class="text-fg-2">[{{ e.ts }}]</span>
              <span
                class="rounded bg-accent-3/15 px-1.5 py-0.5 text-[10px] uppercase tracking-wider text-accent-3"
                >pending</span
              >
              <span class="text-accent-3">{{ e.tool }}</span>
              <button
                type="button"
                class="ml-auto rounded bg-accent-1 px-2 py-0.5 text-[11px] font-medium text-bg-0"
              >
                Approve
              </button>
            </div>
            <div class="mt-1.5 text-fg-1">↳ {{ e.rationale }}</div>
          </div>
        </template>

        <template v-else-if="e.kind === 'approve'">
          <span class="text-fg-2">[{{ e.ts }}]</span>
          <span class="ml-2 text-accent-2">✓ approved</span>
          <span class="ml-2 text-fg-2">{{ e.tool }}</span>
        </template>

        <template v-else-if="e.kind === 'result'">
          <span class="text-fg-2">[{{ e.ts }}]</span>
          <span class="ml-2 text-accent-4">result</span>
          <span class="ml-2 text-fg-2">{{ e.tool }}:</span>
          <div class="ml-6 mt-0.5 text-fg-0">{{ e.text }}</div>
        </template>

        <template v-else-if="e.kind === 'finding'">
          <div class="mt-1 rounded-md border border-accent-2/40 bg-accent-2/5 px-3 py-2">
            <div class="flex items-center gap-2">
              <span class="text-fg-2">[{{ e.ts }}]</span>
              <span
                class="rounded bg-accent-2/15 px-1.5 py-0.5 text-[10px] uppercase tracking-wider text-accent-2"
                >finding</span
              >
            </div>
            <div class="mt-1 text-fg-0">📌 {{ e.text }}</div>
          </div>
        </template>
      </div>
    </div>

    <!-- Prompt -->
    <div
      class="flex shrink-0 items-center gap-2 border-t border-white/5 bg-bg-2 px-4 py-2 font-mono text-xs text-fg-2"
    >
      <span class="text-accent-2">$</span>
      <span>reconops observe --investigation 42</span>
      <span class="ml-1 h-3.5 w-1.5 animate-pulse bg-fg-1" />
    </div>
  </div>
</template>

<style scoped>
.fade-in {
  animation: fadeIn 280ms ease both;
}
@keyframes fadeIn {
  from {
    opacity: 0;
    transform: translateY(4px);
  }
  to {
    opacity: 1;
    transform: translateY(0);
  }
}
@media (prefers-reduced-motion: reduce) {
  .fade-in {
    animation: none;
  }
}
</style>
