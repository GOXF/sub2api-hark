#!/usr/bin/env bash
# 把 hark2api 的 gateway 包拷进 backend/internal/pkg/harkembed/gateway。
# 用法：tools/sync-hark2api.sh <hark2api 仓库目录>
set -euo pipefail
src="${1:?hark2api 仓库目录}"
dst="$(cd "$(dirname "$0")/.." && pwd)/backend/internal/pkg/harkembed/gateway"
test -f "$src/gateway/embed.go" || { echo "缺少 $src/gateway/embed.go（hark2api 需带嵌入模式）" >&2; exit 1; }
rm -rf "$dst"
mkdir -p "$dst"
cp -R "$src/gateway/." "$dst/"
rm -f "$dst"/*_test.go
git -C "$src" rev-parse HEAD > "$dst/UPSTREAM_COMMIT"
echo "已同步 hark2api $(cat "$dst/UPSTREAM_COMMIT")"
