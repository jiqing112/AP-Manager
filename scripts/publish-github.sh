#!/usr/bin/env bash
# publish-github.sh —— 把 main 当前状态以干净快照发布到 GitHub。
# 用法：GITHUB_TOKEN=xxx ./scripts/publish-github.sh [user/repo]
# 双通道：优先 git push；git 智能协议（github.com:443）链路不稳时自动回退
#        GitHub REST API（api.github.com，Git Data API 增量上传，实测可靠）。
set -euo pipefail
REPO="${1:-jiqing112/AP-Manager}"
: "${GITHUB_TOKEN:?请设置 GITHUB_TOKEN 环境变量（repo 权限）}"
cd "$(dirname "$0")/.."

HEAD_MSG=$(git log -1 --pretty=%s main)
git checkout -q --orphan gh-publish
git commit -q -m "AP Manager 快照 — ${HEAD_MSG}"
trap 'git checkout -q main; git branch -D gh-publish >/dev/null 2>&1 || true' EXIT

if git -c credential.helper= push -f \
     "https://jiqing112:${GITHUB_TOKEN}@github.com/${REPO}.git" gh-publish:main 2>/dev/null; then
  echo "[publish] git 通道发布成功"
  exit 0
fi
echo "[publish] git 通道失败，回退 REST API 通道…"
git checkout -q main
git branch -D gh-publish >/dev/null 2>&1 || true

python3 - "$REPO" "$GITHUB_TOKEN" "$HEAD_MSG" <<'PYEOF'
import base64, json, subprocess, sys, urllib.request
repo, token, head_msg = sys.argv[1], sys.argv[2], sys.argv[3]
def api(method, path, body=None):
    req = urllib.request.Request(f"https://api.github.com/repos/{repo}/{path}",
        data=json.dumps(body).encode() if body is not None else None, method=method,
        headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json",
                 "User-Agent": "apmanager-publish"})
    with urllib.request.urlopen(req, timeout=60) as r:
        return json.loads(r.read() or b"{}")
full = api("GET", "commits/main")
base_sha, base_tree = full["sha"], full["commit"]["tree"]["sha"]
files = subprocess.check_output(["git", "ls-files"], text=True).splitlines()
items = []
for f in files:
    data = open(f, "rb").read()
    blob = api("POST", "git/blobs", {"content": base64.b64encode(data).decode(), "encoding": "base64"})
    mode = "100755" if (data[:2] == b"#!" or f.startswith("scripts/")) else "100644"
    items.append({"path": f, "mode": mode, "type": "blob", "sha": blob["sha"]})
tree = api("POST", "git/trees", {"base_tree": base_tree, "tree": items})
c = api("POST", "git/commits", {"message": "AP Manager 快照 — " + head_msg, "tree": tree["sha"], "parents": [base_sha]})
api("PATCH", "git/refs/heads/main", {"sha": c["sha"]})
print(f"[publish] API 通道发布成功: {c['sha'][:8]}")
PYEOF
