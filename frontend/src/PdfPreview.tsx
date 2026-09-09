import { useEffect, useRef, useState } from "react";
import { Alert, Button, InputNumber, Select, Spin, Tooltip } from "antd";
import { DownloadOutlined, LeftOutlined, RightOutlined } from "@ant-design/icons";
import { getDocument, GlobalWorkerOptions, type PDFDocumentProxy, type RenderTask } from "pdfjs-dist";
import workerUrl from "pdfjs-dist/build/pdf.worker.min.mjs?url";
import { errorText } from "./api";

GlobalWorkerOptions.workerSrc = workerUrl;

export default function PdfPreview({ file, name }: { file: Blob; name: string }) {
  const [pdf, setPdf] = useState<PDFDocumentProxy>();
  const [page, setPage] = useState(1);
  const [zoom, setZoom] = useState(1);
  const [width, setWidth] = useState(0);
  const [error, setError] = useState("");
  const [rendering, setRendering] = useState(true);
  const [downloadUrl, setDownloadUrl] = useState("");
  const viewport = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const url = URL.createObjectURL(file);
    setDownloadUrl(url);
    const task = getDocument({
      url,
      cMapUrl: `${import.meta.env.BASE_URL}pdfjs/cmaps/`,
      cMapPacked: true,
      standardFontDataUrl: `${import.meta.env.BASE_URL}pdfjs/standard_fonts/`,
      wasmUrl: `${import.meta.env.BASE_URL}pdfjs/wasm/`,
    });
    let cancelled = false;
    task.promise.then((document) => {
      if (!cancelled) setPdf(document);
    }).catch((cause) => {
      if (!cancelled) {
        setError(errorText(cause));
        setRendering(false);
      }
    });
    return () => {
      cancelled = true;
      void task.destroy();
      URL.revokeObjectURL(url);
    };
  }, [file]);

  useEffect(() => {
    const element = viewport.current!;
    const observer = new ResizeObserver(([entry]) => setWidth(entry.contentRect.width));
    observer.observe(element);
    return () => observer.disconnect();
  }, []);

  useEffect(() => {
    if (!pdf || width <= 0) return;
    let cancelled = false;
    let task: RenderTask | undefined;
    const host = viewport.current!;
    host.replaceChildren();
    host.scrollTo(0, 0);
    setRendering(true);
    setError("");
    async function render() {
      try {
        const source = await pdf!.getPage(page);
        if (cancelled) return;
        const original = source.getViewport({ scale: 1 });
        const display = source.getViewport({ scale: (width / original.width) * zoom });
        const ratio = Math.min(window.devicePixelRatio || 1, 2);
        const canvas = document.createElement("canvas");
        canvas.width = Math.ceil(display.width * ratio);
        canvas.height = Math.ceil(display.height * ratio);
        canvas.style.width = `${display.width}px`;
        canvas.style.height = `${display.height}px`;
        canvas.setAttribute("role", "img");
        canvas.setAttribute("aria-label", `${name}，第 ${page} 页`);
        task = source.render({ canvas, viewport: display, transform: [ratio, 0, 0, ratio, 0, 0] });
        await task.promise;
        if (!cancelled) host.replaceChildren(canvas);
      } catch (cause) {
        if (!cancelled) setError(errorText(cause));
      } finally {
        if (!cancelled) setRendering(false);
      }
    }
    void render();
    return () => {
      cancelled = true;
      task?.cancel();
    };
  }, [pdf, page, zoom, width, name]);

  return <div className="pdf-preview">
    <div className="pdf-toolbar" role="toolbar" aria-label="PDF 工具栏">
      <div className="pdf-page-controls">
        <Tooltip title="上一页"><Button aria-label="上一页" icon={<LeftOutlined />}
          disabled={!pdf || page <= 1} onClick={() => setPage((value) => value - 1)} /></Tooltip>
        <InputNumber aria-label="PDF 页码" min={1} max={pdf?.numPages || 1} precision={0}
          value={page} disabled={!pdf} onChange={(value) => {
            if (value && pdf) setPage(Math.max(1, Math.min(pdf.numPages, value)));
          }} />
        <span className="pdf-page-count">/ {pdf?.numPages ?? "..."}</span>
        <Tooltip title="下一页"><Button aria-label="下一页" icon={<RightOutlined />}
          disabled={!pdf || page >= pdf.numPages} onClick={() => setPage((value) => value + 1)} /></Tooltip>
      </div>
      <Select aria-label="PDF 缩放" value={zoom} onChange={setZoom} virtual={false}
        options={[{ value: 0.5, label: "50%" }, { value: 0.75, label: "75%" },
          { value: 1, label: "适合宽度" }, { value: 1.25, label: "125%" },
          { value: 1.5, label: "150%" }, { value: 2, label: "200%" }]} />
      <Tooltip title="下载 PDF"><Button aria-label="下载 PDF" icon={<DownloadOutlined />}
        href={downloadUrl || undefined} download={name} disabled={!downloadUrl} /></Tooltip>
    </div>
    {error && <Alert type="error" showIcon title="PDF 预览失败" description={error} />}
    {rendering && <div className="preview-loading" role="status"><Spin /> 正在显示 PDF</div>}
    <div ref={viewport} className="pdf-viewport" aria-busy={rendering} />
  </div>;
}
