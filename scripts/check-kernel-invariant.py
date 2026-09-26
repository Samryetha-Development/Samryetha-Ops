#!/usr/bin/env python3
"""内核不变量检查。

内核的核心主张是"内核不知道更新为何物"。这不能靠人记，必须可执行。

规则：`kernel/` 下**除白名单外**的文件，不得出现领域词汇。
白名单是"服务装配层"——它必然要知道有哪些服务（如 pm2 驱动、deploy.yaml 布局），
但这类知识必须集中在一处、且体量可控，而不是散落进内核各处。

用法：python3 scripts/check-kernel-invariant.py
"""
import re
import sys
from pathlib import Path

# 领域词汇（小写匹配）
DOMAIN_WORDS = [
    "deploy", "deployment", "build", "branch", "pm2", "systemd", "caddy",
    "git", "rollback", "release",
]

# 允许出现领域词的"装配层"文件。
# 为什么允许：装配层的工作就是"把具体服务接进来"，它必然知道服务是什么。
# 限制它的意义在于——这类知识只此一处，内核其余部分保持纯净。
ASSEMBLY_ALLOWLIST = {
    "main.go",     # 入口：服务装配
    "wiring.go",   # 组件构造（含 deploy.yaml 布局）
}

# 允许的例外：纯注释里的举例说明不影响运行时行为
def strip_comments(src: str) -> str:
    out = []
    in_block = False
    for line in src.split("\n"):
        s = line
        if in_block:
            end = s.find("*/")
            if end < 0:
                continue
            s = s[end + 2:]
            in_block = False
        # 去掉 // 之后的内容（不处理字符串内的 //，对本检查足够）
        i = s.find("//")
        if i >= 0:
            s = s[:i]
        out.append(s)
    return "\n".join(out)


def main() -> int:
    kernel = Path("kernel")
    if not kernel.is_dir():
        print("kernel/ not found", file=sys.stderr)
        return 1

    violations = []
    for f in sorted(kernel.rglob("*.go")):
        rel = f.relative_to(kernel)
        if rel.name in ASSEMBLY_ALLOWLIST:
            continue
        src = strip_comments(f.read_text(encoding="utf-8"))
        for i, line in enumerate(src.split("\n"), 1):
            low = line.lower()
            for w in DOMAIN_WORDS:
                # 用词边界匹配，避免 "rebuild" 命中 "build" 这类误报
                if re.search(rf"\b{w}\b", low):
                    violations.append((str(rel), i, w, line.strip()[:90]))
                    break

    print(f"内核文件: {len(list(kernel.rglob('*.go')))} · 装配层豁免: {sorted(ASSEMBLY_ALLOWLIST)}")
    if violations:
        print(f"\n✗ 发现 {len(violations)} 处领域词汇（内核不应知道这些概念）:")
        for rel, ln, w, text in violations:
            print(f"  {rel}:{ln}  [{w}]  {text}")
        print("\n修法：把这类知识移进装配层（main.go / wiring.go），")
        print("      或改为由服务通过 syscall 声明，而不是写进内核。")
        return 1
    print("✓ 内核保持纯净：除装配层外无领域词汇")
    return 0


if __name__ == "__main__":
    sys.exit(main())
