import { describe, expect, it } from "vitest";

import fixtures from "./__fixtures__/investigation.json";
import type { InvestigationEvent, PageDetail } from "./contract";
import {
  activeTurnId,
  currentStatus,
  initialTimeline,
  latestProposal,
  messagesForPage,
  timelineReducer,
  toolAttempts,
  turnSummaries,
} from "./events";

function fixture<T>(value: unknown): T {
  return value as T;
}

const events = fixture<Record<string, InvestigationEvent[]>>(fixtures.events);
const details = fixture<Record<string, PageDetail>>(fixtures.details);

function timelineFor(pageId: string) {
  return {
    ...initialTimeline,
    detail: details[pageId],
    events: events[pageId],
    lastSeq: details[pageId].lastEventId,
    loading: false,
  };
}

describe("tool attempts", () => {
  it("keeps every attempt in sequence, including a denial and a retry", () => {
    const attempts = toolAttempts(events["p-checkout"]);
    expect(attempts.map((attempt) => attempt.callId)).toEqual(["c1", "c2", "c3", "c4"]);
    expect(attempts.map((attempt) => attempt.status)).toEqual(["denied", "ok", "truncated", "ok"]);
    expect(attempts.map((attempt) => attempt.durationMs)).toEqual([12, 84, 220, 51]);
    // The attempted Secret read stays visible rather than being merged into the later Deployment read.
    expect(attempts[0].tool).toBe("resources_get");
    expect(attempts[0].redacted).toBeUndefined();
    expect(attempts[2].truncated).toBe(true);
  });

  it("shows a finished attempt whose start precedes the reader's cursor", () => {
    const tail = events["p-checkout"].filter((event) => event.seq >= 5);
    const attempts = toolAttempts(tail);
    expect(attempts.map((attempt) => [attempt.callId, attempt.status])).toEqual([
      ["c2", "ok"],
      ["c3", "truncated"],
      ["c4", "ok"],
    ]);
  });

  it("sorts an out-of-order batch by sequence", () => {
    const batch = [...events["p-nofix"]].reverse();
    expect(toolAttempts(batch).map((attempt) => attempt.callId)).toEqual(["c10"]);
    expect(turnSummaries(batch)[0].turnId).toBe("t2");
  });
});

describe("turn summaries", () => {
  it("records a completed turn with its answer and proposal", () => {
    const turns = turnSummaries(events["p-checkout"]);
    expect(turns).toHaveLength(1);
    expect(turns[0].status).toBe("completed");
    expect(turns[0].attempts).toHaveLength(4);
    expect(turns[0].answer?.citations.map((citation) => citation.callId)).toEqual(["c4", "c12"]);
    expect(turns[0].proposal?.baseSha).toBe("4f3c2b1a9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b");
  });

  it("keeps an interrupted turn visible after a restart", () => {
    const turns = turnSummaries(events["p-interrupted"]);
    expect(turns[0].status).toBe("interrupted");
    expect(currentStatus(timelineFor("p-interrupted"))).toBe("interrupted");
  });

  it("reports the running page and its active turn", () => {
    const state = timelineFor("p-running");
    expect(currentStatus(state)).toBe("running");
    expect(activeTurnId(state)).toBe("t4");
  });

  it("has no proposal for the no-fix answer", () => {
    expect(latestProposal(events["p-nofix"])).toBeNull();
  });

  it("prefers a newer terminal detail read when a stop event was missed", () => {
    const stopped = timelineReducer(timelineFor("p-running"), {
      type: "detail",
      detail: { ...details["p-running"], status: "stopped", activeTurnId: null, lastEventId: 3 },
    });
    // The stream never delivered turn.finished, but the detail read is newer than the open turn.
    expect(currentStatus(stopped)).toBe("stopped");
    expect(activeTurnId(stopped)).toBeNull();
  });

  it("still shows a new live turn that is newer than the last detail read", () => {
    const started = timelineReducer(timelineFor("p-running"), {
      type: "event",
      event: {
        seq: 3,
        pageId: "p-running",
        turnId: "t4",
        type: "turn.started",
        time: "2026-11-26T10:05:30Z",
        data: {},
      },
    });
    expect(currentStatus(started)).toBe("running");
    expect(activeTurnId(started)).toBe("t4");
  });
});

describe("replay", () => {
  it("reconnects from a durable cursor without losing or duplicating attempts", () => {
    const first = timelineReducer(
      { ...initialTimeline, loading: false, detail: details["p-reconnect"], lastSeq: 0 },
      { type: "replay", events: events["p-reconnect"].slice(0, 4) },
    );
    expect(first.lastSeq).toBe(4);
    expect(toolAttempts(first.events).map((attempt) => [attempt.callId, attempt.status])).toEqual([
      ["c40", "ok"],
      ["c41", "running"],
    ]);

    const second = timelineReducer(first, {
      type: "replay",
      events: events["p-reconnect"].slice(3),
    });
    expect(second.lastSeq).toBe(9);
    expect(second.events.map((event) => event.seq)).toEqual([1, 2, 3, 4, 5, 6, 7, 8, 9]);
    expect(toolAttempts(second.events).map((attempt) => [attempt.callId, attempt.status])).toEqual([
      ["c40", "ok"],
      ["c41", "denied"],
      ["c42", "ok"],
    ]);
    expect(turnSummaries(second.events)[0].status).toBe("completed");
    expect(messagesForPage(details["p-reconnect"], second.events).filter((message) => message.role === "assistant")).toHaveLength(1);
  });

  it("drops a replayed sequence that is already durable", () => {
    const state = timelineReducer(timelineFor("p-checkout"), {
      type: "replay",
      events: events["p-checkout"],
    });
    expect(state.events).toHaveLength(12);
    expect(state.lastSeq).toBe(12);
  });

  it("drops a duplicate sequence inside one replay batch", () => {
    const state = timelineReducer(initialTimeline, { type: "replay", events: events["p-checkout"].slice(0, 3) });
    const duplicated = timelineReducer(state, {
      type: "replay",
      events: [events["p-checkout"][3], events["p-checkout"][3], events["p-checkout"][4]],
    });
    expect(duplicated.events.map((event) => event.seq)).toEqual([1, 2, 3, 4, 5]);
  });

  it("sorts an out-of-order replay batch before folding it", () => {
    const state = timelineReducer(initialTimeline, {
      type: "replay",
      events: [events["p-checkout"][2], events["p-checkout"][1], events["p-checkout"][0]],
    });
    expect(state.events.map((event) => event.seq)).toEqual([1, 2, 3]);
    expect(state.gap).toBe(false);
  });

  it("marks a sequence jump instead of hiding it", () => {
    const state = timelineReducer(initialTimeline, { type: "event", event: events["p-checkout"][0] });
    const jumped = timelineReducer(state, { type: "event", event: events["p-checkout"][4] });
    expect(jumped.gap).toBe(true);
    expect(jumped.events.map((event) => event.seq)).toEqual([1, 5]);
  });

  it("ignores a malformed event", () => {
    const malformed = { ...events["p-checkout"][0], seq: "2" } as unknown as InvestigationEvent;
    const state = timelineReducer(initialTimeline, { type: "event", event: malformed });
    expect(state.events).toHaveLength(0);
    expect(state.lastSeq).toBe(0);
  });
});

describe("messages", () => {
  it("uses the stored transcript when it already contains the answer", () => {
    const messages = messagesForPage(details["p-checkout"], events["p-checkout"]);
    expect(messages).toHaveLength(2);
    expect(messages[1].id).toBe("m2");
  });

  it("adds a live answer that the stored detail has not seen yet", () => {
    const partial = { ...details["p-reconnect"], messages: details["p-reconnect"].messages };
    const messages = messagesForPage(partial, events["p-reconnect"]);
    expect(messages.map((message) => message.role)).toEqual(["user", "assistant"]);
    expect(messages[1].citations[0].pointer).toBe("/spec/schedule");
  });
});
