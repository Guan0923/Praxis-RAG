import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import {
  Alert,
  App,
  Button,
  Empty,
  Input,
  Modal,
  Progress,
  Segmented,
  Select,
  Spin,
  Table,
  Tag,
  Tooltip,
  type TableColumnsType,
} from "antd";
import { CrownOutlined, ReloadOutlined } from "@ant-design/icons";
import {
  closeMembershipOrder,
  createMembershipOrder,
  errorText,
  fetchAdminBillingOrders,
  fetchMembership,
  fetchMembershipOrder,
  fetchMembershipOrders,
  formatDateTime,
  formatQuota,
  manualAdminBillingOrder,
  orderStatusColor,
  orderStatusLabels,
  orderStatusText,
  queryAdminBillingOrder,
  refundAdminBillingOrder,
  submitAlipayForm,
  thousands,
  type AdminOrderView,
  type BillingPeriod,
  type CreateOrderResult,
  type MembershipOverview,
  type MembershipView,
  type OrderView,
  type PlanCatalogItem,
  type Role,
} from "./api";

type Props = {
  role?: Role;
  // onPlanName keeps the Console's cached account badge in sync after a load.
  onPlanName?: (planName: string) => void;
};

// --- Display helpers ---------------------------------------------------------

// limitText renders one plan limit: the server's unlimited flag wins over a
// non-positive placeholder number.
export function limitText(limit: number | undefined, unlimited?: boolean) {
  if (unlimited || (limit ?? 0) < 0) return "不限";
  return thousands(limit ?? 0);
}

function planRank(plan: string | undefined) {
  switch (plan) {
    case "pro":
      return 3;
    case "plus":
      return 2;
    default:
      return 1;
  }
}

export type PlanAction = {
  label: string;
  disabled: boolean;
  intent?: "upgrade";
  hint?: string;
};

// planActionFor is the single rule for every catalog card button. The server
// owns prices and availability, so the label only reflects membership state and
// the purchase switch: Free is never purchasable, a higher tier is an upgrade
// (intent=upgrade), the current paid tier renews, a lower tier stays disabled.
export function planActionFor(
  membership: MembershipView,
  item: PlanCatalogItem,
): PlanAction {
  if (item.plan === "free") {
    return {
      label: membership.plan === "free" ? "当前套餐" : "已包含",
      disabled: true,
    };
  }
  const purchase = membership.purchase;
  if (!purchase || !purchase.available) {
    return {
      label: "暂不可购买",
      disabled: true,
      hint: purchase?.reason || "购买通道暂未开放",
    };
  }
  if (item.plan === membership.plan) return { label: "续费", disabled: false };
  if (membership.plan === "plus" && membership.upgrade_plan === item.plan) {
    return { label: `升级到 ${item.name}`, disabled: false, intent: "upgrade" };
  }
  if (planRank(item.plan) > planRank(membership.plan)) {
    return { label: "立即开通", disabled: false };
  }
  return { label: "当前套餐更高", disabled: true };
}

function priceText(item: PlanCatalogItem, period: BillingPeriod) {
  if (item.plan === "free" || item.monthly_cents === 0 && item.yearly_cents === 0) {
    return "免费";
  }
  return `¥${period === "year" ? item.yearly_yuan : item.monthly_yuan}`;
}

// --- Small layout primitives -------------------------------------------------

function Metric({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 4, minWidth: 0 }}>
      <span className="secondary">{label}</span>
      <span style={{ fontSize: 18, fontWeight: 600, minWidth: 0 }}>{children}</span>
    </div>
  );
}

function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div style={{ display: "grid", gridTemplateColumns: "96px 1fr", gap: 8 }}>
      <span className="secondary">{label}</span>
      <span style={{ minWidth: 0 }}>{children}</span>
    </div>
  );
}

// --- Orders table ------------------------------------------------------------

function orderActionable(order: OrderView) {
  return order.status === "pending";
}

// --- Admin reconciliation ----------------------------------------------------

const adminStatusOptions = [
  { value: "", label: "全部状态" },
  ...Object.entries(orderStatusLabels).map(([value, label]) => ({ value, label })),
];

function AdminBillingSection() {
  const { message, modal } = App.useApp();
  const [rows, setRows] = useState<AdminOrderView[]>([]);
  const [status, setStatus] = useState("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [loaded, setLoaded] = useState(false);
  const [busy, setBusy] = useState<string>();
  const [refundTarget, setRefundTarget] = useState<AdminOrderView>();
  const [refundReason, setRefundReason] = useState("");
  const [manualTarget, setManualTarget] = useState<AdminOrderView>();
  const [manualNote, setManualNote] = useState("");

  const load = useCallback(async (nextStatus = status) => {
    setLoading(true);
    setError("");
    try {
      const orders = await fetchAdminBillingOrders(nextStatus);
      setRows(orders);
      setLoaded(true);
    } catch (loadError) {
      setRows([]);
      setError(errorText(loadError));
    } finally {
      setLoading(false);
    }
  }, [status]);

  useEffect(() => {
    void load("");
    // The reconciliation list is loaded once when an administrator opens the
    // page; later refreshes use the filter/refresh controls.
  }, []);

  function replaceRow(next: OrderView) {
    setRows((current) => current.map((row) => (row.id === next.id ? { ...row, ...next } : row)));
  }

  async function queryOrder(row: AdminOrderView) {
    if (busy) return;
    setBusy(row.id);
    try {
      const next = await queryAdminBillingOrder(row.id);
      replaceRow(next);
      void message.success(`订单 ${row.id} 状态：${orderStatusText(next)}`);
    } catch (queryError) {
      void message.error(errorText(queryError));
    } finally {
      setBusy(undefined);
    }
  }

  function startRefund(row: AdminOrderView) {
    setRefundTarget(row);
    setRefundReason("");
  }

  function confirmRefund() {
    const target = refundTarget;
    const reason = refundReason.trim();
    if (!target) return;
    if (!reason) {
      void message.warning("请填写退款原因");
      return;
    }
    setRefundTarget(undefined);
    // Second confirmation: a whole-order refund cannot be undone.
    modal.confirm({
      title: "确认整单退款？",
      content: `订单 ${target.id}（${target.plan_name} · ${target.period_label} · ¥${target.amount_yuan}）将整单退款。退款后已用记录与文档保留，套餐权限按服务端结果回收。`,
      okText: "确认退款",
      cancelText: "取消",
      okButtonProps: { danger: true },
      onOk: async () => {
        setBusy(target.id);
        try {
          const next = await refundAdminBillingOrder(target.id, reason);
          replaceRow(next);
          void message.success(`已提交退款：${next.status_label}`);
        } catch (refundError) {
          void message.error(errorText(refundError));
          throw refundError;
        } finally {
          setBusy(undefined);
        }
      },
    });
  }

  function startManual(row: AdminOrderView) {
    setManualTarget(row);
    setManualNote("");
  }

  async function confirmManual() {
    const target = manualTarget;
    const note = manualNote.trim();
    if (!target) return;
    if (!note) {
      void message.warning("请填写人工处理备注");
      return;
    }
    setBusy(target.id);
    try {
      const next = await manualAdminBillingOrder(target.id, note);
      replaceRow(next);
      setManualTarget(undefined);
      void message.success(`已标记人工处理：${next.status_label}`);
    } catch (manualError) {
      void message.error(errorText(manualError));
    } finally {
      setBusy(undefined);
    }
  }

  const columns: TableColumnsType<AdminOrderView> = [
    {
      title: "用户",
      key: "username",
      width: 84,
      render: (_, row) => <span className="break-text">{row.username || "—"}</span>,
    },
    {
      title: "订单号",
      key: "id",
      render: (_, row) => (
        <div>
          <code className="break-text">{row.id}</code>
          <div className="secondary">{formatDateTime(row.created_at)}</div>
        </div>
      ),
    },
    {
      title: "套餐",
      key: "plan",
      width: 112,
      render: (_, row) => (
        <span className="break-text">
          {row.plan_name} · {row.period_label} · {row.kind_label}
        </span>
      ),
    },
    {
      title: "金额",
      key: "amount",
      width: 72,
      render: (_, row) => <span>¥{row.amount_yuan}</span>,
    },
    {
      title: "状态",
      key: "status",
      width: 84,
      render: (_, row) => (
        <Tag color={orderStatusColor(row)}>{orderStatusText(row)}</Tag>
      ),
    },
    {
      title: "操作",
      key: "actions",
      width: 156,
      render: (_, row) => (
        <div style={{ display: "flex", gap: 4, flexWrap: "wrap" }}>
          <Button size="small" loading={busy === row.id} onClick={() => void queryOrder(row)}>
            查单
          </Button>
          <Button
            size="small"
            danger
            disabled={row.status !== "paid" && row.status !== "manual"}
            onClick={() => startRefund(row)}
          >
            整单退款
          </Button>
          <Button size="small" onClick={() => startManual(row)}>
            人工处理
          </Button>
        </div>
      ),
    },
  ];

  return (
    <section className="settings-section" aria-label="订单对账" style={{ marginTop: 24 }}>
      <div className="section-heading">
        <h2>订单对账</h2>
        <span className="secondary">
          以服务端订单为准；退款保留已用记录与文档，失败订单可标记人工处理。
        </span>
      </div>
      <div className="toolbar" style={{ marginBottom: 12, gap: 8, flexWrap: "wrap" }}>
        <Select
          aria-label="订单状态筛选"
          value={status}
          style={{ minWidth: 140 }}
          options={adminStatusOptions}
          onChange={(value) => {
            setStatus(value);
            void load(value);
          }}
        />
        <Button
          icon={<ReloadOutlined />}
          loading={loading}
          onClick={() => void load()}
        >
          加载订单
        </Button>
        {loaded && <span className="secondary">共 {rows.length} 笔</span>}
      </div>
      {error && (
        <Alert
          className="page-alert"
          type="error"
          showIcon
          title="订单加载失败"
          description={error}
          action={<Button size="small" onClick={() => void load()}>重试</Button>}
        />
      )}
      <Table<AdminOrderView>
        rowKey="id"
        size="small"
        dataSource={rows}
        loading={loading}
        columns={columns}
        pagination={{ defaultPageSize: 10, hideOnSinglePage: true, showSizeChanger: false, size: "small" }}
        locale={{
          emptyText: (
            <Empty
              image={Empty.PRESENTED_IMAGE_SIMPLE}
              description={loaded ? "没有符合条件的订单" : "尚未加载订单"}
            />
          ),
        }}
      />
      <Modal
        title="整单退款"
        open={!!refundTarget}
        okText="确认退款"
        cancelText="取消"
        okButtonProps={{ danger: true }}
        onOk={confirmRefund}
        onCancel={() => setRefundTarget(undefined)}
      >
        <div style={{ display: "grid", gap: 12 }} aria-label="退款确认">
          <div className="secondary break-text">
            订单 {refundTarget?.id} · {refundTarget?.plan_name} · {refundTarget?.period_label} · ¥
            {refundTarget?.amount_yuan}
          </div>
          <div className="secondary">
            退款后已用记录与文档保留，套餐权限按服务端结果回收；此操作不可撤销。
          </div>
          <Input.TextArea
            aria-label="退款原因"
            rows={3}
            maxLength={200}
            value={refundReason}
            placeholder="填写退款原因（必填，用于对账留档）"
            onChange={(event) => setRefundReason(event.target.value)}
          />
        </div>
      </Modal>
      <Modal
        title="标记人工处理"
        open={!!manualTarget}
        okText="提交"
        cancelText="取消"
        onOk={() => void confirmManual()}
        onCancel={() => setManualTarget(undefined)}
      >
        <div style={{ display: "grid", gap: 12 }}>
          <div className="secondary break-text">
            订单 {manualTarget?.id} 将被标记为待人工处理，等待后台核对支付宝流水。
          </div>
          <Input.TextArea
            aria-label="人工处理备注"
            rows={3}
            maxLength={200}
            value={manualNote}
            placeholder="填写处理备注（必填）"
            onChange={(event) => setManualNote(event.target.value)}
          />
        </div>
      </Modal>
    </section>
  );
}

// --- Page --------------------------------------------------------------------

export function MembershipPage({ role, onPlanName }: Props) {
  const { message } = App.useApp();
  const [overview, setOverview] = useState<MembershipOverview>();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [period, setPeriod] = useState<BillingPeriod>("month");
  const [ordering, setOrdering] = useState<string>();
  const [pendingOrder, setPendingOrder] = useState<CreateOrderResult>();
  const [callbackOrder, setCallbackOrder] = useState<OrderView>();
  const [callbackError, setCallbackError] = useState("");
  const [busyOrder, setBusyOrder] = useState<string>();
  const lifetime = useRef(new AbortController());
  // onPlanRef keeps the load callback stable: the Console passes a state setter,
  // and re-running the mount effect on every parent render would reload twice.
  const onPlanRef = useRef(onPlanName);
  onPlanRef.current = onPlanName;

  const isAdmin = role === "admin";
  const membership = overview?.membership;
  const plans = overview?.plans || [];
  const orders = overview?.orders || [];

  const load = useCallback(async (signal?: AbortSignal) => {
    setLoading(true);
    setError("");
    try {
      const data = await fetchMembership(signal);
      if (signal?.aborted) return;
      setOverview(data);
      onPlanRef.current?.(data.membership?.plan_name || "");
    } catch (loadError) {
      if (signal?.aborted) return;
      setOverview(undefined);
      setError(errorText(loadError));
    } finally {
      if (!signal?.aborted) setLoading(false);
    }
  }, []);

  const refreshOrders = useCallback(async () => {
    try {
      const rows = await fetchMembershipOrders();
      setOverview((current) => current ? { ...current, orders: rows } : current);
    } catch (refreshError) {
      void message.error(errorText(refreshError));
    }
  }, [message]);

  // The Alipay return URL is /console?out_trade_no=...: query the order with
  // refresh=1, show only the server status, then strip the query so a reload
  // cannot replay the notice.
  useEffect(() => {
    const controller = new AbortController();
    lifetime.current = controller;
    const params = new URLSearchParams(window.location.search);
    // Alipay appends its query to the configured return_url. The Console
    // return URL carries the route in the hash, so accept `out_trade_no` from
    // either the ordinary search string or the hash's query segment.
    const originalHash = window.location.hash;
    const hashQueryIndex = originalHash.indexOf("?");
    const hashPath = hashQueryIndex >= 0 ? originalHash.slice(0, hashQueryIndex) : originalHash;
    const hashParams = new URLSearchParams(
      hashQueryIndex >= 0 ? originalHash.slice(hashQueryIndex + 1) : "",
    );
    const tradeNo = params.get("out_trade_no") || hashParams.get("out_trade_no");
    if (!tradeNo) {
      void load(controller.signal);
      return () => controller.abort();
    }
    params.delete("out_trade_no");
    hashParams.delete("out_trade_no");
    const query = params.toString();
    const hashQuery = hashParams.toString();
    window.history.replaceState(
      null,
      "",
      `${window.location.pathname}${query ? `?${query}` : ""}${hashPath}${hashQuery ? `?${hashQuery}` : ""}`,
    );
    void (async () => {
      try {
        const result = await fetchMembershipOrder(tradeNo, true);
        if (!controller.signal.aborted) setCallbackOrder(result.order);
      } catch (callbackFailure) {
        if (!controller.signal.aborted) setCallbackError(errorText(callbackFailure));
      } finally {
        if (!controller.signal.aborted) await load(controller.signal);
      }
    })();
    return () => controller.abort();
    // Loading runs exactly once per mount; the callback trade number comes from
    // the URL that opened this page.
  }, []);

  async function placeOrder(item: PlanCatalogItem, intent?: "upgrade") {
    if (!overview || ordering) return;
    setOrdering(item.plan);
    try {
      const result = await createMembershipOrder(
        intent === "upgrade"
          ? { intent: "upgrade", plan: item.plan }
          : { plan: item.plan, period },
      );
      if (lifetime.current.signal.aborted) return;
      // Show the server-generated amount/validity before the gateway redirect.
      setPendingOrder(result);
      setOverview((current) => current
        ? {
            ...current,
            orders: [result.order, ...current.orders.filter((row) => row.id !== result.order.id)],
          }
        : current);
    } catch (orderError) {
      void message.error(errorText(orderError));
    } finally {
      setOrdering(undefined);
    }
  }

  function confirmPayment() {
    const pending = pendingOrder;
    if (!pending) return;
    const pay = pending.pay;
    if (!pay?.gateway || !pay.params) {
      void message.warning("服务端未返回支付参数，请在订单列表稍后重试");
      setPendingOrder(undefined);
      return;
    }
    try {
      submitAlipayForm(pay);
      setPendingOrder(undefined);
      void message.info("已在支付宝收银台新窗口打开，支付完成后返回本页刷新状态");
      void refreshOrders();
    } catch (submitError) {
      void message.error(errorText(submitError));
    }
  }

  // 继续支付 re-reads the order first: the server may have already received the
  // callback, in which case no gateway redirect is needed.
  async function continuePayment(order: OrderView) {
    if (busyOrder) return;
    setBusyOrder(order.id);
    try {
      const result = await fetchMembershipOrder(order.id, true);
      if (result.order) {
        setOverview((current) => current
          ? {
              ...current,
              orders: current.orders.map((row) =>
                row.id === result.order.id ? result.order : row),
            }
          : current);
      }
      if (result.pay?.gateway) {
        submitAlipayForm(result.pay);
        void message.info("已在支付宝收银台新窗口打开，支付完成后返回本页刷新状态");
      } else if (result.order.status === "pending") {
        void message.warning("订单仍待支付，暂未获取到支付参数，请稍后重试");
      } else {
        void message.info(`订单状态：${orderStatusText(result.order)}`);
      }
    } catch (paymentError) {
      void message.error(errorText(paymentError));
    } finally {
      setBusyOrder(undefined);
    }
  }

  async function closeOrder(order: OrderView) {
    if (busyOrder) return;
    setBusyOrder(order.id);
    try {
      const closed = await closeMembershipOrder(order.id);
      setOverview((current) => current
        ? {
            ...current,
            orders: current.orders.map((row) => (row.id === closed.id ? closed : row)),
          }
        : current);
      void message.success("订单已关闭");
    } catch (closeError) {
      void message.error(errorText(closeError));
    } finally {
      setBusyOrder(undefined);
    }
  }

  const orderColumns: TableColumnsType<OrderView> = [
    {
      title: "订单号",
      key: "id",
      render: (_, row) => (
        <div>
          <code className="break-text">{row.id}</code>
          <div className="secondary">创建于 {formatDateTime(row.created_at)}</div>
        </div>
      ),
    },
    {
      title: "套餐",
      key: "plan",
      width: 120,
      render: (_, row) => (
        <span className="break-text">
          {row.plan_name} · {row.period_label} · {row.kind_label}
        </span>
      ),
    },
    {
      title: "金额",
      key: "amount",
      width: 72,
      render: (_, row) => <span>¥{row.amount_yuan}</span>,
    },
    {
      title: "状态",
      key: "status",
      width: 84,
      render: (_, row) => <Tag color={orderStatusColor(row)}>{orderStatusText(row)}</Tag>,
    },
    {
      title: "支付信息",
      key: "paid",
      width: 136,
      render: (_, row) => (
        <div className="secondary break-text">
          {row.paid_at ? `支付于 ${formatDateTime(row.paid_at)}` : "尚未支付"}
          {row.alipay_trade_no && <div>交易号 {row.alipay_trade_no}</div>}
        </div>
      ),
    },
    {
      title: "操作",
      key: "actions",
      width: 104,
      render: (_, row) => {
        if (!orderActionable(row)) return <span className="secondary">—</span>;
        return (
          <div style={{ display: "flex", gap: 4, flexWrap: "wrap" }}>
            <Button
              size="small"
              loading={busyOrder === row.id}
              onClick={() => void continuePayment(row)}
            >
              继续支付
            </Button>
            <Button size="small" onClick={() => void closeOrder(row)}>
              关闭订单
            </Button>
          </div>
        );
      },
    },
  ];

  if (loading && !overview) {
    return (
      <div className="auth-shell">
        <Spin size="large" description="正在加载会员信息…" />
      </div>
    );
  }

  return (
    <>
      <div className="page-heading">
        <div className="page-heading-title">
          <h1>会员与用量</h1>
          {membership && <Tag color={membership.plan === "free" ? "default" : "gold"}>
            {membership.plan_name}
          </Tag>}
        </div>
        <div className="toolbar">
          <Tooltip title="刷新会员信息">
            <Button
              aria-label="刷新会员信息"
              icon={<ReloadOutlined />}
              loading={loading}
              onClick={() => void load()}
            />
          </Tooltip>
        </div>
      </div>
      {error && (
        <Alert
          className="page-alert"
          type="error"
          showIcon
          title="会员信息加载失败"
          description={error}
          action={<Button size="small" onClick={() => void load()}>重试</Button>}
        />
      )}
      {callbackOrder && (
        <Alert
          className="page-alert"
          type={callbackOrder.status === "paid" ? "success" : "info"}
          showIcon
          title={`支付结果：${orderStatusText(callbackOrder)}`}
          description={`订单 ${callbackOrder.id} · ${callbackOrder.plan_name} · ${callbackOrder.period_label} · ¥${callbackOrder.amount_yuan}。状态以服务端查询结果为准。`}
        />
      )}
      {callbackError && (
        <Alert
          className="page-alert"
          type="warning"
          showIcon
          title="无法确认支付结果"
          description={callbackError}
        />
      )}
      {membership && !membership.purchase?.available && (
        <Alert
          className="page-alert"
          type="warning"
          showIcon
          title="购买通道暂未开放"
          description={membership.purchase?.reason || "当前无法创建新订单，请稍后再试。"}
        />
      )}
      {membership && (
        <section
          aria-label="会员概览"
          style={{
            display: "grid",
            gridTemplateColumns: "repeat(auto-fit, minmax(190px, 1fr))",
            gap: 16,
            marginBottom: 20,
          }}
        >
          <Metric label="当前套餐">
            <Tag color={membership.plan === "free" ? "default" : "gold"} icon={<CrownOutlined />}>
              {membership.plan_name}
            </Tag>
            <span className="secondary" style={{ marginLeft: 8, fontSize: 13 }}>
              {membership.source === "paid" ? "付费会员" : "免费版"}
            </span>
          </Metric>
          <Metric label="到期时间">
            {membership.expires_at ? formatDateTime(membership.expires_at) : "长期 Free"}
          </Metric>
          <Metric label="额度刷新时间">{formatDateTime(membership.refresh_at)}</Metric>
          <Metric label="自有知识库">
            {thousands(membership.usage?.knowledge_bases ?? 0)} /{" "}
            {limitText(membership.limits?.knowledge_bases, membership.limits?.unlimited_knowledge_bases)}
          </Metric>
          <Metric label="每库文件上限">
            {limitText(membership.limits?.files_per_kb, membership.limits?.unlimited_files)}
          </Metric>
          <Metric label="分享权限">
            {membership.can_share ? "可创建分享邀请" : "暂不可分享"}
          </Metric>
        </section>
      )}
      {membership && (
        <section className="settings-section" aria-label="Embedding 用量" style={{ marginBottom: 20 }}>
          <div className="section-heading">
            <h2>Embedding 字符用量</h2>
            <span className="secondary">
              {formatDateTime(membership.usage?.period_start)} 至{" "}
              {formatDateTime(membership.usage?.period_end)}
            </span>
          </div>
          {(() => {
            const allowance = membership.usage?.embedding_allowance
              ?? membership.limits?.embedding_chars_per_month ?? 0;
            const used = membership.usage?.embedding_used ?? 0;
            const reserved = membership.usage?.embedding_reserved ?? 0;
            const remaining = Math.max(0, allowance - used - reserved);
            const percent = allowance > 0
              ? Math.min(100, Math.round(((used + reserved) / allowance) * 100))
              : 0;
            return (
              <>
                <div style={{ display: "flex", gap: 16, flexWrap: "wrap", alignItems: "baseline" }}>
                  <strong style={{ fontSize: 18 }}>
                    {formatQuota(used)} / {formatQuota(allowance)}
                  </strong>
                  <span className="secondary">
                    预留 {formatQuota(reserved)} · 剩余 {formatQuota(remaining)}
                  </span>
                </div>
                <Progress percent={percent} showInfo size="small" />
                <span className="secondary">下次刷新：{formatDateTime(membership.refresh_at)}</span>
              </>
            );
          })()}
        </section>
      )}
      <section className="settings-section" aria-label="套餐选择">
        <div className="section-heading">
          <h2>套餐</h2>
          <Segmented
            aria-label="计费周期"
            value={period}
            onChange={(value) => setPeriod(value as BillingPeriod)}
            options={[
              { value: "month", label: "月卡" },
              { value: "year", label: "年卡" },
            ]}
          />
        </div>
        {plans.length === 0 ? (
          <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="暂无可购买套餐" />
        ) : (
          <div
            style={{
              display: "grid",
              gridTemplateColumns: "repeat(auto-fit, minmax(230px, 1fr))",
              gap: 12,
            }}
          >
            {plans.map((item) => {
              const action = membership ? planActionFor(membership, item) : {
                label: "加载中…",
                disabled: true,
              };
              const yearly = period === "year";
              return (
                <div
                  key={item.plan}
                  aria-label={`${item.name} 套餐`}
                  style={{
                    border: item.plan === membership?.plan
                      ? "1px solid var(--ant-color-primary, #1677ff)"
                      : "1px solid var(--ant-color-border, #d9d9d9)",
                    borderRadius: 8,
                    padding: 16,
                    display: "flex",
                    flexDirection: "column",
                    gap: 10,
                  }}
                >
                  <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
                    <strong style={{ fontSize: 16 }}>{item.name}</strong>
                    {item.plan === membership?.plan && <Tag color="gold">当前套餐</Tag>}
                  </div>
                  <div>
                    <span style={{ fontSize: 26, fontWeight: 600 }}>
                      {priceText(item, period)}
                    </span>
                    {item.plan !== "free" && (
                      <span className="secondary"> / {yearly ? "年" : "月"}</span>
                    )}
                    {yearly && item.year_discount && (
                      <Tag color="red" style={{ marginLeft: 8 }}>年付八折</Tag>
                    )}
                  </div>
                  <ul className="secondary" style={{ margin: 0, paddingLeft: 18, display: "grid", gap: 4 }}>
                    <li>
                      知识库 {limitText(item.limits?.knowledge_bases, item.limits?.unlimited_knowledge_bases)} 个
                    </li>
                    <li>
                      每库文件 {limitText(item.limits?.files_per_kb, item.limits?.unlimited_files)} 个
                    </li>
                    <li>
                      每月 Embedding {formatQuota(item.limits?.embedding_chars_per_month ?? 0)} 字符
                    </li>
                    <li>{item.can_share ? "可创建分享邀请" : "不可创建分享邀请"}</li>
                  </ul>
                  <Tooltip title={action.disabled ? action.hint : undefined}>
                    <Button
                      type={item.plan === membership?.plan ? "default" : "primary"}
                      block
                      disabled={action.disabled}
                      loading={ordering === item.plan}
                      onClick={() => void placeOrder(item, action.intent)}
                    >
                      {action.label}
                    </Button>
                  </Tooltip>
                </div>
              );
            })}
          </div>
        )}
      </section>
      {membership && membership.plan === "plus" && (() => {
        const upgradeItem = plans.find((item) => item.plan === membership.upgrade_plan);
        if (!upgradeItem) return null;
        return (
          <Alert
            className="page-alert"
            type="info"
            showIcon
            title={`升级到 ${upgradeItem.name}`}
            description={`升级保留本月已用量与当前到期日，应付金额按服务端报价（¥${period === "year" ? upgradeItem.yearly_yuan : upgradeItem.monthly_yuan}）结算。`}
            action={(
              <Button type="primary" onClick={() => void placeOrder(upgradeItem, "upgrade")}>
                升级到 {upgradeItem.name}
              </Button>
            )}
          />
        );
      })()}
      <section className="settings-section" aria-label="我的订单" style={{ marginTop: 20 }}>
        <div className="section-heading">
          <h2>我的订单</h2>
          <Button size="small" icon={<ReloadOutlined />} onClick={() => void refreshOrders()}>
            刷新订单
          </Button>
        </div>
        <Table<OrderView>
          rowKey="id"
          size="small"
          dataSource={orders}
          columns={orderColumns}
          pagination={{ defaultPageSize: 10, hideOnSinglePage: true, showSizeChanger: false, size: "small" }}
          locale={{
            emptyText: (
              <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="暂无订单" />
            ),
          }}
        />
      </section>
      {isAdmin && <AdminBillingSection />}
      <Modal
        title="确认支付"
        open={!!pendingOrder}
        width={520}
        okText={pendingOrder?.pay?.gateway ? "前往支付" : "关闭"}
        cancelText="取消"
        onOk={confirmPayment}
        onCancel={() => setPendingOrder(undefined)}
      >
        {pendingOrder && (
          <div style={{ display: "grid", gap: 8 }} aria-label="订单支付信息">
            <Field label="订单号"><code className="break-text">{pendingOrder.order.id}</code></Field>
            <Field label="套餐">
              {pendingOrder.order.plan_name} · {pendingOrder.order.period_label} ·{" "}
              {pendingOrder.order.kind_label}
            </Field>
            <Field label="应付金额">
              <strong style={{ fontSize: 18 }}>¥{pendingOrder.order.amount_yuan}</strong>
            </Field>
            <Field label="生效时间">
              {formatDateTime(pendingOrder.order.quote?.starts_at)}
            </Field>
            <Field label="到期时间">
              {formatDateTime(pendingOrder.order.quote?.ends_at)}
            </Field>
            <p className="secondary" style={{ margin: 0 }}>
              金额与有效期来自服务端报价；点击「前往支付」将跳转支付宝收银台。
            </p>
          </div>
        )}
      </Modal>
    </>
  );
}
