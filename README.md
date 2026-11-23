[![Deploy on Railway](https://railway.com/button.svg)](https://railway.com/new/template/obscura?utm_medium=integration&utm_source=button&utm_campaign=obscura)

# Obscura on Railway

One-click [Obscura](https://github.com/h4ckf0r0day/obscura) on [Railway](https://railway.com): an open-source headless browser for AI agents and web scraping, written in Rust. Puppeteer and Playwright drive it over the Chrome DevTools Protocol (CDP), AI agents drive it over MCP, and it renders screenshots and PDFs without Chromium.

**Version:** Obscura **0.2.7**, official image `h4ckf0r0day/obscura:0.2.7@sha256:e70c2abc925f2194c9338762075432c69328032cae220d77393f1495e90c601a` (Apache-2.0).

## What the template deploys

One service, **Obscura**, built from this repo's `Dockerfile`:

- The official Obscura image, plus a small gateway (`gateway/`, Go standard library only) that is the container's entry point.
- The gateway listens on `$PORT` (8080) behind the Railway domain:
  - `/devtools/browser` and `/json/*`: CDP for Puppeteer and Playwright. Discovery URLs are rewritten to your public `wss://` address.
  - `/mcp`: Obscura's MCP server (streamable HTTP) with its browser tools.
  - `/healthz`: health check, no token needed.
  - Every CDP and MCP request needs `OBSCURA_TOKEN`, as `Authorization: Bearer <token>` or `?token=<token>`. Obscura itself listens only on loopback with its own random internal tokens.
- A volume at `/data` keeps cookies and localStorage (`/data/cdp-profile` for CDP, `/data/mcp-profile` for MCP), so logins survive restarts and redeploys.
- Both Obscura processes run as the image's unprivileged user (uid 65532). If one of them exits, the gateway exits non-zero and Railway's `ON_FAILURE` policy restarts the service.

## Memory

A live browser session with a heavy page (GitHub, with a screenshot) uses about 250 MB. The gateway reads the service's memory limit and allows `(MB - 120) / 250` sessions at a time (at least 1). Further sessions get **HTTP 503** with a clear message instead of an out-of-memory kill.

| Service memory | Parallel sessions |
|---|---|
| 0.5 GB (Free plan) | 1 |
| 1 GB | 3 |
| 2 GB | 7 |

The MCP server counts as one session while it runs. It starts on the first MCP request and stops after 3 idle minutes (`OBSCURA_MCP_IDLE_SECONDS`), which frees its memory. Measured on Railway at 0.5 GB / 1 vCPU: peak 224 MB with one CDP session loading six heavy pages with screenshots, no OOM kill.

## Variables

| Variable | Default | What it does |
|---|---|---|
| `OBSCURA_TOKEN` | generated, 48 characters | Access token for CDP and MCP. Generated at deploy; nobody can use the browser without it. |
| `PORT` | `8080` | Port the gateway listens on. |
| `BROWSER_WS_ENDPOINT` | `wss://${{RAILWAY_PUBLIC_DOMAIN}}/devtools/browser?token=…` | Ready-made public CDP URL for Playwright `connectOverCDP` or Puppeteer `browserWSEndpoint`. |
| `BROWSER_WS_ENDPOINT_PRIVATE` | `ws://${{RAILWAY_PRIVATE_DOMAIN}}:${{PORT}}/devtools/browser?token=…` | The same over Railway's private network, for services in the same project. |
| `MCP_URL` | `https://${{RAILWAY_PUBLIC_DOMAIN}}/mcp` | MCP endpoint; send the token as a bearer header. |

Optional, not in the template (add them on the service if you need them): `OBSCURA_STEALTH=1` (consistent fingerprint and tracker blocking), `OBSCURA_PROXY` (HTTP or SOCKS5 proxy for page traffic), `OBSCURA_MAX_SESSIONS` (override the session limit), `OBSCURA_MCP_IDLE_SECONDS` (default 180), `OBSCURA_V8_FLAGS` (raw V8 flags), `OBSCURA_TIMEZONE`, `OBSCURA_NAV_TIMEOUT_MS` and Obscura's other [environment variables](https://github.com/h4ckf0r0day/obscura/blob/v0.2.7/docs/Environment-variables.md). Pages can't reach private or cloud-metadata addresses (Obscura's SSRF guard); `OBSCURA_ALLOW_PRIVATE_NETWORK=1` lifts that, but then pages can also reach your Railway private network.

## Connect

Playwright:

```javascript
import { chromium } from 'playwright-core';
const browser = await chromium.connectOverCDP(process.env.BROWSER_WS_ENDPOINT);
const page = await browser.newPage();
await page.goto('https://example.com');
console.log(await page.title());
await browser.close();
```

Puppeteer: `puppeteer.connect({ browserWSEndpoint: process.env.BROWSER_WS_ENDPOINT })`.

MCP (Claude Code):

```bash
claude mcp add --transport http obscura https://YOUR-DOMAIN/mcp --header "Authorization: Bearer YOUR-OBSCURA-TOKEN"
```

MCP (clients with a JSON config):

```json
{
  "mcpServers": {
    "obscura": {
      "type": "http",
      "url": "https://YOUR-DOMAIN/mcp",
      "headers": { "Authorization": "Bearer YOUR-OBSCURA-TOKEN" }
    }
  }
}
```

## Updates

Deployers get Railway's "update available" notice when this repo's `main` branch changes. Each change bumps the pinned image (version and digest together) and adds an entry to [`CHANGELOG.md`](CHANGELOG.md), with breaking changes listed first. Read it before you apply an update.

## Not included

- Obscura's multi-worker mode (`--workers`): one process is enough for the session limits above.
- The `-stealth` release build with TLS fingerprint impersonation: the official image is the render build. `OBSCURA_STEALTH=1` still gives a consistent fingerprint and tracker blocking.

## Licence

Obscura is Apache-2.0 (© its authors). This wrapper (Dockerfile, gateway, docs) is also Apache-2.0.
