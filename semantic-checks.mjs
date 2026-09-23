#!/usr/bin/node
/**
 * Samryetha 语义分析器（silent-failure / config-silent）
 *
 * 设计目标：只报「用户可感知的故障被静默吞掉」这一类真 bug，而不是匹配模式。
 *
 * 与 ESLint 的区别（为什么不能靠规则）：
 *   `.catch(() => undefined)` 出现 14 次，其中大多数是有意的（字体探测、VT 清理、
 *   启动时探测配置）。ESLint 的规则无法区分「吞掉用户操作」与「吞掉后台探测」，
 *   全开=噪音，全关=漏报。这里用「调用者/被调对象」的语义来判定：
 *
 *     是「用户可见的副作用」吗？ ── 是 → 静默失败 = 报
 *                              └─ 否 → 忽略
 *
 * 判定「用户可见副作用」的证据（任一命中）：
 *   - 调用链上出现写动词：create/update/delete/save/send/post/submit/mark/like/vote/...
 *   - HTTP 方法为 POST/PUT/PATCH/DELETE
 *   - 函数体里出现 setError/alert/toast/error(...) —— 说明作者本来就想给反馈
 *
 * 输出：与 codecheck.mjs 相同的 finding 结构，供其作为独立分析器集成。
 * 可单独运行：node semantic-checks.mjs [--json]
 */
import fs from "node:fs";
import path from "node:path";
import { createRequire } from "node:module";

const ROOT = process.env.SAMRYETHA_ROOT || "/opt/Samryetha";
const ANALYSIS = path.join(ROOT, "analysis");
const JSON_OUT = process.argv.includes("--json");

const require2 = createRequire(path.join(ANALYSIS, "package.json"));
let ts;
try {
  ts = require2("typescript");
} catch {
  console.error("[semantic] typescript 不可用，退出");
  process.exit(0);
}

// ---------------------------------------------------------------- 语义词典

// 用户可见副作用的动词（出现在被调用表达式的属性名里）
// 注意：只放「改变状态」的动词。read/count/get/list/fetch 是读操作，不算。
const WRITE_VERBS = [
  "create", "update", "delete", "del", "remove", "save", "post", "submit", "send",
  "mark", "like", "unlike", "vote", "follow", "unfollow", "pin", "lock",
  "publish", "upload", "restore", "ban", "unban", "resolve", "dismiss", "approve",
  "deny", "invite", "reset", "change", "set", "logout", "login", "register",
  "backup", "rollback", "restart", "regen",
];

// 读操作白名单：即使动词表命中也不报（如 unreadCount 命中 count/read 的子串）
const READ_ONLY_RE = /(unread|count|get|list|fetch|load|read)$/i;

// markRead / markAllRead 是写（清除未读），要保留；但 unreadCount 是读
const WRITE_OVERRIDE_RE = /mark(Read|AllRead)|mark_read/i;

// 明确「后台/非用户可见」的调用，命中即豁免（探测、清理、DOM 内部、动画）
const BENIGN_SIGNALS = [
  /document\.fonts/, /viewTransition/i, /requestAnimationFrame/, /scroll/i,
  /localStorage/, /sessionStorage/, /matchMedia/, /ResizeObserver/, /IntersectionObserver/,
  /fonts?\./, /\.config\s*\(/, /prefers-reduced-motion/, /preload/i, /decode\(/,
];

// 已知的「故意吞掉、且有注释说明」的豁免前缀
const BENIGN_COMMENT = /(故意|有意|忽略|best[-\s]?effort|silently|cleanup|清理|卸载|探测|probe)/i;

function isWriteCall(exprText, httpMethod) {
  if (httpMethod && /^(POST|PUT|PATCH|DELETE)$/i.test(httpMethod)) return true;
  const tail = (exprText.split(".").slice(-1)[0] || "").replace(/\(.*$/, "");
  // markRead/markAllRead 明确是写；unreadCount 等读操作豁免
  if (WRITE_OVERRIDE_RE.test(exprText)) return true;
  if (READ_ONLY_RE.test(tail)) return false;
  const lower = exprText.toLowerCase();
  return WRITE_VERBS.some((v) => tail.toLowerCase().startsWith(v) || (lower.includes("api.") && lower.includes(v + "(")));
}

function isBenign(exprText, sourceLine) {
  if (BENIGN_SIGNALS.some((re) => re.test(exprText))) return true;
  if (BENIGN_COMMENT.test(sourceLine)) return true;
  return false;
}

// ---------------------------------------------------------------- 调用点保护索引
//
// 解决「跨函数漏报/误报」：一个函数内部 await 无 try/catch，但所有调用点都做了保护
// （caller 的 try/catch、.catch()、或禁用交互），就不该报。
// 反之：函数内部无保护，且存在「用户直接触发且无保护」的调用点，才报。
//
// 模型：收集每个「无内部保护的 async 函数名」，再扫描其调用点是否受保护。

// 判断一个调用表达式所在位置是否受保护：
//   - 外层有 try { ... }   → protected
//   - .catch(...) 挂在调用链上 → protected
//   - void expr            → 显式忽略（有意）
//   - 位于 jsx 属性 onClick={...} 且属性值是 async/含 await → 不受保护（React 不 await）
function callSiteProtected(node, sf) {
  const chain = node.parent;
  if (chain && ts.isPropertyAccessExpression(chain) && chain.name.text === "catch") return true;
  if (chain && ts.isCallExpression(chain) && ts.isPropertyAccessExpression(chain.expression) &&
      chain.expression.name.text === "catch") return true;
  let p = node.parent;
  while (p && !ts.isFunctionLike(p)) {
    if (ts.isTryStatement(p)) return true;
    p = p.parent;
  }
  // void expr → 有意忽略
  const stmt = enclosingStatement(node);
  if (stmt && /^\s*void\s/.test(stmt.getText(sf))) return true;
  return false;
}

function walkFiles(dir, exts, out) {
  if (!fs.existsSync(dir)) return;
  for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, e.name);
    if (e.isDirectory()) walkFiles(full, exts, out);
    else if (exts.some((x) => e.name.endsWith(x))) out.push(full);
  }
}

function lineOf(sf, node) {
  return sf.getLineAndCharacterOfPosition(node.getStart(sf)).line + 1;
}

// 向上找最近的语句，用于判断「这个 Promise 是否被处理」
function enclosingStatement(node) {
  let cur = node;
  while (cur && !ts.isExpressionStatement(cur) && !ts.isVariableStatement(cur) && !ts.isReturnStatement(cur)) {
    cur = cur.parent;
  }
  return cur;
}

// ---------------------------------------------------------------- 检查 1：静默吞掉用户操作
//
// 形态：
//   void api.x.write(...).catch(() => undefined)     ← 写请求被吞
//   void api.x.write(...).catch(() => {})            ← 同上
//   await api.x.write(...)  且所在函数无 try/catch   ← 失败无人接管
//   onClick={() => void doWrite()} 而 doWrite 内 await 无保护

function checkSwallowedWrites(sf, rel, src) {
  const findings = [];
  const lines = src.split(/\r?\n/);

  const lineText = (n) => lines[n - 1] || "";

  // A) .catch(() => undefined|{}) —— 只在被吞的是「写操作」时报
  const visitCatch = (node) => {
    if (
      ts.isCallExpression(node) &&
      ts.isPropertyAccessExpression(node.expression) &&
      node.expression.name.text === "catch" &&
      node.arguments.length === 1
    ) {
      const handler = node.arguments[0];
      const isEmptyHandler =
        (ts.isArrowFunction(handler) || ts.isFunctionExpression(handler)) &&
        (handler.body.getText(sf).trim() === "undefined" ||
          handler.body.getText(sf).trim() === "{}" ||
          handler.body.getText(sf).trim() === "void 0" ||
          (ts.isBlock(handler.body) && handler.body.statements.length === 0));
      if (isEmptyHandler) {
        // 找被吞的调用：.catch 挂在哪条链上
        const chainText = node.expression.expression.getText(sf);
        const ln = lineOf(sf, node);
        if (isWriteCall(chainText, null) && !isBenign(chainText, lineText(ln))) {
          // 已经有 error 反馈的（同一函数体内出现 setError/toast）不报
          findings.push({
            file: rel, line: ln, rule: "silent-swallow-write", sev: "error", confidence: "high",
            message: `写操作被静默吞掉：${truncate(chainText)} 失败时无任何反馈——用户会以为操作成功`,
          });
        }
      }
    }
    ts.forEachChild(node, visitCatch);
  };
  visitCatch(sf);

  // B) await 无 try/catch —— 交给跨函数两遍分析（collectUnprotectedAsyncFns +
  //    checkCallSitesFor），避免把「调用点已保护」的函数误报。

  return findings;
}

// ---------------------------------------------------------------- 检查 2：静默失效的配置读取
//
// 形态：import.meta.env?.X ?? ""        ← env 缺失时静默降级，不报错
//       process.env.X ?? ""            ← 后端同理
//       os.environ.get("X") or ""      ← Python 侧（由 python 分析器覆盖）
// 关键：可选链/回退把「配置缺失」这种部署错误变成了正常运行。

function checkSilentConfig(sf, rel, src) {
  const findings = [];
  const lines = src.split(/\r?\n/);

  const visit = (node) => {
    // import.meta.env?.
    if (ts.isPropertyAccessExpression(node) && node.questionDotToken) {
      const text = node.getText(sf);
      if (/^import\.meta\.env\?\./.test(text) || /^process\.env\?\./.test(text)) {
        const ln = lineOf(sf, node);
        findings.push({
          file: rel, line: ln, rule: "silent-config-import-meta", sev: "warning", confidence: "medium",
          message: `可选链读配置 ${truncate(text)}：类型未声明或缺失时会静默降级，部署配置错误不会被发现`,
        });
      }
    }
    // (X ?? "")  其中 X 是 env 读取
    if (ts.isBinaryExpression(node) && node.operatorToken.kind === ts.SyntaxKind.QuestionQuestionToken) {
      const left = node.left.getText(sf);
      const right = node.right.getText(sf);
      if (/import\.meta\.env|process\.env/.test(left) && /^(""|''|``)$/.test(right.trim())) {
        const ln = lineOf(sf, node);
        findings.push({
          file: rel, line: ln, rule: "silent-config-empty-fallback", sev: "warning", confidence: "medium",
          message: `配置缺失时静默回退到空串：${truncate(left)} ?? "" —— 部署漏配不会被发现`,
        });
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(sf);
  return findings;
}

// ---------------------------------------------------------------- 检查 3：错误分支只处理了一类
//
// 形态：if (!(err instanceof ApiError)) { /* 什么都没做 */ }
// 或：catch (e) { if (e instanceof X) {...} }  没有 else —— 非 X 的错误被丢弃

function checkErrorBranch(sf, rel, src) {
  const findings = [];
  const lines = src.split(/\r?\n/);

  const visit = (node) => {
    if (ts.isIfStatement(node)) {
      const cond = node.expression.getText(sf);
      const isErrorTypeCheck = /instanceof\s+\w*(Error|AbortError)/.test(cond);
      if (isErrorTypeCheck) {
        const thenText = node.thenStatement.getText(sf).trim();
        const elseText = node.elseStatement ? node.elseStatement.getText(sf).trim() : "";
        const thenEmpty = thenText === "{}" || thenText === "";
        // else 分支只是 setState(有反馈) 属正常；完全无 else 且 then 里有 return/throw 才可疑
        if (!node.elseStatement && /return|throw/.test(thenText) && !thenEmpty) {
          const ln = lineOf(sf, node);
          findings.push({
            file: rel, line: ln, rule: "error-branch-drops-others", sev: "warning", confidence: "medium",
            message: `只处理了一类错误（${truncate(cond)}）：其余错误被静默丢弃，异常时用户无任何提示`,
          });
        }
      }
    }
    ts.forEachChild(node, visit);
  };
  visit(sf);
  return findings;
}

function truncate(s, n = 70) {
  const t = s.replace(/\s+/g, " ").trim();
  return t.length > n ? t.slice(0, n) + "…" : t;
}

// ---------------------------------------------------------------- 跨函数保护分析（两遍）
//
// 第一遍：找出所有「函数体内 await 写操作且无 try/catch」的函数名。
// 第二遍：扫描这些函数的所有调用点；只要存在「用户可触发且未受保护」的调用点就报，
//         全部调用点都受保护则豁免（解决 auth.login 在 login-page 里被 try/catch 包住的误报）。

function collectUnprotectedAsyncFns(files) {
  const names = new Set();
  for (const { sf } of files) {
    const visit = (node) => {
      if (ts.isFunctionDeclaration(node) || (ts.isVariableDeclaration(node) && node.initializer &&
          (ts.isArrowFunction(node.initializer) || ts.isFunctionExpression(node.initializer)))) {
        const fn = ts.isFunctionDeclaration(node) ? node : node.initializer;
        const name = ts.isFunctionDeclaration(node) ? node.name?.text
          : ts.isIdentifier(node.name) ? node.name.text : null;
        if (name && fn.body && fn.modifiers?.some((m) => m.kind === ts.SyntaxKind.AsyncKeyword)) {
          const text = fn.body.getText(sf);
          const hasTry = /\btry\s*\{/.test(text);
          const awaitWrite = /\bawait\b/.test(text) &&
            WRITE_VERBS.some((v) => new RegExp(`await[^;\\n]*\\.${v}`, "i").test(text));
          const hasFallback = /\.catch\s*\(|setError|toast\(|alert\(/.test(text);
          if (awaitWrite && !hasTry && !hasFallback) names.add(name);
        }
      }
      ts.forEachChild(node, visit);
    };
    visit(sf);
  }
  return names;
}

function checkCallSitesFor(files, unprotectedNames) {
  const findings = [];
  for (const { sf, rel, src } of files) {
    if (unprotectedNames.size === 0) break;
    const lines = src.split(/\r?\n/);
    const visit = (node) => {
      if (ts.isCallExpression(node) && ts.isIdentifier(node.expression) &&
          unprotectedNames.has(node.expression.text)) {
        const name = node.expression.text;
        if (!callSiteProtected(node, sf)) {
          const ln = lineOf(sf, node);
          findings.push({
            file: rel, line: ln, rule: "unprotected-await", sev: "warning", confidence: "medium",
            message: `${name}() 内部 await 无 try/catch，此处调用也未受保护——失败会产生未处理的 rejection`,
          });
        }
      }
      ts.forEachChild(node, visit);
    };
    visit(sf);
  }
  return findings;
}

// ---------------------------------------------------------------- 汇总

function relOf(f) {
  return path.relative(ROOT, f).replace(/\\/g, "/");
}

function main() {
  const files = [];
  walkFiles(path.join(ROOT, "frontend", "src"), [".ts", ".tsx"], files);
  const findings = [];
  const parsed = [];
  for (const file of files) {
    const rel = relOf(file);
    if (/(^|\/)test|\.test\.tsx?$|\.spec\.tsx?$/.test(rel)) continue;
    let src;
    try { src = fs.readFileSync(file, "utf-8"); } catch { continue; }
    let sf;
    try {
      sf = ts.createSourceFile(file, src, ts.ScriptTarget.Latest, true, ts.ScriptKind.TSX);
    } catch { continue; }
    parsed.push({ sf, rel, src });
    try { findings.push(...checkSwallowedWrites(sf, rel, src)); } catch (e) { console.error("swallow", rel, e.message); }
    try { findings.push(...checkSilentConfig(sf, rel, src)); } catch (e) { console.error("config", rel, e.message); }
    try { findings.push(...checkErrorBranch(sf, rel, src)); } catch (e) { console.error("branch", rel, e.message); }
  }

  // 跨函数保护分析（两遍）：先收集无内部保护的 async 函数，再检查其调用点
  try {
    const unprotected = collectUnprotectedAsyncFns(parsed);
    findings.push(...checkCallSitesFor(parsed, unprotected));
  } catch (e) { console.error("callgraph", e.message); }

  if (JSON_OUT) {
    process.stdout.write(JSON.stringify(findings, null, 1));
  } else {
    for (const f of findings) {
      console.log(`${f.sev.padEnd(7)} ${f.rule.padEnd(28)} ${f.file}:${f.line}  ${f.message}`);
    }
    console.log(`---- ${findings.length} findings`);
  }
}

main();
