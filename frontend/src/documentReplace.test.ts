import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { AccessView, Connection } from "./api";
import {
  canReplaceDocument,
  documentExtension,
  documentFormatFamily,
  replaceAcceptAttribute,
  replaceDocument,
  replaceFileError,
  replaceMaxFileBytes,
  replaceTypeLabel,
} from "./documentReplace";

const fetchMock = vi.fn();

// jsonResponse builds the minimal Response surface apiResponse() touches.
function jsonResponse(payload: unknown, status = 200) {
  const text = JSON.stringify(payload);
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: "test",
    headers: new Headers(),
    json: async () => JSON.parse(text),
    text: async () => text,
  } as unknown as Response;
}

function access(overrides: Partial<AccessView> = {}): AccessView {
  return {
    namespace: "kb",
    owner: false,
    can_manage: false,
    can_share: false,
    role: "user",
    can_read: true,
    can_write: true,
    scope: {
      whole_read: true,
      whole_write: false,
      read_documents: [],
      write_documents: [],
    },
    ...overrides,
  };
}

const wholeWrite = access({
  owner: true,
  can_manage: true,
  can_share: true,
  role: "admin",
  scope: {
    whole_read: true,
    whole_write: true,
    read_documents: [],
    write_documents: [],
  },
});
const fileWrite = access({
  scope: {
    whole_read: false,
    whole_write: false,
    read_documents: [],
    write_documents: ["a.md"],
  },
});
const readOnly = access({
  can_write: false,
  scope: {
    whole_read: true,
    whole_write: false,
    read_documents: [],
    write_documents: [],
  },
});

beforeEach(() => {
  fetchMock.mockReset();
  vi.stubGlobal("fetch", fetchMock);
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("documentExtension", () => {
  it("normalizes the suffix and ignores dotfiles", () => {
    expect(documentExtension("使用手册.PDF")).toBe(".pdf");
    expect(documentExtension("notes.tar.md")).toBe(".md");
    expect(documentExtension(".env")).toBe("");
    expect(documentExtension("no-extension")).toBe("");
  });
});

describe("documentFormatFamily", () => {
  it("collapses html and htm into one family", () => {
    expect(documentFormatFamily(".html")).toBe(".html");
    expect(documentFormatFamily(".htm")).toBe(".html");
    expect(documentFormatFamily(".HTM")).toBe(".html");
    expect(documentFormatFamily(".docx")).toBe(".docx");
    expect(documentFormatFamily(".doc")).toBe(".doc");
    expect(documentFormatFamily(".md")).toBe(".md");
    expect(documentFormatFamily("")).toBe("");
  });
});

describe("replaceFileError", () => {
  it("accepts a renamed file of the same type", () => {
    expect(replaceFileError("使用手册.pdf", { name: "使用手册-修订版.pdf", size: 1024 }))
      .toBeNull();
    expect(replaceFileError("manual.DOCX", { name: "manual-v2.docx", size: 2048 }))
      .toBeNull();
  });

  it("accepts html and htm files of the same, interchangeable family", () => {
    expect(replaceFileError("index.html", { name: "index-v2.html", size: 1024 }))
      .toBeNull();
    expect(replaceFileError("index.htm", { name: "index-v2.htm", size: 1024 }))
      .toBeNull();
    // HTML and HTM are two spellings of one format, so either may replace the
    // other even though the name changed.
    expect(replaceFileError("index.html", { name: "index.htm", size: 1024 }))
      .toBeNull();
    expect(replaceFileError("使用说明.HTM", { name: "使用说明.html", size: 1024 }))
      .toBeNull();
  });

  it("rejects a different type, including html against another format", () => {
    const problem = replaceFileError("manual.pdf", { name: "manual.txt", size: 10 });
    expect(problem).toContain("文件类型必须与原文档一致");
    expect(problem).toContain(".pdf");
    expect(problem).toContain(".txt");

    const toHtml = replaceFileError("manual.md", { name: "manual.html", size: 10 });
    expect(toHtml).toContain("文件类型必须与原文档一致");
    expect(toHtml).toContain(".md");
    expect(toHtml).toContain(".html");
    expect(replaceFileError("manual.html", { name: "manual.md", size: 10 }))
      .toContain("文件类型必须与原文档一致");
    expect(replaceFileError("manual.html", { name: "manual.docx", size: 10 }))
      .toContain("文件类型必须与原文档一致");
  });

  it("rejects unsupported, empty and oversized files", () => {
    expect(replaceFileError("manual.md", { name: "manual.exe", size: 10 }))
      .toContain("仅支持");
    expect(replaceFileError("manual.md", { name: "manual.xhtml", size: 10 }))
      .toContain("仅支持");
    expect(replaceFileError("manual.html", { name: "manual.exe", size: 10 }))
      .toContain("HTML");
    expect(replaceFileError("manual.md", { name: "manual.md", size: 0 }))
      .toContain("不能为空");
    expect(replaceFileError("manual.md", {
      name: "manual.md",
      size: 10 * 1024 * 1024 + 1,
    })).toContain("10 MiB");
    expect(replaceFileError("manual.html", {
      name: "manual.html",
      size: replaceMaxFileBytes + 1,
    })).toContain("10 MiB");
  });

  it("requires a selected file", () => {
    expect(replaceFileError("manual.md", undefined)).toContain("请选择");
  });
});

describe("replace picker hints", () => {
  it("offers both html spellings and one suffix for the other formats", () => {
    expect(replaceAcceptAttribute("a.htm")).toBe(".html,.htm");
    expect(replaceAcceptAttribute("a.HTML")).toBe(".html,.htm");
    expect(replaceAcceptAttribute("a.pdf")).toBe(".pdf");
    expect(replaceAcceptAttribute("README")).toBe("");

    expect(replaceTypeLabel("a.htm")).toBe(".html 或 .htm");
    expect(replaceTypeLabel("a.html")).toBe(".html 或 .htm");
    expect(replaceTypeLabel("a.docx")).toBe(".docx");
  });
});

describe("canReplaceDocument", () => {
  it("allows whole-write, the matching file grant and legacy mode", () => {
    expect(canReplaceDocument(wholeWrite, true, "a.md")).toBe(true);
    expect(canReplaceDocument(fileWrite, true, "a.md")).toBe(true);
    expect(canReplaceDocument(fileWrite, true, "b.md")).toBe(false);
    expect(canReplaceDocument(readOnly, true, "a.md")).toBe(false);
    expect(canReplaceDocument(undefined, true, "a.md")).toBe(true);
    expect(canReplaceDocument(undefined, false, "a.md")).toBe(true);
  });
});

describe("replaceDocument", () => {
  it("posts one multipart file to the encoded replace route", async () => {
    fetchMock.mockResolvedValue(jsonResponse({
      status: "updated",
      document: { name: "使用手册.md" },
      documents: [{ name: "使用手册.md" }],
    }));
    const connection: Connection = { namespace: "team-kb", token: "token-1" };
    const file = new File(["new content"], "使用手册-修订版.md", { type: "text/markdown" });

    const result = await replaceDocument("使用手册.md", file, connection);

    expect(result.status).toBe("updated");
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [path, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(path).toBe(
      "/v1/documents/%E4%BD%BF%E7%94%A8%E6%89%8B%E5%86%8C.md/replace",
    );
    expect(init.method).toBe("POST");
    const headers = new Headers(init.headers);
    expect(headers.get("X-RAG-Namespace")).toBe("team-kb");
    expect(headers.get("Authorization")).toBe("Bearer token-1");
    const body = init.body as FormData;
    expect(body).toBeInstanceOf(FormData);
    const posted = body.get("file") as File;
    expect(posted.name).toBe("使用手册-修订版.md");
    expect(await posted.text()).toBe("new content");
  });

  it("surfaces the server reason when the replacement is rejected", async () => {
    fetchMock.mockResolvedValue(jsonResponse(
      { detail: "替换文件的类型必须与原文档一致：原文档为 .md，新文件为 .pdf" },
      400,
    ));
    await expect(replaceDocument("a.md", new File(["x"], "a.pdf"), {
      namespace: "kb",
      token: "",
    })).rejects.toThrow(/HTTP 400：替换文件的类型必须与原文档一致/);
  });
});
