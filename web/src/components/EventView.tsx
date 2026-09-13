import { AgentEvent } from "../api";

// EventView renders one agent event. Only action summaries, tool calls,
// results and final text are shown — never raw chain-of-thought fields.
export function EventView({ event: e }: { event: AgentEvent }) {
  switch (e.type) {
    case "user_message":
      return (
        <div className="msg user">
          <div className="role">You</div>
          <div className="bubble">{String(e.data?.content ?? "")}</div>
        </div>
      );
    case "agent_message":
      return (
        <div className="msg agent">
          <div className="role">Agent</div>
          <div className="bubble pre">{String(e.data?.content ?? "")}</div>
        </div>
      );
    case "llm_delta":
      // Live stream of the assistant reply (deltas are think-filtered by the
      // backend). Aggregated per iteration by App.
      return (
        <div className="msg agent">
          <div className="role">Agent</div>
          <div className="bubble pre">
            {String(e.data?.content ?? "")}
            <span className="cursor">▍</span>
          </div>
        </div>
      );
    case "tool_call":
      return (
        <div className="tool-call">
          <span className="tool-name">⚙ {e.tool}</span>
          <code className="tool-args">{summarizeArgs(e)}</code>
        </div>
      );
    case "tool_result":
      return (
        <details className={"tool-result" + (e.success === false ? " failed" : "")}>
          <summary>
            {e.success === false ? "✗" : "✓"} {e.tool} result
            {e.duration ? ` (${e.duration})` : ""}
          </summary>
          <pre>{String(e.data?.content ?? "")}</pre>
        </details>
      );
    case "error":
      return <div className="error">Error: {String(e.data?.error ?? "")}</div>;
    case "context_compacted":
      return (
        <div className="compacted">
          Context compacted: {formatBytes(e.data?.bytes_before)} →{" "}
          {formatBytes(e.data?.bytes_after)} (earlier messages summarized)
        </div>
      );
    case "agent_finished":
      return (
        <div className={"finished" + (e.success === false ? " failed" : "")}>
          {e.success === false
            ? `Agent stopped: ${String(e.data?.error ?? "")}`
            : `Task finished in ${String(e.data?.iterations ?? "?")} iterations`}
        </div>
      );
    default:
      return null;
  }
}

function formatBytes(v: unknown): string {
  const n = Number(v ?? 0);
  if (n >= 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${n} B`;
}

function summarizeArgs(e: AgentEvent): string {
  const raw = e.data?.args;
  if (typeof raw !== "string") return "";
  try {
    const obj = JSON.parse(raw) as Record<string, unknown>;
    return Object.entries(obj)
      .map(([k, v]) => {
        let s = String(v);
        if (s.length > 100) s = s.slice(0, 100) + "…";
        return `${k}=${JSON.stringify(s)}`;
      })
      .join(" ");
  } catch {
    return raw.length > 150 ? raw.slice(0, 150) + "…" : raw;
  }
}
