const $ = (id) => document.getElementById(id);
const state = {
  namespace: localStorage.getItem("rag.namespace") || "default",
  token: sessionStorage.getItem("rag.token") || "",
  strategies: [],
  uploading: false,
};
const retrievalLabels = { bm25: "BM25", embedding: "Embedding", hybrid: "Hybrid" };
const rerankLabels = {
  none: "不重排",
  cross_encoder: "Cross Encoder",
  bi_encoder: "Bi Encoder（Mock）",
};

function headers(json = false) {
  const value = { "X-RAG-Namespace": state.namespace, Accept: "application/json" };
  if (json) value["Content-Type"] = "application/json";
  if (state.token) value.Authorization = `Bearer ${state.token}`;
  return value;
}

async function api(path, options = {}) {
  const response = await fetch(path, options);
  let payload;
  try {
    payload = await response.json();
  } catch {
    throw new Error(`HTTP ${response.status} 返回了无效 JSON`);
  }
  if (!response.ok) throw new Error(`HTTP ${response.status}：${payload.detail || "请求失败"}`);
  return payload;
}

function toast(message, error = false) {
  const target = $("toast");
  target.textContent = message;
  target.className = error ? "show error" : "show";
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => { target.className = ""; }, 3500);
}

function escapeHtml(value) {
  return String(value ?? "").replace(
    /[&<>"']/g,
    (char) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#039;" })[char],
  );
}

async function health() {
  try {
    const data = await api("/health");
    $("statusDot").className = "ok";
    $("statusText").textContent = "服务正常";
    $("statusDetail").textContent = `${data.backend || "unknown"} · ${data.collection || "no collection"}`;
    renderModelStatus(data.models);
  } catch (error) {
    $("statusDot").className = "error";
    $("statusText").textContent = "服务异常";
    $("statusDetail").textContent = error.message;
    $("modelStatus").textContent = error.message;
  }
}

function renderModelStatus(models) {
  if (!models?.embedding || !models?.reranker) {
    $("modelStatus").innerHTML = '<div class="compare-note">当前服务进程尚未报告模型状态，请重启 RAG Service 后刷新。</div>';
    return;
  }
  $("modelStatus").innerHTML = [
    ["Embedding", models.embedding],
    ["Reranker", models.reranker],
  ].map(([label, model]) => `
    <div class="model-row">
      <strong>${label}</strong>
      <div>${escapeHtml(model.name)}<br><small>device: ${escapeHtml(model.device)}</small></div>
      <span class="model-state ${model.loaded ? "loaded" : ""}">${model.loaded ? "已载入" : "未载入"}</span>
    </div>
  `).join("") + (models.reranker.loaded ? "" : '<div class="compare-note">Cross Encoder 首次调用会加载或下载模型；网络不可达时会明确显示失败，不再保留旧结果。</div>');
}

async function loadDocuments() {
  $("documents").innerHTML = '<div class="empty">正在读取文档…</div>';
  try {
    const data = await api("/v1/documents", { headers: headers() });
    $("documentCount").textContent = `${data.documents.length} 个`;
    $("documents").innerHTML = data.documents.length
      ? data.documents.map((doc) => `<div class="document"><div><strong>${escapeHtml(doc.name)}</strong><small>${doc.chunk_count} chunks · ${formatBytes(doc.size)} · ${escapeHtml(chunkLabel(doc.chunking))} · ${escapeHtml(doc.collection)}</small></div><button class="delete" data-name="${escapeHtml(doc.name)}" ${state.uploading ? "disabled" : ""}>删除</button></div>`).join("")
      : '<div class="empty">当前 namespace 还没有知识文档。</div>';
    document.querySelectorAll(".delete").forEach((button) => button.addEventListener("click", () => removeDocument(button.dataset.name)));
  } catch (error) {
    $("documents").innerHTML = `<div class="empty">${escapeHtml(error.message)}</div>`;
  }
}

function chunkLabel(configuration) {
  const config = configuration || { strategy: "character", chunk_size: 700, chunk_overlap: 100 };
  const strategy = state.strategies.find((item) => item.id === config.strategy);
  return `${strategy?.label || config.strategy} · ${config.chunk_size}/${config.chunk_overlap} ${config.strategy === "character" ? "字符" : "token"}`;
}

async function loadStrategies() {
  try {
    const data = await api("/v1/chunking/strategies", { headers: headers() });
    state.strategies = data.strategies;
    $("chunkStrategy").innerHTML = data.strategies.map((item) => `<option value="${escapeHtml(item.id)}">${escapeHtml(item.label)}</option>`).join("");
    $("chunkStrategy").value = data.default;
    $("chunkStrategy").disabled = state.uploading;
    $("uploadButton").disabled = state.uploading;
    setStrategyDefaults();
  } catch (error) {
    $("uploadResult").hidden = false;
    $("uploadResult").textContent = error.message;
  }
}

function setStrategyDefaults() {
  const strategy = state.strategies.find((item) => item.id === $("chunkStrategy").value);
  if (!strategy) return;
  $("chunkSize").value = strategy.chunk_size;
  $("chunkSize").min = strategy.id === "character" ? "100" : "1";
  $("chunkOverlap").value = strategy.chunk_overlap;
  document.querySelectorAll(".chunk-unit").forEach((item) => { item.textContent = strategy.unit === "characters" ? "字符" : "token"; });
}

function uploadBusy(busy) {
  state.uploading = busy;
  ["fileInput", "chunkStrategy", "chunkSize", "chunkOverlap", "uploadButton", "namespace", "token", "saveConnection"].forEach((id) => { $(id).disabled = busy; });
  document.querySelectorAll(".delete").forEach((button) => { button.disabled = busy; });
}

async function upload() {
  if (state.uploading) return;
  const files = Array.from($("fileInput").files || []);
  if (!files.length) return toast("请先选择 Markdown、TXT、PDF 或 DOCX 文件", true);
  if (files.length > 10 || files.some((file) => file.size > 10 * 1024 * 1024 || file.size === 0)) return toast("每批最多 10 个非空文件，单文件不超过 10 MiB", true);
  const configuration = { strategy: $("chunkStrategy").value, chunk_size: Number($("chunkSize").value), chunk_overlap: Number($("chunkOverlap").value) };
  if (!Number.isInteger(configuration.chunk_size) || configuration.chunk_size < (configuration.strategy === "character" ? 100 : 1) || !Number.isInteger(configuration.chunk_overlap) || configuration.chunk_overlap < 0 || configuration.chunk_overlap >= configuration.chunk_size) return toast("请检查块长度和重叠上限", true);
  const requestHeaders = headers();
  const button = $("uploadButton");
  uploadBusy(true);
  button.textContent = "正在检查文件…";
  $("uploadResult").hidden = true;
  try {
    if (!globalThis.crypto?.subtle) throw new Error("文件哈希需要 HTTPS 或 localhost 安全连接");
    const fingerprints = [];
    for (const file of files) {
      const digest = await crypto.subtle.digest("SHA-256", await file.arrayBuffer());
      fingerprints.push({ name: file.name, sha256: Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, "0")).join("") });
    }
    const checked = await api("/v1/documents/check", { method: "POST", headers: { ...requestHeaders, "Content-Type": "application/json" }, body: JSON.stringify({ files: fingerprints, ...configuration }) });
    const skipped = checked.files.filter((item) => item.document).map((item) => ({ ...item.document, requested_name: item.name }));
    const pending = files.filter((_, index) => !checked.files[index].document);
    let data = { imported: [], skipped: [], failed: [] };
    if (pending.length) {
      const body = new FormData();
      pending.forEach((file) => body.append("files", file));
      Object.entries(configuration).forEach(([key, value]) => body.append(key, value));
      button.textContent = "正在生成索引…";
      data = await api("/v1/documents", { method: "POST", headers: requestHeaders, body });
    }
    skipped.push(...(data.skipped || []));
    const summary = `新增 ${data.imported.length} 个，跳过 ${skipped.length} 个，失败 ${data.failed.length} 个`;
    $("uploadResult").textContent = [summary, ...skipped.map((item) => `${item.requested_name || item.name}：已存在 ${item.name}`), ...data.failed.map((item) => `${item.name}：${item.error}`)].join("\n");
    $("uploadResult").hidden = false;
    toast(summary, data.failed.length > 0);
    if (!data.failed.length) $("fileInput").value = "";
    await loadDocuments();
    await health();
  } catch (error) {
    $("uploadResult").hidden = false;
    $("uploadResult").textContent = error.message;
    toast(error.message, true);
  } finally {
    uploadBusy(false);
    button.textContent = "上传并索引";
  }
}

async function removeDocument(name) {
  if (!confirm(`删除 ${name} 及其全部向量？`)) return;
  try {
    await api(`/v1/documents/${encodeURIComponent(name)}`, { method: "DELETE", headers: headers() });
    toast(`已删除 ${name}`);
    await loadDocuments();
  } catch (error) {
    toast(error.message, true);
  }
}

function currentRequest() {
  const candidateK = Number($("candidateK").value);
  const topK = Number($("topK").value);
  if (!Number.isInteger(candidateK) || candidateK < 1 || candidateK > 100) throw new Error("候选数必须是 1～100 的整数");
  if (!Number.isInteger(topK) || topK < 1 || topK > 10) throw new Error("返回数必须是 1～10 的整数");
  if (topK > candidateK) throw new Error("返回数不能大于候选数");
  return {
    query: $("query").value.trim(),
    retrieval: $("retrieval").value,
    rerank: $("rerank").value,
    candidate_k: candidateK,
    top_k: topK,
    filters: {},
  };
}

function setBusy(busy, action = "search") {
  $("searchButton").disabled = busy;
  $("compareButton").disabled = busy;
  $("searchButton").textContent = busy && action === "search" ? "检索 / 重排中…" : "运行检索";
  $("compareButton").textContent = busy && action === "compare" ? "正在对比…" : "三模式对比";
}

function resetMetrics() {
  ["metricRetrieval", "metricRerank", "metricCandidates", "metricEvidence", "metricRetrievalMs", "metricRerankMs"]
    .forEach((id) => { $(id).textContent = "—"; });
}

function clearSearchError() {
  $("searchError").hidden = true;
  $("searchError").textContent = "";
}

function showSearchError(error) {
  const message = friendlySearchError(error instanceof Error ? error.message : String(error));
  resetMetrics();
  $("searchError").hidden = false;
  $("searchError").textContent = message;
  $("results").innerHTML = '<div class="empty">本次请求失败，旧结果已清空。</div>';
  toast("检索失败，详情已显示在页面中", true);
}

function friendlySearchError(message) {
  if (/Cross-encoder|WinError 10060|rerank/i.test(message)) {
    return `${message}\n\nCross Encoder 模型可能尚未下载，或当前网络无法访问模型源。可以先选择“不重排”验证召回，再配置可用模型缓存或网络后重启 RAG Service。`;
  }
  return message;
}

async function search() {
  let request;
  try {
    request = currentRequest();
    if (!request.query) throw new Error("请输入查询内容");
  } catch (error) {
    showSearchError(error);
    return;
  }
  clearSearchError();
  resetMetrics();
  $("results").innerHTML = `<div class="empty">正在执行 ${retrievalLabels[request.retrieval]}，随后使用 ${rerankLabels[request.rerank]}…</div>`;
  setBusy(true, "search");
  try {
    const payload = await api("/v1/search", {
      method: "POST",
      headers: headers(true),
      body: JSON.stringify(request),
    });
    renderSearchResult(payload);
    await health();
  } catch (error) {
    showSearchError(error);
  } finally {
    setBusy(false);
  }
}

function renderSearchResult(payload) {
  const debug = payload.debug || {};
  $("metricRetrieval").textContent = retrievalLabels[payload.retrieval] || payload.retrieval;
  $("metricRerank").textContent = rerankLabels[payload.rerank] || payload.rerank;
  $("metricCandidates").textContent = debug.candidate_count ?? "—";
  $("metricEvidence").textContent = payload.evidence_status === "found" ? "已找到" : "无证据";
  $("metricRetrievalMs").textContent = debug.retrieval_ms == null ? "—" : `${debug.retrieval_ms} ms`;
  $("metricRerankMs").textContent = debug.rerank_ms == null ? "—" : `${debug.rerank_ms} ms`;
  $("results").innerHTML = payload.results?.length
    ? payload.results.map((item) => renderResultCard(item, payload.rerank)).join("")
    : '<div class="empty">没有找到可用证据。可以改写查询、切换检索方式，或检查当前 namespace 的知识文档。</div>';
}

function renderResultCard(item, rerank) {
  const retrievalScore = item.metadata?.retrieval_score;
  const badges = rerank === "cross_encoder" && typeof retrievalScore === "number"
    ? `<span class="badge">重排 ${formatScore(item.score)}</span><span class="badge secondary">召回 ${formatScore(retrievalScore)}</span>`
    : `<span class="badge">分数 ${formatScore(item.score)}</span>`;
  return `<article class="result"><div class="result-head"><h3>#${item.rank} ${escapeHtml(item.title || item.source)}</h3><div class="score-group">${badges}</div></div><div class="meta">${escapeHtml(item.source)} · ${escapeHtml(item.chunk_id)} · ${escapeHtml(item.document_id)}</div><p>${escapeHtml(item.text)}</p></article>`;
}

async function compareRetrievals() {
  let base;
  try {
    base = currentRequest();
    if (!base.query) throw new Error("请输入查询内容");
  } catch (error) {
    showSearchError(error);
    return;
  }
  clearSearchError();
  resetMetrics();
  $("metricRetrieval").textContent = "三模式";
  $("metricRerank").textContent = "不重排（固定）";
  $("results").innerHTML = '<div class="empty">正在依次执行 BM25、Embedding 和 Hybrid；为观察原始召回差异，本次固定关闭重排。</div>';
  setBusy(true, "compare");
  const outcomes = [];
  for (const retrieval of ["bm25", "embedding", "hybrid"]) {
    try {
      const payload = await api("/v1/search", {
        method: "POST",
        headers: headers(true),
        body: JSON.stringify({ ...base, retrieval, rerank: "none" }),
      });
      outcomes.push({ retrieval, payload });
    } catch (error) {
      outcomes.push({ retrieval, error: error instanceof Error ? error.message : String(error) });
    }
  }
  setBusy(false);
  renderComparison(outcomes);
}

function renderComparison(outcomes) {
  const successful = outcomes.filter((outcome) => outcome.payload);
  const failures = outcomes.filter((outcome) => outcome.error);
  $("metricCandidates").textContent = successful.map((outcome) => outcome.payload.debug?.candidate_count ?? "—").join(" / ") || "—";
  $("metricEvidence").textContent = `${successful.length}/3 成功`;
  $("metricRetrievalMs").textContent = successful.map((outcome) => `${outcome.payload.debug?.retrieval_ms ?? "—"} ms`).join(" / ") || "—";
  $("metricRerankMs").textContent = "0 ms";
  if (failures.length) {
    $("searchError").hidden = false;
    $("searchError").textContent = failures.map((outcome) => `${retrievalLabels[outcome.retrieval]}：${outcome.error}`).join("\n");
  }
  $("results").innerHTML = '<div class="compare-note">候选数和检索耗时依次按 BM25 / Embedding / Hybrid 显示。对比模式固定 rerank=none。</div>'
    + `<div class="comparison">${outcomes.map(renderCompareColumn).join("")}</div>`;
}

function renderCompareColumn(outcome) {
  const label = retrievalLabels[outcome.retrieval];
  if (outcome.error) return `<section class="compare-column"><h3>${label}<span class="badge secondary">失败</span></h3><p>${escapeHtml(outcome.error)}</p></section>`;
  const payload = outcome.payload;
  const items = payload.results?.map((item) => `<div class="compare-item"><strong>#${item.rank} ${escapeHtml(item.title || item.source)}</strong><small>score ${formatScore(item.score)} · ${escapeHtml(item.chunk_id)}</small></div>`).join("") || '<div class="empty">无结果</div>';
  return `<section class="compare-column"><h3>${label}<span class="badge">${payload.debug?.retrieval_ms ?? "—"} ms</span></h3>${items}</section>`;
}

function saveConnection() {
  if (state.uploading) return;
  const namespace = $("namespace").value.trim();
  if (!/^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/.test(namespace)) return toast("Namespace 只能包含字母、数字、点、下划线和横线", true);
  state.namespace = namespace;
  state.token = $("token").value;
  localStorage.setItem("rag.namespace", namespace);
  sessionStorage.setItem("rag.token", state.token);
  renderConfig();
  toast("连接配置已保存");
  loadDocuments();
  loadStrategies();
}

function renderConfig() {
  $("namespace").value = state.namespace;
  $("token").value = state.token;
  const config = {
    url: `${location.origin}/mcp`,
    headers: { "X-RAG-Namespace": state.namespace },
  };
  if (state.token) config.headers.Authorization = "Bearer <RAG_SERVICE_TOKEN>";
  $("mcpConfig").textContent = JSON.stringify(config, null, 2);
}

function switchView(name) {
  document.querySelectorAll(".nav").forEach((item) => item.classList.toggle("active", item.dataset.view === name));
  document.querySelectorAll(".view").forEach((item) => item.classList.remove("active"));
  $(`${name}View`).classList.add("active");
  $("pageTitle").textContent = { search: "检索调试", documents: "知识文档", connection: "连接配置" }[name];
  if (name === "documents") loadDocuments();
  if (name === "connection") health();
}

function formatBytes(value) {
  if (value < 1024) return `${value} B`;
  if (value < 1048576) return `${(value / 1024).toFixed(1)} KB`;
  return `${(value / 1048576).toFixed(1)} MB`;
}

function formatScore(value) {
  const number = Number(value);
  return Number.isFinite(number) ? number.toFixed(4) : "—";
}

document.querySelectorAll(".nav").forEach((item) => item.addEventListener("click", () => switchView(item.dataset.view)));
$("refreshButton").addEventListener("click", health);
$("searchButton").addEventListener("click", search);
$("compareButton").addEventListener("click", compareRetrievals);
$("uploadButton").addEventListener("click", upload);
$("chunkStrategy").addEventListener("change", setStrategyDefaults);
$("saveConnection").addEventListener("click", saveConnection);
$("query").addEventListener("keydown", (event) => { if ((event.ctrlKey || event.metaKey) && event.key === "Enter") search(); });
renderConfig();
health();
loadStrategies();
