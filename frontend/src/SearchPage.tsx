import { useEffect, useRef, useState } from "react";
import { AssetView } from "./AssetView";
import {
  Alert,
  Button,
  Empty,
  Form,
  Input,
  InputNumber,
  Select,
  Spin,
  Tag,
} from "antd";
import {
  BarChartOutlined,
  FileTextOutlined,
  SearchOutlined,
} from "@ant-design/icons";
import {
  api,
  errorText,
  formatScore,
  retrievalLabels,
  rerankLabels,
  type Connection,
  type Hit,
  type Retrieval,
  type Rerank,
  type SearchResult,
} from "./api";

type SearchValues = {
  query: string;
  retrieval: Retrieval;
  rerank: Rerank;
  candidate_k: number;
  top_k: number;
};
type Outcome = { retrieval: Retrieval; payload?: SearchResult; error?: string };
const emptyMetrics = ["—", "—", "—", "—", "—", "—"];
const metricLabels = [
  "实际检索方式",
  "实际重排方式",
  "候选 Chunk",
  "证据状态",
  "检索耗时",
  "重排耗时",
];

function Result({ item, rerank }: { item: Hit; rerank: Rerank }) {
  const recall = item.metadata?.retrieval_score;
  return (
    <article className="search-result">
      <span className="result-rank">{String(item.rank).padStart(2, "0")}</span>
      <div className="result-body">
        <div className="result-heading">
          <h3>
            <FileTextOutlined /> {item.title || item.source}
          </h3>
          <div className="scores">
            <Tag color="blue">
              {rerank === "cross_encoder" ? "重排" : "分数"}{" "}
              {formatScore(item.score)}
            </Tag>
            {typeof recall === "number" && (
              <Tag>召回 {formatScore(recall)}</Tag>
            )}
          </div>
        </div>
        <div className="result-source">
          {item.source} · {item.chunk_id} · {item.document_id}
        </div>
        {item.metadata?.location && <div className="secondary">
          {item.metadata.location.headings?.join(" / ")}
          {item.metadata.location.paragraph ? ` · 第 ${item.metadata.location.paragraph} 段` : ""}
        </div>}
        {item.asset ? <AssetView asset={item.asset} source={item.source} /> : <p className="result-text">{item.text}</p>}
        {item.asset?.type === "image" && item.metadata?.footnotes?.map((note, index) => <p className="asset-note" key={index}>{note.text}</p>)}
      </div>
    </article>
  );
}

export function SearchPage({
  connection,
  onHealth,
}: {
  connection: Connection;
  onHealth: () => Promise<void>;
}) {
  const [form] = Form.useForm<SearchValues>();
  const [busy, setBusy] = useState<"search" | "compare" | null>(null);
  const [error, setError] = useState("");
  const [result, setResult] = useState<SearchResult>();
  const [outcomes, setOutcomes] = useState<Outcome[]>();
  const [metrics, setMetrics] = useState<string[]>(emptyMetrics);
  const [attempted, setAttempted] = useState(false);
  const request = useRef<AbortController | null>(null);
  const locked = useRef(false);
  useEffect(() => () => request.current?.abort(), []);

  async function run(mode: "search" | "compare") {
    if (locked.current) return;
    locked.current = true;
    setResult(undefined);
    setOutcomes(undefined);
    setMetrics(emptyMetrics);
    setError("");
    setAttempted(true);
    const controller = new AbortController();
    request.current = controller;
    try {
      const values = await form.validateFields();
      if (controller.signal.aborted) return;
      setBusy(mode);
      const body = { ...values, query: values.query.trim(), filters: {} };
      const search = (retrieval: Retrieval, rerank: Rerank) =>
        api<SearchResult>("/v1/search", connection, {
          method: "POST",
          body: JSON.stringify({ ...body, retrieval, rerank }),
          signal: controller.signal,
        });
      if (mode === "search") {
        const data = await search(values.retrieval, values.rerank);
        if (controller.signal.aborted) return;
        setResult(data);
        setMetrics([
          retrievalLabels[data.retrieval] || data.retrieval,
          rerankLabels[data.rerank] || data.rerank,
          String(data.debug?.candidate_count ?? "—"),
          data.evidence_status === "found" ? "已找到" : "无证据",
          data.debug?.retrieval_ms == null
            ? "—"
            : `${data.debug.retrieval_ms} ms`,
          data.debug?.rerank_ms == null ? "—" : `${data.debug.rerank_ms} ms`,
        ]);
      } else {
        const rows: Outcome[] = [];
        for (const retrieval of ["bm25", "embedding", "hybrid"] as const) {
          if (controller.signal.aborted) return;
          try {
            rows.push({ retrieval, payload: await search(retrieval, "none") });
          } catch (error) {
            if (controller.signal.aborted) return;
            rows.push({ retrieval, error: errorText(error) });
          }
        }
        if (controller.signal.aborted) return;
        setOutcomes(rows);
        setMetrics([
          "三模式",
          "不重排（固定）",
          rows
            .map((row) => row.payload?.debug?.candidate_count ?? "—")
            .join(" / "),
          `${rows.filter((row) => row.payload).length}/3 成功`,
          rows
            .map((row) =>
              row.payload?.debug?.retrieval_ms == null
                ? "—"
                : `${row.payload.debug.retrieval_ms} ms`,
            )
            .join(" / "),
          "0 ms",
        ]);
      }
      void onHealth();
    } catch (error) {
      if (!controller.signal.aborted)
        setError(
          error && typeof error === "object" && "errorFields" in error
            ? "请检查查询内容和参数。旧结果已清空。"
            : `${errorText(error)}\n本次请求失败，旧结果已清空。`,
        );
    } finally {
      if (!controller.signal.aborted) {
        locked.current = false;
        setBusy(null);
      }
    }
  }

  return (
    <>
      <div className="page-heading">
        <h1>检索调试</h1>
      </div>
      <Form
        form={form}
        layout="vertical"
        initialValues={{
          query: "",
          retrieval: "hybrid",
          rerank: "none",
          candidate_k: 10,
          top_k: 5,
        }}
      >
        <Form.Item
          name="query"
          label="查询内容"
          rules={[
            { required: true, whitespace: true, message: "请输入查询内容" },
          ]}
        >
          <Input.TextArea
            autoSize={{ minRows: 3, maxRows: 10 }}
            placeholder="输入需要检索的问题"
            onKeyDown={(event) => {
              if ((event.ctrlKey || event.metaKey) && event.key === "Enter") {
                event.preventDefault();
                void run("search");
              }
            }}
          />
        </Form.Item>
        <div className="search-controls">
          <Form.Item name="retrieval" label="检索方式">
            <Select
              virtual={false}
              options={Object.entries(retrievalLabels).map(
                ([value, label]) => ({ value, label }),
              )}
            />
          </Form.Item>
          <Form.Item name="rerank" label="重排方式">
            <Select
              virtual={false}
              options={Object.entries(rerankLabels).map(([value, label]) => ({
                value,
                label,
              }))}
            />
          </Form.Item>
          <Form.Item
            name="candidate_k"
            label="候选数"
            rules={[
              {
                type: "integer",
                min: 1,
                max: 100,
                required: true,
                message: "请输入 1–100 的整数",
              },
            ]}
          >
            <InputNumber min={1} max={100} />
          </Form.Item>
          <Form.Item
            name="top_k"
            label="返回数"
            dependencies={["candidate_k"]}
            rules={[
              {
                type: "integer",
                min: 1,
                max: 10,
                required: true,
                message: "请输入 1–10 的整数",
              },
              ({ getFieldValue }) => ({
                validator: (_, value) =>
                  value > getFieldValue("candidate_k")
                    ? Promise.reject(new Error("不能大于候选数"))
                    : Promise.resolve(),
              }),
            ]}
          >
            <InputNumber min={1} max={10} />
          </Form.Item>
        </div>
        <div className="search-actions">
          <Button
            aria-label="三模式对比"
            icon={<BarChartOutlined />}
            loading={busy === "compare"}
            disabled={!!busy}
            onClick={() => void run("compare")}
          >
            三模式对比
          </Button>
          <Button
            aria-label="运行检索"
            type="primary"
            icon={<SearchOutlined />}
            loading={busy === "search"}
            disabled={!!busy}
            onClick={() => void run("search")}
          >
            运行检索
          </Button>
        </div>
      </Form>
      <div className="metrics" aria-label="检索指标">
        {metricLabels.map((label, index) => (
          <div key={label}>
            <span>{label}</span>
            <strong>{metrics[index]}</strong>
          </div>
        ))}
      </div>
      {error && (
        <Alert
          className="page-alert"
          type="error"
          showIcon
          title="检索失败"
          description={<span className="preserve-lines">{error}</span>}
        />
      )}
      <div className="section-heading">
        <h2>{outcomes ? "三模式对比" : "检索结果"}</h2>
        {result && (
          <span className="secondary">{result.results.length} 个片段</span>
        )}
      </div>
      <div aria-live="polite" aria-busy={!!busy}>
        {busy ? (
          <div className="empty-state">
            <Spin />
            <span>
              {busy === "compare"
                ? "正在依次检索 BM25 / Embedding / Hybrid"
                : "正在检索与重排"}
            </span>
          </div>
        ) : outcomes ? (
          <>
            <div className="comparison-summary secondary">
              BM25 / Embedding / Hybrid · 不重排
            </div>
            <div className="comparison">
              {outcomes.map((row) => (
                <section key={row.retrieval}>
                  <div className="section-heading">
                    <h3>{retrievalLabels[row.retrieval]}</h3>
                    <span className="secondary">
                      {row.payload?.debug?.retrieval_ms == null
                        ? "—"
                        : `${row.payload.debug.retrieval_ms} ms`}
                    </span>
                  </div>
                  {row.error ? (
                    <Alert
                      type="error"
                      showIcon
                      title="检索失败"
                      description={row.error}
                    />
                  ) : row.payload?.results.length ? (
                    row.payload.results.map((item) => (
                      <Result key={item.chunk_id} item={item} rerank="none" />
                    ))
                  ) : (
                    <Empty
                      image={Empty.PRESENTED_IMAGE_SIMPLE}
                      description="无结果"
                    />
                  )}
                </section>
              ))}
            </div>
          </>
        ) : result?.results.length ? (
          result.results.map((item) => (
            <Result key={item.chunk_id} item={item} rerank={result.rerank} />
          ))
        ) : (
          <Empty
            image={Empty.PRESENTED_IMAGE_SIMPLE}
            description={
              error
                ? "旧结果已清空"
                : attempted
                  ? "没有找到可用证据"
                  : "暂无检索结果"
            }
          />
        )}
      </div>
    </>
  );
}
