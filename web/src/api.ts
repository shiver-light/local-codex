// API types mirroring the Go backend.

export interface AgentEvent {
  timestamp: string;
  session_id?: string;
  iteration?: number;
  type:
    | "user_message"
    | "llm_request"
    | "llm_response"
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
  final_answer?: string;
  done: boolean;
}

async function req<T>(method: string, url: string, body?: unknown): Promise<T> {
  const resp = await fetch(url, {
    method,
    headers: body ? { "Content-Type": "application/json" } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  });
  if (!resp.ok) {
    const text = await resp.text();
    throw new Error(`${method} ${url}: ${resp.status} ${text}`);
  }
  return (await resp.json()) as T;
}

export const api = {
  createTask: (task: string) =>
    req<{ session_id: string }>("POST", "/api/tasks", { task }),
  getSession: (id: string) => req<Session>("GET", `/api/sessions/${id}`),
  gitStatus: () => req<{ status: string }>("GET", "/api/git/status"),
  gitDiff: () => req<{ diff: string }>("GET", "/api/git/diff"),
  files: (path: string) =>
    req<{ tree: string }>("GET", `/api/files?path=${encodeURIComponent(path)}`),
  approve: (id: string, approved: boolean) =>
    req<{ resolved: boolean }>("POST", "/api/approvals", { id, approved }),
};
