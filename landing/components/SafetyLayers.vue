<script setup lang="ts">
const layers = [
  {
    name: 'Protocol',
    title: 'Протокол',
    desc:
      'HubMsg содержит только CollectRequest / CancelRequest / ConfigUpdate. В протоколе физически нет verb, меняющего состояние хоста.',
  },
  {
    name: 'Catalog',
    title: 'Каталог коллекторов',
    desc:
      'Compiled-in список read-only функций. Никаких runtime-плагинов, никакого eval. Новый коллектор — новый релиз агента.',
  },
  {
    name: 'Gateway',
    title: 'Exec gateway',
    desc:
      'Если коллектор запускает команду, она проходит через whitelist разрешённых бинарей с валидированными аргументами.',
  },
  {
    name: 'OS',
    title: 'OS-уровень',
    desc:
      'Systemd: ProtectSystem=strict, MemoryDenyWriteExecute, минимальный CapabilityBoundingSet. Процесс агента не имеет прав на запись.',
  },
  {
    name: 'CI',
    title: 'CI lint',
    desc:
      'В коде collectors/ запрещены destructive syscalls и импорты — проверяется в CI на каждом PR.',
  },
]

const active = ref(0)
</script>

<template>
  <div class="grid gap-8 md:grid-cols-[1.1fr_1fr] md:items-center">
    <div class="relative aspect-square w-full max-w-md">
      <div
        v-for="(l, i) in layers"
        :key="l.name"
        class="absolute inset-0 flex items-center justify-center rounded-full border transition"
        :style="{
          inset: `${i * 9}%`,
          borderColor: active === i ? '#4ec9a4' : 'rgba(255,255,255,0.10)',
          background:
            active === i
              ? 'rgba(78, 201, 164, 0.07)'
              : 'rgba(255,255,255,0.015)',
        }"
        @mouseenter="active = i"
        @click="active = i"
      >
        <span
          class="absolute top-2 left-1/2 -translate-x-1/2 font-mono text-[11px] tracking-wider text-fg-2"
        >
          {{ l.name }}
        </span>
      </div>
      <div
        class="absolute left-1/2 top-1/2 -translate-x-1/2 -translate-y-1/2 text-xs font-mono text-accent-2"
      >
        ✓ host
      </div>
      <div
        class="pointer-events-none absolute -left-6 top-6 font-mono text-xs text-accent-danger"
      >
        rm -rf / →
      </div>
    </div>

    <div class="space-y-3">
      <button
        v-for="(l, i) in layers"
        :key="l.name"
        class="block w-full rounded-xl border border-white/10 bg-bg-1 p-4 text-left transition hover:border-accent-2/50"
        :class="{ 'border-accent-2/60 bg-accent-2/5': active === i }"
        @click="active = i"
        @mouseenter="active = i"
      >
        <div class="flex items-center gap-3">
          <span
            class="inline-flex h-6 w-6 items-center justify-center rounded-md bg-white/5 font-mono text-xs"
            >{{ i + 1 }}</span
          >
          <span class="font-display text-lg">{{ l.title }}</span>
        </div>
        <p class="mt-2 text-sm text-fg-1">{{ l.desc }}</p>
      </button>
    </div>
  </div>
</template>
