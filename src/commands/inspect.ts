import { openDb } from "../store.ts";

interface InspectOptions {
  agent?: string;
  type?: string;
  session?: string;
  since?: string;
  limit?: number;
}

interface EventRow {
  id: string;
  ts: string;
  event_type: string;
  agent_slug: string;
  session_id: string | null;
  cwd: string | null;
  file_path: string | null;
  rel_file_path: string | null;
  payload: string | null;
}

export async function runInspect(opts: InspectOptions): Promise<void> {
  const db = openDb();

  const where: string[] = [];
  const params: Array<string | number> = [];
  if (opts.agent) {
    where.push("agent_slug = ?");
    params.push(opts.agent);
  }
  if (opts.type) {
    where.push("event_type = ?");
    params.push(opts.type);
  }
  if (opts.session) {
    where.push("session_id = ?");
    params.push(opts.session);
  }
  if (opts.since) {
    where.push("ts >= ?");
    params.push(opts.since);
  }
  const limit = Math.max(1, Math.min(opts.limit ?? 10, 1000));

  const sql = `
    SELECT id, ts, event_type, agent_slug, session_id, cwd, file_path, rel_file_path, payload
    FROM events
    ${where.length ? `WHERE ${where.join(" AND ")}` : ""}
    ORDER BY ts DESC
    LIMIT ${limit}
  `;
  const rows = db.query(sql).all(...params) as EventRow[];

  if (rows.length === 0) {
    console.log("(no matching events)");
    return;
  }

  for (const r of rows) {
    let payload: unknown = r.payload;
    if (r.payload) {
      try {
        payload = JSON.parse(r.payload);
      } catch {
        // leave as raw string
      }
    }
    const block = {
      ts: r.ts,
      agent: r.agent_slug,
      event_type: r.event_type,
      session_id: r.session_id,
      cwd: r.cwd,
      file_path: r.file_path,
      rel_file_path: r.rel_file_path,
      payload,
    };
    console.log(JSON.stringify(block, null, 2));
    console.log("---");
  }

  console.log(`(${rows.length} event${rows.length === 1 ? "" : "s"})`);
}
