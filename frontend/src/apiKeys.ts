import type { Role } from "./api";

// ApiKeyPageRole is the account role the API Key page was opened with;
// undefined means legacy/no-session local management.
export type ApiKeyPageRole = Role | undefined;

// isAdminLike decides whether the caller manages every API key: administrators
// always do, and so does legacy no-session local management. VIP and ordinary
// accounts only manage the keys they created.
export function isAdminLike(role: ApiKeyPageRole) {
  return role === undefined || role === "admin";
}

// apiKeyNamespaceOptions builds the namespace picker options. A non-admin
// account may only bind knowledge bases it owns, so it sees only
// ownedNamespaces; administrators and legacy local management see every
// namespace.
export function apiKeyNamespaceOptions(
  namespaces: string[],
  ownedNamespaces: string[] | undefined,
  role: ApiKeyPageRole,
) {
  const values = isAdminLike(role) ? namespaces : ownedNamespaces ?? [];
  return [...new Set(values)].map((value) => ({ value, label: value }));
}

// apiKeyOwnerText renders the owner column; legacy keys have no owner account.
export function apiKeyOwnerText(ownerUsername?: string) {
  return ownerUsername ? ownerUsername : "—（未绑定）";
}
