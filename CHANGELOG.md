# Changelog

## 0.2.7 (2026-11-23)

First release of this template.

- Obscura 0.2.7, official image `h4ckf0r0day/obscura:0.2.7@sha256:e70c2abc925f2194c9338762075432c69328032cae220d77393f1495e90c601a`.
- Token gateway on `$PORT`: CDP at `/devtools/browser` and `/json/*` (discovery rewritten to the public `wss://` URL), MCP at `/mcp`, health at `/healthz`.
- Cookies and localStorage on the `/data` volume.
- Browser sessions limited from the memory limit (one at 0.5 GB); MCP server started on demand and stopped after 3 idle minutes.
