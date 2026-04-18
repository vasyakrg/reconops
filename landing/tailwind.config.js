/** @type {import('tailwindcss').Config} */
export default {
  content: [
    './components/**/*.{vue,js,ts}',
    './layouts/**/*.vue',
    './pages/**/*.vue',
    './app.vue',
  ],
  theme: {
    extend: {
      colors: {
        bg: { 0: '#0a0b0d', 1: '#111215', 2: '#171a1f' },
        fg: { 0: '#e6e8eb', 1: '#a8adb5', 2: '#6b7079' },
        accent: {
          1: '#7aa2ff',
          2: '#4ec9a4',
          3: '#e5b567',
          4: '#c3a6e0',
          danger: '#e06c75',
        },
      },
      fontFamily: {
        display: ['Geist', 'Inter Display', 'system-ui', 'sans-serif'],
        sans: ['Inter', 'system-ui', 'sans-serif'],
        mono: ['JetBrains Mono', 'Geist Mono', 'ui-monospace', 'monospace'],
      },
      fontSize: {
        display: ['56px', { lineHeight: '1.1', letterSpacing: '-0.02em' }],
        h1: ['48px', { lineHeight: '1.15', letterSpacing: '-0.02em' }],
        h2: ['32px', { lineHeight: '1.2', letterSpacing: '-0.01em' }],
        h3: ['24px', { lineHeight: '1.25' }],
      },
      backgroundImage: {
        'grad-aurora':
          'linear-gradient(135deg, #7aa2ff 0%, #c3a6e0 50%, #4ec9a4 100%)',
        'grad-warm': 'linear-gradient(135deg, #e5b567 0%, #e06c75 100%)',
        'grad-cool': 'linear-gradient(135deg, #7aa2ff 0%, #4ec9a4 100%)',
      },
      maxWidth: { container: '1200px' },
    },
  },
  plugins: [],
}
