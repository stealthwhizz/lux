import type { Task } from "../../shared/types";
import type { Nav } from "../App";
import { timeAgo } from "../lib/lux";
import { StateChip } from "../components/common";

export function TaskRows({ tasks, nav }: { tasks: Task[]; nav: Nav }) {
  return (
    <table className="table">
      <tbody>
        {tasks.map((t) => (
          <tr key={t.id} className="clickable" onClick={() => nav.go("tasks", t.id)} tabIndex={0} onKeyDown={(e) => e.key === "Enter" && nav.go("tasks", t.id)}>
            <td style={{ width: "1%", whiteSpace: "nowrap" }}><StateChip state={t.state} /></td>
            <td>
              <div style={{ fontWeight: 560 }}>{t.prompt.length > 120 ? t.prompt.slice(0, 120) + "…" : t.prompt}</div>
              <div style={{ color: "var(--ink-3)", fontSize: "var(--t-sm)" }}>
                {t.agent_name}, {timeAgo(t.created_at)}
              </div>
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}
