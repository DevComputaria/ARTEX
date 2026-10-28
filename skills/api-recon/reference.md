# API Recon Reference

This reference contains grep recipes, configuration templates, hook coverage, permission-tree reconstruction, and parameter-analysis guidance. Run grep commands against the copied `js/` directory. If a bundle is one minified line, use `js-beautify`, `sed 's/}/}\n/g'`, or a bounded raw-grep context window.

## Script Templates

Every file under `scripts/` is a reference template and must be adapted before use.

| File | Typical target-specific changes |
| --- | --- |
| `harvest_static.py` | Endpoint regexes, webpack/Vite manifest parsing, micro-frontend `publicPath`, retries, concurrency |
| `runtime_harvest.js` | Neutralized fields/success values, stub shapes, route source, WS capture, timeouts, proxy |
| `preload.js` | `loginPathRe`, L1 stubs, neutralization, `apiPattern`, mock tier, detail recording, observers, Vue-router handling |
| `spider_mpa.py` | Destructive-link exclusions, cookies, depth/maximum, same-origin policy |
| `extract_route_map.py` | `routeMap`/`routeLink` patterns and key naming |
| `build_perm_tree.py` | `userRouteAuth`, root/prefix hierarchy, response-wrapper fields |
| `config.json` | Single target-specific configuration entry point |

Place adapted copies in the task directory, such as `recon/`, and document changes in `CHANGES.md`.

## A. Three Authentication Gates

### A1. Rendering gate: how is sign-in state determined?

```bash
grep -rhoaE '.{0,40}(isLogin|isAuthenticated|loggedIn|hasLogin|requireAuth)\b.{0,80}' js | head
grep -rhoaE 'function (getUser|getToken|getAuth)[0-9]?\([^)]*\)\{.{0,200}' js | head
grep -rhoaE '(localStorage|sessionStorage)\.getItem\("[^"]+"\)' js | sort -u
grep -rhoaE '(Cookies?|cookie)\.(get|load)\("[^"]+"\)' js | sort -u
grep -rhoaE '\batob\(|JSON\.parse\(|jwt|decode' js | head
```

Trace chains such as `isLogin = f(getUser())` and `getUser = decode(storage.read(KEY))`. Record the storage key, container, and encoding.

| Encoding | Mock configuration |
| --- | --- |
| Plain string, `"1"`, or token | `"value": "anything-truthy"` |
| `JSON.parse(x)` | `"value": "json:{\"id\":1,\"username\":\"admin\"}"` |
| `JSON.parse(atob(x))` | `"value": "b64json:{\"id\":1,\"username\":\"admin\"}"` |
| JWT | Unsigned/`alg:none` JWT, or a key found in the bundle when authorized |
| SM2/AES/RSA | Use a hard-coded key only when the rendering gate merely requires a decodable blob; otherwise retain static coverage |

Write the result to `cookies` or `localStorage`.

### A2. Interceptor gate: what redirects to `/login`?

```bash
grep -rhoaE '.{0,60}(interceptors\.response|axios|request\.use).{0,120}' js | head
grep -rhoaE '.{0,40}(response_code|errcode|errno|\bcode\b|\bret\b|\bstatus\b)\s*[=!]==?\s*[\-0-9]{1,4}.{0,60}' js | head -20
grep -rhoaE '.{0,40}(not\s*logged\s*in|unauthorized|forbidden|token.{0,10}invalid).{0,40}' js | head
grep -rhoaE '.{0,30}(location\.href|router\.(push|replace)|navigate)\([^)]*login[^)]*\)' js | head
```

Identify the code field, its success value, and redirect-triggering failures. When authorized, validate with a junk client-side session:

```bash
curl -sk -X POST -H 'Cookie: <fake-key>=junk' https://target/api/<protected> -d '{}' -H 'Content-Type: application/json'
```

Write the result to `neutralize.fields` and `neutralize.success`.

### A3. Content gate: where do menus and permissions come from?

```bash
grep -rhoaE '"/api[^"]*(permission|perm|role|menu|acl|resource|nav)[^"]*"' js | sort -u
grep -rhoaE '.{0,30}(menus|permissions|menuList|routeList|authList|role_permissions)\b.{0,120}' js | head
grep -rhoaE 'userRouteAuth|getResultTree|routeMap|routeLink|hasPermission|checkAuth' js | head
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head
```

Common enterprise applications use two layers:

| Source | Typical payload | Consumer |
| --- | --- | --- |
| `role_permissions` | `{ permissions: string[], role_type }` | Route guards and button-level ACLs |
| `permissions/all` | `tree[{ code, position, children }]` | Sidebar rendering |
| Bundle `userRouteAuth` | `{ CODE: { url, name? } }` | Permission code to frontend path |
| Bundle `routeMap` | `{ KEY: { name, link } }` | Webpack alias resolution |

Inspect the consumer to learn how `getResultTree`, `v-if`, or `hasAuth(code)` filters data. Small sites may need only permissive manual stubs; larger sites require Section I.

## B. `config.json` Template

```json
{
  "baseUrl": "https://target/",
  "runtimeMode": "both",
  "chromium": "/usr/bin/chromium",
  "cookies": [
    {"name": "auth", "value": "b64json:{\"id\":1,\"username\":\"admin\",\"role\":\"admin\",\"permissions\":[\"*\"]}"}
  ],
  "localStorage": {"token": "faketoken", "isLogin": "1"},
  "neutralize": {
    "fields": ["response_code", "code", "errno", "ret", "status"],
    "success": 0,
    "flags": {"success": true, "message": "ok"}
  },
  "forward": true,
  "loginUrlPattern": "/login",
  "apiPattern": "/api/|/rest/|/graphql",
  "mockTier": "L1+L2",
  "recordDetail": true,
  "observe": {"storageReads": false, "cookieReads": false, "xhrHeaders": true},
  "neutralizeVueRouter": true,
  "stubs": [
    {
      "match": "permissions/all|/menu|role_permissions",
      "body": {
        "response_code": 0,
        "code": 0,
        "data": {
          "permissions": ["*"],
          "menus": [
            {"name": "dashboard", "path": "/dashboard", "show": true, "children": []},
            {"name": "alert", "path": "/alert", "show": true, "children": []}
          ]
        }
      }
    }
  ],
  "explore": {"clickTabs": true, "clickTables": true, "pushStateFallback": true, "maxMenuItems": 50},
  "routes": ["/dashboard", "/alert", "/asset", "/device", "/report", "/config", "/system"],
  "waitMs": 1500,
  "perRouteMs": 900,
  "headless": true,
  "waitUntil": "domcontentloaded",
  "routeTimeout": 12000,
  "proxy": "",
  "captureResponses": true,
  "recordWs": true,
  "respMax": 600
}
```

- `b64json:` means base64-encoded JSON; `json:` means raw JSON; no prefix means a literal value.
- `forward: true` forwards real requests and rewrites code fields; `false` is fully offline.
- `mockTier` controls coverage-mode preload layers such as `L1+L2+L3`.
- `routes` normally comes from `routes.txt`; a forged menu can add anchors to the harness.
- `captureResponses` and `recordWs` affect depth mode only.
- Prefer `domcontentloaded` for large SPAs; `networkidle2` may never settle.
- `proxy` maps to Puppeteer's `--proxy-server`; `HTTP_PROXY` and `HTTPS_PROXY` are also supported.

Use paired stubs when the application expects both flat permissions and a tree:

```json
"stubs": [
  {
    "match": "role_permissions",
    "body": {"response_code": 0, "data": {"permissions": ["MONITOR", "MONITOR_ALERT", "THREAT", "ASSETS_RISK"], "role_type": "SUPER_ADMIN"}}
  },
  {
    "match": "permissions/all",
    "body": {"response_code": 0, "data": [{"code": "MONITOR", "position": 1, "children": [{"code": "MONITOR_ALERT", "position": 1, "children": []}]}]}
  }
]
```

Response wrappers must match the interceptor gate, and flat permissions must cover all required tree leaves.

## C. Coverage-mode Preload

Adapt the `CONFIG` object at the top of `preload.js` before document-start injection:

```javascript
const CONFIG = {
  loginPathRe: /\/(login|signin)(\/|$|\?)/i,
  mockTier: 'L1+L2',
  forward: true,
  recordDetail: true,
  extractUrlsFromResponse: true,
  neutralizeVueRouter: true,
  observe: { storageReads: false, cookieReads: false, xhrHeaders: true },
  neutralize: { fields: ['response_code', 'code'], success: 0 },
  stubs: [],
  apiPattern: /\/(api|apis|v\d+|dev|internal|graphql)\//i,
};
```

Confirm `window.__API_RECON_PRELOAD__ === true` and a stable pathname. Export results with:

```javascript
JSON.stringify({
  apis: [...window.__API_RECON_LOG__],
  detail: window.__API_RECON_DETAIL__,
  routes: [...(window.__API_RECON_ROUTES__ || [])],
  observe: window.__API_RECON_OBSERVE__,
}, null, 2)
```

## D. Hook Coverage

| Capability | Value | Coverage |
| --- | --- | --- |
| Hook fetch and XHR.open | Request URL/method | `recordDetail`, `__API_RECON_LOG__` |
| Hook XHR.setRequestHeader | Authorization and custom headers | `observe.xhrHeaders` |
| Observe storage/cookie reads | Session-key evidence | Optional observers |
| Read loaded Vue routes | Complete frontend routes | `__API_RECON_ROUTES__` |
| Neutralize Vue guards/login redirects | Mount protected modules | `neutralizeVueRouter` plus native redirect handling |
| React routes | Complete routes | Static results plus clicks; no dedicated hook |
| Hook encryption functions | Plaintext before encryption | Manual hook required |
| Anti-debug bypass | Restore runtime capture | Manual handling required; static results remain valid |

## E. Endpoint Regex Fallback

If static output is unexpectedly small, relax `extract_endpoints` in the copied harvester:

```bash
grep -rhoaE '"/[a-z][A-Za-z0-9_/\-]{3,}"' js | sort -u
grep -rhoaE '/api/[a-zA-Z0-9_./-]+' js | sort -u
```

## F. Troubleshooting

| Symptom | Resolution |
| --- | --- |
| Very few static APIs | Relax endpoint patterns |
| Far fewer chunks than the manifest | Distinguish CSS-only/undeployed chunks and retry 404s |
| Runtime remains on login | Recheck rendering-gate key, container, encoding, domain, and injection timing |
| Shell opens but modules are blank | Fix the content gate, forged menu, or route paths |
| Every route emits only bootstrap/locale | Reconstruct permissions and pair both permission stubs |
| Sidebar exists but child pages are blank | Add intermediate tree nodes and align codes with `userRouteAuth` |
| Every API redirects to login | Fix neutralization and nested-field walking |
| No WS frames | Trigger the subscription interaction and increase `perRouteMs` |
| Empty response bodies | Real bodies require `forward: true` |
| Chromium missing | Install Chromium or set `config.chromium` / `CHROMIUM` |
| Many mocks but login persists | Inject at document start and handle `location.href` |
| Empty lists | L3 empty arrays are expected; continue through tabs/settings/details |
| Redux actions mistaken for routes | Filter internal get/set/change/clear/toggle/upload paths |
| Response URLs absent from log | Enable `extractUrlsFromResponse` or inspect detail output |
| Unknown authorization header | Enable `observe.xhrHeaders` or inspect DevTools |
| Runtime slow or timing out | Use `domcontentloaded`, reduce route timeouts, avoid `networkidle2` |
| Proxy failure | Align config/environment proxy values for Puppeteer and curl |

## G. Hardened Targets

Signed cookies, server-side session checks, or non-stubbable server-rendered menus may stop runtime discovery at the shell. Static discovery can still enumerate paths. Only when the authorization explicitly permits it, the same harness may use a real session with `forward: true` and no neutralization.

## H. Per-task Checklist

1. Confirm authorization and scope.
2. Adapt and run `harvest_static.py`; review `api_static.txt` and `routes.txt`.
3. Expand path anchors and trace bindings into `param_candidates.json`.
4. Analyze all three gates and write target-specific configuration.
5. Adapt runtime scripts before execution.
6. Run depth and/or coverage with document-start preload.
7. Complete the parameter-trigger matrix.
8. Reconstruct permissions when modules do not render, then rerun.
9. Diff parameter samples and use validation errors carefully.
10. Merge outputs and report coverage, gaps, and script changes honestly.

## I. Permission-tree Reconstruction

Use this when a simple permissive menu does not mount child modules.

```bash
grep -l 'userRouteAuth' js/*.js
grep -l 'routeMap\|routeLink' js/*.js
grep -rhoaE 'getResultTree|role_permissions|permissions/all' js | head
python3 scripts/extract_route_map.py recon/js recon/
python3 scripts/build_perm_tree.py recon/js recon/ --config recon/config.json
```

Record permission endpoint paths, response fields, and consumer chunks. If route extraction fails, adapt its regex or inspect:

```bash
grep -rhoaE '([A-Z_][A-Z0-9_]*):\{name:"[^"]*",link:"/[^"]+"\}' js | head -20
```

The builder resolves `userRouteAuth`, webpack aliases, and parent prefixes; it emits `permissions_tree.json`, `permissions_all_stub.json`, and `role_permissions_stub.json`, and can update `config.json`. Adapt `DEFAULT_ROOTS`, `DEFAULT_PREFIX_PARENT`, and `DEFAULT_EXTRA_PARENT` to the target.

Validate consistency:

```bash
# The permission count should approximately match userRouteAuth entries.
wc -l recon/perm_codes_all.txt
# Routes should cover every route-map link.
python3 -c "import json; m=json.load(open('recon/route_map.json')); r=set(json.load(open('recon/config.json'))['routes']); print('missing', [v['link'] for v in m.values() if v['link'] not in r])"
node recon/runtime_harvest.js recon/config.json
```

A successful reconstruction changes identical bootstrap-only traffic into distinct module APIs. If it still fails, use coverage clicks, compare authorized real response nesting with stubs, inspect button-level guards, and retain static endpoints as the fallback.

## J. Parameter Reverse Engineering

This is a methodology, not a universal script: use regexes for paths, then anchor expansion, UI binding chains, multi-sample diffs, and validation errors for fields.

```bash
# Known path to payload construction.
grep -n '"/api/user/list"' js/*.js
grep -rhoaE '.{0,120}("/api[^"]+").{0,200}' js | head
grep -rhoaE '(params|data|body|payload)\s*:\s*\{' js | head
grep -rhoaE '(get|post|put|delete|patch)\([^,]+,\s*\{' js | head

# Wrappers and transports.
grep -rhoaE '(axios|request)\.(get|post|put|delete|patch)\(' js | head
grep -rhoaE 'interceptors\.(request|response)' js | head
grep -rhoaE '(query|mutation)\s+\w+|gql`|graphql\(' js | head
grep -rhoaE '\$[a-zA-Z_]+\s*:\s*(Int|String|Boolean|\[)' js | head
grep -rhoaE 'FormData|\.append\(' js | head
grep -rhoaE 'path:\s*"/[^"]*:[^"]+"|useParams|route\.params|\$route\.params' js | head

# Validation and bindings.
grep -rhoaE '(required|message|pattern|enum|validator)\s*:' js | head
grep -rhoaE 'yup\.|zod\.|async-validator|Form\.Item|a-form-item|el-form-item' js | head
grep -rhoaE 'onFinish|handleSubmit|getFieldsValue|validateFields' js | head
grep -rhoaE '(pick|omit|transform|dayjs|moment)\(' js | head
grep -rhoaE 'encrypt|decrypt|sign|CryptoJS|sm2|sm3|sm4|RSA|AES' js | head
```

Do not guess fields from ciphertext. Hook the encryption function's input and record plaintext before encryption.

Capture and diff at least initial list, search, advanced-filter, create/edit, bulk/export, and sort/page actions. Write samples as:

```json
[{"path":"/api/example","method":"POST","action":"search","body":{},"query":{},"headers":{}}]
```

| Confidence | Evidence |
| --- | --- |
| High | Static call site plus at least two matching runtime samples |
| Medium | Static-only evidence or one runtime sample |
| Low | Response/error inference without confirmation |
| Not triggered | Static field known but its UI/permission path was not reached |

Keep different body shapes for the same path separated by `action`; do not force one schema. Read outbound requests even when stub responses are synthetic. Account for wrappers such as `data`, `bizData`, and `variables`.
