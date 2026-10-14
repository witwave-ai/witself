# Product screenshots

The brochure photographs the real Agent Console and the real terminal model with
closed, synthetic fixtures. No live agent, account, credential, or database is used.
The browser console loads the embedded application through its normal authenticated
loopback server. The TUI uses `agenttui.Model.Snapshot` (which refuses live sources),
then displays its actual ANSI output in xterm.js for a PNG capture. Neither image is
an illustration or a separately maintained HTML imitation of the product.

## Refresh

From the repository root, with Go from `go.mod` and Node 22:

```sh
npm ci --ignore-scripts --prefix scripts/dashboard-acceptance
npm ci --ignore-scripts --prefix web/product/screenshots
node scripts/dashboard-acceptance/node_modules/playwright/cli.js install chromium
node web/product/screenshots/capture.mjs
node web/product/build.mjs
```

Review both `web/product/assets/screenshots/*.png` and the local brochure before
committing the two PNGs and their `manifest.json`. Keep generated `dist` untracked.
An existing brochure copy edit needs only the last command, with no browser or Go
build. The capture tools and demo helper never enter the published static assets.

For candidate images without replacing the brochure's approved assets:

```sh
node web/product/screenshots/capture.mjs --out /tmp/witself-screenshot-candidates
```

The helper accepts an ANSI output path only: it has no endpoint, account, or token
input. Its random tokens protect local synthetic listeners and stay in the parent
process, outside output images and manifests. The browser refuses external network
requests. The parent lifetime controls helper shutdown; temporary files are cleaned
up on both success and failure. Captures stop on missing content or browser errors.

## Keep images current

The manifest records a digest of the UI sources, demo fixtures, capture scripts and
locked dependencies, together with each PNG's dimensions and digest. The brochure
build refuses stale or altered assets. Normal copy changes do not invalidate them.
This is source freshness, not a claim that every UI change alters the screenshot or
that screenshots replace application acceptance tests.

The `product-screenshots` workflow runs on relevant UI/capture changes, generates
fresh candidates as a downloadable artifact, and flags stale committed images.
Download that artifact, visually review it, then replace the PNGs and manifest in
`web/product/assets/screenshots/`, or run the local command above. Commit the approved
assets with the UI change. Publishing continues through the existing manual product
workflow; screenshot generation has no deployment credentials or repository write
access. This workflow becomes active once its source is merged.

Browser and terminal versions are locked. Platform fonts can still produce small
rendering differences, and the console displays a local listener address. Freshness
therefore compares source inputs, not pixel-for-pixel equality of a new capture.
The manifest also records the capture platform and browser version for review.

Use captions that say **real interfaces with synthetic demo data**. The console and
TUI are optional local inspection views, not a hosted account-management dashboard.
Keep the brochure's AI/CLI management message primary.
