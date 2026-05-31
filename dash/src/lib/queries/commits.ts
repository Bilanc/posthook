import { db } from "../db";
import type { CommitRow } from "@/types/posthook";

// Commits whose captured AI line ranges are attributed to this session.
export function commitsForSession(sessionId: string): CommitRow[] {
  const conn = db();
  return conn
    .prepare(
      `SELECT c.*
       FROM commits c
       JOIN commit_sessions cs ON cs.commit_id = c.id
       WHERE cs.session_id = ?
       ORDER BY datetime(c.committed_at) DESC`,
    )
    .all(sessionId) as CommitRow[];
}
