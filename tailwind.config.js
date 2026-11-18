/**
 * Tailwind 构建配置
 *
 * 颜色不写死，全部映射到 CSS 变量（定义见 web/input.css）：
 * 同一套类名在日间/夜间主题下取到不同值，模板无需感知主题，
 * 玻璃拟态的透明度叠加也能正常工作（rgb(var(--x) / <alpha-value>)）。
 */
const c = (name) => `rgb(var(--c-${name}) / <alpha-value>)`;

module.exports = {
  content: [
    './internal/web/templates/**/*.html',
    './internal/web/*.go',
    './internal/web/assets/app.js',
  ],
  theme: {
    extend: {
      colors: {
        slate: {
          200: c('slate-200'),
          300: c('slate-300'),
          400: c('slate-400'),
          500: c('slate-500'),
          600: c('slate-600'),
          700: c('slate-700'),
          800: c('slate-800'),
          900: c('slate-900'),
          950: c('slate-950'),
        },
        emerald: {
          300: c('emerald-300'),
          400: c('emerald-400'),
          500: c('emerald-500'),
        },
        rose: {
          300: c('rose-300'),
          400: c('rose-400'),
          500: c('rose-500'),
        },
        amber: {
          300: c('amber-300'),
          400: c('amber-400'),
          500: c('amber-500'),
        },
        sky: {
          300: c('sky-300'),
          400: c('sky-400'),
          500: c('sky-500'),
        },
      },
      fontFamily: {
        sans: [
          'Inter',
          '-apple-system',
          'BlinkMacSystemFont',
          'Segoe UI',
          'PingFang SC',
          'Hiragino Sans GB',
          'Microsoft YaHei',
          'sans-serif',
        ],
        mono: ['ui-monospace', 'SFMono-Regular', 'Menlo', 'Consolas', 'monospace'],
      },
    },
  },
  plugins: [],
};
