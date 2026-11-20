import { useState } from "react";
import { createRoot } from "react-dom/client";
import { App as AntApp, ConfigProvider, theme } from "antd";
import zhCN from "antd/locale/zh_CN";
import { Console } from "./Console";
import "./styles.css";

function Root() {
  const [dark, setDark] = useState(
    document.documentElement.dataset.theme === "dark",
  );
  function toggleTheme() {
    const next = !dark;
    document.documentElement.dataset.theme = next ? "dark" : "light";
    try {
      localStorage.setItem("rag.theme", next ? "dark" : "light");
    } catch {
      /* Theme still works without storage. */
    }
    setDark(next);
  }
  return (
    <ConfigProvider
      locale={zhCN}
      button={{ autoInsertSpace: false }}
      theme={{
        algorithm: dark ? theme.darkAlgorithm : theme.defaultAlgorithm,
        token: {
          colorPrimary: dark ? "#79a6ff" : "#2563eb",
          borderRadius: 4,
          colorBgBase: dark ? "#191919" : "#ffffff",
          colorTextBase: dark ? "#eeeeee" : "#202124",
          fontFamily: '"Segoe UI", "Microsoft YaHei", sans-serif',
          // 管理台以密度优先：13px 正文与 30px 控件高度让表格和表单在
          // 1080p 及以下分辨率也能一屏看到更多条目。
          fontSize: 13,
          controlHeight: 30,
        },
        components: {
          Button: { primaryShadow: "none" },
          Table: {
            headerBg: dark ? "#242424" : "#f7f7f8",
            // 三档表格尺寸统一收紧行内边距，条目更紧凑但不贴边。
            cellPaddingBlock: 10,
            cellPaddingInline: 8,
            cellPaddingBlockMD: 8,
            cellPaddingInlineMD: 7,
            cellPaddingBlockSM: 6,
            cellPaddingInlineSM: 6,
          },
          Tabs: { horizontalItemPadding: "8px 0", horizontalMargin: "0 0 8px" },
          Pagination: { itemSize: 28 },
        },
      }}
    >
      <AntApp>
        <Console dark={dark} toggleTheme={toggleTheme} />
      </AntApp>
    </ConfigProvider>
  );
}

createRoot(document.getElementById("root")!).render(<Root />);
