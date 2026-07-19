# Design: Service Worker offline fallback for Dashboards

Status: proposed, not implemented.

## Problem

When the entire cluster (including ingress-nginx) is unreachable — not just a
single backend returning a 5xx, but no response at all (DNS resolves, TCP
connect fails or times out) — the browser shows its own native offline
interstitial ("Network unreachable" / `chrome-error://`). No HTTP response
ever reaches the browser, so nothing server-side (a themed default backend,
a custom error page, etc.) can intervene.

This showed up concretely on a Dashboard consumer's home cluster: `app.jyje.online`
is a CNAME to a residential DDNS hostname, and the router doesn't do NAT
hairpin/loopback, so clients on the same LAN as the cluster get a hard
connection failure, not an HTTP error. That specific case is a router
configuration issue outside this operator's control, but it's one concrete
instance of a general problem: **any full outage** (cluster down, ISP
outage, node failure) produces the same browser-native failure page today,
for every Dashboard this operator manages.

The only way to intercept that and show something themed instead is a
**Service Worker** registered on a prior successful page load, with a cached
offline fallback page served on subsequent `fetch` failures. This is
unrelated to (and complementary with) server-side themed error pages like
[`tarampampam/error-pages`](https://github.com/tarampampam/error-pages) used
as an ingress-nginx default backend — that only helps when ingress-nginx
itself is reachable and returns an HTTP status. A Service Worker is the only
mechanism that works when *no* server is reachable at all.

## Why the current asset pipeline can't do this

Today (`pkg/homer/config.go`, `CreateDeployment`), each Dashboard Pod runs:

- an init/sidecar container `config-sync` (`alpine:3.18`) running a shell
  script built by `buildSidecarCommand()`, which symlinks `config.yml`,
  copies `AssetsConfig.ConfigMapRef` contents, and writes `manifest.json`
  (from `generatePWAManifest()`) — all under `/www/assets`
- the main `homer` container (`b4bz/homer` image), which serves the
  **baked-in** `/www` directory from its own image layer

Both containers mount a shared `assets-volume`, but **only at `/www/assets`**
(`buildSidecarVolumeMounts()` / the main container's `VolumeMounts` in
`CreateDeployment`). `/www/index.html` is never on a writable, shared volume
— it's whatever `b4bz/homer` baked into its image.

A Service Worker only intercepts navigation requests within its registration
**scope**, and registration itself only happens via a `<script>` tag that
must exist in the served HTML. Since we can't touch `index.html` today, we
can't register anything, regardless of what we write into `/www/assets`.
(Serving `sw.js` from `/www/assets/sw.js` with a `Service-Worker-Allowed: /`
response header could in principle broaden its scope to `/`, but we'd still
need `index.html` to contain the registration `<script>` — that header
alone doesn't solve the registration problem.)

## Proposed change

### 1. CRD API (`api/v1alpha1/dashboard_types.go`)

Add a new field to `AssetsConfig` (next to `Icons`/`PWA`, same file, ~line 244):

```go
type AssetsConfig struct {
	ConfigMapRef *AssetConfigMapRef `json:"configMapRef,omitempty"`
	Icons        *IconConfig        `json:"icons,omitempty"`
	PWA          *PWAConfig         `json:"pwa,omitempty"`
	// ServiceWorker configures an offline-fallback Service Worker
	ServiceWorker *ServiceWorkerConfig `json:"serviceWorker,omitempty"`
}

// ServiceWorkerConfig defines offline-fallback Service Worker configuration
type ServiceWorkerConfig struct {
	// Enabled controls whether the offline-fallback Service Worker is installed
	Enabled bool `json:"enabled,omitempty"`
	// OfflineHTML is inline HTML for the offline fallback page. If empty, a
	// default page is generated from the Dashboard's title/logo/theme colors
	// (same fields PWAConfig and HomerConfig.Colors already carry).
	OfflineHTML string `json:"offlineHtml,omitempty"`
	// CacheAssets are additional same-origin paths to precache alongside the
	// offline page (e.g. a logo, a webfont) so the fallback page renders
	// fully offline, not just as bare text.
	CacheAssets []string `json:"cacheAssets,omitempty"`
}
```

Mirrors the existing style of `PWAConfig`/`IconConfig` exactly (flat struct,
`omitempty`, doc comment per field) so `make generate` / `make manifests`
picks it up the same way.

### 2. Runtime: give the pipeline write access to the whole `/www`, not just `/www/assets`

This is the actual architecture change. Steps, in `pkg/homer/config.go`:

1. **New init container** (runs once, before `config-sync` and the main
   container start), using the *same* `HomerImage` the Dashboard already
   pulls (`config.HomerImage` — no new image to build or maintain):
   ```
   cp -a /www/. /shared-www/
   ```
   This seeds a new shared `emptyDir` volume (call it `www-volume`) with a
   full, writable copy of whatever that Homer image version actually ships
   — so this stays correct across `b4bz/homer` version bumps without this
   operator hardcoding the image's internal file list.

2. **Extend `config-sync`'s script** (`buildSidecarCommand`) to, when
   `ServiceWorker.Enabled`, additionally:
   - write `/shared-www/sw.js` (generated — see below)
   - write `/shared-www/offline.html` (from `ServiceWorkerConfig.OfflineHTML`,
     or a generated default — see below)
   - patch `/shared-www/index.html`: insert a registration `<script>` before
     `</body>` with `sed` (same string-building style already used for the
     PWA manifest heredoc, just a different target file)

3. **Change volume mounts**: both `config-sync` and the main `homer`
   container currently mount `assets-volume` at `/www/assets`. Replace that
   with mounting the new `www-volume` at `/www` in the main container (so it
   serves the patched copy instead of its own baked-in one), and mount
   `www-volume` with `subPath: assets` at `/www/assets` in `config-sync` (so
   its existing logic — `config.yml` symlink, custom assets copy, PWA
   manifest — is unaffected; it's just operating on a subdirectory of the
   same volume now instead of a separate one).

   This only activates when `ServiceWorker.Enabled` is true, to avoid
   the extra init container / copy cost and avoid touching the volume
   layout for every existing Dashboard that doesn't use this feature.

### 3. Generated `sw.js`

Minimal network-first-with-offline-fallback, scoped to `/`:

```js
const CACHE = 'homer-offline-v1'; // bump the suffix on operator-side content changes
const OFFLINE_URL = '/offline.html';

self.addEventListener('install', (event) => {
  event.waitUntil(
    caches.open(CACHE).then((cache) => cache.addAll([OFFLINE_URL /* + CacheAssets */]))
  );
  self.skipWaiting();
});

self.addEventListener('activate', (event) => {
  event.waitUntil(self.clients.claim());
});

self.addEventListener('fetch', (event) => {
  if (event.request.mode !== 'navigate') return; // only intercept page loads
  event.respondWith(
    fetch(event.request).catch(() => caches.match(OFFLINE_URL))
  );
});
```

`CacheAssets` entries get interpolated into the `cache.addAll([...])` list at
generation time.

### 4. Generated `offline.html` default

When `ServiceWorkerConfig.OfflineHTML` is empty, generate a minimal page
reusing fields the reconciler already has in hand (`Dashboard.Spec.HomerConfig.Title`,
`.Logo`, `.Colors.Dark` / `.Colors.Light`) so it visually matches the
Dashboard instead of being a generic stub — same spirit as `generatePWAManifest()`,
just emitting HTML instead of JSON.

## Rollout / testing

- New field is `omitempty` and off by default — zero behavior change for
  existing Dashboards.
- `internal/controller/assets_test.go` and `dashboard_controller_test.go`
  already cover the sidecar-command and volume-mount builders; extend both
  with `ServiceWorker.Enabled = true` cases rather than adding a parallel
  test file.
- Manual verification: enable on one Dashboard, load it once, then block
  the Pod's Service (e.g. `kubectl delete svc`) or the node's network and
  confirm the browser shows `offline.html`, not its native error page.

## Open questions / risks

- **Image layout drift**: step 1 assumes `b4bz/homer`'s `/www` layout is
  stable enough to blindly `cp -a`. Low risk in practice (it's a static
  site), but worth a comment in code pointing back here if the copy ever
  needs to get more selective.
- **Cache invalidation**: `sw.js`'s `CACHE` name needs bumping whenever the
  operator changes the generated offline page's shape, or returning users
  get a stale cached fallback. Simplest fix: derive the cache name suffix
  from a hash of the generated `offline.html` content instead of a hardcoded
  version string.
- **CSP / security headers**: if a future Dashboard sets a restrictive
  `Content-Security-Policy`, the injected inline `<script>` registration tag
  could be blocked. Not a concern today (no CSP support exists yet), but
  worth keeping in mind if that's ever added.
- This does **not** replace the ingress-nginx-level themed error pages
  (`tarampampam/error-pages`) already deployed in `jyje/cluster` — the two
  are complementary and cover different failure modes (backend down vs.
  cluster fully unreachable).

## Non-goals

- Full PWA "add to homescreen" installability is already covered by the
  existing `PWAConfig`; this design only adds the offline-fallback piece.
- No attempt to make Homer itself function offline (its data comes from
  live cluster discovery) — the offline page is explicitly a "come back
  later" placeholder, not a cached dashboard.
