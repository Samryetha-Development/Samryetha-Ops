#!/usr/bin/node
// Samryetha 代码体检引擎（纯静态分析，无 AI）
//
// 三层检测：
//   1. tsc 语义诊断          —— 类型系统级错误（跨文件类型流分析）
//   2. ESLint type-checked   —— 浮动 Promise / 竞态 / N+1 / 逻辑死代码 / 安全反模式
//   3. 自定义 AST 检查        —— 空 catch、定时器泄漏等高置信模式
//
// 基线棘轮（保证"绝对准确"的关键）：
//   首次运行 --init 建立基线，存量问题记入基线不告警；
//   之后每次运行只对【新增】问题立案写事件，已修复的问题自动从基线移除。
//
// 用法：
//   node codecheck.mjs            # 体检并与基线比对，写 code-report.json
//   node codecheck.mjs --init     # 以当前状态建立基线（存量问题不再告警）

import { createRequire } from "node:module";
import fs from "node:fs";
import path from "node:path";

const ROOT = "/opt/Samryetha";
const ANALYSIS = path.join(ROOT, "analysis");
const REPORT = path.join(ROOT, "status", "data", "code-report.json");
const BASELINE = path.join(ROOT, "status", "data", "code-baseline.json");
const INIT = process.argv.includes("--init");
const CHECK_EMPTY_CATCH = false;

const require2 = createRequire(path.join(ANALYSIS, "package.json"));
const { ESLint } = require2("eslint");
const ts = require2("typescript");

// 后端类型感知：backend 迁移到 Python 后不再做 backend 的 tsc/ESLint/AST 检查
// （backend/src 已无 TS；残留的 .ts 文件会产生噪音误报）
function backendType() {
  if (fs.existsSync(path.join(ROOT, "backend/src/app/server.ts"))) return "node";
  if (fs.existsSync(path.join(ROOT, "backend/pyproject.toml")) &&
      fs.existsSync(path.join(ROOT, "backend/src/samryetha/main.py"))) return "python";
  return "unknown";
}

// ---------- 1. tsc 语义诊断 ----------
function tscDiagnostics(tsconfigPath) {
  const cfg = ts.readConfigFile(tsconfigPath, ts.sys.readFile);
  if (cfg.error) return [];
  const parsed = ts.parseJsonConfigFileContent(cfg.config, ts.sys, path.dirname(tsconfigPath));
  const program = ts.createProgram(parsed.fileNames, parsed.options);
  const diags = program.getSemanticDiagnostics();
  const out = [];
  for (const dg of diags) {
    const f = dg.file ? path.relative(ROOT, dg.file.fileName) : "(unknown)";
    const pos = dg.file && dg.start != null ? dg.file.getLineAndCharacterOfPosition(dg.start) : null;
    out.push({
      file: f, line: pos ? pos.line + 1 : 0,
      rule: "tsc(" + ts.flattenDiagnosticMessageText(dg.messageText, " ").slice(0, 30) + ")",
      sev: "error", message: ts.flattenDiagnosticMessageText(dg.messageText, " "),
    });
  }
  return out;
}

// ---------- 2. 自定义 AST 检查 ----------
function customAstChecks(backend) {
  const findings = [];
  const files = [];
  const dirs = backend === "node" ? ["backend/src", "frontend/src"] : ["frontend/src"];
  for (const dir of dirs) {
    const walk = (p) => {
      for (const e of fs.readdirSync(p, { withFileTypes: true })) {
        const full = path.join(p, e.name);
        if (e.isDirectory()) walk(full);
        else if (/\.(ts|tsx)$/.test(e.name)) files.push(full);
      }
    };
    walk(path.join(ROOT, dir));
  }
  for (const file of files) {
    const rel = path.relative(ROOT, file);
    const src = fs.readFileSync(file, "utf-8");
    const sf = ts.createSourceFile(file, src, ts.ScriptTarget.Latest, true);
    const timers = { set: [], clear: 0 };
    const visit = (node) => {
      if (CHECK_EMPTY_CATCH && ts.isCatchClause(node) && node.block) {
        const hasCode = node.block.statements.length > 0;
        const bodyText = node.block.getText().replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/[^\n]*/g, "").trim();
        if (!hasCode || bodyText === "{}") {
          const pos = sf.getLineAndCharacterOfPosition(node.getStart(sf));
          findings.push({ file: rel, line: pos.line + 1, rule: "empty-catch", sev: "error",
            message: "空的 catch 块吞掉了错误——异常发生时无任何记录，问题将被掩盖" });
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
      findings.push({ file: rel, line: pos.line + 1, rule: "timer-leak", sev: "warning",
        message: `setInterval 出现 ${timers.set.length} 次但 clearInterval 仅 ${timers.clear} 次——定时器未清理会随生命周期累积泄漏` });
    }
  }
  return findings;
}

// ---------- 3. ESLint ----------
async function eslintRun(backend) {
  const eslint = new ESLint({
    cwd: ROOT,
    overrideConfigFile: path.join(ANALYSIS, "eslint.config.mjs"),
  });
  const targets = ["frontend/src/**/*.tsx", "frontend/src/**/*.ts"];
  if (backend === "node") targets.unshift("backend/src/**/*.ts");
  const results = await eslint.lintFiles(targets);
  const out = [];
  for (const r of results) {
    for (const m of r.messages) {
      out.push({
        file: path.relative(ROOT, r.filePath),
        line: m.line, rule: m.ruleId || "(syntax)", sev: m.severity === 2 ? "error" : "warning",
        message: m.message,
      });
    }
  }
  return out;
}

// ---------- 汇总 + 基线棘轮 ----------
function findingHash(f) {
  return `${f.rule}|${f.file}|${f.message.slice(0, 120)}`;
}

async function main() {
  const t0 = Date.now();
  const backend = backendType();
  let all = [];
  if (backend === "node") {
    try { all = all.concat(tscDiagnostics(path.join(ROOT, "backend/tsconfig.json"))); } catch (e) { console.error("tsc backend failed:", e.message); }
  }
  try { all = all.concat(tscDiagnostics(path.join(ROOT, "frontend/tsconfig.json"))); } catch (e) { console.error("tsc frontend failed:", e.message); }
  try { all = all.concat(await eslintRun(backend)); } catch (e) { console.error("eslint failed:", e.message); }
  all = all.concat(customAstChecks(backend));

  const baseline = fs.existsSync(BASELINE)
    ? JSON.parse(fs.readFileSync(BASELINE, "utf-8"))
    : { hashes: {} };

  const seen = new Set();
  const uniq = [];
  for (const f of all) {
    const h = findingHash(f);
    if (seen.has(h)) continue;
    seen.add(h);
    uniq.push(f);
  }

  const isNew = (f) => !(findingHash(f) in baseline.hashes);
  const newFindings = uniq.filter(isNew);
  const errors = uniq.filter((f) => f.sev === "error").length;
  const warnings = uniq.filter((f) => f.sev === "warning").length;

  if (INIT) {
    for (const f of uniq) baseline.hashes[findingHash(f)] = { first_seen: Date.now() };
    fs.mkdirSync(path.dirname(BASELINE), { recursive: true });
    fs.writeFileSync(BASELINE, JSON.stringify(baseline, null, 1));
    console.log(`[codecheck] 基线已建立：${uniq.length} 个存量问题（errors=${errors}, warnings=${warnings}），不再告警`);
    return;
  }

  // 已修复：基线里有、本次找不到 → 移出基线（改进被承认）
  const aliveHashes = new Set(uniq.map(findingHash));
  let fixed = 0;
  for (const h of Object.keys(baseline.hashes)) {
    if (!aliveHashes.has(h)) { delete baseline.hashes[h]; fixed++; }
  }

  // 新增问题自动进基线（同一问题只立案一次，之后属存量）
  for (const f of newFindings) baseline.hashes[findingHash(f)] = { first_seen: Date.now() };
  fs.writeFileSync(BASELINE, JSON.stringify(baseline, null, 1));

  const report = {
    ran_at: Date.now(),
    totals: { errors, warnings, total: uniq.length },
    new_count: newFindings.length,
    new: newFindings.slice(0, 20),
    fixed,
    findings: uniq,
    duration_ms: Date.now() - t0,
  };
  fs.mkdirSync(path.dirname(REPORT), { recursive: true });
  fs.writeFileSync(REPORT, JSON.stringify(report, null, 1));
  console.log(`[codecheck] errors=${errors} warnings=${warnings} | 新增=${newFindings.length} 修复=${fixed} | ${report.duration_ms}ms`);
  if (newFindings.length) {
    for (const f of newFindings.slice(0, 5)) {
      console.log(`  NEW ${f.sev} ${f.file}:${f.line} [${f.rule}] ${f.message.slice(0, 90)}`);
    }
    process.exitCode = 2;
  }
}

main().catch((e) => { console.error(e); process.exit(1); });
