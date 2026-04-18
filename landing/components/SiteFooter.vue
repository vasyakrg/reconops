<template>
  <footer class="border-t border-white/5 bg-bg-0">
    <div class="container-x py-14">
      <div class="grid gap-10 md:grid-cols-4">
        <div class="md:col-span-1">
          <Logo size="lg" />
          <p class="mt-4 text-sm text-fg-1">
            Read-only ИИ-расследования инцидентов на Linux-парке. Каждый шаг —
            под контролем оператора.
          </p>
        </div>

        <div>
          <h3 class="text-xs font-medium uppercase tracking-[0.18em] text-fg-2">
            Продукт
          </h3>
          <ul class="mt-4 space-y-2.5 text-sm">
            <li><a href="#quickstart" class="text-fg-1 hover:text-fg-0">Quickstart</a></li>
            <li><a href="#features" class="text-fg-1 hover:text-fg-0">Возможности</a></li>
            <li><a href="#safety" class="text-fg-1 hover:text-fg-0">Безопасность</a></li>
            <li><a href="#how" class="text-fg-1 hover:text-fg-0">Как работает</a></li>
          </ul>
        </div>

        <div>
          <h3 class="text-xs font-medium uppercase tracking-[0.18em] text-fg-2">
            Сообщество
          </h3>
          <ul class="mt-4 space-y-2.5 text-sm">
            <li><a href="#" class="text-fg-1 hover:text-fg-0">GitHub</a></li>
            <li><a href="#" class="text-fg-1 hover:text-fg-0">Документация</a></li>
            <li><a href="#" class="text-fg-1 hover:text-fg-0">Changelog</a></li>
            <li><a href="#" class="text-fg-1 hover:text-fg-0">Архитектура</a></li>
          </ul>
        </div>

        <div>
          <h3 class="text-xs font-medium uppercase tracking-[0.18em] text-fg-2">
            Контакт
          </h3>
          <ul class="mt-4 space-y-2.5 text-sm">
            <li>
              <a
                href="mailto:hello@reconops.ru"
                class="text-accent-1 hover:underline"
                >hello@reconops.ru</a
              >
            </li>
            <li class="text-fg-2">Пишите — ответим.</li>
          </ul>
        </div>
      </div>

      <div
        class="mt-12 flex flex-col gap-2 border-t border-white/5 pt-6 text-xs text-fg-2 md:flex-row md:items-center md:justify-between"
      >
        <span>© 2026 reconops.ru</span>
        <span class="font-mono">Open source · Self-hosted · mTLS</span>
      </div>
    </div>
  </footer>
</template>
