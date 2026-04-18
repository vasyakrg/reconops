<script setup lang="ts">
interface FaqItem {
  q: string
  a: string
}
const props = defineProps<{ faq: FaqItem[] }>()

const json = computed(() => {
  const graph = [
    {
      '@type': 'Organization',
      '@id': 'https://reconops.ru/#org',
      name: 'reconops',
      url: 'https://reconops.ru/',
      logo: 'https://reconops.ru/site/favicon.svg',
      email: 'hello@reconops.ru',
    },
    {
      '@type': 'WebSite',
      '@id': 'https://reconops.ru/#site',
      url: 'https://reconops.ru/site/',
      name: 'reconops',
      inLanguage: 'ru-RU',
      publisher: { '@id': 'https://reconops.ru/#org' },
    },
    {
      '@type': 'SoftwareApplication',
      name: 'reconops',
      applicationCategory: 'DeveloperApplication',
      operatingSystem: 'Linux',
      description:
        'Self-hosted инструмент для ИИ-расследования инцидентов на Linux-парке. Read-only коллекторы, LLM планирует шаги, оператор аппрувит каждый.',
      url: 'https://reconops.ru/site/',
      offers: { '@type': 'Offer', price: '0', priceCurrency: 'USD' },
      featureList: [
        '11 read-only коллекторов',
        'Step-by-step approval flow',
        'mTLS между хабом и агентом',
        'OpenAI-совместимый LLM backend (OpenRouter / vLLM / LiteLLM)',
        'Audit log всех действий',
        'Экспорт post-mortem в Markdown',
      ],
    },
    {
      '@type': 'FAQPage',
      mainEntity: props.faq.map((f) => ({
        '@type': 'Question',
        name: f.q,
        acceptedAnswer: { '@type': 'Answer', text: f.a },
      })),
    },
  ]
  return JSON.stringify({ '@context': 'https://schema.org', '@graph': graph })
})

useHead({
  script: [{ type: 'application/ld+json', innerHTML: json.value }],
})
</script>

<template><span /></template>
