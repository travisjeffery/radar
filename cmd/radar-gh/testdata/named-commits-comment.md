/radar

Round 4 (review of `c3d4e5f60718`).

**"Retry test covers only one backoff shape" (P2): fixed in `9f8e7d6c5b`.** `TestRetryStopsAtTheAttemptLimit` builds each client with the real option constructors and the real default policy. It counts the attempts the client makes, so a further retry fails the test, including one added inside the client constructor:
- the HTTP client (`withBackoff` + `jitteredDelay`) is checked at ≤ 5 attempts for both backoff shapes. The capped shape is covered twice, once via `max_delay` and once via `deadline`; the uncapped shape once.
- the queue consumer (`fixedDelay` + `namedRetryPolicy`) is pinned at exactly 5.

**"Decide what happens to callers on the old retry setting" (P2): no code.** Callers that never set the new option keep the old unbounded retry on idempotent requests. That is kept for now and listed under Known limits in the PR body. On every non-idempotent call, the guard from `c3d4e5f607` already limits it to a single attempt. Removing the old setting is tracked in [ENG-901](https://tracker.example.com/ENG-901) ("Remove the unbounded retry setting"), a sub-issue of ENG-900.
