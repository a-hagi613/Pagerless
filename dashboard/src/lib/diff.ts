export type DiffLineKind = "add" | "remove" | "context" | "meta";

export interface DiffLine {
  kind: DiffLineKind;
  text: string;
  // Line numbers in the file before and after the change; absent on meta lines.
  before?: number;
  after?: number;
}

const hunkHeader = /^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@/;

// The server derives the diff, so the dashboard only has to read it. Anything that is not an addition, a
// removal, or a context line is chrome and is labelled as such, never guessed into a change.
export function parseUnifiedDiff(diff: string): DiffLine[] {
  const lines: DiffLine[] = [];
  let before = 0;
  let after = 0;

  const rows = diff.split("\n");
  for (const [index, raw] of rows.entries()) {
    const last = index === rows.length - 1;
    if (last && raw === "") {
      break;
    }
    if (raw.startsWith("diff --git") || raw.startsWith("index ") || raw.startsWith("new file mode") || raw.startsWith("deleted file mode")) {
      lines.push({ kind: "meta", text: raw });
      continue;
    }
    if (raw.startsWith("--- ") || raw.startsWith("+++ ")) {
      lines.push({ kind: "meta", text: raw });
      continue;
    }
    const header = hunkHeader.exec(raw);
    if (header !== null) {
      before = Number(header[1]);
      after = Number(header[2]);
      lines.push({ kind: "meta", text: raw });
      continue;
    }
    if (raw.startsWith("+")) {
      lines.push({ kind: "add", text: raw.slice(1), after });
      after += 1;
      continue;
    }
    if (raw.startsWith("-")) {
      lines.push({ kind: "remove", text: raw.slice(1), before });
      before += 1;
      continue;
    }
    if (raw.startsWith(" ") || raw === "") {
      lines.push({ kind: "context", text: raw.slice(1), before, after });
      before += 1;
      after += 1;
      continue;
    }
    lines.push({ kind: "meta", text: raw });
  }

  return lines;
}

export function diffCounts(lines: readonly DiffLine[]): { added: number; removed: number } {
  let added = 0;
  let removed = 0;
  for (const line of lines) {
    if (line.kind === "add") {
      added += 1;
    } else if (line.kind === "remove") {
      removed += 1;
    }
  }
  return { added, removed };
}
