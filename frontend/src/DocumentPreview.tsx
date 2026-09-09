import { lazy, Suspense, useEffect, useState } from "react";
import { Alert, Button, Spin } from "antd";
import { api, apiResponse, errorText, type Connection, type DocumentInfo } from "./api";
import { WordDiagnostics } from "./AssetView";

const PdfPreview = lazy(() => import("./PdfPreview"));

export function DocumentPreview({ name, connection }: { name: string; connection: Connection }) {
  const [content, setContent] = useState<Blob | string>();
  const [error, setError] = useState("");
  const [attempt, setAttempt] = useState(0);
  const [info, setInfo] = useState<DocumentInfo>();

  useEffect(() => {
    const controller = new AbortController();
    setContent(undefined);
    setError("");
    setInfo(undefined);
    if (/\.docx?$/i.test(name)) void api<{ documents: DocumentInfo[] }>("/v1/documents", connection, { signal: controller.signal })
      .then((value) => { if (!controller.signal.aborted) setInfo(value.documents.find((item) => item.name === name)); })
      .catch(() => {});
    async function load() {
      try {
        const response = await apiResponse(
          `/v1/documents/${encodeURIComponent(name)}/content`, connection,
          { signal: controller.signal, headers: { Accept: "application/pdf, text/plain" } },
        );
        const result = response.headers.get("Content-Type")?.startsWith("application/pdf")
          ? await response.blob() : await response.text();
        if (!controller.signal.aborted) setContent(result);
      } catch (cause) {
        if (!controller.signal.aborted) setError(errorText(cause));
      }
    }
    void load();
    return () => controller.abort();
  }, [name, connection.namespace, connection.token, attempt]);

  if (error) return <Alert type="error" showIcon title="文档内容加载失败" description={error}
    action={<Button onClick={() => setAttempt((value) => value + 1)}>重试</Button>} />;
  if (content === undefined) return <div className="preview-loading" role="status"><Spin /> 正在加载文档</div>;
  if (typeof content === "string") return <><WordDiagnostics diagnostics={info?.diagnostics} count={info?.skipped_object_count} />
    <div className="document-text">{content}</div></>;
  return <Suspense fallback={<div className="preview-loading" role="status"><Spin /> 正在加载 PDF</div>}>
    <PdfPreview file={content} name={name} />
  </Suspense>;
}
