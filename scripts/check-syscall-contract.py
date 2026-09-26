#!/usr/bin/env python3
"""syscall 契约自检。

内核有两处必须严格对应：
  1. register.go 里**实现**的调用
  2. types.go 里**声明权限**的调用

两者不一致会造成真实故障：
  - 实现了但权限表缺失 → 调用时误报 unknown_call（曾真实发生：proc.output）
  - 声明了但未实现     → 调用时报 unknown_call，且 UI/文档会误导使用者

本脚本用作 CI/本地自检，任何不一致都 fail。第三个列表是"故意未实现"的
白名单（属于后续阶段），必须显式登记，避免悄悄漏实现。
"""
import re
import sys

REG = "kernel/syscall/register.go"
TYPES = "kernel/syscall/types.go"

# 尚未实现、但已在权限表登记的计划内调用（随实现推进逐步清空）
PLANNED_NOT_IMPLEMENTED = {
    "log.query", "event.subscribe", "fs.read", "fs.write", "fs.list",
    "config.get", "config.watch", "auth.public", "auth.check",
    "route.mount", "ws.mount", "task.once", "task.cron",
}


def main() -> int:
    reg = open(REG, encoding="utf-8").read()
    types = open(TYPES, encoding="utf-8").read()

    implemented = set(re.findall(r't\["([a-z][a-z_]*\.[a-z_]+)"\]', reg))
    declared = set(re.findall(r'"([a-z][a-z_]*\.[a-z_]+)":\s*"', types))

    missing_perm = implemented - declared            # 会导致 unknown_call
    not_impl = declared - implemented                # 声明了却没实现
    unexpected = not_impl - PLANNED_NOT_IMPLEMENTED  # 未登记的计划外缺口

    print(f"已实现 {len(implemented)} · 权限表 {len(declared)} · 计划内未实现 {len(PLANNED_NOT_IMPLEMENTED)}")

    rc = 0
    if missing_perm:
        print(f"✗ 实现了但权限表缺失（调用会误报 unknown_call）: {sorted(missing_perm)}")
        rc = 1
    else:
        print("✓ 所有已实现的调用都在权限表内")

    if unexpected:
        print(f"✗ 权限表里有既未实现也未登记的调用: {sorted(unexpected)}")
        rc = 1
    else:
        print("✓ 未实现的调用都已显式登记为计划内")

    return rc


if __name__ == "__main__":
    sys.exit(main())
