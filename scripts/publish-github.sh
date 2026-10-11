#!/usr/bin/env bash
# publish-github.sh —— 把 main 当前状态以干净快照发布到 GitHub。
# 用法：GITHUB_TOKEN=xxx ./scripts/publish-github.sh [user/repo]
# 说明：GitHub 上的 main 是"快照式"单提交（本地完整 git 历史保留在开发机），
#       这样旧历史里的内部文档/大二进制永远不会被推上去，且每次增量传输最小。
set -euo pipefail
REPO="${1:-jiqing112/AP-Manager}"
: "${GITHUB_TOKEN:?请设置 GITHUB_TOKEN 环境变量（repo 权限）}"
cd "$(dirname "$0")/.."

HEAD_MSG=$(git log -1 --pretty=%s main)
git checkout -q --orphan gh-publish
git commit -q -m "AP Manager 快照 — ${HEAD_MSG}"
trap 'git checkout -q main; git branch -D gh-publish >/dev/null 2>&1 || true' EXIT
git -c credential.helper= push -f \
  "https://jiqing112:${GITHUB_TOKEN}@github.com/${REPO}.git" gh-publish:main
echo "[publish] 已发布快照到 https://github.com/${REPO}"
