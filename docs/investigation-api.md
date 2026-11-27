# Investigation API

Both clients use JSON under `/api/v1`. An operator token supplied by the installer grants access to the installation. CLI requests send an Authorization bearer header. The browser exchanges the same token at `POST /api/v1/session` with a JSON `token` field for an HttpOnly, SameSite=Strict session cookie. Require same-origin browser writes, reject untrusted origins, and keep secrets out of URLs and browser storage.

| Method and path | Request and response |
|---|---|
| POST /pages | Send message, optional namespace and target. Return HTTP 201 with pageId and turnId. |
| GET /pages | Return an object with a pages array of summaries, newest activity first. |
| GET /pages/{id} | Return a page summary with a messages array and lastEventId. |
| POST /pages/{id}/turns | Send message. Return HTTP 201 with pageId and turnId. |
| POST /pages/{id}/stop | Stop the active turn. Return HTTP 200 with status. Repeated calls are safe. |
| GET /pages/{id}/events?after=0 | Return an events array in sequence order. With Accept: text/event-stream, replay then follow with SSE id equal to seq and JSON data equal to the event. Support Last-Event-ID. |
| GET /pages/{id}/evidence/{callId} | Return sanitized tool evidence including callId, tool, arguments, result, status, durationMs, redacted and truncated. |
| GET /pages/{id}/diff | Return files containing path and diff, and optional prUrl. An empty files array means no proposal. |

Use camelCase wire names. IDs are opaque strings except for per-page event sequence numbers, which start at one. A page summary includes id, title, status, updatedAt and activeTurnId (nullable). Messages contain id, turnId, role, content, createdAt and citations (callId and optional JSON pointer). Timestamps use UTC RFC 3339. Each event contains seq, pageId, turnId, type, time and data. Event types are turn.started, tool.started, tool.finished, answer, proposal and turn.finished. Tool events include callId and tool. Completed attempts carry status and durationMs. Turn statuses are running, completed, stopped, failed and interrupted. SSE reconnects resume after the last durable sequence. A client disconnect leaves the turn active. Clients send Idempotency-Key on create and reply. Reject a conflicting active turn with HTTP 409. Errors carry an error object with code and message.

For example, a tool.started event for a Service read can carry:

```json
{"seq":2,"pageId":"p1","turnId":"t1","type":"tool.started","time":"2026-01-01T00:00:01Z","data":{"callId":"c1","tool":"resources_get","arguments":{"kind":"Service","name":"checkout","namespace":"shop"}}}
```

The private agent interface runs inside the installation. Go sends POST /turn to the Python service with pageId, turnId, messages, namespace, target, tools (name, description, inputSchema), and an installation-controlled callback base URL. Authenticate both directions using an installer-provided internal bearer credential. Go owns cancellation and calls POST /turns/{turnId}/stop on Python. Python responds to POST /turn with a terminal JSON object carrying content, citations and optional proposal (baseSha, files containing path and content). During the turn, Python calls POST /internal/turns/{turnId}/tools on Go with callId, tool and arguments and receives status, result, redacted and truncated. Tool calls may read evidence only. Go validates and publishes any proposal after the agent returns. Bind callbacks and proposals to the active turn, reject late work and prevent duplicate publication after retries.
