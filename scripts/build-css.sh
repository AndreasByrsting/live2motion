#!/usr/bin/env bash
# 重新编译 Tailwind CSS 到 internal/web/assets/app.css
# 仓库里已经提交了编译产物，只有改动模板 class 或 web/input.css 时才需要重跑。
set -euo pipefail

cd "$(dirname "$0")/.."

TAILWIND_VERSION="${TAILWIND_VERSION:-3.4.17}"

echo "→ 使用 tailwindcss@${TAILWIND_VERSION} 编译…"
npx --yes "tailwindcss@${TAILWIND_VERSION}" \
  -c tailwind.config.js \
  -i web/input.css \
  -o internal/web/assets/app.css \
  --minify

echo "✓ 已生成 internal/web/assets/app.css ($(wc -c < internal/web/assets/app.css) 字节)"
