---
name: api-recon
description: Use this skill to discover a website's API endpoints, parameters, frontend routes, and UI trigger points within an explicitly authorized scope.
---

# API Recon

Within an explicitly authorized scope, discover backend APIs (paths, methods, parameters, and response bodies), frontend routes, and UI trigger points such as tabs, dialogs, and table actions.

## Scope and Prohibitions

This skill performs API and parameter-surface reconnaissance only. It is not a vulnerability-discovery or exploitation workflow.

| Area | Allowed | Prohibited |
| --- | --- | --- |
| Objective | Enumerate paths, methods, parameters, routes, and UI triggers | SQL injection, XSS, broken-access-control testing, brute force, vulnerability fuzzing, request tampering, or destructive actions |
| Authentication | Hook or stub the client-side login gate | Ask for or guess credentials; submit a real login form |
| Runtime | Use mocks to render the authenticated application shell without credentials | Depend on a real backend session to continue |

### Credential-free dynamic analysis

Phase 3 uses this approach by default:

1. Intercept and stub login, permission, menu, and other bootstrap endpoints with `preload.js` or `runtime_harvest.js`.
2. Return structurally valid success responses for business queries; the data may be empty.
3. Keep the frontend rendered even when the backend is absent or returns 401, allowing more XHR, fetch, WebSocket, and SSE calls to fire.
4. Empty tables and placeholder UI are expected. Do not switch to real authentication or vulnerability testing.

Use mocks to mount routes and components, and record outbound requests only. Backend data is not the objective; the objective is to learn which requests the frontend attempts.

### Hard workflow rules

| Do not | Use instead |
| --- | --- |
| Read, grep, or curl the main `index-*.js` entry to extract APIs before Phase 1 completes | Run `OUTDIR/harvest_static.py` |
| Write a replacement such as `extract_apis.py` | Modify `OUTDIR/harvest_static.py` and rerun it |
| Repeat the same failed grep or command two or more times | Change strategy: inspect tool logs, modify the harvester, or consult the reference |
| Skip Gate A or B and run the original files under `scripts/` | Copy scripts into `OUTDIR` and adapt them to the target |
| Use real passwords, OTP, OAuth, or other credentials | Use stubs and mocks |
| Skip stubbing to obtain real data or test authorization/injection | Record outbound requests only |
| Delete or export sensitive data, perform bulk writes, or trigger irreversible operations | Keep coverage interactions read-only and reversible |
| Claim complete coverage without runtime enumeration | State the limitation or satisfy the Definition of Done |
| Infer required/optional parameters from one runtime sample | Compare multiple samples or use validation/error evidence |

## Discovery Model and Runtime Modes

| Layer | Output | Limitation |
| --- | --- | --- |
| Static JS bundles | Endpoint paths, draft routes, and candidate payload fields | No reliable HTTP method; may miss runtime-built URLs |
| Runtime session | Methods, bodies, responses, dynamic URLs, WS/SSE, and parameter samples | A request appears only after its page or component renders |

| Mode | Engine | Best use |
| --- | --- | --- |
| `depth` | `runtime_harvest.js` with Puppeteer | Reproducible API inventory, methods, parameters, responses, WS, and SSE |
| `coverage` | Browser plus `preload.js` | Deeper feature coverage through tabs, dialogs, tables, and controls |
| `both` | Depth, then coverage | Most complete and most time-consuming |

For parameters, combine path anchors, expanded context windows, UI binding chains, multi-sample diffs, and error-driven inference. See [reference.md](reference.md#j-parameter-reverse-engineering).

## Definition of Done

All applicable items must be complete before claiming that reconnaissance is complete:

- [ ] Phase 1 produced `api_static.txt`, `routes.txt`, and `js/`.
- [ ] At least one runtime mode ran; `coverage` and `both` include an active hook and dynamic enumeration loop.
- [ ] Business routes remain outside `/login`, including hash-based routing.
- [ ] `coverage` or `both` produced `param_samples.json`; Phase 5 produced `params_merged.json`.
- [ ] If module pages were empty, Phase 4 reconstructed permissions and Phase 3 was rerun until module-level APIs appeared.
- [ ] Phase 5 outputs are complete and `insert_assets` received every discovered service and endpoint asset.

## Scripts and Quality Gates

Files under `scripts/` are reference templates, not ready-to-run final tools. Read them, copy them into `OUTDIR`, adapt them to the target, and record changes in `CHANGES.md`.

| Gate | When | Reference files | Typical changes |
| --- | --- | --- | --- |
| A — static | After Phase 0 and before the first harvest/spider run | `harvest_static.py`, `spider_mpa.py` | Endpoint regexes, webpack/Vite manifests, micro-frontend `publicPath`, MPA exclusions, cookies |
| B — runtime | After Phase 2 and before depth/coverage | `runtime_harvest.js`, `preload.js`, `config.json` | Storage keys, neutralization values, stubs, login patterns, API prefixes, hash/history routing |

For an SPA, after Phase 0 the next shell command must be the adapted harvester:

```bash
python3 OUTDIR/harvest_static.py <URL> OUTDIR
```

Do not manually extract APIs first. Before Phase 1 ends, verify output counts and fix the harvester if chunks return 404. From Phase 1b onward, grep only `OUTDIR/js/*.js`. For an MPA, run the adapted `spider_mpa.py` immediately after Phase 0.

## Tool and Output Constraints

| Constraint | Rule |
| --- | --- |
| Large bundles | Do not read or grep `index-*.js` files larger than 100 KB into the conversation context; process them with scripts |
| Grep output | Use `head -20` or `-m 5`; retain path summaries, not bundle excerpts |
| Validation | Use counts such as `wc -l` and `ls ... | wc -l`; do not read entire directories |
| Initial regex probe | At most once, and only on HTML or a chunk up to 50 KB |
| Reference material | Use [reference.md](reference.md) for recipes, templates, and troubleshooting |

## Workflow

1. [ ] Phase 0: classify SPA/MPA and create `OUTDIR`.
2. [ ] Gate A and Phase 1: copy, adapt, and immediately run the static harvester.
3. [ ] Phase 1b: reverse-engineer parameters into `param_candidates.json`.
4. [ ] Phase 2: identify the three authentication gates and write `config.json`.
5. [ ] Gate B: adapt runtime scripts.
6. [ ] Phase 3: run `depth`, `coverage`, or `both`; verify the application shell and capture parameter samples.
7. [ ] Phase 4, when required: reconstruct the permission tree and rerun Phase 3.
8. [ ] Phase 5: merge outputs, report limitations, and call `insert_assets` for all discovered assets.

## Phase 0 — Classification

Fetch the entry HTML and create `OUTDIR`; never modify the skill's original `scripts/` directory.

- SPA: an empty shell such as `<div id=app>` plus chunks; continue through Phases 1–5.
- MPA: server-rendered forms with no endpoint bundle; after Gate A run:

```bash
python3 recon/spider_mpa.py <BASE_URL> <OUTDIR> [--cookie "session=..."] [--max 300] [--depth 5] [--exclude "logout|delete|destroy"]
```

Expected MPA outputs are `forms.txt`, `links.txt`, and `api_inline.txt`.

## Phase 1 — Static Discovery

```bash
python3 recon/harvest_static.py <BASE_URL> <OUTDIR>
wc -l OUTDIR/api_static.txt OUTDIR/routes.txt
ls OUTDIR/js | wc -l
```

The harvester parses HTML scripts and webpack/Vite manifests, downloads lazy chunks, and writes `js/`, `api_static.txt`, `routes.txt`, and `chunkmap.txt`. If chunks return 404 or counts are unexpectedly low, fix the copied harvester and rerun it instead of downloading chunks manually.

### Phase 1b — Parameter Reverse Engineering

For each important endpoint, determine field names, transport location, inferred type, required/optional status, sample values, and confidence.

| Transport | Parameter location | Static evidence |
| --- | --- | --- |
| REST JSON | Body and query | `(params|data|body): { ... }` near the path |
| GraphQL | `variables` | GraphQL templates and declarations such as `$page: Int` |
| Traditional form | URL-encoded body | `<form>`, `FormData` |
| Upload | Multipart body | `FormData.append` |
| Path parameter | URL path | Route table, `useParams`, `$route.params` |
| Encrypted/signed | Wrapped in `sign` or `data` | Hook the encryption function's input |

Start from known paths and expand the surrounding context:

```bash
grep -n '"/api/user/list"' OUTDIR/js/*.js | head -20
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' OUTDIR/js/*.js | head -20
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' OUTDIR/js/*.js | head -20
```

Trace `Form field → submit handler → transform → API payload`, including interceptors that inject tenant IDs, pagination, authorization, or signatures. Write candidates as `{ path, fields[], source, confidence }` to `param_candidates.json`. Required, optional, and conditional fields require Phase 3 multi-sample evidence or Phase 5 error evidence.

## Phase 2 — Three Authentication Gates

Search only under `OUTDIR/js/` and record the findings in `config.json`.

| Gate | Question | Typical evidence |
| --- | --- | --- |
| Rendering gate | How does the client decide that a user is signed in? | `isLogin`, `getToken`, cookies, local/session storage |
| Interceptor gate | Which response causes a redirect to `/login`? | `response_code`, `errno`, Axios interceptors |
| Content gate | Where do menus, permissions, and routes come from? | `menu`, `permission`, `role`, `acl`, `routes` |

A storage key name is not a credential and is not sufficient evidence; confirm it from the bundle and request chain. Gate B is complete only after the conclusions are reflected in `config.json`, `OUTDIR/runtime_harvest.js`, and `preload.js`.

Optional observation settings:

| Setting | Output |
| --- | --- |
| `recordDetail: true` | `__API_RECON_DETAIL__` |
| `observe.xhrHeaders: true` | Request-header observations |
| `extractUrlsFromResponse: true` | Nested API URLs found in responses |
| `observe.storageReads/cookieReads: true` | Evidence for session-key configuration |
| `neutralizeVueRouter: true` | `__API_RECON_ROUTES__` |

## Phase 3 — Runtime Discovery

Set `runtimeMode` to `depth`, `coverage`, or `both`. Gate B and the credential-free mock strategy are mandatory.

| Stub tier | Scope | Purpose |
| --- | --- | --- |
| L1 exact | Authentication, permissions, and bootstrap endpoints | Pass initial client-side checks |
| L2 negative-response patch | All JSON responses | Convert logged-out codes to success |
| L3 fallback | Unmatched API paths | Return an empty success body so the UI can mount |

- `depth`: fake client auth, patch business codes, apply `stubs`, traverse hash/history routes, and write `runtime_api.json`.
- `coverage`: inject `preload.js` at document start with CDP `addScriptToEvaluateOnNewDocument` or a userscript.

Verify that `window.__API_RECON_PRELOAD__` exists and business routes do not return to `/login`.

```bash
cd recon && npm install
node runtime_harvest.js config.json
```

For coverage, click every safe navigation item, tab, first-row view/details action, filter, and reversible create/edit surface. Avoid irreversible deletion. Controlled `pushState` exploration is allowed for uncovered SPA routes, never for an MPA.

Capture at least one sample for each operation and compare bodies/queries:

| Operation | Typical additional fields |
| --- | --- |
| Initial list | Pagination and default filters |
| Search | Keywords and filters |
| Advanced filter | Optional fields |
| Create/edit | Full entity fields |
| Bulk/export/sort | `ids[]`, `exportType`, `sortField` |

Even with stubs, outbound request bodies and headers are genuine client output. Save `scan_raw.json`, `param_samples.json`, and `api_detail.json`.

## Phase 4 — Permission-tree Reconstruction

Run this phase when the application shell opens but module pages are blank or every route emits only bootstrap requests.

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl)[^"]*"' OUTDIR/js/*.js | sort -u | head -30
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|menuList|authList' OUTDIR/js/*.js | head -20
python3 recon/extract_route_map.py recon/js recon/
python3 recon/build_perm_tree.py recon/js recon/ --config recon/config.json
```

A common chain is `role_permissions` (flat codes) plus `permissions/all` (tree), passed through `getResultTree` and mapped through `userRouteAuth[CODE].url`.

Expected intermediate files include `route_map.json`, `userRouteAuth.json`, `permissions_tree.json`, `*_stub.json`, and `perm_codes_all.txt`. Verify that response wrappers match the interceptor gate, flat codes align with tree leaves, and `routes` covers every route-map link. Update `config.json` and rerun Phase 3.

## Phase 5 — Merge and Report

| Output | Phase | Content |
| --- | --- | --- |
| `js/`, `api_static.txt`, `routes.txt`, `chunkmap.txt` | 1 | Static bundles and paths |
| `param_candidates.json` | 1b | Static parameter candidates |
| `config.json` | 2 | Authentication-gate and runtime configuration |
| `runtime_api.json` | 3a | Detailed depth capture, including WS/SSE |
| `param_samples.json`, `scan_raw.json`, `api_detail.json` | 3b | Multiple samples and interaction logs |
| `route_map.json` and related files | 4 | Permission-tree artifacts |
| `params_merged.json` | 5 | Merged parameter fields and confidence |
| `api_merged.txt` | 5 | `METHOD /path [params] [static|runtime|both]` |
| `site_map.json` | 5 | Routes, APIs, parameters, trigger points, and limitations |

Diff parameter samples manually; there is no universal merge script. Within the authorized scope, incomplete requests may be used to read 400-level validation errors such as `field 'x' is required`. This remains parameter reconnaissance, not vulnerability testing.

The report must state the runtime mode, static/runtime API counts, parameter confidence, uncovered modules, and the target-specific changes recorded in `CHANGES.md`. Finally, call `insert_assets` for every discovered service and endpoint; do not omit known assets.

## General Notes

- The method applies to webpack, Vite, Angular, and similar lazy-loaded applications.
- REST/JSON, GraphQL, WebSocket, and SSE are in scope; gRPC-web is not.
- Client-side SSR fetches can be captured, but RSC and Server Actions may not be fully enumerable.
- JSVMP, WASM, HMAC, and mTLS can block runtime inspection; retain static results and state the limitation.
- Conditional fields, hidden parameters, and WASM payload construction may remain “not triggered” or “unreachable.”
- Static results remain the fallback when runtime access is blocked.

## Additional Resources

See [reference.md](reference.md) for grep recipes, configuration templates, hook details, permission-tree procedures, parameter methodology, and troubleshooting.
