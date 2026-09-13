// API types mirroring the Go backend.

export interface AgentEvent {
  timestamp: string;
  session_id?: string;
  iteration?: number;
  type:
    | "user_message"
    | "llm_request"
    | "llm_response"
    | "llm_delta"
    | "tool_call"
    | "tool_result"
    | "agent_message"
    | "approval"
    | "agent_finished"
    | "error";
  tool?: string;
  success?: boolean;
  duration?: string;
  data?: Record<string, unknown>;
}

// UsageStats mirrors the Go session.UsageStats accumulator.
export interface UsageStats {
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
  llm_calls: number;
}

export interface ToolRecord {
  name: string;
  args: string;
  is_error: boolean;
  started_at: string;
  duration: string;
}

export interface Session {
  id: string;
  task: string;
  created_at: string;
  tool_history: ToolRecord[];
  modified_files: string[];
  usage?: UsageStats;
  final_answer?: string;
  done: boolean;
}

async function req<T>(method: string, url: string, body?: unknown): Promise<T> {
  const token = getToken();
  const headers: Record<string, string> = {};
  if (body) headers["Content-Type"] = "application/json";
  if (token) headers["Authorization"] = `Bearer ${token}`;
  const resp = await fetch(url, {
    method,
    headers,
    body: body ? JSON.stringify(body) : undefined,
  });
  if (!resp.ok) {
    const text = await resp.text();
    if (resp.status === 401 || resp.status === 403) {
      throw new Error(
        `unauthorized (${resp.status}) — set the API token (top-right "token" button)`
      );
    }
    throw new Error(`${method} ${url}: ${resp.status} ${text}`);
  }
  return (await resp.json()) as T;
}

const TOKEN_KEY = "local-codex.token";

// getToken prefers ?token= in the URL (persisting it to localStorage),
// then falls back to whatever was stored previously.
export function getToken(): string {
  const q = new URLSearchParams(window.location.search).get("token");
  if (q) {
    localStorage.setItem(TOKEN_KEY, q);
    return q;
  }
  return localStorage.getItem(TOKEN_KEY) ?? "";
}

export function setToken(token: string) {
  if (token) {
    localStorage.setItem(TOKEN_KEY, token);
  } else {
    localStorage.removeItem(TOKEN_KEY);
  }
}

// EventSource cannot set headers, so the token travels as a query param.
export function eventsURL(): string {
  return `/api/events?token=${encodeURIComponent(getToken())}`;
}

export interface ApprovalRequest {
  id: string;
  command: string;
  reason: string;
}

export const api = {
  createTask: (task: string) =>
    req<{ session_id: string }>("POST", "/api/tasks", { task }),
  getSession: (id: string) => req<Session>("GET", `/api/sessions/${id}`),
  getApproval: (id: string) =>
    req<ApprovalRequest>("GET", `/api/approvals/${encodeURIComponent(id)}`),
  listApprovals: () => req<ApprovalRequest[]>("GET", "/api/approvals"),
  gitStatus: () => req<{ status: string }>("GET", "/api/git/status"),
  gitDiff: () => req<{ diff: string }>("GET", "/api/git/diff"),
  files: (path: string) =>
    req<{ tree: string }>("GET", `/api/files?path=${encodeURIComponent(path)}`),
  approve: (id: string, approved: boolean) =>
    req<{ resolved: boolean }>("POST", "/api/approvals", { id, approved }),
};
