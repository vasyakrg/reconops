<script setup lang="ts">
defineProps<{ size?: 'sm' | 'md' | 'lg' }>()
</script>

<template>
  <span class="inline-flex items-center gap-2">
    <svg
      viewBox="0 0 64 64"
      fill="none"
      aria-hidden="true"
      :class="[
        size === 'lg' ? 'h-8 w-8' : size === 'sm' ? 'h-5 w-5' : 'h-6 w-6',
      ]"
    >
      <rect width="64" height="64" rx="14" fill="#111215" />
      <circle
        cx="26"
        cy="26"
        r="13"
        stroke="#7aa2ff"
        stroke-width="3.5"
      />
      <line
        x1="26"
        y1="14"
        x2="26"
        y2="38"
        stroke="#7aa2ff"
        stroke-width="2"
        stroke-linecap="round"
      />
      <line
        x1="14"
        y1="26"
        x2="38"
        y2="26"
        stroke="#7aa2ff"
        stroke-width="2"
        stroke-linecap="round"
      />
      <path
        d="M36 36l14 14"
        stroke="#c3a6e0"
        stroke-width="4"
        stroke-linecap="round"
        opacity="0.6"
      />
      <circle cx="50" cy="50" r="6" fill="#4ec9a4" />
    </svg>
    <span
      class="font-display font-semibold tracking-tight"
      :class="[
        size === 'lg' ? 'text-2xl' : size === 'sm' ? 'text-sm' : 'text-lg',
      ]"
    >
      <span class="text-fg-0">recon</span><span class="text-fg-2">·</span><span class="text-accent-2 font-medium">ops</span>
    </span>
  </span>
</template>
