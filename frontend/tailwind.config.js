import { heroui } from '@heroui/theme'

/** @type {import('tailwindcss').Config} */
export default {
  content: ['./index.html', './src/**/*.{js,ts,jsx,tsx}', './node_modules/@heroui/theme/dist/**/*.{js,ts,jsx,tsx}'],
  darkMode: 'class',
  theme: { extend: { colors: { ocean: { 950: '#07131d', 900: '#0b1c28', 800: '#102635', 700: '#193747', 500: '#2eb5a1', 400: '#57d2bd' } }, boxShadow: { glow: '0 0 32px rgba(46,181,161,.12)' } } },
  plugins: [heroui()],
}
