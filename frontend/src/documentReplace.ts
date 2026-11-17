import { api, type AccessView, type Connection } from "./api";

// Replacement reuses the ordinary document limits: the new file must be a
// supported type and no larger than 10 MiB.
export const replaceMaxFileBytes = 10 * 1024 * 1024;

const supportedReplaceExtensions = [
  ".md",
  ".txt",
  ".pdf",
  ".docx",
  ".doc",
  ".html",
  ".htm",
];

// documentExtension returns the lower-cased suffix (including the dot), or ""
// when the name has none.
export function documentExtension(name: string) {
  const index = name.lastIndexOf(".");
  return index > 0 ? name.slice(index).toLowerCase() : "";
}

// documentFormatFamily collapses suffixes that name the same document format.
// HTML and HTM are two spellings of one format, so they may replace each other;
// every other suffix stays its own family and must match exactly. The argument
// is normalized here so callers may pass a raw suffix as well.
export function documentFormatFamily(extension: string) {
  const normalized = extension.toLowerCase();
  return normalized === ".htm" ? ".html" : normalized;
}

// replaceAcceptAttribute is the accept filter of the replacement picker. It
// offers both HTML spellings so either variant of an interchangeable pair stays
// selectable in the file dialog.
export function replaceAcceptAttribute(name: string) {
  const family = documentFormatFamily(documentExtension(name));
  return family === ".html" ? ".html,.htm" : family;
}

// replaceTypeLabel is the human-readable type hint of the replacement dialog:
// the HTML family is shown as both spellings because either one is accepted.
export function replaceTypeLabel(name: string) {
  const family = documentFormatFamily(documentExtension(name));
  return family === ".html" ? ".html 或 .htm" : family;
}

// canReplaceDocument reports whether the caller may replace one document. A
// file-level write grant is enough for its own document; whole-write covers
// every document. Legacy API-key mode and a failed access probe keep the
// historical optimistic behaviour because the server still enforces the rule.
export function canReplaceDocument(
  access: AccessView | undefined,
  authEnabled: boolean,
  name: string,
) {
  if (!authEnabled || !access) return true;
  return access.scope.whole_write || access.scope.write_documents.includes(name);
}

// replaceFileError validates one replacement selection and returns a Chinese
// reason, or null when the file may be submitted. The new file may have a
// different name, but its format family must match the original document
// (HTML and HTM are the same family; every other suffix must match exactly).
export function replaceFileError(
  originalName: string,
  file: { name: string; size: number } | undefined,
): string | null {
  if (!file) return "请选择要替换的文件";
  const uploaded = documentExtension(file.name);
  if (!supportedReplaceExtensions.includes(uploaded)) {
    return "仅支持 Markdown、TXT、PDF、DOCX、DOC、HTML 和 HTM 文件";
  }
  if (file.size <= 0) return "替换文件不能为空";
  if (file.size > replaceMaxFileBytes) return "单文件不超过 10 MiB";
  const original = documentExtension(originalName);
  if (documentFormatFamily(uploaded) !== documentFormatFamily(original)) {
    return `文件类型必须与原文档一致：原文档为 ${original || "未知"}，新文件为 ${uploaded}`;
  }
  return null;
}

// replaceDocumentPath is the REST route of one document replacement.
export function replaceDocumentPath(name: string) {
  return `/v1/documents/${encodeURIComponent(name)}/replace`;
}

// ReplaceDocumentResult is the synchronously rebuilt document payload returned
// by POST /v1/documents/{name}/replace.
export type ReplaceDocumentResult = {
  status?: string;
  document?: { name?: string } & Record<string, unknown>;
  documents?: ({ name?: string } & Record<string, unknown>)[];
};

// replaceDocument posts one file as multipart "file" and waits for the server
// to replace the content and rebuild the index.
export async function replaceDocument(
  name: string,
  file: File,
  connection?: Connection,
  signal?: AbortSignal,
): Promise<ReplaceDocumentResult> {
  const body = new FormData();
  body.append("file", file, file.name);
  return await api<ReplaceDocumentResult>(replaceDocumentPath(name), connection, {
    method: "POST",
    body,
    signal,
  });
}
