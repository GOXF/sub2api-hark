#!/usr/bin/env bash
# 把 hark2api 的 gateway 包拷进 backend/internal/pkg/harkembed/gateway。
# 用法：tools/sync-hark2api.sh <hark2api 仓库目录>
set -euo pipefail
src="${1:?hark2api 仓库目录}"
dst="$(cd "$(dirname "$0")/.." && pwd)/backend/internal/pkg/harkembed/gateway"
overlay="$(cd "$(dirname "$0")" && pwd)/harkembed-transport.patch"
test -f "$src/gateway/embed.go" || { echo "缺少 $src/gateway/embed.go（hark2api 需带嵌入模式）" >&2; exit 1; }
stage="$(mktemp -d)"
trap 'rm -rf -- "$stage"' EXIT
git -c core.autocrlf=false -C "$src" archive HEAD:gateway | tar -x -C "$stage"
rm -f "$stage"/*_test.go
# Keep the sub2api-owned transport without silently overwriting upstream edits.
git -c core.autocrlf=false -C "$stage" apply --unidiff-zero --check "$overlay"
git -c core.autocrlf=false -C "$stage" apply --unidiff-zero "$overlay"
rm -rf "$dst"
mkdir -p "$dst"
cp -R "$stage/." "$dst/"
git -C "$src" rev-parse HEAD > "$dst/UPSTREAM_COMMIT"
echo "已同步 hark2api $(cat "$dst/UPSTREAM_COMMIT")"
