#!/usr/bin/env node
// Samryetha 代码体检引擎（纯静态分析，无 AI）
//
// 分析器（互相独立，任一失败不影响其它）：
//   1. tsc               —— 前端类型系统级错误（子进程 + 硬超时）
//   2. eslint            —— type-checked 规则（子进程 + 硬超时）
//   3. ast               —— 自定义 TS AST 检查（定时器泄漏 / 空 catch）
//   4. python            —— Python 后端 AST 检查（python3 子进程 + 硬超时）
//   5. secrets           —— 硬编码密钥扫描（.py / .ts / .tsx）
//
// 基线棘轮（保证"绝对准确"的关键）：
//   首次运行 --init 建立基线，存量问题记入基线不告警；
//   之后每次运行只对【新增】问题立案写事件，已修复的问题自动从基线移除。
//   指纹 = rule|相对文件|归一化消息（剥离行号/耗时/地址等易变内容），
//   并兼容旧指纹，避免升级或无关行移动导致棘轮重置。
//
// 用法：
//   node codecheck.mjs            # 体检并与基线比对，写 code-report.json
//   node codecheck.mjs --init     # 以当前状态建立基线（存量问题不再告警）
//   node codecheck.mjs --json     # 把完整报告 JSON 打印到 stdout
//   node codecheck.mjs --quiet    # 只打印一行汇总
//
// 环境变量：
//   SAMRYETHA_ROOT        仓库根目录（默认 /opt/Samryetha）
//   CODECHECK_TIMEOUT_MS  每个子分析器硬超时，默认 120000
//   CODECHECK_COUNT_MEDIUM=1  让 medium 置信度发现也计入 new_count（默认否）

import { spawn } from "node:child_process";
import { createRequire } from "node:module";
import fs from "node:fs";
import path from "node:path";

// ---------- 配置 ----------
const ROOT = process.env.SAMRYETHA_ROOT || "/opt/Samryetha";
const ANALYSIS = path.join(ROOT, "analysis");
const REPORT = path.join(ROOT, "status", "data", "code-report.json");
const BASELINE = path.join(ROOT, "status", "data", "code-baseline.json");

const ARGV = process.argv.slice(2);
const INIT = ARGV.includes("--init");
const JSON_OUT = ARGV.includes("--json");
const QUIET = ARGV.includes("--quiet");

const TIMEOUT_MS = Math.max(1000, Number(process.env.CODECHECK_TIMEOUT_MS) || 120000);
const COUNT_MEDIUM = process.env.CODECHECK_COUNT_MEDIUM === "1";
const MAX_FINDINGS = 500;
const CHECK_EMPTY_CATCH = false; // 与旧版保持一致（默认关闭）
const SKIP_DIRS = new Set([
  "node_modules", ".git", "dist", "build", ".next", "coverage", "venv", ".venv",
  "__pycache__", "status", ".turbo", "out", ".cache", "htmlcov", ".mypy_cache", ".pytest_cache",
]);

const SEV_RANK = { error: 0, warning: 1, info: 2 };

// ---------- 通用工具 ----------
function hasAnalysis() {
  return fs.existsSync(path.join(ANALYSIS, "node_modules"));
}

function log(msg) {
  if (!JSON_OUT) console.log(msg);
}

function warn(msg) {
  // 警告始终写 stderr（--json 时 stdout 必须保持纯 JSON）
  console.error(`[codecheck] warning: ${msg}`);
}

function normalizeRelPath(p) {
  let s = String(p == null ? "" : p).replace(/\\/g, "/");
  if (ROOT) {
    const rootPosix = ROOT.replace(/\\/g, "/").replace(/\/+$/, "");
    if (s.startsWith(rootPosix + "/")) s = s.slice(rootPosix.length + 1);
    else if (s === rootPosix) s = "";
  }
  s = s.replace(/^\.\//, "");
  return s;
}

function normalizeSeverity(s) {
  const v = String(s == null ? "" : s).toLowerCase();
  if (v === "error" || v === "err" || v === "fatal" || v === "critical") return "error";
  if (v === "info" || v === "notice") return "info";
  return "warning";
}

// 剥离消息里易变的部分：行号、列号、耗时、地址、绝对路径、ANSI 颜色
function normalizeMessage(msg) {
  let s = String(msg == null ? "" : msg);
  s = s.replace(/\u001b\[[0-9;]*m/g, "");
  if (ROOT) {
    s = s.split(ROOT).join("<root>");
    s = s.split(ROOT.replace(/\\/g, "/")).join("<root>");
  }
  s = s
    .replace(/\b0x[0-9a-fA-F]+\b/g, "0xADDR")
    .replace(/\(\d+\s*,\s*\d+\)/g, "(L,C)")
    .replace(/\b\d+:\d+\b/g, "L:C")
    .replace(/\bline\s+\d+\b/gi, "line N")
    .replace(/\b\d+(?:\.\d+)?\s*ms\b/gi, "Nms")
    .replace(/\b\d+(?:\.\d+)?\s*s\b/g, "Ns")
    .replace(/\s+/g, " ")
    .trim();
  return s.slice(0, 200);
}

function findingHash(f) {
  return `${f.rule}|${f.file}|${normalizeMessage(f.message)}`;
}

// 旧版指纹（升级兼容用，避免一次性全量误报）
function legacyHash(f) {
  return `${f.rule}|${f.file}|${String(f.message == null ? "" : f.message).slice(0, 120)}`;
}

function normalizeFinding(raw, analyzer) {
  const sev = normalizeSeverity(raw.sev != null ? raw.sev : raw.severity);
  return {
    file: normalizeRelPath(raw.file),
    line: Number.isFinite(raw.line) ? raw.line : 0,
    rule: String(raw.rule || "(unknown)"),
    sev,
    severity: sev,
    confidence: raw.confidence === "medium" ? "medium" : "high",
    analyzer: analyzer || raw.analyzer || "(unknown)",
    message: String(raw.message == null ? "" : raw.message).trim(),
  };
}

// 只有 high 置信度的 Python / secrets 发现默认计入 new_count
function countsTowardNew(f) {
  if (COUNT_MEDIUM) return true;
  if (f.confidence === "high") return true;
  return f.analyzer !== "python" && f.analyzer !== "secrets";
}

// 旧基线里没有 analyzer 字段时，从规则名推断归属（用于失败分析器的棘轮保护）
function inferAnalyzer(rule) {
  if (/^py-/.test(rule)) return "python";
  if (rule === "secret-hardcoded") return "secrets";
  if (/^tsc\(/.test(rule)) return "tsc";
  if (rule === "timer-leak" || rule === "empty-catch") return "ast";
  return "eslint";
}

function dedupeSort(findings) {
  const byHash = new Map();
  for (const f of findings) {
    const h = findingHash(f);
    const prev = byHash.get(h);
    if (!prev) { byHash.set(h, f); continue; }
    if (SEV_RANK[f.sev] < SEV_RANK[prev.sev]) { byHash.set(h, f); continue; }
    if (SEV_RANK[f.sev] === SEV_RANK[prev.sev] && prev.confidence === "medium" && f.confidence === "high") {
      byHash.set(h, f);
    }
  }
  const list = [...byHash.values()];
  list.sort((a, b) =>
    SEV_RANK[a.sev] - SEV_RANK[b.sev] ||
    a.file.localeCompare(b.file) ||
    a.line - b.line ||
    a.rule.localeCompare(b.rule)
  );
  return list;
}

function parseJsonArray(stdout) {
  const text = String(stdout == null ? "" : stdout).trim();
  if (!text) return [];
  try {
    const v = JSON.parse(text);
    if (Array.isArray(v)) return v;
  } catch { /* 容忍子进程的额外输出 */ }
  const lines = text.split(/\r?\n/);
  for (let i = lines.length - 1; i >= 0; i--) {
    const line = lines[i].trim();
    if (!line.startsWith("[")) continue;
    try {
      const v = JSON.parse(line);
      if (Array.isArray(v)) return v;
    } catch { /* 继续往前找 */ }
  }
  return [];
}

// 子进程运行器：硬超时，超时后 SIGKILL，返回结构化结果（不抛异常）
function runChild(command, args, opts = {}) {
  const timeoutMs = opts.timeoutMs || TIMEOUT_MS;
  const started = Date.now();
  return new Promise((resolve) => {
    let child;
    try {
      child = spawn(command, args, {
        cwd: opts.cwd || ROOT,
        env: { ...process.env, FORCE_COLOR: "0", NO_COLOR: "1", ...(opts.env || {}) },
        stdio: ["pipe", "pipe", "pipe"],
      });
    } catch (e) {
      resolve({ ok: false, code: null, timedOut: false, error: e.message, stdout: "", stderr: "", ms: Date.now() - started });
      return;
    }
    let stdout = "", stderr = "", timedOut = false, settled = false;
    const finish = (result) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      resolve({ ms: Date.now() - started, stdout, stderr, ...result });
    };
    const timer = setTimeout(() => {
      timedOut = true;
      // 立即结算，不等待 close：SIGKILL 直接子进程，若其孙进程仍持有管道也不能拖住整轮体检
      finish({ ok: false, code: null, timedOut, error: `timeout after ${timeoutMs}ms` });
      try { child.kill("SIGKILL"); } catch { /* 忽略 */ }
      try { child.stdout.destroy(); child.stderr.destroy(); child.stdin.destroy(); } catch { /* 忽略 */ }
      try { child.unref(); } catch { /* 忽略 */ }
    }, timeoutMs);
    child.stdout.on("data", (d) => { stdout += d; });
    child.stderr.on("data", (d) => { stderr += d; });
    child.on("error", (e) => finish({ ok: false, code: null, timedOut, error: e.message }));
    child.on("close", (code) => finish({
      ok: !timedOut && code === 0,
      code,
      timedOut,
      error: timedOut ? `timeout after ${timeoutMs}ms` : (code === 0 ? undefined : `exit code ${code}`),
    }));
    try {
      if (opts.input != null) child.stdin.end(opts.input);
      else child.stdin.end();
    } catch { /* 子进程可能已退出 */ }
  });
}

function walkFiles(dir, exts, out, deadline) {
  if (Date.now() > deadline) return;
  let entries;
  try { entries = fs.readdirSync(dir, { withFileTypes: true }); } catch { return; }
  for (const e of entries) {
    if (Date.now() > deadline) return;
    const full = path.join(dir, e.name);
    if (e.isDirectory()) {
      if (SKIP_DIRS.has(e.name)) continue;
      walkFiles(full, exts, out, deadline);
    } else if (exts.some((x) => e.name.endsWith(x))) {
      out.push(full);
    }
  }
}

// ---------- 分析器 1：tsc（子进程） ----------
const TSC_RUNNER = String.raw`
import { createRequire } from "node:module";
import path from "node:path";
const [analysisDir, root, tsconfigPath] = process.argv.slice(1);
const require2 = createRequire(path.join(analysisDir, "package.json"));
const ts = require2("typescript");
const cfg = ts.readConfigFile(tsconfigPath, ts.sys.readFile);
if (cfg.error) { console.log("[]"); process.exit(0); }
const parsed = ts.parseJsonConfigFileContent(cfg.config, ts.sys, path.dirname(tsconfigPath));
const program = ts.createProgram(parsed.fileNames, parsed.options);
const diags = program.getSemanticDiagnostics().concat(program.getSyntacticDiagnostics());
const out = [];
for (const dg of diags) {
  const f = dg.file ? path.relative(root, dg.file.fileName) : "(unknown)";
  const pos = dg.file && dg.start != null ? dg.file.getLineAndCharacterOfPosition(dg.start) : null;
  const message = ts.flattenDiagnosticMessageText(dg.messageText, " ");
  out.push({ file: f, line: pos ? pos.line + 1 : 0, rule: "tsc(TS" + dg.code + ")", sev: "error", confidence: "high", message: message });
}
console.log(JSON.stringify(out));
`;

async function analyzerTsc() {
  if (!hasAnalysis()) throw new Error("analysis/node_modules not found; skipped");
  const tsconfig = path.join(ROOT, "frontend", "tsconfig.json");
  if (!fs.existsSync(tsconfig)) throw new Error("frontend/tsconfig.json not found");
  const res = await runChild(process.execPath, ["--input-type=module", "-e", TSC_RUNNER, ANALYSIS, ROOT, tsconfig]);
  if (res.timedOut) throw new Error(`timeout after ${TIMEOUT_MS}ms`);
  if (!res.ok && !res.stdout.trim()) throw new Error(res.error || (res.stderr || "tsc runner failed").slice(0, 200));
  return parseJsonArray(res.stdout).map((f) => normalizeFinding(f, "tsc"));
}

// ---------- 分析器 2：eslint（子进程） ----------
const ESLINT_RUNNER = String.raw`
import { createRequire } from "node:module";
import path from "node:path";
const [analysisDir, root, ...targets] = process.argv.slice(1);
const require2 = createRequire(path.join(analysisDir, "package.json"));
const { ESLint } = require2("eslint");
const eslint = new ESLint({ cwd: root, overrideConfigFile: path.join(analysisDir, "eslint.config.mjs") });
const results = await eslint.lintFiles(targets);
const out = [];
for (const r of results) {
  for (const m of r.messages) {
    out.push({
      file: path.relative(root, r.filePath),
      line: m.line || 0,
      rule: m.ruleId || "(syntax)",
      sev: m.severity === 2 ? "error" : "warning",
      confidence: "high",
      message: m.message,
    });
  }
}
console.log(JSON.stringify(out));
`;

async function analyzerEslint() {
  if (!hasAnalysis()) throw new Error("analysis/node_modules not found; skipped");
  const cfg = path.join(ANALYSIS, "eslint.config.mjs");
  if (!fs.existsSync(cfg)) throw new Error("analysis/eslint.config.mjs not found");
  const targets = ["frontend/src/**/*.tsx", "frontend/src/**/*.ts"];
  const res = await runChild(process.execPath, ["--input-type=module", "-e", ESLINT_RUNNER, ANALYSIS, ROOT, ...targets]);
  if (res.timedOut) throw new Error(`timeout after ${TIMEOUT_MS}ms`);
  if (!res.ok && !res.stdout.trim()) throw new Error(res.error || (res.stderr || "eslint runner failed").slice(0, 200));
  return parseJsonArray(res.stdout).map((f) => normalizeFinding(f, "eslint"));
}

// ---------- 分析器 3：自定义 TS AST ----------
function analyzerAst() {
  if (!hasAnalysis()) throw new Error("analysis/node_modules not found; skipped");
  const require2 = createRequire(path.join(ANALYSIS, "package.json"));
  const ts = require2("typescript");
  const findings = [];
  const files = [];
  const deadline = Date.now() + TIMEOUT_MS;
  for (const dir of ["frontend/src"]) {
    const full = path.join(ROOT, dir);
    if (fs.existsSync(full)) walkFiles(full, [".ts", ".tsx"], files, deadline);
  }
  for (const file of files) {
    if (Date.now() > deadline) break;
    let src, sf;
    try {
      src = fs.readFileSync(file, "utf-8");
      sf = ts.createSourceFile(file, src, ts.ScriptTarget.Latest, true);
    } catch { continue; }
    const rel = normalizeRelPath(path.relative(ROOT, file));
    const timers = { set: [], clear: 0 };
    const visit = (node) => {
      if (CHECK_EMPTY_CATCH && ts.isCatchClause(node) && node.block) {
        const hasCode = node.block.statements.length > 0;
        const bodyText = node.block.getText().replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/[^\n]*/g, "").trim();
        if (!hasCode || bodyText === "{}") {
          const pos = sf.getLineAndCharacterOfPosition(node.getStart(sf));
          findings.push(normalizeFinding({
            file: rel, line: pos.line + 1, rule: "empty-catch", sev: "error", confidence: "high",
            message: "空的 catch 块吞掉了错误——异常发生时无任何记录，问题将被掩盖",
          }, "ast"));
        }
      }
      if (ts.isCallExpression(node)) {
        const exprText = node.expression.getText(sf);
        if (/\bsetInterval$/.test(exprText)) timers.set.push(node.getStart(sf));
        if (/\bclearInterval\b/.test(exprText)) timers.clear++;
      }
      ts.forEachChild(node, visit);
    };
    visit(sf);
    if (timers.set.length > timers.clear) {
      const pos = sf.getLineAndCharacterOfPosition(timers.set[0]);
      findings.push(normalizeFinding({
        file: rel, line: pos.line + 1, rule: "timer-leak", sev: "warning", confidence: "high",
        message: "setInterval 出现 " + timers.set.length + " 次但 clearInterval 仅 " + timers.clear + " 次——定时器未清理会随生命周期累积泄漏",
      }, "ast"));
    }
  }
  return findings;
}

// ---------- 分析器 4：Python AST（python3 子进程） ----------
const PYTHON_SCRIPT = String.raw`
import ast, json, os, sys

root = sys.argv[1] if len(sys.argv) > 1 else "."
src_root = os.path.join(root, "backend", "src")


def rel(p):
    try:
        return os.path.relpath(p, root).replace(os.sep, "/")
    except Exception:
        return p


def is_test_file(p):
    parts = p.replace(os.sep, "/").lower().split("/")
    name = parts[-1] if parts else ""
    if name.startswith("test_") or name.endswith("_test.py") or name == "conftest.py":
        return True
    return any(x in ("test", "tests", "__tests__") for x in parts)


def is_noop(stmt):
    if isinstance(stmt, ast.Pass):
        return True
    if isinstance(stmt, ast.Expr) and isinstance(stmt.value, ast.Constant):
        return True
    return False


def is_literal(node):
    return isinstance(node, ast.Constant) and isinstance(node.value, (str, bytes))


findings = []

for dirpath, dirnames, filenames in os.walk(src_root):
    for fn in filenames:
        if not fn.endswith(".py"):
            continue
        full = os.path.join(dirpath, fn)
        relp = rel(full)
        try:
            with open(full, "r", encoding="utf-8") as fh:
                src = fh.read()
            tree = ast.parse(src, filename=full)
        except Exception:
            # 单个文件解析失败绝不能中断整轮体检
            continue

        for node in ast.walk(tree):
            if isinstance(node, ast.ExceptHandler):
                if node.type is None:
                    findings.append({
                        "file": relp, "line": node.lineno, "rule": "py-bare-except",
                        "sev": "error", "confidence": "high",
                        "message": "bare except catches every exception (including KeyboardInterrupt/SystemExit); errors are silently swallowed",
                    })
                else:
                    names = []
                    if isinstance(node.type, ast.Name):
                        names = [node.type.id]
                    elif isinstance(node.type, ast.Tuple):
                        names = [e.id for e in node.type.elts if isinstance(e, ast.Name)]
                    body = [s for s in node.body if not is_noop(s)]
                    if "Exception" in names and len(body) == 0:
                        findings.append({
                            "file": relp, "line": node.lineno, "rule": "py-swallowed-exception",
                            "sev": "warning", "confidence": "high",
                            "message": "except Exception with only pass/ellipsis and no logging; the error is silently discarded",
                        })

            elif isinstance(node, ast.Call):
                func = node.func
                if isinstance(func, ast.Name) and func.id in ("eval", "exec") and node.args:
                    if not all(is_literal(a) for a in node.args):
                        findings.append({
                            "file": relp, "line": node.lineno, "rule": "py-dynamic-eval",
                            "sev": "warning", "confidence": "medium",
                            "message": "call to " + func.id + "() with a non-literal argument; dynamic code execution is an injection risk",
                        })
                if isinstance(func, ast.Attribute):
                    owner = func.value
                    if func.attr == "system" and isinstance(owner, ast.Name) and owner.id == "os":
                        findings.append({
                            "file": relp, "line": node.lineno, "rule": "py-os-system",
                            "sev": "warning", "confidence": "medium",
                            "message": "os.system() runs a shell command; prefer subprocess with an argument list",
                        })
                    if isinstance(owner, ast.Name) and owner.id == "subprocess":
                        shell_true = False
                        for kw in node.keywords:
                            if kw.arg == "shell" and isinstance(kw.value, ast.Constant) and kw.value.value is True:
                                shell_true = True
                        if shell_true and node.args:
                            cmd = node.args[0]
                            if isinstance(cmd, (ast.JoinedStr, ast.BinOp)):
                                findings.append({
                                    "file": relp, "line": node.lineno, "rule": "py-shell-injection",
                                    "sev": "error", "confidence": "high",
                                    "message": "subprocess called with shell=True and a dynamically built command; command injection risk",
                                })

            elif isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
                defaults = list(node.args.defaults)
                for d in node.args.kw_defaults:
                    if d is not None:
                        defaults.append(d)
                for d in defaults:
                    kind = None
                    if isinstance(d, ast.List) and not d.elts:
                        kind = "[]"
                    elif isinstance(d, ast.Dict) and not d.keys:
                        kind = "{}"
                    elif isinstance(d, ast.Call) and isinstance(d.func, ast.Name) and d.func.id in ("list", "dict", "set") and not d.args:
                        kind = d.func.id + "()"
                    if kind:
                        findings.append({
                            "file": relp, "line": node.lineno, "rule": "py-mutable-default",
                            "sev": "warning", "confidence": "high",
                            "message": "mutable default argument " + kind + "; evaluated once and shared across calls",
                        })

            elif isinstance(node, ast.Assert):
                if not is_test_file(full):
                    findings.append({
                        "file": relp, "line": node.lineno, "rule": "py-assert-runtime",
                        "sev": "warning", "confidence": "medium",
                        "message": "assert used for runtime validation; assertions are stripped when Python runs with -O",
                    })

sys.stdout.write(json.dumps(findings, ensure_ascii=False))
`;

async function analyzerPython() {
  const srcRoot = path.join(ROOT, "backend", "src");
  if (!fs.existsSync(srcRoot)) throw new Error("backend/src not found; skipped");
  const res = await runChild("python3", ["-", ROOT], { input: PYTHON_SCRIPT });
  if (res.timedOut) throw new Error(`timeout after ${TIMEOUT_MS}ms`);
  if (!res.ok && !res.stdout.trim()) {
    throw new Error(res.error || (res.stderr || "python3 failed").trim().slice(0, 200));
  }
  return parseJsonArray(res.stdout).map((f) => normalizeFinding(f, "python"));
}

// ---------- 分析器 5：硬编码密钥扫描 ----------
const SECRET_KEY_RE = /(api[_-]?key|apikey|secret|password|passwd|pwd|token|access[_-]?key|private[_-]?key|client[_-]?secret|auth[_-]?token|credential)/i;
const PLACEHOLDER_RE = /^(?:changeme|change[-_]?me|your[-_]?|example|placeholder|dummy|redacted|todo|fixme|xxx+|abc123|test|none|null|undefined|true|false|secret|password|token|\*+|<[^>]*>|\{\{[^}]*\}\}|\.{3}|-+)$/i;
const SECRET_PREFIX_RE = /^(?:sk-|pk-|ghp_|gho_|ghs_|github_pat_|xox[baprs]-|AKIA|ASIA|AIza|ya29\.|eyJ)/;
const SECRET_ASSIGN_RES = [
  /([A-Za-z_$][\w$]*)\s*[:=]\s*(["'`])([^"'`\n]{6,})\2/g,
  /([A-Za-z_][\w]*)\s*(?::[^=\n]+)?=\s*(["'])([^"'\n]{6,})\2/g,
];

function shannonEntropy(str) {
  const freq = new Map();
  for (const ch of str) freq.set(ch, (freq.get(ch) || 0) + 1);
  let h = 0;
  for (const c of freq.values()) {
    const p = c / str.length;
    h -= p * Math.log2(p);
  }
  return h;
}

function looksLikeSecret(value) {
  if (value.length < 8) return false;
  if (PLACEHOLDER_RE.test(value)) return false;
  if (/(process\.env|import\.meta\.env|os\.environ|os\.getenv|getenv\(|\$\{)/.test(value)) return false;
  // 错误码 / 枚举名 / 常量标识符（全大写下划线）不是密钥值
  if (/^[A-Z][A-Z0-9_]{5,}$/.test(value)) return false;
  if (SECRET_PREFIX_RE.test(value)) return true;
  if (value.length < 12) return false;
  const hasMix = /[A-Za-z]/.test(value) && (/[0-9]/.test(value) || /[^A-Za-z0-9]/.test(value));
  return hasMix && shannonEntropy(value) >= 3.0;
}

// 明显的非密钥上下文：pydantic Settings 字段默认、错误码定义、类型注解
function isNonSecretContext(line, key) {
  // 错误码/常量：NAME: ... = "FOO_BAR" 或 _CODE = "FOO"
  if (/(_CODE|_ERROR|_TYPE|_STATUS|_KIND|_REASON|_NAME|_LABEL|_KEY\b)\s*[:=]/.test(line) &&
      !/secret|token|password/i.test(key)) return true;
  // pydantic/pydantic-settings 字段声明（带类型注解 + Field(...)/default），值只是占位默认
  if (/^\s*[a-z_][\w]*\s*:\s*(str|int|bool|SecretStr|list|dict|float)\b/.test(line)) return true;
  return false;
}

function isTestFile(p) {
  const s = p.replace(/\\/g, "/").toLowerCase();
  if (/\/(test|tests|__tests__|spec|specs|fixtures|__mocks__)\//.test(s)) return true;
  if (/(^|\/)(test_[^/]*|[^/]*_test|conftest)\.py$/.test(s)) return true;
  if (/\.(test|spec)\.(ts|tsx|js|jsx)$/.test(s)) return true;
  return false;
}

function analyzerSecrets() {
  const findings = [];
  const files = [];
  const deadline = Date.now() + TIMEOUT_MS;
  for (const dir of ["frontend/src", "backend/src"]) {
    const full = path.join(ROOT, dir);
    if (fs.existsSync(full)) walkFiles(full, [".py", ".ts", ".tsx"], files, deadline);
  }
  for (const file of files) {
    if (Date.now() > deadline) break;
    if (isTestFile(file) || /\.env\.example$/i.test(file)) continue;
    let src;
    try { src = fs.readFileSync(file, "utf-8"); } catch { continue; }
    const rel = normalizeRelPath(path.relative(ROOT, file));
    const lines = src.split(/\r?\n/);
    for (let i = 0; i < lines.length; i++) {
      const line = lines[i];
      const trimmed = line.trim();
      if (!trimmed || trimmed.startsWith("//") || trimmed.startsWith("#") ||
          trimmed.startsWith("*") || trimmed.startsWith("/*")) continue;
      for (const re of SECRET_ASSIGN_RES) {
        re.lastIndex = 0;
        let m;
        while ((m = re.exec(line)) !== null) {
          const key = m[1];
          const value = m[3];
          if (!SECRET_KEY_RE.test(key)) continue;
          if (isNonSecretContext(line, key)) continue;
          if (!looksLikeSecret(value)) continue;
          const before = line.slice(0, m.index);
          if (before.includes("//") || before.includes("#")) continue;
          findings.push(normalizeFinding({
            file: rel, line: i + 1, rule: "secret-hardcoded", sev: "warning", confidence: "high",
            message: `硬编码密钥赋值给 '${key}'——应改为从环境变量/密钥管理读取`,
          }, "secrets"));
          break;
        }
      }
    }
  }
  return findings;
}

// ---------- 分析器调度：独立 try/catch + 计时 ----------
async function runAnalyzer(name, fn) {
  const t0 = Date.now();
  try {
    const result = await fn();
    const findings = Array.isArray(result) ? result : [];
    return { name, ok: true, ms: Date.now() - t0, count: findings.length, findings };
  } catch (e) {
    const message = e && e.message ? e.message : String(e);
    warn(`analyzer "${name}" failed: ${message}`);
    return { name, ok: false, ms: Date.now() - t0, count: 0, error: message, findings: [] };
  }
}

function loadBaseline() {
  try {
    if (!fs.existsSync(BASELINE)) return { hashes: {} };
    const parsed = JSON.parse(fs.readFileSync(BASELINE, "utf-8"));
    if (!parsed || typeof parsed !== "object" || typeof parsed.hashes !== "object" || parsed.hashes === null) {
      return { hashes: {} };
    }
    return parsed;
  } catch (e) {
    warn(`baseline unreadable, starting empty: ${e.message}`);
    return { hashes: {} };
  }
}

function writeJson(file, data) {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, JSON.stringify(data, null, 1));
}

// ---------- 主流程 ----------
async function main() {
  const t0 = Date.now();

  const specs = [
    ["tsc", analyzerTsc],
    ["eslint", analyzerEslint],
    ["ast", analyzerAst],
    ["python", analyzerPython],
    ["secrets", analyzerSecrets],
  ];

  const results = [];
  for (const [name, fn] of specs) {
    results.push(await runAnalyzer(name, fn));
  }

  const analyzers = results.map((r) => {
    const entry = { name: r.name, ok: r.ok, ms: r.ms, count: r.count };
    if (r.error) entry.error = r.error;
    return entry;
  });

  const raw = [];
  for (const r of results) for (const f of r.findings) raw.push(f);
  const uniq = dedupeSort(raw);

  const errors = uniq.filter((f) => f.sev === "error").length;
  const warnings = uniq.filter((f) => f.sev === "warning").length;
  const infos = uniq.filter((f) => f.sev === "info").length;

  const baseline = loadBaseline();
  const entries = baseline.hashes;

  const newHashToFinding = new Map();
  const legacyToNewHash = new Map();
  for (const f of uniq) {
    const nh = findingHash(f);
    newHashToFinding.set(nh, f);
    const lh = legacyHash(f);
    if (!legacyToNewHash.has(lh)) legacyToNewHash.set(lh, nh);
  }
  const isNew = (f) => !(findingHash(f) in entries) && !(legacyHash(f) in entries);

  // 本轮失败的分析器：其基线条目必须保留，否则会被误判为"已修复"，
  // 下轮恢复后同一批问题又会以"新增"形式爆出来。
  const failedAnalyzers = new Set(results.filter((r) => !r.ok).map((r) => r.name));

  if (INIT) {
    const now = Date.now();
    for (const f of uniq) entries[findingHash(f)] = { first_seen: now, analyzer: f.analyzer };
    baseline.fingerprint_version = 2;
    baseline.updated_at = now;
    writeJson(BASELINE, baseline);
    const summary = { init: true, total: uniq.length, errors, warnings, analyzers, duration_ms: Date.now() - t0 };
    if (JSON_OUT) console.log(JSON.stringify(summary, null, 1));
    else log(`[codecheck] 基线已建立：${uniq.length} 个存量问题（errors=${errors}, warnings=${warnings}），不再告警`);
    return;
  }

  const allNew = uniq.filter(isNew);
  const newFindings = allNew.filter(countsTowardNew);
  const suppressedNew = allNew.filter((f) => !countsTowardNew(f));

  // 修复 + 旧指纹迁移
  let fixed = 0;
  for (const h of Object.keys(entries)) {
    if (newHashToFinding.has(h)) {
      const f = newHashToFinding.get(h);
      if (entries[h] && !entries[h].analyzer) entries[h].analyzer = f.analyzer;
      continue;
    }
    const entry = entries[h] || {};
    const owner = entry.analyzer || inferAnalyzer(String(h).split("|")[0]);
    if (failedAnalyzers.has(owner)) continue; // 分析器本轮失败 → 保留基线，不计修复
    const migrated = legacyToNewHash.get(h);
    if (migrated && newHashToFinding.has(migrated)) {
      if (!(migrated in entries)) entries[migrated] = entries[h];
      entries[migrated].analyzer = entries[migrated].analyzer || inferAnalyzer(String(migrated).split("|")[0]);
      delete entries[h];
      continue;
    }
    delete entries[h];
    fixed++;
  }

  // 新增问题自动进基线（同一问题只立案一次，之后属存量）
  for (const f of allNew) entries[findingHash(f)] = { first_seen: Date.now(), analyzer: f.analyzer };
  baseline.fingerprint_version = 2;
  baseline.updated_at = Date.now();
  writeJson(BASELINE, baseline);

  const cappedFindings = uniq.slice(0, MAX_FINDINGS);
  const report = {
    ran_at: Date.now(),
    totals: { errors, warnings, info: infos, total: uniq.length },
    new_count: newFindings.length,
    new: newFindings.slice(0, 20),
    fixed,
    findings: cappedFindings,
    duration_ms: Date.now() - t0,
    analyzers,
  };
  if (suppressedNew.length) report.new_suppressed = suppressedNew.length;
  if (uniq.length > MAX_FINDINGS) report.findings_truncated = true;

  writeJson(REPORT, report);

  if (JSON_OUT) {
    console.log(JSON.stringify(report, null, 1));
  } else {
    log(`[codecheck] errors=${errors} warnings=${warnings} | 新增=${newFindings.length} 修复=${fixed} | ${report.duration_ms}ms`);
    if (!QUIET) {
      for (const f of newFindings.slice(0, 5)) {
        log(`  NEW ${f.sev} ${f.file}:${f.line} [${f.rule}] ${f.message.slice(0, 90)}`);
      }
    }
  }
  if (newFindings.length) process.exitCode = 2;
}

main().catch((e) => {
  console.error("[codecheck] fatal:", e && e.stack ? e.stack : e);
  process.exit(1);
});
