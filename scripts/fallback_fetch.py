#!/usr/bin/env python3
# fallback_fetch.py —— 从 GitHub Release 下载预编译二进制并做 sha256 校验 + 原子替换。
# fallback_fetch.py — download a prebuilt binary from a GitHub Release, verify sha256, atomically replace.
#
# 用途 / Purpose：
#   生产服务器自更新时下载 update-service 二进制，替代旧脚本里的「curl 下载 + go build 回退」。
#   生产不再现场编译，因此完全不依赖 Go 工具链，也就不会用上未修补的 Go（CVE-2026-111943 影响范围）。
#   Production self-update downloads the update-service binary, replacing the old "curl + go build fallback".
#   No on-server compilation, so no Go toolchain is used and no unpatched Go can be picked up.
#
# 为什么不用 go build 回退 / Why drop the go build fallback：
#   现场编译会调用本机 Go；go.mod 的 toolchain 行只是"建议"，GOTOOLCHAIN 默认 auto 时
#   本机有更新版（如 1.27.2，仍在 CVE-2026-111943 范围）会照用，约束不住。所以回退只做
#   "下载 + 校验 + 原子替换"，失败直接报错退出，绝不退回 go build。
#   On-server compilation calls the local Go; the go.mod toolchain line is only a suggestion, and with
#   GOTOOLCHAIN=auto a newer local Go (e.g. 1.27.2, still in CVE-2026-111943) is used, so it cannot constrain.
#   The fallback therefore only does "download + verify + atomic replace", and errors out on failure — never go build.
#
# 职责边界 / Scope：
#   本脚本只负责"取到一个校验通过的二进制到指定路径"；停服务、替换线上二进制、重启、健康检查、
#   失败回滚仍由调用方（ops_self_update.sh）的 systemctl 流程负责，避免重复实现那段逻辑。
#   This script only fetches a verified binary to the target path; stop/replace/restart/health-check/rollback
#   remain in the caller (ops_self_update.sh) systemctl flow, to avoid duplicating that logic.
#
# 用法 / Usage：
#   python3 fallback_fetch.py <binary_url> <sha256_url> <output_path>
#   退出码 / Exit code：0=成功，非 0=失败（失败不回退到 go build，只报错）。
#   0=success, non-zero=failure (no go build fallback, only error out).
#
# 依赖 / Dependencies：仅 Python 标准库（urllib.request / hashlib / os），无第三方包。
#   Python stdlib only (urllib.request / hashlib / os), no third-party packages.

import hashlib
import os
import sys
import urllib.error
import urllib.request

# 超时（秒）/ timeouts (seconds)：二进制大、给足余量；sha 清单很小、短超时即可。
# Binary is large — allow a generous timeout; the sha manifest is tiny — a short timeout suffices.
BINARY_TIMEOUT = 180
SHA_TIMEOUT = 60
# 分块大小 / chunk size，流式计算 sha256 避免整文件载入内存。
# Stream sha256 by chunk to avoid loading the whole file into memory.
CHUNK = 256 * 1024


def log(msg):
    # 写日志到 stderr，调用方用 2>> 把它收进日志文件；stdout 保持干净只用于退出码。
    # Write log to stderr so the caller captures it via 2>> redirect; stdout stays clean for exit code.
    sys.stderr.write(msg + "\n")
    sys.stderr.flush()


def require_https(url):
    # 强制 HTTPS：下载二进制绝不允许降级到明文 HTTP，防中间人篡改。
    # Enforce HTTPS: a binary download must never downgrade to plaintext HTTP (MITM guard).
    if not url.startswith("https://"):
        log("拒绝非 HTTPS 地址 / refusing non-HTTPS url: %s" % url)
        return False
    return True


def download(url, timeout):
    # 下载并跟随重定向。urllib 的默认 opener 内置 HTTPRedirectHandler，会跟随 301/302/307/308；
    # GitHub 的 /releases/latest/download/<asset> 会 302 到带签名的 HTTPS CDN 地址，默认行为即可覆盖。
    # 显式设置 User-Agent：GitHub 对缺省 UA 可能返回 403。
    # Download and follow redirects. urllib's default opener has a built-in HTTPRedirectHandler that follows
    # 301/302/307/308; GitHub's /releases/latest/download/<asset> 302-redirects to a signed HTTPS CDN URL,
    # so the default behavior covers it. An explicit User-Agent is set because GitHub may 403 the default UA.
    req = urllib.request.Request(url, headers={"User-Agent": "Samryetha-Ops/fallback-fetch"})
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return resp.read()


def sha256_of(path):
    # 流式计算文件 sha256 / stream the file's sha256。
    h = hashlib.sha256()
    with open(path, "rb") as f:
        while True:
            block = f.read(CHUNK)
            if not block:
                break
            h.update(block)
    return h.hexdigest()


def is_elf_x86_64(path):
    # 轻量 ELF 校验，替换旧脚本里的 `file` 命令（纯 Python 读 ELF 头，不依赖外部工具）。
    # 只检查魔数与 e_machine，防止下到 HTML 错误页或错误架构被当作二进制安装。
    # Lightweight ELF check, replacing the old `file` command (pure Python reads the ELF header).
    # Checks only the magic and e_machine, to avoid installing an HTML error page or wrong architecture.
    with open(path, "rb") as f:
        head = f.read(20)
    if len(head) < 20:
        return False
    # ELF 魔数 / ELF magic：\x7fELF
    if head[0:4] != b"\x7fELF":
        return False
    # e_machine 位于偏移 18（小端 2 字节），0x3E = EM_X86_64。
    # e_machine sits at offset 18 (2 bytes little-endian), 0x3E = EM_X86_64.
    machine = head[18] | (head[19] << 8)
    return machine == 0x3E


def main():
    if len(sys.argv) != 4:
        log("用法 / usage: fallback_fetch.py <binary_url> <sha256_url> <output_path>")
        return 2

    binary_url, sha_url, output = sys.argv[1], sys.argv[2], sys.argv[3]

    # HTTPS 强制校验 / enforce HTTPS on both inputs。
    if not require_https(binary_url) or not require_https(sha_url):
        return 1

    # 1) 下载二进制 / download the binary。
    try:
        log("下载二进制 / downloading binary: %s" % binary_url)
        data = download(binary_url, BINARY_TIMEOUT)
    except (urllib.error.URLError, urllib.error.HTTPError, TimeoutError, OSError) as e:
        log("下载二进制失败 / binary download failed: %s" % e)
        return 1
    if not data:
        log("下载二进制为空 / binary download is empty")
        return 1

    # 2) 下载 sha256 清单并解析期望值。清单形如 "<hash>  dist/update-service"，取第一列。
    # 2) Download the sha256 manifest and parse the expected hash. The manifest is "<hash>  <name>"; take the first column.
    try:
        log("下载 sha256 清单 / downloading sha256 manifest: %s" % sha_url)
        manifest = download(sha_url, SHA_TIMEOUT).decode("utf-8", "replace")
    except (urllib.error.URLError, urllib.error.HTTPError, TimeoutError, OSError) as e:
        log("下载 sha256 清单失败 / sha256 manifest download failed: %s" % e)
        return 1
    want = manifest.split()[0] if manifest.split() else ""

    # 3) 写临时文件并校验 sha256。临时文件放在输出同目录，保证 os.replace 的原子性（同一文件系统）。
    # 3) Write a temp file and verify sha256. The temp file lives in the output directory so os.replace
    # stays atomic (same filesystem).
    tmp_bin = output + ".download.tmp"
    try:
        with open(tmp_bin, "wb") as f:
            f.write(data)
        os.chmod(tmp_bin, 0o755)
    except OSError as e:
        log("写临时文件失败 / write temp file failed: %s" % e)
        return 1

    got = sha256_of(tmp_bin)
    # sha256 校验失败即拒绝，绝不落地未校验的二进制。
    # On sha256 mismatch, reject — never land an unverified binary.
    if not want or want != got:
        log("sha256 校验失败 / sha256 mismatch: want=%s got=%s" % (want or "(空/empty)", got))
        try:
            os.remove(tmp_bin)
        except OSError:
            pass
        return 1

    # 4) 轻量 ELF 校验，防止 HTML 错误页 / 错误架构被当作二进制。
    # 4) Lightweight ELF check, guarding against HTML error pages / wrong architecture.
    if not is_elf_x86_64(tmp_bin):
        log("下载物不是 linux/amd64 ELF / downloaded file is not a linux/amd64 ELF")
        try:
            os.remove(tmp_bin)
        except OSError:
            pass
        return 1

    # 5) 原子替换：先写临时文件再 os.replace，目标路径绝不出现"半写入"状态。
    # 5) Atomic replace: temp file then os.replace, so the target never shows a half-written state.
    try:
        os.replace(tmp_bin, output)
    except OSError as e:
        log("原子替换失败 / atomic replace failed: %s" % e)
        try:
            os.remove(tmp_bin)
        except OSError:
            pass
        return 1

    log("下载并校验通过 / download and verify ok: sha256=%s" % got[:16])
    return 0


if __name__ == "__main__":
    sys.exit(main())
