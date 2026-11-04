"use strict";

const $ = (selector) => document.querySelector(selector);
const pageNames = {overview:"概览", accounts:"账号管理", proxies:"代理管理", keys:"API Keys", playground:"请求测试", requests:"请求记录", logs:"运行日志", settings:"设置"};
let state = null;
let currentPage = "overview";
let toastTimer;
let testOutput = null;
let requestDraft = JSON.stringify({model:"hark", session_id:newSessionId(), messages:[{role:"user", content:"你好，Hark。"}], stream:false}, null, 2);
let freshSecret = "";
let liveOutput = null;
let liveController = null;
let pendingFiles = [];
let reqData = null;
let reqFilter = {failed:false, account:"", key:""};

function newSessionId() {
  if (typeof crypto.randomUUID === "function") return crypto.randomUUID();
  // getRandomValues also works on public HTTP, where randomUUID is absent.
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = Array.from(bytes, byte => byte.toString(16).padStart(2, "0")).join("");
  return `${hex.slice(0,8)}-${hex.slice(8,12)}-${hex.slice(12,16)}-${hex.slice(16,20)}-${hex.slice(20)}`;
}

function esc(value) {
  return String(value ?? "").replace(/[&<>"']/g, c => ({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[c]));
}
function icon(name) { return `<svg aria-hidden="true"><use href="#i-${name}"/></svg>`; }
function date(value, short = false) {
  if (!value) return "尚未检测";
  return new Intl.DateTimeFormat("zh-CN", {timeZone:state?.settings.timezone || "Asia/Shanghai", ...(short ? {hour:"2-digit", minute:"2-digit", second:"2-digit", hour12:false} : {month:"2-digit", day:"2-digit", hour:"2-digit", minute:"2-digit", hour12:false})}).format(new Date(value));
}
function toast(message, error = false) {
  const el = $("#toast");
  el.textContent = message;
  el.className = `toast show${error ? " error" : ""}`;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.remove("show"), 3600);
}
async function api(path, method = "GET", body) {
  const response = await fetch(path, {method, headers:{"Content-Type":"application/json", "X-Hark-Admin":"1"}, ...(body !== undefined ? {body:JSON.stringify(body)} : {})});
  const data = await response.json();
	if(response.status===401&&data.error?.code==="admin_auth_required"){location.replace("/signin.html");throw new Error("需要管理员登录");}
  if (!response.ok) throw new Error(data.error?.message || `请求失败 (${response.status})`);
  return data;
}
async function load(renderPage = true) {
  state = await api("/admin/state");
  $("#workspace-name").textContent = state.settings.name;
  $("#nav-account-count").textContent = state.accounts.length;
  if ($("#nav-proxy-count")) $("#nav-proxy-count").textContent = (state.proxies||[]).length;
  $("#service-status").innerHTML = '<span class="status-dot"></span>管理服务在线';
  $("#footer-status").textContent = state.provider.connected ? "独立 Project · 聊天已实测通过" : "独立 Project · 等待本账号聊天验证";
  if (renderPage) render();
}
function heading(eyebrow, title, subtitle, actions = "") {
  return `<div class="page-heading"><div><div class="eyebrow">${esc(eyebrow)}</div><h1>${esc(title)}</h1><p>${esc(subtitle)}</p></div>${actions ? `<div class="heading-actions">${actions}</div>` : ""}</div>`;
}
function eventRows(events, logs = false) {
  if (!events.length) return '<div class="empty-search">没有匹配的日志</div>';
  return [...events].reverse().map(event => `<div class="activity-row"><span class="event-icon">${icon(event.level === "success" ? "check" : event.level === "warning" ? "clock" : "terminal")}</span><div class="event-content"><strong>${esc(event.action)}</strong><small>${esc(event.detail)}</small></div><span class="event-time">${date(event.time, !logs)}</span></div>`).join("");
}
function exampleCommand(kind = "models") {
  const continuation = " " + String.fromCharCode(92);
  const auth = '  -H "Authorization: Bearer <YOUR_API_KEY>"';
  if (kind === "models") return [`curl ${state.endpoint}/models${continuation}`, auth].join("\n");
  return [`curl ${state.endpoint}/chat/completions${continuation}`, `${auth}${continuation}`, `  -H "Content-Type: application/json"${continuation}`, `  -d '${requestDraft.replace(/'/g, "'\\''")}'`].join("\n");
}
function overview() {
  const accounts = state.accounts.length, keys = state.keys.length;
  return `${heading("YOUR WORKSPACE", "概览", "连接、管理与测试，在一个工作空间里完成。", `<button class="btn subtle" data-action="refresh">${icon("refresh")}刷新状态</button><button class="btn primary" data-action="create-key">${icon("plus")}创建 API Key</button>`)}
    <section class="hero"><div class="hero-main"><div class="hero-label"><span class="status-dot"></span>HARK · LOCAL GATEWAY</div><h2>让 Hark，<em>连接你的工具。</em></h2><p>管理你的账号与 API 入口，从这里开始搭建 Hark 工作流。<br>API 会话使用隔离的 Hark Project，并返回关联的完整文字回复。</p><div class="hero-actions"><button class="btn primary" data-action="import-account">配置 Hark 账号${icon("arrow")}</button><a class="text-link" href="#playground">打开请求测试${icon("arrow")}</a></div></div><div class="hero-art" aria-hidden="true"><div class="orbit"></div><div class="orbit two"></div><div class="orbit three"></div><div class="star-core">✦</div><div class="art-tag">ONE GATEWAY. YOUR TOOLS.</div></div></section>
    <section class="stats" aria-label="运行状态"><div class="stat"><div class="stat-top">Hark 账号${icon("user")}</div><div class="stat-number">${accounts}<small>个账号</small></div><div class="stat-caption">${state.accounts.filter(a=>a.status === "valid").length} 个会话检测有效</div></div><div class="stat"><div class="stat-top">API Keys${icon("key")}</div><div class="stat-number">${keys}<small>个可用 Key</small></div><div class="stat-caption">客户端访问凭据 · 本地保存</div></div><div class="stat"><div class="stat-top">上游聊天${icon("link")}</div><div class="stat-pill"><span class="${state.provider.connected ? "status-dot" : "amber-dot"}"></span>${state.provider.connected ? "已接通" : "待本账号实测"}</div><div class="stat-caption">独立 Project · ${esc(state.provider.status)}</div></div></section>
    <div class="overview-grid"><section class="panel"><div class="panel-head"><div><div class="panel-title">${icon("terminal")}接入你的应用</div><div class="panel-sub">OpenAI 兼容入口</div></div><span class="badge">本地</span></div><div class="panel-body"><div class="endpoint-label">BASE URL</div><div class="endpoint-box"><code>${esc(state.endpoint)}</code><button class="icon-btn" data-action="copy-endpoint" aria-label="复制 API 地址">${icon("copy")}</button></div><div class="code-toolbar"><span>cURL · 验证 API Key</span><button class="icon-btn" data-action="copy-models" aria-label="复制模型查询命令">${icon("copy")}</button></div><pre class="code-block">${esc(exampleCommand())}</pre><div class="integration-foot">${icon("clock")}使用 API Key 调用聊天接口，同一 Key 与会话 ID 复用 Project，闲置后归档。</div></div></section>
    <section class="panel"><div class="panel-head"><div><div class="panel-title">快速开始</div><div class="panel-sub">三个步骤，准备你的工作空间</div></div><span class="badge">${Number(accounts > 0) + Number(keys > 0) + Number(state.provider.connected)} / 3</span></div><div class="panel-body"><div class="setup-item"><span class="step-number ${accounts ? "done" : ""}">${accounts ? icon("check") : "1"}</span><div class="setup-text"><strong>添加 Hark 账号</strong><p>粘贴 Hark 登录会话 Cookie。</p></div><button class="text-link" data-action="import-account">${accounts ? "添加" : "配置"}${icon("arrow")}</button></div><div class="setup-item"><span class="step-number ${keys ? "done" : ""}">${keys ? icon("check") : "2"}</span><div class="setup-text"><strong>创建客户端 Key</strong><p>为你的应用生成独立的访问凭据。</p></div><button class="text-link" data-action="create-key">创建${icon("arrow")}</button></div><div class="setup-item"><span class="step-number ${state.provider.connected ? "done" : ""}">${state.provider.connected ? icon("check") : "3"}</span><div class="setup-text"><strong>验证聊天链路</strong><p>用 API Key 测试自动创建 Project 并获取回复。</p></div><a class="text-link" href="#playground">测试${icon("arrow")}</a></div></div></section></div>
    <section class="panel recent-panel"><div class="panel-head"><div class="panel-title">最近活动</div><a class="text-link" href="#logs">查看全部${icon("arrow")}</a></div><div class="panel-body">${eventRows(state.events.slice(-3))}</div></section>`;
}
const accountStatus = {unchecked:["未检测",""], valid:["会话有效",""], expired:["会话失效","red"], blocked:["访问受阻","amber"], limited:["检测限流","amber"], error:["检测失败","red"]};
function accounts() {
  return `${heading("CONNECTIONS", "账号管理", "管理 Hark 登录会话，查看每个账号的连接状态。", `${state.accounts.length ? `<button class="btn" data-action="batch-proxy">${icon("globe")}批量设置代理</button>` : ""}${state.accounts_portable !== false ? `<button class="btn" data-action="export-accounts">${icon("arrow")}导出账号</button><button class="btn" data-action="export-accounts" data-format="sub2api" title="导出为 sub2api 账号导入文件（Hark 平台）">${icon("arrow")}导出到 sub2api</button>` : ""}<button class="btn primary" data-action="import-account">${icon("plus")}导入账号</button>`)}
    ${state.accounts.length ? `<div class="account-grid">${state.accounts.map(a => { const badge = accountStatus[a.status] || accountStatus.error; return `<article class="account-card"><div class="account-top"><span class="account-avatar">${esc(a.name.slice(0,1).toUpperCase())}</span><div><strong>${esc(a.name)}</strong><small>Hark · ${a.proxy ? `代理 ${esc(a.proxy)}` : "已保存"}</small></div><span class="badge ${badge[1]}">${badge[0]}</span></div><div class="account-proxy"><label>${icon("globe")}代理</label>${proxySelect(a)}</div><div class="account-detail">${esc(a.detail)}<br><span class="muted">最后检测：${date(a.checked)}</span><br><span class="muted">${usageText(a)} · 进行中 ${Number(a.in_flight||0)}</span></div><div class="account-bottom"><button class="btn small" data-action="check-account" data-id="${esc(a.id)}">${icon("refresh")}检测会话</button><button class="btn small" data-action="refresh-usage" data-id="${esc(a.id)}">${icon("clock")}刷新额度</button><button class="text-link" data-action="delete-account" data-id="${esc(a.id)}">移除账号</button></div></article>`; }).join("")}</div>` : `<section class="panel empty-state"><div class="empty-icon">${icon("user")}</div><h2>连接你的第一个 Hark 账号</h2><p>输入账号名称并粘贴 Cookie，导入后检测会话状态。</p><button class="btn primary" data-action="import-account">${icon("plus")}导入账号</button></section>`}
    <div class="info-strip">${icon("clock")}<div>Cookie 已加密保存在本机，服务重启后自动恢复。API 会话分别使用 Project，并要求 Hark 只使用当前 Project 和客户端提供的上下文，不读取其他 Project 或全局记忆。这是行为约束，不是账号权限隔离。</div></div>`;
}
function usageText(a) {
  const u = a.usage;
  if (!u) return "额度：未查询";
  if (u.error) return `额度：查询失败（${u.error}）`;
  return `额度：今日 ${u.daily_used}/${u.daily_limit} · 本期 ${u.pool_used}/${u.pool_limit}${u.plan ? ` · ${u.plan}` : ""}`;
}
function attachmentChips() {
  if (!pendingFiles.length) return '<small class="muted">可选：图片会作为 image_url，其他文件作为 file 内容块附加到最后一条 user 消息。</small>';
  return pendingFiles.map((f, i) => `<span class="badge">${esc(f.name)} · ${(f.size/1024).toFixed(1)} KB <button type="button" class="text-link" data-action="remove-file" data-id="${i}" aria-label="移除附件">×</button></span>`).join(" ");
}
function draftSession() {
  try { return JSON.parse(requestDraft).session_id || ""; } catch { return ""; }
}
function withAttachments(data) {
  if (!pendingFiles.length || !Array.isArray(data.messages)) return data;
  const index = data.messages.map(m => m.role).lastIndexOf("user");
  if (index < 0) throw new Error("附件需要至少一条 user 消息");
  const message = {...data.messages[index]};
  let parts = typeof message.content === "string" ? [{type:"text", text:message.content}] : Array.isArray(message.content) ? [...message.content] : [];
  for (const f of pendingFiles) {
    parts.push(f.type.startsWith("image/") ? {type:"image_url", image_url:{url:f.dataURL}} : {type:"file", file:{filename:f.name, file_data:f.dataURL}});
  }
  message.content = parts;
  const messages = [...data.messages];
  messages[index] = message;
  return {...data, messages};
}
function proxyOptions(sel) {
  const opts=[["","默认（"+(state.accounts_external?"跟随 Hub 线路":"账号自带 / 全局")+"）"],["direct","直连"]].concat((state.proxies||[]).map(p=>[p.id,`${p.name} · ${p.display}${p.status==="error"?" ⚠":""}`]));
  return opts.map(([v,t])=>`<option value="${esc(v)}"${v===(sel||"")?" selected":""}>${esc(t)}</option>`).join("");
}
function proxySelect(a) { return `<select class="proxy-select" data-account="${esc(a.id)}">${proxyOptions(a.proxy_id)}</select>`; }
function proxies() {
  const list=state.proxies||[];
  const st=p=>p.status==="ok"?`<span class="badge">可用</span>`:p.status==="error"?`<span class="badge red" title="${esc(p.error||"")}">失败</span>`:`<span class="badge">未测试</span>`;
  return `${heading("NETWORK", "代理管理", "添加 HTTP / SOCKS5 代理，测试出口 IP 与延迟，再分配给账号。分配的代理只用于本网关访问 Hark，优先于账号自带代理"+(state.accounts_external?"（Hub 线路）":"")+"。", `${list.length?`<button class="btn" data-action="test-all-proxies">${icon("refresh")}全部测试</button>`:""}<button class="btn primary" data-action="add-proxy">${icon("plus")}添加代理</button>`)}
  ${list.length ? `<section class="panel table-wrap"><table><thead><tr><th>名称</th><th>地址</th><th>状态</th><th>出口 IP</th><th>延迟</th><th>账号</th><th>操作</th></tr></thead><tbody>${list.map(p=>`<tr><td><span class="key-name">${icon("globe")}${esc(p.name)}</span></td><td><code>${esc(p.display)}</code>${p.auth?` <span class="muted">需认证</span>`:""}</td><td>${st(p)}</td><td>${esc(p.exit_ip||"—")}${p.location?` <span class="muted">${esc(p.location)}</span>`:""}</td><td>${p.latency_ms?`${p.latency_ms} ms`:"—"}</td><td>${p.used||0}</td><td><button class="btn small" data-action="test-proxy" data-id="${esc(p.id)}">测试</button> <button class="btn small" data-action="edit-proxy" data-id="${esc(p.id)}">编辑</button> <button class="btn small danger" data-action="delete-proxy" data-id="${esc(p.id)}">删除</button></td></tr>`).join("")}</tbody></table></section>` : `<section class="panel empty-state"><div class="empty-icon">${icon("globe")}</div><h2>还没有代理</h2><p>一次可以粘贴多行，每行一个代理。</p><button class="btn primary" data-action="add-proxy">${icon("plus")}添加代理</button></section>`}
  <div class="info-strip">${icon("check")}支持 http://user:pass@host:port、socks5://host:port、host:port、host:port:user:pass。测试会经代理访问 hark.com。</div>`;
}
function addProxy() {
  modal("添加代理", "每行一个，支持批量粘贴。", `<form id="proxy-form"><div class="field"><label for="proxy-name">名称（可选）</label><input id="proxy-name" name="name" maxlength="40" placeholder="留空用 主机:端口"></div><div class="field"><label for="proxy-lines">代理地址</label><textarea id="proxy-lines" name="lines" rows="6" spellcheck="false" autocomplete="off" placeholder="http://user:pass@1.2.3.4:8080\nsocks5://5.6.7.8:1080\n9.9.9.9:3128:user:pass" required></textarea></div><div class="modal-error" role="alert"></div><div class="form-actions"><button type="button" class="btn subtle" data-action="close-modal">取消</button><button class="btn primary" type="submit">${icon("plus")}添加</button></div></form>`);
}
function batchProxy() {
  modal("批量设置代理", "勾选账号，选择要使用的代理。", `<form id="batch-proxy-form"><div class="field"><label>代理</label><select name="proxy">${proxyOptions("")}</select></div><div class="field"><label><input type="checkbox" data-action="toggle-all-accounts"> 全选</label><div class="batch-list">${state.accounts.map(a=>`<label class="batch-item"><input type="checkbox" name="acct" value="${esc(a.id)}"> ${esc(a.name)} <span class="muted">${esc(a.proxy||"")}</span></label>`).join("")}</div></div><div class="modal-error" role="alert"></div><div class="form-actions"><button type="button" class="btn subtle" data-action="close-modal">取消</button><button class="btn primary" type="submit">应用</button></div></form>`);
}
function keys() {
  return `${heading("ACCESS CONTROL", "API Keys", "为每个应用创建独立的访问凭据。完整 Key 仅在创建时显示。", `<button class="btn primary" data-action="create-key">${icon("plus")}创建 API Key</button>`)}
  ${state.keys.length ? `<section class="panel table-wrap"><table><thead><tr><th>名称</th><th>KEY</th><th>创建时间</th><th>状态</th><th>操作</th></tr></thead><tbody>${state.keys.map(k=>`<tr><td><span class="key-name">${icon("key")}${esc(k.name)}</span></td><td><code>${esc(k.prefix)}••••••••</code></td><td class="muted">${date(k.created)}</td><td><span class="badge"><span class="status-dot"></span>可用</span></td><td><button class="btn small danger" data-action="delete-key" data-id="${esc(k.id)}">撤销</button></td></tr>`).join("")}</tbody></table></section>` : `<section class="panel empty-state"><div class="empty-icon">${icon("key")}</div><h2>一个 Key，一个应用入口</h2><p>为客户端生成 API Key，用它查询模型列表。<br>聊天接口沿用相同凭据。</p><button class="btn primary" data-action="create-key">${icon("plus")}创建 API Key</button></section>`}
  <div class="info-strip">${icon("check")}本地仅保存 Key 的哈希值。完整 Key 关闭创建窗口后不再展示，请及时复制。</div>`;
}
function playground() {
  const busy = liveController !== null;
  const result = liveOutput ? `<div class="output-result" aria-live="polite"><div class="live-result-meta"><span class="badge ${liveOutput.error ? "red" : ""}">${liveOutput.error ? "请求失败" : "已收到回复"}</span><span>${liveOutput.status ? `HTTP ${liveOutput.status} · ` : ""}${liveOutput.elapsed} 秒${liveOutput.mode ? ` · ${esc(liveOutput.mode)}` : ""}</span>${liveOutput.content ? `<button class="icon-btn" data-action="copy-reply" title="复制回复" aria-label="复制回复">${icon("copy")}</button>` : ""}</div>${liveOutput.error ? `<p class="live-error">${esc(liveOutput.error)}</p>` : `<div class="live-reply" id="live-reply">${esc(liveOutput.content)}</div>`}${liveOutput.raw ? `<details class="live-raw"><summary>原始响应</summary><pre>${esc(liveOutput.raw)}</pre></details>` : ""}</div>` : `<div class="output-empty" role="status">${icon(busy ? "clock" : "terminal")}${busy ? "正在等待 Hark 回复…" : "暂无回复"}</div>`;
  return `${heading("PLAYGROUND", "请求测试", "Hark · Project", `<button class="btn subtle" data-action="copy-chat">${icon("copy")}复制调用示例</button>`)}
  <div class="playground-grid"><section class="panel"><div class="panel-head"><div class="panel-title">请求内容</div><span class="badge">POST · Hark Project</span></div><form id="test-form" class="panel-body"><div class="field"><label for="request-editor">JSON 请求</label><textarea id="request-editor" class="editor" spellcheck="false" required ${busy ? "disabled" : ""}>${esc(requestDraft)}</textarea></div><div class="field"><label for="attach-input">附件（图片 / 文件）</label><input type="file" id="attach-input" multiple ${busy ? "disabled" : ""}><div class="attach-list">${attachmentChips()}</div></div><div class="form-actions playground-actions"><button type="button" class="btn subtle" data-action="new-conversation" ${busy ? "disabled" : ""}>${icon("plus")}新开对话</button><button type="button" class="btn subtle" data-action="reset-request" ${busy ? "disabled" : ""}>重置</button><button type="submit" name="operation" value="validate" class="btn" ${busy ? "disabled" : ""}>${icon("check")}校验格式</button>${busy ? `<button type="button" class="btn danger" data-action="cancel-live">${icon("close")}停止等待</button>` : `<button type="submit" name="operation" value="send" class="btn primary">${icon("arrow")}发送到 Hark</button>`}</div></form>${testOutput ? `<div class="validation-result"><span class="badge ${testOutput.valid ? "" : "red"}">${testOutput.valid ? "格式通过" : "格式错误"}</span><pre>${esc(JSON.stringify(testOutput,null,2))}</pre></div>` : ""}</section><section class="panel live-response-panel" aria-busy="${busy}"><div class="panel-head"><div class="panel-title">Hark 回复</div><span class="badge">${busy ? "等待中" : "Project"}</span></div>${result}</section></div>`;
}

async function sendLiveRequest(data) {
  if (liveController) return;
  const controller = new AbortController();
  liveController = controller;
  liveOutput = null;
  render();
  const started = performance.now();
  let status;
  try {
    const response = await fetch("/admin/playground/chat", {method:"POST", headers:{"Content-Type":"application/json", "X-Hark-Admin":"1"}, body:JSON.stringify(withAttachments(data)), signal:controller.signal});
    status = response.status;
    const raw = await response.text();
    let content = "";
    if (!response.ok) {
      let message = `请求失败 (HTTP ${status})`;
      try { const body = JSON.parse(raw); message = body.error?.message || message; } catch {}
      throw new Error(message);
    }
    if (response.headers.get("Content-Type")?.includes("text/event-stream")) {
      let done = false;
      for (const line of raw.split(/\r?\n/)) {
        if (!line.startsWith("data: ")) continue;
        const chunk = line.slice(6);
        if (chunk === "[DONE]") { done = true; continue; }
        const event = JSON.parse(chunk);
        if (event.error) throw new Error(event.error.message || "Hark 回复失败");
        content += event.choices?.[0]?.delta?.content || "";
      }
      if (!done) throw new Error("回复流未完整结束");
    } else {
      content = JSON.parse(raw).choices?.[0]?.message?.content;
    }
    if (typeof content !== "string" || !content.trim()) throw new Error("Hark 返回空回复");
    pendingFiles = [];
    const reused = response.headers.get("X-Hark-Project-Reused") === "true";
    liveOutput = {content, raw, status, mode:`${response.headers.get("X-Hark-Session-Mode") || ""}${reused ? " · 复用会话" : " · 新会话"}`, elapsed:((performance.now()-started)/1000).toFixed(1)};
  } catch (error) {
    liveOutput = {error:controller.signal.aborted ? "已停止等待；Hark 可能已受理请求，未自动重发。" : error.message || "连接中断；请求可能已受理，未自动重发。", status, elapsed:((performance.now()-started)/1000).toFixed(1)};
  } finally {
    liveController = null;
    render();
  }
  try { await load(false); } catch {}
}
const reqCodeText = {invalid_api_key:"Key 无效", no_valid_account:"没有可用账号", quota_exhausted:"额度用尽", session_account_unavailable:"会话账号不可用", client_closed:"客户端断开", upstream_timeout:"上游超时", timeout:"超时"};
function reqStatus(l) {
  if (l.status >= 400 || l.code) return `<span class="badge red" title="${esc(l.code)}">${esc(l.status)} ${esc(reqCodeText[l.code] || l.code || "失败")}</span>`;
  return `<span class="badge"><span class="status-dot"></span>${esc(l.status)}</span>`;
}
function ms(v) { return v >= 1000 ? `${(v/1000).toFixed(1)} s` : `${v} ms`; }
function requests() {
  const st = (reqData && reqData.stats) || state.request_stats || {total:0, ok:0, failed:0, last_hour:0, avg_ms:0, in_progress:0};
  const list = reqData ? reqData.requests : null;
  const accts = state.accounts.map(a => `<option value="${esc(a.id)}" ${reqFilter.account === a.id ? "selected" : ""}>${esc(a.name)}</option>`).join("");
  const keyNames = [...new Set(state.keys.map(k => k.name).concat("控制台测试"))].map(n => `<option value="${esc(n)}" ${reqFilter.key === n ? "selected" : ""}>${esc(n)}</option>`).join("");
  const rows = list === null ? '<tr><td colspan="8" class="muted">正在读取…</td></tr>' : !list.length ? '<tr><td colspan="8" class="muted">还没有请求记录</td></tr>' :
    list.map(l => `<tr><td class="muted">${date(l.time, true)}</td><td>${esc(l.key || "—")}</td><td>${esc(l.path.replace("/v1/","").replace("/admin/playground/","控制台/"))}${l.stream ? ' <span class="badge">流式</span>' : ""}</td><td>${esc(l.account || "—")}${l.reused ? ' <span class="muted">复用</span>' : ""}</td><td>${l.attempts || 0}${l.skipped && l.skipped.length ? ` <span class="muted" title="${esc(l.skipped.join("\n"))}">跳过 ${l.skipped.length}</span>` : ""}</td><td>${reqStatus(l)}</td><td>${ms(l.duration_ms)}</td><td class="muted">${l.reply_chars ? l.reply_chars + " 字" : ""}${l.files ? ` · ${l.files} 附件` : ""}</td></tr>`).join("");
  return `${heading("TRAFFIC", "请求记录", "每次 API 调用用了哪个 Key、哪个账号、试了几次、结果如何。只保存在内存，最多最近 500 条，不含消息内容。", `<button class="btn subtle" data-action="clear-requests">清空</button><button class="btn" data-action="refresh-requests">${icon("refresh")}刷新</button>`)}
  <section class="stats" aria-label="请求统计"><div class="stat"><div class="stat-top">请求${icon("list")}</div><div class="stat-number">${st.total}<small>条</small></div><div class="stat-caption">最近 1 小时 ${st.last_hour} 条 · 进行中 ${st.in_progress}</div></div><div class="stat"><div class="stat-top">成功${icon("check")}</div><div class="stat-number">${st.ok}<small>条</small></div><div class="stat-caption">${st.total ? Math.round(st.ok*100/st.total) : 0}% 成功率</div></div><div class="stat"><div class="stat-top">失败${icon("close")}</div><div class="stat-number">${st.failed}<small>条</small></div><div class="stat-caption">平均耗时 ${ms(st.avg_ms)}</div></div></section>
  <div class="log-toolbar"><select id="req-account" aria-label="按账号筛选"><option value="">全部账号</option>${accts}</select><select id="req-key" aria-label="按 Key 筛选"><option value="">全部 Key</option>${keyNames}</select><label class="muted"><input type="checkbox" id="req-failed" ${reqFilter.failed ? "checked" : ""}> 只看失败</label></div>
  <section class="panel table-wrap"><table><thead><tr><th>时间</th><th>KEY</th><th>接口</th><th>账号</th><th>尝试</th><th>结果</th><th>耗时</th><th>回复</th></tr></thead><tbody>${rows}</tbody></table></section>`;
}
async function loadRequests() {
  const q = new URLSearchParams({limit:"200"});
  if (reqFilter.failed) q.set("failed","1");
  if (reqFilter.account) q.set("account",reqFilter.account);
  if (reqFilter.key) q.set("key",reqFilter.key);
  reqData = await api(`/admin/requests?${q}`);
  if (currentPage === "requests") $("#page-content").innerHTML = requests();
}
function logs() {
  return `${heading("ACTIVITY", "运行日志", "查看本次运行的操作记录。最多保留最近 200 条。")}
  <div class="log-toolbar"><input class="search-input" id="log-search" placeholder="搜索操作或详情…" aria-label="搜索日志"><button class="btn small" data-action="export-logs">${icon("list")}导出日志</button><button class="btn small" data-action="refresh">${icon("refresh")}刷新</button></div><section class="panel logs-list" id="log-rows">${eventRows(state.events,true)}</section>`;
}
function settings() {
  return `${heading("PREFERENCES", "设置", "调整你的本地工作空间配置。")}
  <div class="settings-grid"><form id="settings-form" class="panel form-panel"><h2>工作空间</h2><p>名称与时区用于管理界面；请求超时限制等待 Project 回复的时间。</p><div class="field"><label for="setting-name">工作空间名称</label><input id="setting-name" name="name" value="${esc(state.settings.name)}" maxlength="40" required></div><div class="field"><label for="setting-zone">时区</label><input id="setting-zone" name="timezone" list="setting-zone-list" value="${esc(state.settings.timezone)}" maxlength="64" required autocomplete="off" spellcheck="false"><datalist id="setting-zone-list">${["Asia/Shanghai","Asia/Hong_Kong","Asia/Taipei","Asia/Tokyo","Asia/Seoul","Asia/Singapore","UTC","Europe/London","Europe/Paris","Europe/Berlin","Europe/Amsterdam","America/New_York","America/Chicago","America/Denver","America/Los_Angeles","America/Phoenix","America/Toronto","Australia/Sydney"].map(z=>`<option value="${z}"></option>`).join("")}</datalist><small>可手填任意 IANA 时区（如 America/Los_Angeles）。界面时间按该时区显示；账号未单独指定时区时，新建 Project 也用它。</small></div><div class="field"><label for="setting-timeout">请求超时（秒）</label><input id="setting-timeout" name="timeout" type="number" min="30" max="600" value="${state.settings.timeout}" required><small>30–600 秒；当前会话检测固定最多等待 20 秒。</small></div><div class="form-actions"><button class="btn primary" type="submit">${icon("check")}保存配置</button></div></form>
  <section class="panel form-panel"><h2>服务信息</h2><p>管理服务与上游连接状态分别显示。</p><div class="detail-row"><span>版本</span><strong>0.2.0 · ${state.remote ? "Server" : "Local"} mode</strong></div><div class="detail-row"><span>单个附件上限</span><strong>${(state.max_upload_bytes/1048576).toFixed(0)} MiB</strong></div><div class="detail-row"><span>每账号并发</span><strong>${state.account_concurrency}</strong></div><div class="detail-row"><span>管理服务</span><strong>运行中</strong></div><div class="detail-row"><span>上游服务</span><strong>Hark</strong></div><div class="detail-row"><span>模型别名</span><strong>hark</strong></div><div class="detail-row"><span>启动时间</span><strong>${date(state.started)}</strong></div><div class="detail-row"><span>API 地址</span><strong class="value-wrap">${esc(state.endpoint)}</strong></div><div class="upstream-placeholder"><strong>Project 聊天链路</strong><p>同一 Key 与会话 ID 复用同一个 Project（只转发新增消息），结束、点击“新开对话”或闲置后归档；图片与文件通过 Hark 附件上传接口发送。</p><a class="text-link" href="https://hark.com/using-hark" target="_blank" rel="noopener noreferrer">查看 Hark 使用说明${icon("external")}</a></div></section></div>`;
}
function render() {
  if (!state) return;
  currentPage = location.hash.slice(1);
  if (!pageNames[currentPage]) currentPage = "overview";
  $("#breadcrumb-page").textContent = pageNames[currentPage];
  document.title = `Hark2API · ${pageNames[currentPage]}`;
  document.querySelectorAll(".nav-item").forEach(link => { const active=link.dataset.page === currentPage; link.classList.toggle("active",active); if (active) link.setAttribute("aria-current","page"); else link.removeAttribute("aria-current"); });
  $("#page-content").innerHTML = ({overview, accounts, proxies, keys, playground, requests, logs, settings})[currentPage]();
  if (currentPage === "requests" && !reqData) loadRequests().catch(e => toast(e.message, true));
  if (currentPage === "accounts") {
    document.querySelectorAll(".account-bottom").forEach(row => {
      const id=row.querySelector('[data-action="check-account"]').dataset.id;
      row.insertAdjacentHTML("beforeend", `<button class="icon-btn" data-action="probe-chat" data-id="${esc(id)}" title="诊断聊天链路" aria-label="诊断聊天链路">${icon("terminal")}</button>`);
    });
  }
}
function modal(title, description, body) {
  $("#modal-content").innerHTML = `<div class="modal-header"><div><h2>${esc(title)}</h2><p>${esc(description)}</p></div><button class="icon-btn" data-action="close-modal" aria-label="关闭窗口">${icon("close")}</button></div><div class="modal-body">${body}</div>`;
  if (!$("#modal").open) $("#modal").showModal();
  const first = $("#modal input, #modal textarea");
  if (first) first.focus();
}
function importAccount() {
  const fileForm = `<form id="account-file-form"><div class="field"><label for="account-file">从导出文件导入</label><input id="account-file" name="file" type="file" accept=".json,application/json" required><small>选择「导出账号」得到的 JSON 文件，一次导入全部账号；同名账号会更新登录。</small></div><div class="modal-error" role="alert"></div><div class="form-actions"><button type="button" class="btn subtle" data-action="close-modal">取消</button><button class="btn primary" type="submit">${icon("plus")}导入文件</button></div></form>`;
  if (state.accounts_external) { modal("导入 Hark 账号", "导入后会在 Hark Hub 里建好账号并写入登录。", fileForm); return; }
  modal("导入 Hark 账号", "粘贴已登录 Hark 账号的 Cookie。", `<form id="account-form"><div class="field"><label for="account-name">账号名称</label><input id="account-name" name="name" placeholder="例如：我的 Hark" maxlength="40" required></div><div class="field"><label for="account-cookie">Cookie</label><textarea id="account-cookie" name="cookie" rows="4" placeholder="name=value; name=value" autocomplete="off" spellcheck="false" required></textarea><small>填写 Cookie 请求头的值，保持单行；导入后可检测会话。</small></div><div class="field"><label for="account-proxy">账号代理（可选）</label><input id="account-proxy" name="proxy" placeholder="http://127.0.0.1:7897 或 socks5://user:pass@host:1080" autocomplete="off" spellcheck="false"><small>仅用于该账号访问 Hark；留空则使用全局上游代理或直连。</small></div><div class="modal-error" role="alert"></div><div class="form-actions"><button type="button" class="btn subtle" data-action="close-modal">取消</button><button class="btn primary" type="submit">${icon("plus")}导入 Cookie</button></div></form><hr class="modal-sep">${fileForm}`);
}
function createKey() {
  modal("创建 API Key", "为你的客户端生成一个独立的访问凭据。", `<form id="key-form"><div class="field"><label for="key-name">名称</label><input id="key-name" name="name" placeholder="例如：Cherry Studio" maxlength="40" required></div><div class="modal-error" role="alert"></div><div class="form-actions"><button type="button" class="btn subtle" data-action="close-modal">取消</button><button class="btn primary" type="submit">生成 Key${icon("arrow")}</button></div></form>`);
}
async function copy(value) {
  try {
    if(navigator.clipboard?.writeText) await navigator.clipboard.writeText(value);
    else {
      const active=document.activeElement;
      const field=document.createElement("textarea");
      field.value=value;field.style.position="fixed";field.style.left="-9999px";
      document.body.append(field);field.select();
      let copied;
      try { copied=document.execCommand("copy"); }
      finally { field.remove();active?.focus(); }
      if(!copied)throw new Error("copy failed");
    }
    toast("已复制到剪贴板");
  }
  catch { toast("复制失败，请手动选择内容复制",true); }
}
document.addEventListener("click", async event => {
  const button = event.target.closest("[data-action]");
  if (!button || button.disabled) return;
  const action=button.dataset.action, id=button.dataset.id;
  try {
    switch (action) {
      case "create-key": createKey(); break;
      case "import-account": importAccount(); break;
      case "close-modal": $("#modal").close(); break;
      case "copy-endpoint": await copy(state.endpoint); break;
      case "copy-models": await copy(exampleCommand()); break;
      case "copy-chat": if ($("#request-editor")) requestDraft=$("#request-editor").value; await copy(exampleCommand("chat")); break;
      case "copy-secret": await copy(freshSecret); break;
      case "copy-reply": await copy(liveOutput?.content || ""); break;
      case "cancel-live": liveController?.abort(); break;
      case "refresh": button.disabled=true; await load(); toast("状态已刷新"); break;
      case "refresh-usage": button.disabled=true; await api(`/admin/accounts/${encodeURIComponent(id)}/usage`,"POST",{}); await load(); toast("额度已刷新"); break;
      case "remove-file": pendingFiles.splice(Number(id),1); render(); break;
      case "new-conversation": {
        if (liveController) break;
        if ($("#request-editor")) requestDraft=$("#request-editor").value;
        const old=draftSession();
        if (old) await api(`/admin/sessions/${encodeURIComponent(old)}/end`,"POST",{});
        let data; try { data=JSON.parse(requestDraft); } catch { data={model:"hark", messages:[{role:"user", content:""}], stream:false}; }
        data.session_id=newSessionId();
        requestDraft=JSON.stringify(data,null,2);testOutput=null;liveOutput=null;pendingFiles=[];render();toast("已开始新对话，下次发送将新建 Project");
        break;
      }
      case "check-account": button.disabled=true; button.textContent="正在检测…"; await api(`/admin/accounts/${encodeURIComponent(id)}/check`,"POST",{}); await load(); toast("会话检测完成"); break;
      case "probe-chat": {
        button.disabled=true;
        const result=await api(`/admin/accounts/${encodeURIComponent(id)}/chat-probe`,"POST",{});
        modal("聊天链路诊断", "", `<pre class="code-block">${esc(JSON.stringify(result,null,2))}</pre><div class="form-actions"><button class="btn" data-action="close-modal">关闭</button></div>`);
        break;
      }
      case "delete-key":
      case "delete-account": {
        const isKey=action === "delete-key";
        modal(isKey ? "撤销 API Key？" : "移除账号？", isKey ? "撤销后，使用该 Key 的客户端将失去访问权限。" : "仅移除本地导入的会话，不会注销 Hark 账号。", `<div class="modal-error" role="alert"></div><div class="form-actions"><button class="btn subtle" data-action="close-modal">取消</button><button class="btn danger" data-action="confirm-delete" data-kind="${isKey ? "keys" : "accounts"}" data-id="${esc(id)}">确认${isKey ? "撤销" : "移除"}</button></div>`);
        break;
      }
      case "confirm-delete": button.disabled=true; await api(`/admin/${button.dataset.kind}/${encodeURIComponent(id)}`,"DELETE"); $("#modal").close(); await load(); toast("已移除"); break;
      case "reset-request": if(liveController) break;requestDraft=JSON.stringify({model:"hark",session_id:newSessionId(),messages:[{role:"user",content:"你好，Hark。"}],stream:false},null,2);testOutput=null;liveOutput=null;render(); break;
      case "refresh-requests": button.disabled=true; await loadRequests(); toast("请求记录已刷新"); break;
      case "clear-requests": button.disabled=true; await api("/admin/requests","DELETE"); await loadRequests(); toast("请求记录已清空"); break;
      case "add-proxy": addProxy(); break;
      case "batch-proxy": batchProxy(); break;
      case "toggle-all-accounts": document.querySelectorAll('#batch-proxy-form input[name="acct"]').forEach(c=>c.checked=button.checked); break;
      case "test-proxy": { button.disabled=true; button.textContent="测试中…"; const r=await api(`/admin/proxies/${encodeURIComponent(id)}/test`,"POST",{}); await load(); toast(r.status==="ok"?`可用：${r.exit_ip} ${r.location||""} · ${r.latency_ms} ms`:`失败：${r.error}`, r.status!=="ok"); break; }
      case "test-all-proxies": { button.disabled=true; button.textContent="测试中…"; const rs=await Promise.all((state.proxies||[]).map(p=>api(`/admin/proxies/${encodeURIComponent(p.id)}/test`,"POST",{}).catch(()=>({status:"error"})))); await load(); toast(`测试完成：可用 ${rs.filter(r=>r.status==="ok").length} / ${rs.length}`); break; }
      case "edit-proxy": { const p=(state.proxies||[]).find(x=>x.id===id); if(!p) break; modal("编辑代理", "地址留空则不修改（原地址含的密码不显示）。", `<form id="proxy-edit-form" data-id="${esc(id)}"><div class="field"><label>名称</label><input name="name" maxlength="40" value="${esc(p.name)}" required></div><div class="field"><label>新地址（可选）</label><input name="url" spellcheck="false" autocomplete="off" placeholder="${esc(p.display)}"></div><div class="modal-error" role="alert"></div><div class="form-actions"><button type="button" class="btn subtle" data-action="close-modal">取消</button><button class="btn primary" type="submit">保存</button></div></form>`); break; }
      case "delete-proxy": modal("删除代理？", "已分配这个代理的账号会改回默认。", `<div class="modal-error" role="alert"></div><div class="form-actions"><button class="btn subtle" data-action="close-modal">取消</button><button class="btn danger" data-action="confirm-delete" data-kind="proxies" data-id="${esc(id)}">确认删除</button></div>`); break;
      case "export-accounts": {
        button.disabled=true;
        const fmt=button.dataset.format||"";
        const response=await fetch("/admin/accounts/export"+(fmt?`?format=${encodeURIComponent(fmt)}`:""),{headers:{"X-Hark-Admin":"1"}});
        const text=await response.text();
        if(!response.ok){let msg=`导出失败 (${response.status})`;try{msg=JSON.parse(text).error?.message||msg;}catch{}throw new Error(msg);}
        const count=(JSON.parse(text).accounts||[]).length;
        const name=(response.headers.get("Content-Disposition")||"").match(/filename="([^"]+)"/)?.[1]||"hark-accounts.json";
        const url=URL.createObjectURL(new Blob([text],{type:"application/json"})), link=document.createElement("a");
        link.href=url;link.download=name;link.click();setTimeout(()=>URL.revokeObjectURL(url),1000);
        button.disabled=false;
        toast(`已导出 ${count} 个账号（文件含登录 Cookie，请妥善保管）`);break;
      }
      case "export-logs": {
        const blob=new Blob([JSON.stringify(state.events,null,2)],{type:"application/json"});
        const url=URL.createObjectURL(blob), link=document.createElement("a");
        link.href=url;link.download="hark2api-logs.json";link.click();setTimeout(()=>URL.revokeObjectURL(url),1000);
        toast("日志已导出");break;
      }
    }
  } catch (error) {
    const slot=$("#modal .modal-error") || $("#login-page-status");
    if (slot) slot.textContent=error.message || "本地服务连接失败";
    toast(error.message || "本地服务连接失败",true);
  }
  finally {if (button.isConnected) button.disabled=false;}
});
document.addEventListener("submit", async event => {
  const form=event.target;
  if (!["key-form","account-form","account-file-form","proxy-form","proxy-edit-form","batch-proxy-form","test-form","settings-form"].includes(form.id)) return;
  event.preventDefault();
  const submit=event.submitter || form.querySelector('[type="submit"]');
  submit.disabled=true;
  try {
    if (form.id === "key-form") {
      const data=await api("/admin/keys","POST",{name:form.elements.name.value});
      freshSecret=data.secret;
      await load();
      modal("API Key 已创建", "请现在复制保存，关闭窗口后不会再次显示完整 Key。", `<div class="secret-box" id="created-secret">${esc(freshSecret)}</div><div class="info-strip">${icon("check")}Key 已可用于模型查询。聊天接口使用此 Key 自动创建独立 Hark Project。</div><div class="form-actions"><button class="btn subtle" data-action="close-modal">我已保存</button><button class="btn primary" data-action="copy-secret">${icon("copy")}复制 Key</button></div>`);
    } else if (form.id === "proxy-form") {
      const r=await api("/admin/proxies","POST",{name:form.elements.name.value, lines:form.elements.lines.value});
      $("#modal").close(); await load(false); location.hash="proxies"; render();
      toast(`已添加 ${r.added} 个代理${r.errors?.length?`，${r.errors.length} 行格式不对`:""}`, !!r.errors?.length);
    } else if (form.id === "proxy-edit-form") {
      await api(`/admin/proxies/${encodeURIComponent(form.dataset.id)}`,"PUT",{name:form.elements.name.value, url:form.elements.url.value});
      $("#modal").close(); await load(); toast("已保存");
    } else if (form.id === "batch-proxy-form") {
      const ids=[...form.querySelectorAll('input[name="acct"]:checked')].map(c=>c.value);
      if(!ids.length) throw new Error("请至少勾选一个账号");
      const r=await api("/admin/account-proxies","PUT",{account_ids:ids, proxy_id:form.elements.proxy.value});
      $("#modal").close(); await load(); toast(`已为 ${r.updated} 个账号设置代理`);
    } else if (form.id === "account-file-form") {
      const file=form.elements.file.files[0];
      if(!file) throw new Error("请选择文件");
      const text=await file.text();
      const response=await fetch("/admin/accounts/import",{method:"POST",headers:{"Content-Type":"application/json","X-Hark-Admin":"1"},body:text});
      const data=await response.json();
      if(!response.ok) throw new Error(data.error?.message||`导入失败 (${response.status})`);
      form.reset(); $("#modal").close();
      await load(false);location.hash="accounts";render();
      toast(`导入完成：新增 ${data.added}，更新 ${data.updated}${data.skipped?`，跳过 ${data.skipped}`:""}${data.errors?.length?`，失败 ${data.errors.length}`:""}`, !!data.errors?.length);
      if(data.errors?.length) modal("部分账号没导入", "", `<pre class="code-block">${esc(data.errors.join("\n"))}</pre><div class="form-actions"><button class="btn" data-action="close-modal">关闭</button></div>`);
    } else if (form.id === "account-form") {
      const created=await api("/admin/accounts","POST",{name:form.elements.name.value, cookie:form.elements.cookie.value, ...(form.elements.proxy.value.trim() ? {proxy:form.elements.proxy.value.trim()} : {})});
      form.reset(); $("#modal").close();
      let checked=null;
      try { checked=await api(`/admin/accounts/${encodeURIComponent(created.id)}/check`,"POST",{}); } catch {}
      await load(false);location.hash="accounts";render();
      toast(checked?.status==="valid" ? "Cookie 已加密保存，会话检测有效" : "Cookie 已保存，但会话检测未通过，请检查 Cookie");
    } else if (form.id === "test-form") {
      if (liveController) return;
      requestDraft=$("#request-editor").value;
      let data;
      try {data=JSON.parse(requestDraft);} catch {throw new Error("JSON 格式不正确，请检查引号和逗号");}
      if (event.submitter?.value === "send") {
        await sendLiveRequest(data);
      } else {
        testOutput=await api("/admin/validate","POST",data);
        render();toast("请求格式通过，未发送到 Hark");
      }
    } else {
      await api("/admin/settings","PUT",{name:form.elements.name.value,timezone:form.elements.timezone.value.trim(),timeout:Number(form.elements.timeout.value)});
      await load();toast("配置已保存");
    }
  } catch (error) {
    if (form.id === "test-form") {testOutput={valid:false,error:error.message || "校验服务连接失败",upstream_sent:false};render();}
    const errorSlot=form.querySelector(".modal-error");
    if (errorSlot) errorSlot.textContent=error.message || "本地服务连接失败";
    else toast(error.message || "本地服务连接失败",true);
  } finally {if (submit.isConnected) submit.disabled=false;}
});
document.addEventListener("change", event => {
  if (["req-account","req-key","req-failed"].includes(event.target.id)) {
    reqFilter = {account:$("#req-account").value, key:$("#req-key").value, failed:$("#req-failed").checked};
    loadRequests().catch(e => toast(e.message, true));
    return;
  }
  if (event.target.id !== "attach-input") return;
  const files=[...event.target.files];
  const limit=state?.max_upload_bytes || 20*1024*1024;
  Promise.all(files.map(file => new Promise((resolve, reject) => {
    if (file.size > limit) { reject(new Error(`${file.name} 超过大小限制`)); return; }
    const reader=new FileReader();
    reader.onload=()=>resolve({name:file.name, type:file.type || "application/octet-stream", size:file.size, dataURL:reader.result});
    reader.onerror=()=>reject(new Error(`${file.name} 读取失败`));
    reader.readAsDataURL(file);
  }))).then(items => { pendingFiles.push(...items); render(); }).catch(error => toast(error.message, true));
});
document.addEventListener("input", event => {
  if (event.target.id === "request-editor") requestDraft=event.target.value;
  if (event.target.id === "log-search") {
    const term=event.target.value.toLowerCase();
    $("#log-rows").innerHTML=eventRows(state.events.filter(e=>(e.action+e.detail).toLowerCase().includes(term)),true);
  }
});
$("#modal").addEventListener("close",()=>{
  freshSecret="";$("#modal-content").innerHTML="";
});
$("#modal").addEventListener("click",event=>{if (event.target === $("#modal")) {const box=event.target.getBoundingClientRect();if(event.clientX<box.left||event.clientX>box.right||event.clientY<box.top||event.clientY>box.bottom) event.target.close();}});
window.addEventListener("hashchange",()=>{reqData=null;render();window.scrollTo(0,0);});
load().catch(()=>{
  $("#service-status").textContent="服务连接中断";
  $("#page-content").innerHTML='<div class="connection-error"><h1>暂未连接到本地服务</h1><p>确认 Hark2API 正在运行，再刷新页面重试。</p><button class="btn primary" data-action="refresh">重新连接</button></div>';
});

document.addEventListener("change", async event => {
  const sel=event.target.closest?.(".proxy-select");
  if(!sel) return;
  sel.disabled=true;
  try { await api("/admin/account-proxies","PUT",{account_ids:[sel.dataset.account], proxy_id:sel.value}); await load(); toast("代理已更新"); }
  catch(e){ toast(e.message,true); sel.disabled=false; }
});
