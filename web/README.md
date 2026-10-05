# LLM Gateway Console — Web (T04)

React + TypeScript + Vite + Ant Design + React Router + TanStack Query.
Same-origin Console session (`/api/v1`) and Gateway data plane (`/v1`); no SSR,
no Next.js, no token in Web Storage.

```bash
cd web
npm ci                 # exact versions from package-lock.json
npm run dev            # http://127.0.0.1:5173 (proxies /api/v1 -> :8092, /v1 -> :8090)
npm run typecheck      # tsc --noEmit (strict)
npm test               # vitest run
npm run build          # tsc --noEmit && vite build -> dist/
```

Run the backends first (`make console-run`, and the Gateway/Control from the
repository root as usual); `make console-bootstrap` creates the first admin.

Deployment: serve `dist/` with `nginx.conf` from this directory (SPA fallback,
`/api/v1` → Console, `/v1` → Gateway with SSE buffering disabled).

Full documentation — pages, wired endpoints, state model, contract gaps and the
browser checklist — is in [`../docs/web/README.md`](../docs/web/README.md).
