# TapePilot console

Use Node from `.node-version` and the package-manager version recorded in `package.json`. From this directory run `npm ci`, `npm run check`, `npm run lint`, `npm test` and `npm run build`.

For native development, run the configured backend on its private port 8788 and `npm run dev -- --host 127.0.0.1 --port 8787 --strictPort`. The configured public origin must match the browser origin. Both the development and preview proxy preserve the public Host for the backend's origin checks. Deployment serves the static build through the repository's same-origin nginx proxy.

The console uses an anonymous CSRF session for sign-in, authenticated cookies and a CSRF token on every mutation. Each run preview retains a distinct idempotency key across explicit retries. Backup and restore keys are separate. No password, session token or CSRF token is placed in local storage. Search, archive and member pages use the server's opaque cursor. Unknown drive observations remain unknown. Accepted write bytes and actual readback bytes remain separate.

`npm run test:e2e` creates a fresh 0700 synthetic installation through the repository's Go fixture, builds a temporary backend and a static frontend, and starts only its own loopback services on 8788 and 18789. It refuses occupied ports. The harness uses `/sbin/chromium` when available. Set `TAPEPILOT_BROWSER_EXECUTABLE` for another Chromium executable or install Playwright's matching Chromium into the user's cache with `npx playwright install chromium`. The harness runs with one worker and removes only its own generated fixture after stopping its child services. Traces and videos stay disabled to avoid retaining login request bodies.

The browser cases exercise real login, backup preview/run, lost-response idempotent retry, readback completion, offline search, selective restore content, destination rejection, media waiting/recheck/cancel, schedule pause, inbox marking, sign-out and source-change errors. Desktop and 390px mobile layouts are checked. The authenticated screenshot at `docs/images/simulator-dashboard.png` contains synthetic fixture data. These browser checks provide simulator evidence.

Initialization and enrollment remain local CLI operations. The console creates jobs and requests cancellation. Backend validation and resource ownership determine every operation's outcome.
