# Deploy and Host Obscura on Railway

Obscura is an open-source headless browser for AI agents and web scraping, written in Rust. It runs page JavaScript in V8, speaks the Chrome DevTools Protocol (CDP) so Puppeteer and Playwright can drive it like remote Chrome, ships an MCP server with browser tools for AI agents, and renders screenshots and PDFs without Chromium.

## About Hosting Obscura

This template runs Obscura 0.2.7 from the official image as one service, built from a public GitHub repo. A small gateway on the Railway domain requires a token generated at deploy (`OBSCURA_TOKEN`) on every CDP and MCP request, rewrites CDP discovery to your public `wss://` address, and serves the MCP server at `/mcp`. Cookies and localStorage are kept on a volume at `/data`, so logins survive restarts and redeploys. Each live browser session takes about 250 MB, so the gateway allows as many sessions as the service's memory fits (one on the Free plan's 0.5 GB) and answers 503 beyond that instead of crashing. Nothing to set up after deploy.

## Common Use Cases

* A remote browser for AI agents (Claude, Cursor, browser-use, Stagehand, LangChain tools) over MCP or CDP, without running Chrome yourself
* Scraping JavaScript-rendered pages to HTML, text, Markdown (`LP.getMarkdown`), screenshots or PDFs from Puppeteer or Playwright
* Logging in to a site once and reusing the saved session cookies across later jobs

## Dependencies for Obscura Hosting

* Obscura 0.2.7, the official `h4ckf0r0day/obscura` image (pinned by digest)
* A Railway volume at `/data` for the browser profiles

### Deployment Dependencies

* Obscura repository and docs: https://github.com/h4ckf0r0day/obscura
* Release notes for 0.2.7: https://github.com/h4ckf0r0day/obscura/releases/tag/v0.2.7
* Template repository (Dockerfile, gateway, changelog): https://github.com/aalfath/obscura-railway-template

### Implementation Details

After deploy, copy `OBSCURA_TOKEN` (or the ready-made `BROWSER_WS_ENDPOINT` and `MCP_URL`) from the service's Variables tab.

Playwright or Puppeteer over CDP:

```javascript
import { chromium } from 'playwright-core';
const browser = await chromium.connectOverCDP(process.env.BROWSER_WS_ENDPOINT);
// wss://YOUR-DOMAIN/devtools/browser?token=YOUR-OBSCURA-TOKEN
const page = await browser.newPage();
await page.goto('https://example.com');
console.log(await page.title());
```

MCP client config (streamable HTTP, for clients that accept a remote URL with headers):

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

Claude Code: `claude mcp add --transport http obscura https://YOUR-DOMAIN/mcp --header "Authorization: Bearer YOUR-OBSCURA-TOKEN"`

What the template deploys: one service, **Obscura** (CDP on `/devtools/browser` and `/json/*`, MCP on `/mcp`, health check on `/healthz`, a volume at `/data`). The MCP server starts on the first MCP request and stops after 3 idle minutes to free its memory. Pages can't reach Railway's private network or cloud metadata addresses (Obscura's default SSRF guard). Optional: set `OBSCURA_STEALTH=1` for a consistent browser fingerprint and tracker blocking, `OBSCURA_PROXY` for an outbound HTTP or SOCKS5 proxy, and `OBSCURA_MAX_SESSIONS` to change the session limit. Give the service 1 GB of memory for 3 parallel sessions. Updates: when the template repo moves to a new Obscura release, Railway shows an update notice on your service; read `CHANGELOG.md` first, then apply it.

## Why Deploy Obscura on Railway?

<!-- Recommended: Keep this section as shown below -->
Railway is a singular platform to deploy your infrastructure stack. Railway will host your infrastructure so you don't have to deal with configuration, while allowing you to vertically and horizontally scale it.

By deploying Obscura on Railway, you are one step closer to supporting a complete full-stack application with minimal burden. Host your servers, databases, AI agents, and more on Railway.
<!-- End recommended section -->
