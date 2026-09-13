import { useEffect, useRef, useState } from "react";
import { AgentEvent, api, eventsURL, getToken, setToken, UsageStats } from "./api";
import { EventView } from "./components/EventView";
import { ApprovalCard } from "./components/ApprovalCard";
import { SidePanel } from "./components/SidePanel";

export default function App() {
  const [events, setEvents] = useState<AgentEvent[]>([]);
  const [task, setTask] = useState("");
  const [sessionId, setSessionId] = useState<string | null>(null);
  const [running, setRunning] = useState(false);
  const [usage, setUsage] = useState<UsageStats | null>(null);
  const [pendingApprovals, setPendingApprovals] = useState<
    { id: string; command: string; reason: string }[]
  >([]);
  const [rightTab, setRightTab] = useState<"files" | "diff">("files");
  const chatRef = useRef<HTMLDivElement>(null);

  // Global SSE stream: append events, track approvals and completion.
  useEffect(() => {
    const es = new EventSource(eventsURL());
    // Recover approvals that were requested while we were disconnected
    // (page load, SSE reconnect): the broadcast is delivered only once.
    const refreshApprovals = () => {
      api
        .listApprovals()
        .then((list) =>
          setPendingApprovals(
            list.map((r) => ({ id: r.id, command: r.command, reason: r.reason }))
          )
        )
        .catch(() => {
          // unauthorized or transient failure; the approval events still work
        });
    };
    es.onopen = refreshApprovals;
    refreshApprovals();
    const refreshUsage = () => {
      if (!sessionId) return;
      api
        .getSession(sessionId)
        .then((s) => setUsage(s.usage ?? null))
        .catch(() => {
          // transient; the next llm_response/agent_finished refreshes again
        });
    };
    const types = [
      "user_message",
      "llm_delta",
      "llm_response",
      "agent_message",
      "tool_call",
      "tool_result",
      "approval",
      "agent_finished",
      "error",
    ];
    for (const t of types) {
      es.addEventListener(t, (ev) => {
        const e = JSON.parse((ev as MessageEvent).data) as AgentEvent;
        if (sessionId && e.session_id && e.session_id !== sessionId) return;
        if (e.type === "llm_delta") {
          // Aggregate deltas of one iteration into a single growing bubble.
          setEvents((prev) => {
            const last = prev[prev.length - 1];
            const chunk = String(e.data?.content ?? "");
            if (
              last?.type === "llm_delta" &&
              last.session_id === e.session_id &&
              last.iteration === e.iteration
            ) {
              const merged = {
                ...last,
                data: {
                  ...last.data,
                  content: String(last.data?.content ?? "") + chunk,
                },
              };
              return [...prev.slice(0, -1), merged];
            }
            return [...prev.slice(-500), { ...e, data: { ...e.data, content: chunk } }];
          });
          return;
        }
        if (e.type === "agent_message") {
          setEvents((prev) => {
            const last = prev[prev.length - 1];
            // The agent_message that closes a streamed iteration repeats the
            // text already shown via llm_delta: swap it in to drop the
            // "generating" styling instead of printing the message twice.
            if (
              last?.type === "llm_delta" &&
              last.session_id === e.session_id &&
              last.iteration === e.iteration &&
              String(last.data?.content ?? "").trim() ===
                String(e.data?.content ?? "").trim()
            ) {
              return [...prev.slice(0, -1), e];
            }
            return [...prev.slice(-500), e];
          });
          return;
        }
        if (e.type === "llm_response" || e.type === "agent_finished") {
          refreshUsage();
        }
        if (e.type === "approval") {
          // The broadcast carries only the approval id; fetch the full
          // request (command, reason) from the API.
          const id = String(e.data?.id);
          api
            .getApproval(id)
            .then((req) =>
              setPendingApprovals((prev) =>
                prev.some((a) => a.id === req.id)
                  ? prev
                  : [
                      ...prev,
                      { id: req.id, command: req.command, reason: req.reason },
                    ]
              )
            )
            .catch(() => {
              // approval may already be resolved; harmless
            });
        }
        if (e.type === "agent_finished") {
          setRunning(false);
          setRightTab("diff");
        }
        if (e.type === "error" && e.data?.retry !== true) {
          // A non-retry error ends the task; the backend also emits
          // agent_finished, this is a belt-and-braces reset.
          setRunning(false);
        }
        setEvents((prev) => [...prev.slice(-500), e]);
      });
    }
    return () => es.close();
  }, [sessionId]);

  useEffect(() => {
    chatRef.current?.scrollTo({ top: chatRef.current.scrollHeight });
  }, [events, pendingApprovals]);

  const submit = async () => {
    const text = task.trim();
    if (!text || running) return;
    setEvents([]);
    setUsage(null);
    setRunning(true);
    setTask("");
    try {
      const { session_id } = await api.createTask(text);
      setSessionId(session_id);
    } catch (err) {
      setRunning(false);
      setEvents((prev) => [
        ...prev,
        {
          timestamp: new Date().toISOString(),
          type: "error",
          data: { error: String(err) },
        },
      ]);
    }
  };

  const resolveApproval = async (id: string, approved: boolean) => {
    setPendingApprovals((prev) => prev.filter((a) => a.id !== id));
    try {
      await api.approve(id, approved);
    } catch {
      // approval may already be gone; harmless
    }
  };

  const editToken = () => {
    const t = window.prompt("API token (from server startup output)", getToken());
    if (t !== null) {
      setToken(t.trim());
      window.location.reload();
    }
  };

  return (
    <div className="layout">
      <header className="topbar">
        <span className="logo">local-codex</span>
        <span className={running ? "status running" : "status"}>
          {running ? "● agent running" : "○ idle"}
        </span>
        {usage && usage.llm_calls > 0 && (
          <span
            className="usage"
            title={`prompt ${usage.prompt_tokens} + completion ${usage.completion_tokens} tokens`}
          >
            {usage.total_tokens} tok · {usage.llm_calls} calls
          </span>
        )}
        <button className="token-btn" onClick={editToken} title="Set API token">
          token
        </button>
      </header>
      <div className="main">
        <aside className="sidebar">
          <SidePanel
            tab={rightTab}
            onTab={setRightTab}
            refreshKey={running ? 0 : 1}
          />
        </aside>
        <section className="chat" ref={chatRef}>
          {events.length === 0 && (
            <div className="empty">
              Describe a coding task below. The agent will explore the
              workspace, edit code, run tests and show its work here.
            </div>
          )}
          {events.map((e, i) => (
            <EventView key={i} event={e} />
          ))}
          {pendingApprovals.map((a) => (
            <ApprovalCard
              key={a.id}
              approval={a}
              onResolve={resolveApproval}
            />
          ))}
        </section>
      </div>
      <footer className="composer">
        <textarea
          value={task}
          placeholder="e.g. 修复 MQTT reconnect bug，并运行相关测试"
          onChange={(e) => setTask(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) submit();
          }}
        />
        <button onClick={submit} disabled={running || !task.trim()}>
          Run ⌘↵
        </button>
      </footer>
    </div>
  );
}
