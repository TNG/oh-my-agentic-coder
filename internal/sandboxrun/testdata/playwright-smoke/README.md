# Playwright smoke fixture for omac sandbox integration tests.

Used by `TestIntegrationPlaywrightSmoke` in
`playwright_integration_linux_test.go`. The Go test copies this tree into a
temp dir, writes `playwright.config.ts` with a free port + host Chromium
path, runs `npm install`, then executes:

```
omac sandbox run --open-port <port> --read <chrome-dir> -- … npx playwright test
```

No `node_modules` is committed; browsers come from the host
`~/.cache/ms-playwright` (or an explicit Chromium on PATH).
