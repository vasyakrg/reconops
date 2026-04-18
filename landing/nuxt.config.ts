const SITE_URL = 'https://reconops.ru'
const SITE_PATH = '/site/'
const CANONICAL = SITE_URL + SITE_PATH
const OG_IMAGE = SITE_URL + SITE_PATH + 'og.png'

export default defineNuxtConfig({
  ssr: true,
  app: {
    baseURL: SITE_PATH,
    buildAssetsDir: '/_nuxt/',
    head: {
      htmlAttrs: { lang: 'ru' },
      title:
        'reconops — ИИ-расследования инцидентов на Linux-парке · read-only',
      meta: [
        { charset: 'utf-8' },
        { name: 'viewport', content: 'width=device-width, initial-scale=1' },
        {
          name: 'description',
          content:
            'reconops — self-hosted инструмент для ИИ-расследования инцидентов на Linux-серверах. LLM ходит по парку через read-only коллекторы, каждый шаг под контролем оператора. Open source, mTLS, OpenAI-совместимый бэкенд.',
        },
        {
          name: 'keywords',
          content:
            'ИИ расследование инцидентов, LLM observability, SRE инструмент, read-only диагностика Linux, self-hosted AI, AI для DevOps, incident response, post-mortem, OpenRouter, Claude для SRE',
        },
        { name: 'author', content: 'reconops' },
        { name: 'robots', content: 'index,follow,max-image-preview:large' },
        { name: 'theme-color', content: '#0a0b0d' },

        { property: 'og:type', content: 'website' },
        { property: 'og:site_name', content: 'reconops' },
        { property: 'og:locale', content: 'ru_RU' },
        { property: 'og:url', content: CANONICAL },
        {
          property: 'og:title',
          content: 'reconops — ИИ-расследования на Linux-парке',
        },
        {
          property: 'og:description',
          content:
            'Read-only ИИ-расследования инцидентов. Каждый шаг — под вашим контролем. Open source, self-hosted, mTLS.',
        },
        { property: 'og:image', content: OG_IMAGE },
        { property: 'og:image:width', content: '1200' },
        { property: 'og:image:height', content: '630' },
        { property: 'og:image:alt', content: 'reconops — read-only ИИ-расследования' },

        { name: 'twitter:card', content: 'summary_large_image' },
        {
          name: 'twitter:title',
          content: 'reconops — ИИ-расследования на Linux-парке',
        },
        {
          name: 'twitter:description',
          content:
            'Read-only ИИ-расследования инцидентов. Каждый шаг — под вашим контролем.',
        },
        { name: 'twitter:image', content: OG_IMAGE },
      ],
      link: [
        { rel: 'icon', type: 'image/svg+xml', href: SITE_PATH + 'favicon.svg' },
        { rel: 'canonical', href: CANONICAL },
        { rel: 'alternate', hreflang: 'ru', href: CANONICAL },
        { rel: 'alternate', hreflang: 'x-default', href: CANONICAL },
      ],
    },
  },
  modules: ['@nuxtjs/tailwindcss'],
  tailwindcss: { cssPath: '~/assets/css/tailwind.css' },
  nitro: { preset: 'static' },
  devtools: { enabled: false },
  compatibilityDate: '2025-01-01',
})
