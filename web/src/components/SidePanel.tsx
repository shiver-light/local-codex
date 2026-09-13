import { useCallback, useEffect, useState } from "react";
import { api } from "../api";

// SidePanel: file tree / git status / git diff tabs.
export function SidePanel({
  tab,
  onTab,
  refreshKey,
}: {
  tab: "files" | "diff";
  onTab: (t: "files" | "diff") => void;
  refreshKey: number; // bump to refresh (e.g. when a task finishes)
}) {
  const [tree, setTree] = useState("");
  const [status, setStatus] = useState("");
  const [diff, setDiff] = useState("");
  const [err, setErr] = useState("");

  const refresh = useCallback(async () => {
    try {
      const [f, s, d] = await Promise.all([
        api.files(""),
        api.gitStatus(),
        api.gitDiff(),
      ]);
      setTree(f.tree);
      setStatus(s.status);
      setDiff(d.diff);
      setErr("");
    } catch (e) {
      setErr(String(e));
    }
  }, []);

  useEffect(() => {
    refresh();
  }, [refresh, refreshKey]);

  return (
    <div className="sidepanel">
      <div className="tabs">
        <button
          className={tab === "files" ? "active" : ""}
          onClick={() => onTab("files")}
        >
          Files
        </button>
        <button
          className={tab === "diff" ? "active" : ""}
          onClick={() => onTab("diff")}
        >
          Git
        </button>
        <button className="refresh" onClick={refresh} title="refresh">
          ⟳
        </button>
      </div>
      {err && <div className="error">{err}</div>}
      {tab === "files" ? (
        <pre className="tree">{tree}</pre>
      ) : (
        <div className="gitview">
          <pre className="status">{status}</pre>
          <pre className="diff">{diff}</pre>
        </div>
      )}
    </div>
  );
}
