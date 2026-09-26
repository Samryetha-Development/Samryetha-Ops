#!/usr/bin/env python3
"""一致性验证：既有 update.sh 与新内核读到的同一组事实。

用 Python 统一发请求，避免 shell heredoc/引号嵌套带来的噪音。
"""
import json
import subprocess
import urllib.request

SUB = "a14dc53e-c7f3-48f0-94d5-4f3cf058ae78"
KERNEL = "http://localhost:3040/api/kernel/call"


def kc(name, args):
    body = json.dumps({"name": name, "args": args, "plugin": "probe"}).encode()
    req = urllib.request.Request(
        KERNEL, data=body,
        headers={"Content-Type": "application/json", "X-Kernel-Subject": SUB})
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return json.load(r)
    except Exception as e:
        return {"ok": False, "error": {"code": "exception", "message": str(e)}}


def kernel_run(argv):
    """经内核跑一条命令并返回其输出。"""
    r = kc("proc.spawn", {"argv": argv, "capture": True})
    if not r.get("ok"):
        return f"<{r.get('error', {}).get('message', 'spawn failed')}>"
    pid = r["data"]["pid"]
    kc("proc.wait", {"pid": pid, "timeout_ms": 15000})
    out = kc("proc.output", {"pid": pid})
    if not out.get("ok"):
        return "<no output>"
    return out["data"]["output"].strip()


def local(argv):
    try:
        return subprocess.run(argv, capture_output=True, text=True, timeout=20).stdout.strip()
    except Exception as e:
        return f"<{e}>"


def curl_code(url):
    return local(["curl", "-sS", "-o", "/dev/null", "-w", "%{http_code}", "--max-time", "5", url])


rows = []

# 1) 主站 HEAD
rows.append(("主站 HEAD", local(["git", "-C", "/opt/Samryetha", "rev-parse", "--short", "HEAD"]),
             kernel_run(["git", "-C", "/opt/Samryetha", "rev-parse", "--short", "HEAD"])))

# 2) dev HEAD
rows.append(("dev HEAD", local(["git", "-C", "/opt/Samryetha-dev", "rev-parse", "--short", "HEAD"]),
             kernel_run(["git", "-C", "/opt/Samryetha-dev", "rev-parse", "--short", "HEAD"])))

# 3) 后端健康
rows.append(("后端 /api/health", curl_code("http://127.0.0.1:3001/api/health"),
             kernel_run(["curl", "-fsS", "-o", "/dev/null", "-w", "%{http_code}",
                         "--max-time", "5", "http://127.0.0.1:3001/api/health"])))

# 4) 前端可达
rows.append(("前端 /", curl_code("http://127.0.0.1:3000/"),
             kernel_run(["curl", "-fsS", "-o", "/dev/null", "-w", "%{http_code}",
                         "--max-time", "5", "http://127.0.0.1:3000/"])))

# 5) 已部署标记
try:
    marker = open("/opt/Samryetha/logs/.last-deployed").read().strip()[:7]
except Exception:
    marker = "?"
rows.append(("已部署标记", marker, kernel_run(["head", "-c", "7", "/opt/Samryetha/logs/.last-deployed"])))

print("======== 一致性验证：既有 update.sh  vs  新内核 ========")
print(f"{'检查项':<20} {'既有':<12} {'内核':<12} 一致")
print("-" * 56)
ok = True
for name, a, b in rows:
    same = a == b
    if not same:
        ok = False
    print(f"{name:<20} {a:<12} {b:<12} {'✓' if same else '✗'}")

print()
print("======== 内核事件流（自动记录） ========")
ev = kc("event.history", {"limit": 20})
if ev.get("ok"):
    for e in ev["data"]["items"]:
        print(f"  {e['topic']}  [{e['source']}]")

print()
print("======== 内核进程原语自检 ========")
pl = kc("proc.list", {})
if pl.get("ok"):
    print(f"  内核已托管进程数: {len(pl['data']['procs'])}")

print()
print("结果:", "全部一致 ✓" if ok else "存在差异 ✗")
