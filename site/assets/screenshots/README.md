# WebUI screenshots

These images are actual browser captures of the gateway's embedded WebUI, not
illustrations or composited mockups. All worker names, paths, session names,
messages, questions, usage figures, and timestamps are fictional demo data.
No live gateway, user conversation, or private configuration is accessed.

Regenerate from the repository root:

```sh
npm run screenshots
```

Alternatively, provide an existing Playwright installation:

```sh
PLAYWRIGHT_MODULE=/path/to/playwright node scripts/capture-site-screenshots.cjs
```

The capture script loads the real HTML, CSS and JavaScript from
`internal/admin/static`, supplies local API and WebSocket fixtures, and blocks
external requests. `capture.json` records dimensions, capture time, and hashes
of the source assets used. Screenshots are displayed at their original aspect
ratios on the public project website.
