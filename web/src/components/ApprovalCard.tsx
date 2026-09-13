// ApprovalCard is shown when the agent wants to run an "ask" command.
export function ApprovalCard({
  approval,
  onResolve,
}: {
  approval: { id: string; command: string; reason: string };
  onResolve: (id: string, approved: boolean) => void;
}) {
  return (
    <div className="approval">
      <div className="approval-title">Agent wants to execute:</div>
      <code className="approval-cmd">{approval.command}</code>
      <div className="approval-reason">{approval.reason}</div>
      <div className="approval-actions">
        <button className="approve" onClick={() => onResolve(approval.id, true)}>
          Approve
        </button>
        <button className="reject" onClick={() => onResolve(approval.id, false)}>
          Reject
        </button>
      </div>
    </div>
  );
}
