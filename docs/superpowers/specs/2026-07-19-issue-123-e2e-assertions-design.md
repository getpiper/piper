# Issue #123 E2E Assertion Design

## Goal

Close the remaining e2e test gap in issue #123 without changing production
behavior. The deploy lifecycle test must prove that stopping an app removes its
user-visible route and that deleting it removes the app resource.

## Design

Keep the existing deploy → stop → delete flow in `TestEndToEndDeploy`.

- After `StopApp`, replace the body-difference check with an exact assertion for
  Caddy's user-visible no-route response: HTTP 200 with an empty body. Continue
  asserting that the app remains listed with status `stopped`.
- After `DeleteApp`, continue asserting that `ListApps` is empty, then call
  `Client.App("blog")` and require a `*client.StatusError` with HTTP 404.
- Fetch the hostname again after deletion and require the same exact no-route
  response, proving the deleted app is not served.

The test uses the public control client and public app hostname only. It does not
query Caddy's admin API, so it remains an end-user behavior test rather than a
configuration-shape test.

## Error Handling

HTTP calls must fail the test on transport or body-read errors. Unexpected
status codes, non-empty no-route bodies, a missing typed status error, or a
non-404 app lookup must each produce focused failure messages.

## Verification

First mutation-prove the stronger checks by temporarily expecting an incorrect
no-route response and confirming the focused e2e test fails for that assertion.
Then restore the intended assertion and run:

1. `RUN_E2E=1 go test ./test/e2e/... -run '^TestEndToEndDeploy$' -count=1 -v`
2. `gofmt -l .`
3. `go vet ./...`
4. `make test`
5. `make cross`

No production files or non-e2e tests are in scope.
