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
          fontSize: 14,
        },
        components: {
          Button: { primaryShadow: "none" },
          Table: { headerBg: dark ? "#242424" : "#f7f7f8" },
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
