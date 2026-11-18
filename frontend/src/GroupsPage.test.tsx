import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { App, ConfigProvider } from "antd";
import { GroupsPage, groupInvitationStatus } from "./GroupsPage";
import {
  type GroupInvitationView,
  type GroupMemberView,
  type GroupView,
  type InviteUser,
} from "./api";

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
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

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

// installFetch serves the group endpoints from an in-memory router.
function installFetch(handler: Handler) {
  fetchMock.mockImplementation(async (path: string, init: RequestInit = {}) => {
    const method = (init.method ?? "GET").toUpperCase();
    const response = handler(path, { ...init, method });
    return response ?? jsonResponse({ detail: "not found" }, 404);
  });
}

const ownerGroup: GroupView = {
  id: "g-1",
  name: "行业研究组",
  owner_user_id: "u-vip",
  owner_username: "carol",
  status: "active",
  member_count: 1,
  created_at: "2026-11-18T10:00:00.000000+00:00",
  updated_at: "2026-11-18T10:00:00.000000+00:00",
  dissolved_at: null,
};

const joinedGroup: GroupView = {
  ...ownerGroup,
  id: "g-2",
  name: "另一个组",
  owner_user_id: "u-other",
  owner_username: "erin",
  member_count: 3,
};

const members: GroupMemberView[] = [{
  user_id: "u-user",
  username: "bob",
  role: "user",
  enabled: true,
  joined_at: "2026-11-18T10:05:00.000000+00:00",
}];

function renderPage(role: "admin" | "vip" | "user") {
  return render(
    <ConfigProvider>
      <App>
        <GroupsPage role={role} onOpenInvite={() => {}} />
      </App>
    </ConfigProvider>,
  );
}

describe("groupInvitationStatus", () => {
  it("marks a pending invitation expired once its TTL elapsed", () => {
    const pending: GroupInvitationView = {
      id: "gi-1", group_id: "g-1", group_name: "行业研究组",
      inviter_user_id: "u-vip", inviter_username: "carol",
      invitee_user_id: "u-user", invitee_username: "bob",
      status: "pending",
      created_at: "2026-11-01T10:00:00.000000+00:00",
      expires_at: "2026-11-08T10:00:00.000000+00:00",
      accepted_at: null,
    };
    expect(groupInvitationStatus(pending)).toBe("expired");

    const fresh: GroupInvitationView = {
      ...pending,
      expires_at: "2999-11-08T10:00:00.000000+00:00",
    };
    expect(groupInvitationStatus(fresh)).toBe("pending");

    const decided: GroupInvitationView = { ...pending, status: "accepted" };
    expect(groupInvitationStatus(decided)).toBe("accepted");
  });
});

describe("GroupsPage", () => {

  it("shows the vip account its owned and joined groups with invitations", async () => {
    installFetch((path) => {
      if (path === "/v1/groups") {
        return jsonResponse({ owned_groups: [ownerGroup], joined_groups: [joinedGroup] });
      }
      if (path === "/v1/group-invitations") {
        return jsonResponse({ invitations: [] });
      }
      return undefined;
    });
    renderPage("vip");

    await waitFor(() => {
      expect(screen.getByText("行业研究组")).toBeTruthy();
      expect(screen.getByText("另一个组")).toBeTruthy();
    });
    expect(screen.getByText("我创建的用户组")).toBeTruthy();
    expect(screen.getByText("我加入的用户组")).toBeTruthy();
    expect(screen.getByRole("button", { name: /创建用户组/ })).toBeTruthy();
  });

  it("opens the member detail with the invite entry and dissolve control", async () => {
    installFetch((path) => {
      if (path === "/v1/groups") {
        return jsonResponse({ owned_groups: [ownerGroup], joined_groups: [] });
      }
      if (path === "/v1/group-invitations") {
        return jsonResponse({ invitations: [] });
      }
      if (path === "/v1/groups/g-1") {
        return jsonResponse({ group: ownerGroup, members, invitations: [] });
      }
      if (path === "/v1/users") {
        return jsonResponse({
          users: [
            { id: "u-user", username: "bob", role: "user" },
          ] satisfies InviteUser[],
        });
      }
      return undefined;
    });
    renderPage("vip");

    await waitFor(() => {
      expect(screen.getByText("行业研究组")).toBeTruthy();
    });
    const managed = await screen.findByRole("button", { name: /管理成员/ });
    fireEvent.click(managed);

    // The detail modal title carries the group name; the in-modal invite
    // button and dissolve control render alongside the member table.
    await waitFor(() => {
      expect(screen.getByText(/成员 · 行业研究组/)).toBeTruthy();
      expect(screen.getByRole("button", { name: /邀请成员/ })).toBeTruthy();
      expect(screen.getByText("bob")).toBeTruthy();
    });
    // The group dissolve control stays in the owned-groups table row.
    expect(screen.getByRole("button", { name: /解散 行业研究组/ })).toBeTruthy();
  });

  it("hides the create button from a plain user but still lists groups", async () => {
    installFetch((path) => {
      if (path === "/v1/groups") {
        return jsonResponse({ owned_groups: [], joined_groups: [joinedGroup] });
      }
      if (path === "/v1/group-invitations") {
        return jsonResponse({ invitations: [] });
      }
      return undefined;
    });
    renderPage("user");

    await waitFor(() => {
      expect(screen.getByText("我加入的用户组")).toBeTruthy();
    });
    expect(screen.getByText("另一个组")).toBeTruthy();
    // The create button is rendered but disabled for a plain user.
    expect((screen.getByRole("button", { name: /创建用户组/ }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("accepts a pasted group invitation link", async () => {
    installFetch((path, init) => {
      if (path === "/v1/groups") {
        return jsonResponse({ owned_groups: [], joined_groups: [] });
      }
      if (path === "/v1/group-invitations") {
        return jsonResponse({ invitations: [] });
      }
      if (path === "/v1/group-invitations/accept" && (init.method ?? "GET").toUpperCase() === "POST") {
        return jsonResponse({
          invitation: {
            id: "gi-9", group_id: "g-1", group_name: "行业研究组",
            inviter_user_id: "u-vip", inviter_username: "carol",
            invitee_user_id: "u-user", invitee_username: "bob",
            status: "accepted", created_at: "2026-11-18T10:00:00.000000+00:00",
            expires_at: "2999-11-25T10:00:00.000000+00:00", accepted_at: null,
          } satisfies GroupInvitationView,
        });
      }
      return undefined;
    });
    renderPage("user");

    const entry = await screen.findByLabelText("粘贴用户组邀请链接");
    fireEvent.change(entry, { target: { value: "#/group-invite?token=abc" } });
    fireEvent.click(screen.getByRole("button", { name: /打开邀请/ }));

    await waitFor(() => {
      const calls = fetchMock.mock.calls.filter(([p]) => p === "/v1/group-invitations/accept");
      expect(calls.length).toBe(1);
      const body = JSON.parse(String(calls[0][1]?.body)) as { token: string };
      expect(body.token).toBe("abc");
    });
  });
});
