# Project website and screenshots

The public project site is [Codex Telegram Gateway](https://arizzi74.github.io/Codex-Telegram-Gateway/).
It is a static website in `site/`, published by `.github/workflows/pages.yml`
to GitHub Pages from `main`. The site has no gateway connection, login screen,
analytics or external asset dependencies. Its installation links point to this
repository's installer and documentation.

## Reproduce the screenshots

The gallery contains real Playwright captures of the embedded browser interface,
using fictional workers, projects and conversations. The screenshot script mocks
the API and WebSocket transport and does not connect to a production gateway.
Do not replace these images with captures containing private conversations,
credentials, deployment hostnames or local paths.

From the repository root, with Node.js 22 available:

```sh
npm ci
npx playwright install chromium
npm run screenshots
npm run test:browser
```

Screenshot files and capture metadata are in `site/assets/screenshots/`. Commit
them alongside interface changes when refreshing the gallery. The screenshots
show a desktop conversation, a mobile conversation, and the mobile session list.
These tools are development dependencies; installed gateways and workers remain
native Go programs and do not require Node.js or Playwright.

## Publication

The repository's Pages source is **GitHub Actions**. The Website workflow tests
the static site in Chromium, uploads only `site/`, and deploys through the
`github-pages` environment. Only the deployment job receives `pages: write` and
`id-token: write`; it runs on `main`. GitHub Actions dependencies are pinned to
commit hashes. The regular CI workflow also runs the admin, Web UI, notification
and website browser checks.

The website is separate from the authenticated application served by a gateway
at `/tgw/webui/`. Publishing the website does not expose a worker, its terminal,
its conversation history or its admin API.
