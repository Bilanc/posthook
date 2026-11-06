import { existsSync } from "node:fs";
import { notFound } from "next/navigation";
import { Conversation } from "@/components/session/conversation";
import { FilesPanel } from "@/components/session/files-panel";
import { SessionHeader } from "@/components/session/header";
import { fileRows, resendDelays, sessionStats } from "@/components/session/model";
import { SessionTimeline } from "@/components/session/timeline";
import { TurnRail } from "@/components/session/turn-rail";
import { TurnTokens } from "@/components/session/turn-tokens";
import { Panel } from "@/components/ui/panel";
import { getSessionDetail } from "@/lib/queries/sessions";
import { sessionTimelineData } from "@/lib/queries/turns";
import { readUsage } from "@/lib/transcript";
import { buildTurns } from "@/lib/turns";

export const dynamic = "force-dynamic";

// A session as it happened: the conversation is the spine, tool calls fold
// under the prompt that caused them, and commits sit in the turn they landed
// in. Everything is read straight from SQLite in this server component.
export default async function SessionDetailPage({ params }: { params: Promise<{ id: string }> }) {
  // Cursor ids carry a colon ("cursor:<uuid>"), which reaches params
  // percent-encoded; decode before looking the session up.
  const id = safeDecode((await params).id);
  const session = getSessionDetail(id);
  if (!session) notFound();

  const data = sessionTimelineData(id);
  const usage = data.transcript_path ? readUsage(data.transcript_path) : null;
  const noUsage =
    usage != null
      ? null
      : data.transcript_path == null
        ? "No transcript was recorded, so token usage is unknown."
        : !existsSync(data.transcript_path)
          ? "The transcript is no longer on disk, so token usage is unknown."
          : `${session.agent_slug} reports no token usage.`;
  const turns = buildTurns({
    started_at: session.started_at,
    ended_at: session.ended_at,
    stops: data.stops,
    prompts: data.prompts,
    tools: data.tools,
    commits: data.commits,
    usage,
  });
  const files = fileRows(data.files, data.commit_files, data.commits);
  const stats = sessionStats(session, turns, files, data.commits, data.active_minutes, usage);
  const placed = new Set(turns.flatMap((t) => t.commits.map((c) => c.id)));
  const orphanCommits = data.commits.filter((c) => !placed.has(c.id));
  const running = session.ended_at == null;

  return (
    <div className="grid gap-3.5">
      <SessionHeader session={session} turns={turns} stats={stats} />

      <Panel title="Timeline" sub="idle gaps over 8 minutes are compressed">
        <SessionTimeline
          startedAt={session.started_at}
          endedAt={session.ended_at}
          turns={turns}
          stops={data.stops}
          commits={data.commits}
        />
      </Panel>

      <div className="grid grid-cols-1 items-start gap-3.5 min-[980px]:grid-cols-[210px_minmax(0,1fr)_250px]">
        <Panel title="Turns" className="min-[980px]:sticky min-[980px]:top-4 min-[980px]:max-h-[calc(100vh-2rem)] min-[980px]:overflow-y-auto">
          <TurnRail turns={turns} running={running} />
        </Panel>

        <Panel title="Conversation" sub="tool calls fold under each prompt">
          <Conversation
            turns={turns}
            agent={session.agent_slug}
            running={running}
            committedFiles={data.commit_files.map((f) => f.file_path)}
            remoteUrl={session.repo_remote_url}
            hasUsage={usage != null}
            resendDelays={resendDelays(data.prompts)}
            orphanCommits={orphanCommits}
          />
        </Panel>

        <Panel title="Files" sub="generated vs committed">
          <FilesPanel files={files} />
          <div className="mb-3 mt-5 text-[11px] font-medium uppercase tracking-wider text-[var(--color-fg-muted)]">
            Output tokens per turn
          </div>
          <TurnTokens turns={turns} noUsage={noUsage} />
        </Panel>
      </div>
    </div>
  );
}

function safeDecode(raw: string): string {
  try {
    return decodeURIComponent(raw);
  } catch {
    return raw;
  }
}
