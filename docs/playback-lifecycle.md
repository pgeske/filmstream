# Playback ownership and reliability

## Reproduced lifecycle defects

Offline fixtures reproduced these failures on the baseline:

1. **Stop did not own unfinished startup.** Start serialized other Start/Park requests, but Stop only removed published streams. DELETE during timeline verification could return while the old Start subsequently published a player URL.
2. **Cleanup used playback-ID directories, not producer-owned directories.** Stop detached an old stream, a replay created its replacement at the same path, and slow old cleanup removed the replacement's files.
3. **A canceled waiter became a source failure.** Cached-stream recovery treated caller cancellation or deadline expiration as failure of playlist growth, stopped the shared producer, and could quarantine its source. API subtitle/HLS handlers also invalidated selected-release caches when the caller went away.
4. **Startup-buffer presence outranked producer outcome.** A producer that had already exited unsuccessfully could still pass readiness using cached segments, and its unfinished live playlist could continue returning HTTP 200.

These are established causes of reproducible lifecycle failures, not proof that any particular reported black screen or stall arose from them. Device symptoms still require client diagnostics and playback-time server evidence.

## Ownership contract

Each playback has a lifetime shared by its probe, startup serialization, published producer, and park requests. Stop atomically retires that lifetime and cancels unpublished work. New work for the same ID gets a different lifetime. Publication and delayed failure callbacks check lifetime identity; cleanup only removes that producer's unique internal directory.

The request context owns a **wait**, not an already-published shared producer. A canceled recovery leaves the producer and release selection intact for another consumer. Shared media probes likewise survive individual waiter cancellation, but stop with their playback lifetime. Failed new startup cleans up its own resources. The existing bounded playlist-growth check still decides whether an old cached producer advances; no retry counts, buffer targets, or timeout budgets were increased.

Producer exit is checked before accepting buffered readiness or serving a live playlist. A successful producer with a complete nonempty final playlist remains playable and reusable, including short media. A failed producer returns HTTP 502 for its playlist instead of serving a misleading frozen event playlist. Existing committed segments remain readable until explicit cleanup.

Torrent sessions and seeding ownership are unchanged. Different playback IDs may share a torrent, but retiring or marking one playback unavailable does not remove the shared torrent or poison another playback's source status. This is covered by an offline torrent fixture.

## Backward-compatible status and diagnostics

`GET /v1/playbacks/{id}` retains its existing top-level torrent/Usenet fields and adds an optional `hls` object:

```json
{
  "state": "streaming",
  "active_streams": 1,
  "hls": {
    "state": "buffering",
    "packaged_segments": 8,
    "packaged_seconds": 32.032,
    "complete": false
  }
}
```

HLS states are `ready`, `buffering`, `parked`, `complete`, and `failed`; `failed` includes an `error`. `buffering` means a running, unparked playlist needs a growth check based on the existing freshness threshold. It does not prove torrent failure, and observing status does not perform recovery. No `hls` object means no published HLS stream, not that torrent playback is unavailable. Service health continues to mean only service availability.

New teardown logs retain packaged segment counts, packaged seconds, and completion state before files disappear. Unexpected producer-exit logs also include those values and bounded FFmpeg diagnostics. Explicit Stop is logged. Existing API request bodies and playlist URLs are unchanged; stopped-generation startup/probe requests return HTTP 409, while canceled HTTP requests do not invalidate source caches.

## Validation and remaining boundaries

The tests use shell-controlled producers/probes and generated torrent bytes only, with synchronization at startup verification and playlist-growth checks. They cover back/cancel/replay, delayed cleanup, canceled shared recovery/probing, slow producer progress versus a real stall, cached-stall rejection, successful completion, failed-live-playlist responses, source-cache preservation, retired failure callbacks, and unrelated consumers sharing a torrent.

The legacy API carries no client operation/generation token. A request arriving **after** DELETE can intentionally start new work for the same playback ID; the server cannot distinguish that from a stale client task. Apple-side ownership and current-item fencing remain necessary. Public playlist URLs also remain stable across in-place seeks; callers must not install results from superseded operations.

An alive producer that stops making progress is reported as buffering; the existing explicit recovery path performs the bounded growth check. This change does not introduce a background stall-killer or assume that a six-second packaging gap proves every connected torrent peer is unserviceable. Torrent readiness still is not a guarantee of future sustained bandwidth. Those limitations are intentional rather than speculative timeout changes.

Backend validation commands are `make test`, `make build`, `go vet ./...`, and `go test -race ./internal/hls ./internal/torrentstream ./internal/api -count=1 -timeout=120s`, plus `gofmt` checks and `git diff --check`. The repository has no separate lint target. Apple validation uses `make apple-test` and the platform build targets described in [the Apple README](../clients/apple/README.md).

Device validation should cover rapid back/replay, recovery canceled by seek/subtitle changes, completed short media, and startup followed by source starvation. Capture item identity, AVPlayer error/stall logs, HLS status, playlist growth, and teardown events together. Local regression success is not a claim that a reported black screen has been reproduced or resolved on-device.
