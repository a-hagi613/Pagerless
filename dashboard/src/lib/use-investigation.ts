import { useCallback, useEffect, useMemo, useReducer, useRef, useState } from "react";

import {
  ApiError,
  getDiff,
  getEvents,
  getPage,
  replyToPage,
  stopTurn,
  subscribeEvents,
  UnauthenticatedError,
} from "./api";
import type {
  DiffResponse,
  InvestigationEvent,
  InvestigationRequest,
  PageMessage,
  PageStatus,
} from "./contract";
import {
  activeTurnId,
  currentStatus,
  initialTimeline,
  latestProposal,
  messagesForPage,
  timelineReducer,
  turnSummaries,
} from "./events";

function messageOf(cause: unknown): string {
  return cause instanceof Error ? cause.message : String(cause);
}

export interface Investigation {
  pageId: string;
  timeline: ReturnType<typeof timelineReducer>;
  messages: PageMessage[];
  status: PageStatus;
  activeTurn: string | null;
  turns: ReturnType<typeof turnSummaries>;
  proposal: ReturnType<typeof latestProposal>;
  diff: DiffResponse | null;
  diffError: string | null;
  activityError: string | null;
  replyPending: boolean;
  replyError: string | null;
  stopping: boolean;
  stopError: string | null;
  retryLoad: () => void;
  refreshDiff: () => Promise<void>;
  reconnect: () => void;
  sendReply: (request: InvestigationRequest, idempotencyKey: string) => Promise<string | null>;
  stop: () => Promise<void>;
  clearErrors: () => void;
}

// useInvestigation owns one page's durable read, ordered stream and operator writes. A reconnect always
// starts from the highest durable sequence the browser has accepted, so replay is exact.
export function useInvestigation(pageId: string, onSettled?: () => void): Investigation {
  const [timeline, dispatch] = useReducer(timelineReducer, initialTimeline);
  const [diff, setDiff] = useState<DiffResponse | null>(null);
  const [diffError, setDiffError] = useState<string | null>(null);
  const [replyPending, setReplyPending] = useState(false);
  const [replyError, setReplyError] = useState<string | null>(null);
  const [stopping, setStopping] = useState(false);
  const [stopError, setStopError] = useState<string | null>(null);
  const [reconnectKey, setReconnectKey] = useState(0);
  const [ready, setReady] = useState(false);
  const [activityError, setActivityError] = useState<string | null>(null);
  const lastSeq = useRef(0);
  const detailRefresh = useRef<AbortController | null>(null);
  const diffRefresh = useRef<AbortController | null>(null);
  const settled = useRef(onSettled);

  useEffect(() => {
    settled.current = onSettled;
  }, [onSettled]);

  const loadDetail = useCallback(
    async (quiet: boolean) => {
      detailRefresh.current?.abort();
      const controller = new AbortController();
      detailRefresh.current = controller;
      try {
        const detail = await getPage(pageId, controller.signal);
        if (!quiet) {
          // The stored transcript is the visual history, but every attempt lives in the append-only log.
          // Reading it from zero is what keeps a reloaded page's activity complete.
          try {
            const events = await getEvents(pageId, 0, controller.signal);
            for (const event of events) {
              lastSeq.current = Math.max(lastSeq.current, event.seq);
            }
            dispatch({ type: "replay", events });
            setActivityError(null);
          } catch (cause) {
            if (!(cause instanceof DOMException && cause.name === "AbortError")) {
              setActivityError(messageOf(cause));
            }
          }
        }
        dispatch({ type: "detail", detail });
        setReady(true);
      } catch (cause) {
        if (cause instanceof DOMException && cause.name === "AbortError") {
          return;
        }
        if (cause instanceof UnauthenticatedError) {
          return;
        }
        if (!quiet) {
          dispatch({ type: "failed", message: messageOf(cause) });
        }
      }
    },
    [pageId],
  );

  const refreshDiff = useCallback(async () => {
    diffRefresh.current?.abort();
    const controller = new AbortController();
    diffRefresh.current = controller;
    try {
      const answer = await getDiff(pageId, controller.signal);
      setDiff(answer);
      setDiffError(null);
    } catch (cause) {
      if (cause instanceof DOMException && cause.name === "AbortError") {
        return;
      }
      if (cause instanceof UnauthenticatedError) {
        return;
      }
      setDiffError(messageOf(cause));
    }
  }, [pageId]);

  useEffect(() => {
    let active = true;
    const detailController = new AbortController();
    detailRefresh.current = detailController;
    // The initial read is a promise chain with its setters in the callbacks, so mount never renders
    // twice before the first byte arrives. Retries and quiet refreshes reuse loadDetail below.
    getPage(pageId, detailController.signal).then(
      (detail) => {
        void (async () => {
          let events: InvestigationEvent[] = [];
          let activityFailure: string | null = null;
          try {
            events = await getEvents(pageId, 0, detailController.signal);
          } catch (cause) {
            if (
              !(cause instanceof DOMException && cause.name === "AbortError") &&
              !(cause instanceof UnauthenticatedError)
            ) {
              activityFailure = messageOf(cause);
            }
          }
          if (!active) {
            return;
          }
          for (const event of events) {
            lastSeq.current = Math.max(lastSeq.current, event.seq);
          }
          dispatch({ type: "replay", events });
          setActivityError(activityFailure);
          dispatch({ type: "detail", detail });
          setReady(true);
        })();
      },
      (cause: unknown) => {
        if (
          active &&
          !(cause instanceof DOMException && cause.name === "AbortError") &&
          !(cause instanceof UnauthenticatedError)
        ) {
          dispatch({ type: "failed", message: messageOf(cause) });
        }
      },
    );

    const diffController = new AbortController();
    diffRefresh.current = diffController;
    getDiff(pageId, diffController.signal).then(
      (answer) => {
        if (active) {
          setDiff(answer);
          setDiffError(null);
        }
      },
      (cause: unknown) => {
        if (
          active &&
          !(cause instanceof DOMException && cause.name === "AbortError") &&
          !(cause instanceof UnauthenticatedError)
        ) {
          setDiffError(messageOf(cause));
        }
      },
    );

    return () => {
      active = false;
      detailRefresh.current?.abort();
      diffRefresh.current?.abort();
    };
  }, [pageId]);

  const loaded = ready;

  useEffect(() => {
    if (!loaded) {
      return;
    }
    const close = subscribeEvents(pageId, lastSeq.current, {
      onOpen: () => {
        dispatch({ type: "stream", status: "live" });
      },
      onEvent: (event: InvestigationEvent) => {
        lastSeq.current = Math.max(lastSeq.current, event.seq);
        dispatch({ type: "event", event });
        if (event.type === "answer") {
          void loadDetail(true);
        } else if (event.type === "proposal") {
          void refreshDiff();
        } else if (event.type === "turn.finished") {
          void loadDetail(true);
          void refreshDiff();
          settled.current?.();
          // A finished turn is the end of the documented event sequence; closing stops EventSource from
          // reconnecting to a stream the server will not extend.
          close();
          dispatch({ type: "stream", status: "closed" });
        }
      },
      onError: (closed) => {
        dispatch({ type: "stream", status: closed ? "closed" : "reconnecting" });
      },
    });
    return close;
    // loadDetail and refreshDiff are stable for this page; the stream restarts only when a reply starts a
    // new turn or the operator asks to reconnect.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pageId, loaded, reconnectKey]);

  // A sequence jump means the browser missed a frame. Ask for the durable tail by cursor and fold it in
  // before continuing, rather than leaving a silent hole in the activity list.
  useEffect(() => {
    if (!timeline.gap) {
      return;
    }
    let cancelled = false;
    void getEvents(pageId, lastSeq.current)
      .then((events) => {
        if (cancelled) {
          return;
        }
        for (const event of events) {
          lastSeq.current = Math.max(lastSeq.current, event.seq);
        }
        dispatch({ type: "replay", events });
        dispatch({ type: "gap", gap: false });
      })
      .catch((cause: unknown) => {
        if (!cancelled && !(cause instanceof UnauthenticatedError)) {
          dispatch({ type: "gap", gap: false });
        }
      });
    return () => {
      cancelled = true;
    };
  }, [pageId, timeline.gap]);

  const sendReply = useCallback(
    async (request: InvestigationRequest, idempotencyKey: string): Promise<string | null> => {
      setReplyPending(true);
      setReplyError(null);
      try {
        const accepted = await replyToPage(pageId, request, idempotencyKey);
        // Show the operator's message immediately; the canonical detail read follows the stream event.
        dispatch({
          type: "message",
          message: {
            id: `pending-${accepted.turnId}`,
            turnId: accepted.turnId,
            role: "user",
            content: request.message,
            createdAt: new Date().toISOString(),
            citations: [],
          },
        });
        lastSeq.current = Math.max(lastSeq.current, timeline.lastSeq);
        dispatch({ type: "stream", status: "connecting" });
        setReconnectKey((current) => current + 1);
        void loadDetail(true);
        return null;
      } catch (cause) {
        if (cause instanceof UnauthenticatedError) {
          return "The session has ended. Sign in again to continue.";
        }
        const failure =
          cause instanceof ApiError && cause.status === 409
            ? `This page is busy: ${cause.message}. Stop the active turn or try again.`
            : messageOf(cause);
        setReplyError(failure);
        return failure;
      } finally {
        setReplyPending(false);
      }
    },
    [loadDetail, pageId, timeline.lastSeq],
  );

  const stop = useCallback(async () => {
    setStopping(true);
    setStopError(null);
    try {
      await stopTurn(pageId);
      // The stream reports the terminal status; a detail read makes it visible even if the stream had
      // already been dropped.
      settled.current?.();
      void loadDetail(true);
    } catch (cause) {
      if (!(cause instanceof UnauthenticatedError)) {
        setStopError(messageOf(cause));
      }
    } finally {
      setStopping(false);
    }
  }, [loadDetail, pageId]);

  const retryLoad = useCallback(() => {
    dispatch({ type: "loading" });
    void loadDetail(false);
    void refreshDiff();
  }, [loadDetail, refreshDiff]);

  const reconnect = useCallback(() => {
    dispatch({ type: "stream", status: "connecting" });
    setReconnectKey((current) => current + 1);
  }, []);

  const clearErrors = useCallback(() => {
    setReplyError(null);
    setStopError(null);
    setDiffError(null);
  }, []);

  const messages = useMemo(
    () => messagesForPage(timeline.detail, timeline.events),
    [timeline.detail, timeline.events],
  );
  const status = useMemo(() => currentStatus(timeline), [timeline]);
  const activeTurn = useMemo(() => activeTurnId(timeline), [timeline]);
  const turns = useMemo(() => turnSummaries(timeline.events), [timeline.events]);
  const proposal = useMemo(() => latestProposal(timeline.events), [timeline.events]);

  return {
    pageId,
    timeline,
    messages,
    status,
    activeTurn,
    turns,
    proposal,
    diff,
    diffError,
    activityError,
    replyPending,
    replyError,
    stopping,
    stopError,
    retryLoad,
    refreshDiff,
    reconnect,
    sendReply,
    stop,
    clearErrors,
  };
}
