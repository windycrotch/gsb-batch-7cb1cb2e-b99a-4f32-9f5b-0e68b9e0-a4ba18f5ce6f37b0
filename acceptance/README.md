# Lifecycle acceptance tests

This package verifies the alignment between **input deletion** and
**packaging task lifecycle** (acceptance checks A1–A7).

```sh
go test ./acceptance -count=1 -v
```

The tests are fully hermetic: they use an in-memory input store/queue and a
fake `gpac` that is implemented by re-executing the test binary
(`TestMain` + `FAKE_GPAC_MODE`). No Postgres, Redis, Docker or real gpac is
required. Run with `-race` to exercise the concurrency guarantees.

## Covered guarantees

| ID | Scenario | Observable assertion |
|----|----------|----------------------|
| A1 | enqueue → delete → worker runs the stale task | task skipped with `asynq.SkipRetry`, gpac never called, no playlist |
| A2 | fake gpac exits non-zero | input retained, `status=failed` (+ message) visible via `GET /inputs/{id}` |
| A3 | concurrent delete vs package/run | never the forbidden “record gone but playlist present” state |
| A4 | repeated / concurrent `DELETE` | idempotent success, no `500` |
| A5 | package a random UUID | `404` with `"code":"INPUT_NOT_FOUND"` (not a blanket `500`) |
| A6 | restart worker (empty memory, same DB/disk) | stale task for a deleted input is skipped and stale artifacts purged |
| A7 | fake gpac succeeds | `index.m3u8` written and input stays `active` |

There is also a long-running gpac test: deleting mid-execution terminates the
subprocess and leaves no playlist by the time `DELETE` returns.

## Fake gpac as a standalone binary / script

A cross-platform fake is provided:

```sh
go build -o /tmp/fakegpac ./acceptance/fakegpac
FAKE_GPAC_MODE=success|fail|slow FAKE_GPAC_CALLS=/tmp/calls.log /tmp/fakegpac ...
```

Point the service/worker at it with `GPAC_BIN=/tmp/fakegpac`. A bash variant
lives at `scripts/fake-gpac/gpac.sh`. `FAKE_GPAC_MODE_FILE=<file>` makes the
fake read the mode from a file on each invocation, so one long-lived worker can
switch between success/failure/slow scenarios.

## HTTP contract

- `POST /api/v1/notifications/package?id=<uuid>`
  - `201` enqueued; `404` + `error.code=INPUT_NOT_FOUND` when absent;
    `409` + `INPUT_DELETING` if deletion is in progress.
- `GET /api/v1/inputs/{id}` returns `data.status` ∈
  `active | packaging | failed | deleting` and `data.status_message`.
- `DELETE /api/v1/inputs/{id}` is idempotent (`200` for repeated deletes).

## How the guarantees are implemented

1. Tasks re-validate the input in the store **before** running gpac
   (restart-safe / stale-snapshot guard) and re-check **after** gpac returns.
2. An in-process `service.Coordinator` cancels and waits for executions during
   delete, then removes the `<PLAYOUT_PATH>/<input-id>/` directory.
3. A persistence watcher cancels a running gpac when its row disappears,
   covering the real deployment where API and worker are different processes.
4. gpac runs through an `exec.CommandContext`; `GPAC_BIN` makes the binary
   configurable.
5. gpac failure persists `status=failed` and keeps the row; only an explicit
   successful delete removes it.
