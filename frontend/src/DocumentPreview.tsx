import { lazy, Suspense, useEffect, useState } from "react";
import { Alert, Button, Spin } from "antd";
import { apiResponse, errorText, type Connection } from "./api";

const PdfPreview = lazy(() => import("./PdfPreview"));

export function DocumentPreview({ name, connection }: { name: string; connection: Connection }) {
  const [content, setContent] = useState<Blob | string>();
  const [error, setError] = useState("");
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    const controller = new AbortController();
    setContent(undefined);
    setError("");
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
  if (typeof content === "string") return <div className="document-text">{content}</div>;
  return <Suspense fallback={<div className="preview-loading" role="status"><Spin /> 正在加载 PDF</div>}>
    <PdfPreview file={content} name={name} />
  </Suspense>;
}
