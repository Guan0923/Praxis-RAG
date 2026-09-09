import { Alert, Button, Image } from "antd";
import { DownloadOutlined } from "@ant-design/icons";
import { useEffect, useState } from "react";
import type { AssetTable, SearchAsset, WordDiagnostic } from "./api";

export function WordDiagnostics({ diagnostics = [], count = 0 }: { diagnostics?: WordDiagnostic[]; count?: number }) {
  if (!diagnostics.length && !count) return null;
  return <Alert className="page-alert" type="warning" showIcon title={count ? `已跳过 ${count} 个无法关联的对象` : "存在未关联的注释"}
    description={<ul className="word-diagnostics">{diagnostics.map((item, index) => <li key={index}>
      {item.location?.headings?.join(" / ")} {item.location?.paragraph ? `第 ${item.location.paragraph} 段：` : ""}
      {item.kind === "unassociated_caption" ? "注释无法确定对应对象" : item.message}
      {item.text && <div>{item.text}</div>}
    </li>)}</ul>} />;
}

export function StructuredTable({ table }: { table: AssetTable }) {
  const cells = new Map<string, NonNullable<AssetTable["cells"]>[number]>();
  const covered = new Set<string>();
  for (const cell of table.cells || []) {
    const row = cell.row ?? cell.start_row_offset_idx ?? 0;
    const col = cell.col ?? cell.start_col_offset_idx ?? 0;
    const rows = cell.row_span ?? ((cell.end_row_offset_idx ?? row + 1) - row);
    const cols = cell.col_span ?? ((cell.end_col_offset_idx ?? col + 1) - col);
    cells.set(`${row}:${col}`, cell);
    for (let r = row; r < row + rows; r++) for (let c = col; c < col + cols; c++) {
      if (r !== row || c !== col) covered.add(`${r}:${c}`);
    }
  }
  return <div className="asset-table-scroll" tabIndex={0} role="region" aria-label={table.caption || "表格"}>
    <table className="asset-table"><tbody>{table.rows.map((row, r) => <tr key={r}>
      {row.map((text, c) => {
        if (covered.has(`${r}:${c}`)) return null;
        const cell = cells.get(`${r}:${c}`);
        const rowSpan = cell?.row_span ?? ((cell?.end_row_offset_idx ?? r + 1) - (cell?.start_row_offset_idx ?? r));
        const colSpan = cell?.col_span ?? ((cell?.end_col_offset_idx ?? c + 1) - (cell?.start_col_offset_idx ?? c));
        return <td key={c} rowSpan={rowSpan} colSpan={colSpan}>{cell?.text ?? text}
          {cell?.nested_tables?.map((id) => table.nested_tables?.[id]
            ? <StructuredTable key={id} table={table.nested_tables[id]} /> : null)}
        </td>;
      })}
    </tr>)}</tbody></table>
  </div>;
}

export function AssetView({ asset, source }: { asset: SearchAsset; source: string }) {
  const [url, setUrl] = useState("");
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    setFailed(false);
    if (asset.type !== "image" || !asset.data) { setUrl(""); return; }
    try {
      const bytes = Uint8Array.from(atob(asset.data), (char) => char.charCodeAt(0));
      const next = URL.createObjectURL(new Blob([bytes], { type: asset.mime_type || "application/octet-stream" }));
      setUrl(next);
      return () => URL.revokeObjectURL(next);
    } catch { setFailed(true); setUrl(""); }
  }, [asset.type, asset.data, asset.mime_type]);
  if (asset.type === "table") return <figure className="result-asset">
    {asset.caption && <figcaption>{asset.caption}</figcaption>}
    <StructuredTable table={asset} />
    {asset.footnotes?.map((note, index) => <p key={index} className="asset-note">{note}</p>)}
  </figure>;
  const previewable = /^(image\/(png|jpeg|gif|webp|avif|bmp))$/.test(asset.mime_type || "");
  const extension = ({ "image/jpeg": "jpg", "image/svg+xml": "svg", "image/x-emf": "emf", "image/x-wmf": "wmf" } as Record<string, string>)[asset.mime_type || ""] || asset.mime_type?.split("/")[1] || "bin";
  return <figure className="result-asset">
    {url && previewable && !failed ? <Image src={url} alt={asset.caption || source} onError={() => setFailed(true)} />
      : <span className="secondary">{failed ? "图片预览失败" : "此图片格式不支持直接预览"}</span>}
    {asset.caption && <figcaption>{asset.caption}</figcaption>}
    {url && <Button type="link" icon={<DownloadOutlined />} href={url} download={`${source}-image.${extension}`}>下载原图</Button>}
  </figure>;
}
