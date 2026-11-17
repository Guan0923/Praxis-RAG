import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { App, ConfigProvider } from "antd";
import { MembershipPage } from "./MembershipPage";
import {
  formatDateTime,
  type MembershipOverview,
  type MembershipView,
  type OrderView,
  type PlanCatalogItem,
  type PlanLimits,
} from "./api";

// jsonResponse builds the minimal Response surface the client touches.
function jsonResponse(payload: unknown, status = 200) {
  const text = JSON.stringify(payload);
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: "test",
    json: async () => JSON.parse(text),
    text: async () => text,
  } as unknown as Response;
}

type Handler = (path: string, init: RequestInit) => Response | undefined;

const fetchMock = vi.fn();

// installFetch serves the membership endpoints from an in-memory router; an
// unhandled path answers 404 like the real server, so a typo shows up as a
// failed request instead of a silently passing test.
function installFetch(handler: Handler) {
  fetchMock.mockImplementation(async (path: string, init: RequestInit = {}) => {
    const method = (init.method ?? "GET").toUpperCase();
    const response = handler(path, { ...init, method });
    return response ?? jsonResponse({ detail: "not found" }, 404);
  });
}

const freeLimits: PlanLimits = {
  knowledge_bases: 5,
  files_per_kb: 10,
  embedding_chars_per_month: 100_000,
  unlimited_knowledge_bases: false,
  unlimited_files: false,
};

const freePlan: PlanCatalogItem = {
  plan: "free",
  name: "Free",
  monthly_cents: 0,
  yearly_cents: 0,
  monthly_yuan: "0.00",
  yearly_yuan: "0.00",
  year_discount: false,
  limits: freeLimits,
  can_share: false,
};

const plusPlan: PlanCatalogItem = {
  plan: "plus",
  name: "Plus",
  monthly_cents: 1990,
  yearly_cents: 19104,
  monthly_yuan: "19.90",
  yearly_yuan: "191.04",
  year_discount: true,
  limits: {
    knowledge_bases: 20,
    files_per_kb: 100,
    embedding_chars_per_month: 10_000_000,
    unlimited_knowledge_bases: false,
    unlimited_files: false,
  },
  can_share: true,
};

const proPlan: PlanCatalogItem = {
  plan: "pro",
  name: "Pro",
  monthly_cents: 19990,
  yearly_cents: 57504,
  monthly_yuan: "199.90",
  yearly_yuan: "575.04",
  year_discount: false,
  limits: {
    knowledge_bases: -1,
    files_per_kb: -1,
    embedding_chars_per_month: 1_000_000_000,
    unlimited_knowledge_bases: true,
    unlimited_files: true,
  },
  can_share: true,
};

// membership mirrors GET /v1/membership for a fresh free account.
function membership(overrides: Partial<MembershipView> = {}): MembershipView {
  return {
    plan: "free",
    plan_name: "Free",
    source: "free",
    started_at: "2026-11-01T00:00:00+08:00",
    expires_at: null,
    refresh_at: "2026-12-01T00:00:00+08:00",
    limits: freeLimits,
    usage: {
      knowledge_bases: 1,
      embedding_allowance: 100_000,
      embedding_used: 0,
      embedding_reserved: 0,
      period_start: "2026-11-01T00:00:00+08:00",
      period_end: "2026-12-01T00:00:00+08:00",
    },
    can_share: false,
    upgrade_plan: "pro",
    purchase: { available: true, paused: false, reason: null },
    ...overrides,
  };
}

// order mirrors one OrderView row, defaulting to a payable Plus monthly card.
function order(overrides: Partial<OrderView> = {}): OrderView {
  return {
    id: "PR202611170001",
    status: "pending",
    status_label: "待支付",
    plan: "plus",
    plan_name: "Plus",
    period: "month",
    period_label: "月卡",
    kind: "purchase",
    kind_label: "新购",
    amount_cents: 1990,
    amount_yuan: "19.90",
    currency: "CNY",
    quote: {
      starts_at: "2026-11-17T00:00:00+08:00",
      ends_at: "2026-12-17T00:00:00+08:00",
      current_plan: "free",
      target_plan: "plus",
      segments: [],
    },
    alipay_trade_no: null,
    created_at: "2026-11-17T10:00:00+08:00",
    expires_at: "2026-11-17T10:30:00+08:00",
    paid_at: null,
    refund_status: null,
    refund_amount_yuan: null,
    ...overrides,
  };
}

function overview(overrides: Partial<MembershipOverview> = {}): MembershipOverview {
  return {
    membership: membership(),
    plans: [freePlan, plusPlan, proPlan],
    orders: [],
    ...overrides,
  };
}

function renderPage(role?: "admin" | "vip" | "user") {
  return render(
    <ConfigProvider button={{ autoInsertSpace: false }}>
      <App>
        <MembershipPage role={role} />
      </App>
    </ConfigProvider>,
  );
}

async function waitForOverview() {
  await waitFor(() => expect(screen.getByText("长期 Free")).toBeTruthy());
}

beforeEach(() => {
  fetchMock.mockReset();
  vi.stubGlobal("fetch", fetchMock);
  vi.stubGlobal("ResizeObserver", class {
    observe() { /* jsdom has no layout */ }
    unobserve() { /* jsdom has no layout */ }
    disconnect() { /* jsdom has no layout */ }
  });
  Object.defineProperty(window, "matchMedia", {
    writable: true,
    configurable: true,
    value: vi.fn().mockImplementation((query: string) => ({
      matches: false,
      media: query,
      onchange: null,
      addListener: vi.fn(),
      removeListener: vi.fn(),
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      dispatchEvent: vi.fn(),
    })),
  });
  window.history.replaceState(null, "", "#/membership");
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe("MembershipPage overview", () => {
  it("renders the current Free plan and the quota numbers from the server", async () => {
    installFetch((path) => path === "/v1/membership" ? jsonResponse(overview()) : undefined);
    renderPage();

    await waitForOverview();
    expect(screen.getAllByText("Free").length).toBeGreaterThan(0);
    expect(screen.getByText("免费版")).toBeTruthy();
    expect(screen.getByText("1 / 5")).toBeTruthy();
    expect(screen.getByText("0 / 100,000")).toBeTruthy();
    expect(screen.getByText(/预留 0 · 剩余 100,000/)).toBeTruthy();
    // Free is never purchasable: the card button is the disabled "当前套餐".
    const freeButton = screen.getByRole("button", { name: "当前套餐" }) as HTMLButtonElement;
    expect(freeButton.disabled).toBe(true);
    // The plan catalog is rendered from the server rows.
    expect(screen.getByLabelText("Free 套餐")).toBeTruthy();
    expect(screen.getByLabelText("Plus 套餐")).toBeTruthy();
    expect(screen.getByLabelText("Pro 套餐")).toBeTruthy();
  });
});

describe("MembershipPage checkout", () => {
  it("switches to the yearly card and shows the Plus discount price", async () => {
    installFetch((path) => path === "/v1/membership" ? jsonResponse(overview()) : undefined);
    renderPage();
    await waitForOverview();

    fireEvent.click(screen.getByText("年卡"));

    await waitFor(() => expect(screen.getByText("¥191.04")).toBeTruthy());
    // Only Plus carries the yearly discount badge.
    expect(screen.getAllByText("年付八折")).toHaveLength(1);
    expect(screen.getByText("¥575.04")).toBeTruthy();
    expect(within(screen.getByLabelText("Plus 套餐")).getByText("年付八折")).toBeTruthy();
    expect(within(screen.getByLabelText("Pro 套餐")).queryByText("年付八折")).toBeNull();
  });

  it("creates the order and posts the server-signed Alipay form", async () => {
    const requests: { path: string; method: string; body: unknown }[] = [];
    const submitted: HTMLFormElement[] = [];
    vi.spyOn(HTMLFormElement.prototype, "submit").mockImplementation(function (
      this: HTMLFormElement,
    ) {
      submitted.push(this);
    });
    const pay = {
      gateway: "https://openapi.alipay.com/gateway.do",
      params: {
        app_id: "2021000000000000",
        method: "alipay.trade.page.pay",
        out_trade_no: "PR202611170002",
        total_amount: "19.90",
        sign: "server-signature",
      },
    };
    installFetch((path, init) => {
      if (path === "/v1/membership") return jsonResponse(overview());
      if (path === "/v1/membership/orders" && init.method === "POST") {
        requests.push({ path, method: init.method, body: JSON.parse(String(init.body)) });
        return jsonResponse({ order: order({ id: "PR202611170002" }), pay });
      }
      if (path === "/v1/membership/orders") return jsonResponse({ orders: [] });
      return undefined;
    });
    renderPage();
    await waitForOverview();

    const plusCard = screen.getByLabelText("Plus 套餐");
    fireEvent.click(within(plusCard).getByRole("button", { name: "立即开通" }));

    // The Console only submits plan+period; the server prices the order.
    await waitFor(() => expect(requests).toHaveLength(1));
    expect(requests[0].body).toEqual({ plan: "plus", period: "month" });

    // The confirmation dialog shows the server amount and validity before the
    // gateway redirect.
    const payInfo = await screen.findByLabelText("订单支付信息");
    expect(within(payInfo).getByText("¥19.90")).toBeTruthy();
    expect(within(payInfo).getByText(formatDateTime(order().quote?.starts_at))).toBeTruthy();
    expect(within(payInfo).getByText(formatDateTime(order().quote?.ends_at))).toBeTruthy();

    fireEvent.click(screen.getByRole("button", { name: "前往支付" }));

    await waitFor(() => expect(submitted).toHaveLength(1));
    const form = submitted[0];
    expect(form.method.toUpperCase()).toBe("POST");
    expect(form.action).toBe(pay.gateway);
    const fields = Array.from(form.querySelectorAll("input")).reduce<Record<string, string>>(
      (result, input) => {
        result[input.name] = input.value;
        return result;
      },
      {},
    );
    expect(fields).toEqual(pay.params);
    // The transient form is removed again after submitting.
    expect(document.querySelector("form")).toBeNull();
  });
});

describe("MembershipPage orders", () => {
  it("labels order states and closes a pending order", async () => {
    const closed: string[] = [];
    const pending = order({ id: "PR-PENDING", status: "pending", status_label: "待支付" });
    const paid = order({
      id: "PR-PAID",
      status: "paid",
      status_label: "已支付",
      alipay_trade_no: "2026111722001456789",
      paid_at: "2026-11-17T10:05:00+08:00",
    });
    installFetch((path, init) => {
      if (path === "/v1/membership") return jsonResponse(overview({ orders: [pending, paid] }));
      if (path === "/v1/membership/orders/PR-PENDING/close" && init.method === "POST") {
        closed.push(path);
        return jsonResponse({
          order: order({ id: "PR-PENDING", status: "closed", status_label: "已关闭" }),
        });
      }
      if (path === "/v1/membership/orders") return jsonResponse({ orders: [pending, paid] });
      return undefined;
    });
    renderPage();
    await waitForOverview();

    expect(screen.getByText("已支付")).toBeTruthy();
    expect(screen.getByText(/交易号 2026111722001456789/)).toBeTruthy();
    expect(screen.getByText("待支付")).toBeTruthy();
    // Only the pending order offers actions.
    expect(screen.getAllByRole("button", { name: "关闭订单" })).toHaveLength(1);
    expect(screen.getAllByRole("button", { name: "继续支付" })).toHaveLength(1);

    fireEvent.click(screen.getByRole("button", { name: "关闭订单" }));

    await waitFor(() => expect(closed).toEqual(["/v1/membership/orders/PR-PENDING/close"]));
    await waitFor(() => expect(screen.getByText("已关闭")).toBeTruthy());
  });

  it("reads out_trade_no on mount, cleans the query and shows only the server status", async () => {
    window.history.replaceState(null, "", "/console?out_trade_no=PR-PAID");
    const queried: string[] = [];
    installFetch((path) => {
      if (path === "/v1/membership/orders/PR-PAID?refresh=1") {
        queried.push(path);
        return jsonResponse({
          order: order({ id: "PR-PAID", status: "paid", status_label: "已支付" }),
        });
      }
      if (path === "/v1/membership") return jsonResponse(overview());
      return undefined;
    });
    renderPage();

    await waitFor(() => expect(queried).toHaveLength(1));
    await waitFor(() => expect(screen.getByText("支付结果：已支付")).toBeTruthy());
    // The query is stripped so a reload cannot replay the notice.
    expect(window.location.search).toBe("");
  });
});

describe("MembershipPage admin reconciliation", () => {
  const adminOrders = [
    { ...order({ id: "PR-ADMIN", status: "paid", status_label: "已支付" }), username: "alice" },
  ];

  it("shows the reconciliation section to administrators only", async () => {
    installFetch((path) => {
      if (path === "/v1/membership") return jsonResponse(overview());
      if (path.startsWith("/v1/admin/billing/orders")) {
        return jsonResponse({ orders: adminOrders });
      }
      return undefined;
    });

    renderPage("user");
    await waitForOverview();
    expect(screen.queryByText("订单对账")).toBeNull();

    cleanup();
    renderPage("admin");
    // The load button carries an icon, so match the label instead of the full
    // accessible name.
    await waitFor(() => expect(screen.getByRole("button", { name: /加载订单/ })).toBeTruthy());
    expect(screen.getByText("订单对账")).toBeTruthy();
    // The section loads the reconciliation list on mount.
    await waitFor(() =>
      expect(fetchMock.mock.calls.some(
        ([path]) => String(path).startsWith("/v1/admin/billing/orders"),
      )).toBe(true));
    await waitFor(() => expect(screen.getByText("alice")).toBeTruthy());
  });
});
