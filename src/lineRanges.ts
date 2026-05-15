// Line-range extraction for AI tool calls.
//
// Strategy: locate by string match against the post-edit file content.
// For Edit/MultiEdit we search for `new_string` in the file as it now exists
// (PostToolUse has already applied the change). For Write the whole file is
// new content so the range is 1..file_lines. The recorded range tells us which
// lines this specific tool call wrote — the foundation of `posthook blame`.
//
// Known limitations (Phase 1):
//   - MultiEdit with identical new_strings attributes both to the first match.
//   - Pure deletions (new_string empty) record no range; they vanish from blame.
//   - Bash edits are not captured here — they need file-state snapshotting,
//     which is Phase 2 work.

export interface LineRange {
  start_line: number;
  end_line: number;
  new_text_lines: number;
}

interface ToolInput {
  file_path?: string;
  old_string?: string;
  new_string?: string;
  content?: string;
  command?: string;
  edits?: Array<{ old_string?: string; new_string?: string }>;
}

export interface ApplyPatchFileEdit {
  file_path: string;
  edits: Array<{ new_string: string }>;
  lines_added: number;
  lines_removed: number;
}

// Count lines that `text` occupies. A trailing newline doesn't add an extra line
// (consistent with how most tools count file lines).
function linesIn(text: string): number {
  if (text.length === 0) return 0;
  let count = 0;
  for (let i = 0; i < text.length; i++) if (text.charCodeAt(i) === 10) count++;
  if (text.charCodeAt(text.length - 1) !== 10) count++;
  return count;
}

function newlinesBefore(text: string, index: number): number {
  let count = 0;
  for (let i = 0; i < index; i++) if (text.charCodeAt(i) === 10) count++;
  return count;
}

interface ClaimedRange {
  start: number;
  end: number; // exclusive
}

function findUnclaimed(haystack: string, needle: string, claimed: ClaimedRange[]): number {
  if (needle.length === 0) return -1;
  let from = 0;
  while (from <= haystack.length - needle.length) {
    const idx = haystack.indexOf(needle, from);
    if (idx === -1) return -1;
    const end = idx + needle.length;
    const overlaps = claimed.some((c) => idx < c.end && end > c.start);
    if (!overlaps) return idx;
    from = idx + 1;
  }
  return -1;
}

function rangeForMatch(postContent: string, matchIndex: number, newString: string): LineRange {
  const start_line = 1 + newlinesBefore(postContent, matchIndex);
  const new_text_lines = linesIn(newString);
  const end_line = Math.max(start_line, start_line + new_text_lines - 1);
  return { start_line, end_line, new_text_lines };
}

export interface ExtractedRanges {
  ranges: LineRange[];
  // Edits we recognized but couldn't locate (new_string not found in post content).
  // Useful as a counter / surfacing accuracy issues.
  unlocated: number;
}

export function extractRanges(
  toolName: string,
  toolInput: ToolInput,
  postContent: string,
): ExtractedRanges {
  switch (toolName) {
    case "Write": {
      const content = typeof toolInput.content === "string" ? toolInput.content : postContent;
      const total = linesIn(content);
      if (total === 0) return { ranges: [], unlocated: 0 };
      return {
        ranges: [{ start_line: 1, end_line: total, new_text_lines: total }],
        unlocated: 0,
      };
    }
    case "Edit": {
      const ns = toolInput.new_string;
      if (typeof ns !== "string" || ns.length === 0) {
        return { ranges: [], unlocated: 0 };
      }
      const idx = postContent.indexOf(ns);
      if (idx === -1) return { ranges: [], unlocated: 1 };
      return { ranges: [rangeForMatch(postContent, idx, ns)], unlocated: 0 };
    }
    case "MultiEdit": {
      const edits = Array.isArray(toolInput.edits) ? toolInput.edits : [];
      const ranges: LineRange[] = [];
      const claimed: ClaimedRange[] = [];
      let unlocated = 0;
      for (const edit of edits) {
        const ns = edit.new_string;
        if (typeof ns !== "string" || ns.length === 0) continue;
        const idx = findUnclaimed(postContent, ns, claimed);
        if (idx === -1) {
          unlocated++;
          continue;
        }
        claimed.push({ start: idx, end: idx + ns.length });
        ranges.push(rangeForMatch(postContent, idx, ns));
      }
      return { ranges, unlocated };
    }
    default:
      return { ranges: [], unlocated: 0 };
  }
}

export function parseApplyPatch(command: string): ApplyPatchFileEdit[] {
  const files: ApplyPatchFileEdit[] = [];
  let current: ApplyPatchFileEdit | null = null;
  let addedBlock: string[] = [];

  const flushAddedBlock = () => {
    if (!current || addedBlock.length === 0) return;
    const newString = addedBlock.join("\n");
    if (newString.length > 0) current.edits.push({ new_string: newString });
    addedBlock = [];
  };

  for (const line of command.replace(/\r\n/g, "\n").split("\n")) {
    if (line.startsWith("*** Add File: ") || line.startsWith("*** Update File: ")) {
      flushAddedBlock();
      current = {
        file_path: line.replace(/^\*\*\* (?:Add|Update) File: /, ""),
        edits: [],
        lines_added: 0,
        lines_removed: 0,
      };
      files.push(current);
      continue;
    }

    if (line.startsWith("*** Delete File: ")) {
      flushAddedBlock();
      current = null;
      continue;
    }

    if (line.startsWith("*** Move to: ")) {
      if (current) current.file_path = line.slice("*** Move to: ".length);
      continue;
    }

    if (line.startsWith("*** ")) {
      flushAddedBlock();
      continue;
    }

    if (!current) continue;

    if (line.startsWith("+")) {
      current.lines_added++;
      addedBlock.push(line.slice(1));
    } else {
      flushAddedBlock();
      if (line.startsWith("-")) current.lines_removed++;
    }
  }

  flushAddedBlock();
  return files.filter((file) => file.file_path.length > 0);
}
