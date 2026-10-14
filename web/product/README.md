# WitSelf product page

This directory owns the one-page product brochure for WitSelf: what it does, supported AI clients, installation,
free-plan limits, and paid plans. It is a static public-information page. Account setup and management stay in the
WitSelf CLI and AI integrations; the brochure has no login flow.

**The product page is live at <https://witself.com/>.** The domain is registered with Cloudflare. Committing this
source locally does not publish it. Brochure-only changes merged into `main` publish automatically after website
checks pass, while the repository's publication switch is enabled. Application or mixed changes retain manual
publication. Neither path configures domains, verifies search properties, or submits pages for indexing.
See [DISCOVERY.md](DISCOVERY.md) for the search/AI setup, recovery procedure, and dated verification record.

## Preview locally

Run these commands from the repository root:

```sh
node web/product/build.mjs
python3 -m http.server 4178 --bind 127.0.0.1 --directory web/product/dist
```

Open <http://127.0.0.1:4178/>. The build writes generated files to `web/product/dist/`.
The default preview emits `noindex` metadata and a `robots.txt` crawl disallow, with no production canonical URL or
domain-ownership claim. Keep this preview local; crawler directives are not access controls.

The production build targets the product domain:

```sh
node web/product/build.mjs --site-url https://witself.com/ --production
```

Production mode generates the canonical URL and sitemap, removes the preview indexing restriction, and permits
crawling. Building does not configure hosting or publish the output.

## Content and maintenance

- [The plan catalog](../plans/plans.json) is the source for plan prices and quotas. Update that catalog before
  rebuilding the brochure; do not maintain a separate price list in generated output.
- [The feature scorecard](../../docs/feature-status.md) is the evidence check for feature claims and availability.
  Distinguish implemented capabilities from conditional rollout, plan entitlement, and future work.
- [The repository README](../../README.md) and current installation and integration documentation govern command
  examples and supported-client wording. Do not imply identical behavior across every AI client.
- Important content is delivered as static HTML, including installation commands and plan details. A Markdown copy
  and `llms.txt` provide convenient text access to the same public information; they must not introduce hidden claims
  or different offers.
- The footer identifies WitSelf as a product of [Witwave](https://witwave.ai/). The teal accent and terminal motif
  carry over Witwave's visual heritage while keeping this product brochure compact and basic.

Keep generated output disposable. Review both the rendered page and its text representation whenever product copy,
prices, or entitlements change.

The brochure includes real terminal and browser console screenshots with synthetic demo data. Their capture tool,
source-freshness checks, and automatic review-artifact workflow are documented in
[screenshots/README.md](screenshots/README.md). Ordinary brochure copy builds use the committed PNGs; refreshing
screenshots is a separate command that needs Go and the locked browser tools.

## Repository ownership and Cloudflare hosting

This source lives in the **WitSelf monorepo at `web/product/`** so product behavior, install instructions, pricing,
and the brochure can change together. A separate marketing-source or publishing repository is not needed.
GitHub stores the source and runs the independent [product-site workflow](../../.github/workflows/product-site.yml).
[Cloudflare Workers Static Assets](https://developers.cloudflare.com/workers/static-assets/) hosts the generated
brochure. This is an assets-only Worker named `witself-product`; [wrangler.toml](wrangler.toml) has no Worker script
or application runtime. Only `web/product/dist/` is uploaded. Its custom domain is `witself.com`; public
`workers.dev` and preview URLs are disabled. Connecting or restoring the custom domain is a separate operator action.
The workflow does not create domain records, add a `www` hostname, or configure redirects.

Both `route` and `routes` are deliberately absent from the Wrangler configuration. With `workers_dev=false`, this
preserves operator-managed routes and domains on subsequent deployments, as described in
[Wrangler's route-management guidance](https://developers.cloudflare.com/workers/wrangler/configuration/#source-of-truth).
Do not add `routes=[]`: an explicit empty list asks Wrangler to clear routes instead of leaving them managed outside
the workflow. GitHub deployment credentials grant **Workers Editor for the existing `witself-product` Worker
only**; they do not grant zone-write or custom-domain management permissions.

### Fast brochure checks

[CI](../../.github/workflows/ci.yml) and the publisher share
[product-site-checks.yml](../../.github/workflows/product-site-checks.yml), which runs `make check-product-site`.
It checks source syntax, preview indexing restrictions, required production files, canonical/robots/sitemap/AI
reading metadata, plan equality, JSON-LD parsing, screenshot freshness, and a locked Wrangler deployment dry run.
It needs Node and the deployment dependencies, without Go, PostgreSQL, Helm, or an application release.

The [scope classifier](../../scripts/ci-product-scope.mjs) uses the complete Git diff, including both sides of
renames, rather than GitHub's limited path-filter list. Its exact file allowlist covers brochure content,
layout/build code, icon, the two screenshots and manifest, and this directory's README and discovery runbook.
Only changes entirely within that list use the fast path. Shared plan prices, dependencies, screenshot tooling,
`wrangler.toml`, workflows/checks, policy, new unknown files, and mixed application changes use the full suite.
An unavailable or empty comparison also takes the full path.

All six required merge-check names remain active: brochure-only changes satisfy them through successful website
validation; application and mixed changes must satisfy their original application jobs as well. This preserves
branch protection without waiting for deliberately skipped application jobs. Screenshot capture still has its
own bounded workflow when its inputs change. Generated files stay excluded from Git.

The HTML build uses Node 22 without package installation. Deployment tooling reuses the committed dependency lock
in `infra/cloudflare/control-plane/`, with lifecycle scripts disabled, and the existing `assertWranglerVersion`
gate in `infra/cloudflare/wrangler-version.mjs`. There is no second Wrangler version or floating `npx` download.
Every Wrangler invocation passes the reviewed empty environment file to avoid loading local dotenv files.

To check locally without credentials or publication:

```sh
npm ci --ignore-scripts --prefix infra/cloudflare/control-plane
make check-product-site
```

The check restores the unindexed local preview even after a failed production check; if it cannot restore it,
it removes `dist/`. To select the correct pre-push gate, first fetch `origin/main`, then run
`node scripts/ci-product-scope.mjs --base origin/main`. Only `"website_only":true` qualifies for the website-only
local gate; otherwise follow the full `make check` policy in [CLAUDE.md](../../CLAUDE.md).

### Automatic publishing and recovery

Launch was authorized and `WITSELF_PRODUCT_PUBLISH_ENABLED` was verified as `true` on 2026-10-14.
For a fresh environment, keep it **false or unset** until launch is authorized. Publication always requires
passed website validation, the `main` branch, the current `main` commit, the `product-site` environment, and an
exactly `true` repository publication variable. Old queued runs cannot overwrite a newer `main` commit.

A push to `main` publishes automatically only when the complete push is positively classified as brochure-only.
Pull requests never publish or receive the Cloudflare secret. Application, shared, or mixed changes do not
publish automatically; after their full CI passes, run **product-site** manually from `main` with `publish=true`.
Manual runs default to `publish=false`, which validates without deploying. This remains the recovery/redeploy path.
The deployment rebuilds production output from the checked commit and exposes the scoped token only to the
final deployment step. The website workflow does not wait for unrelated application CI.

For a fresh environment or hosting recovery:

1. In a fresh account, an operator must first create `witself-product` without public routes, a custom domain,
   `workers.dev`, or preview URLs, using the reviewed assets and pinned Wrangler. This bootstrap is needed only
   once: a token limited to an existing Worker cannot create it. The current account already has this Worker.
2. Confirm the `witself.com` zone is active in the intended Cloudflare account. At first authorized launch, the
   operator separately connects `witself.com` to `witself-product` through Cloudflare's
   [custom-domain setup](https://developers.cloudflare.com/workers/configuration/routing/custom-domains/), including
   review of conflicting DNS records and certificate readiness. Connecting the domain makes the current assets
   publicly reachable; it is a launch action, not part of the GitHub workflow.
3. Configure the GitHub environment **product-site**, restricted to deployments from `main`. Set the repository
   variable `CLOUDFLARE_ACCOUNT_ID` to the intended account and add the environment secret `CLOUDFLARE_API_TOKEN`.
   Use **Workers Editor scoped only to the existing `witself-product` Worker**, without zone-write permissions.
   The operator manages the custom domain separately. Follow
   [Cloudflare's GitHub Actions guidance](https://developers.cloudflare.com/workers/ci-cd/external-cicd/github-actions/).
   The account ID is public configuration; keep the API token out of this repository and ordinary Actions variables.
4. Enable the repository variable `WITSELF_PRODUCT_PUBLISH_ENABLED=true`, then manually run **product-site** from
   `main` with `publish=true`. The selected commit must still be the current `main` commit at deployment time.
5. Verify `https://witself.com/`, its HTTPS certificate, canonical URL, public assets, `robots.txt`, `sitemap.xml`,
   Markdown guide, and plan catalog.

Remove or set the launch variable to `false` to block further workflow publishing. That hold does not unpublish an
already live site. Domain registration, GitHub settings, and credentials remain separate operator actions;
the workflow does not create or modify them. No Cloudflare automatic build integration or GitHub Pages site is required.

## Search and AI discovery

The discovery foundation is useful public text, semantic headings, descriptive links, metadata, an accurate canonical
URL, matching structured data, and a sitemap. Google states that its AI search features require no special AI files
or schema, and that crawling, indexing, and appearance are not guaranteed. `llms.txt` and Markdown are convenience
formats, not ranking mechanisms. See [Google's AI features guidance](https://developers.google.com/search/docs/appearance/ai-features).

The build and CI preserve the on-page foundation. Search Console, Bing Webmaster Tools, DNS ownership, Cloudflare
crawler access, and inbound links require separate operational configuration. [DISCOVERY.md](DISCOVERY.md) records
what was done, what can be rebuilt from Git, what must be restored with the providers, and how to verify the result.
It also distinguishes submission, crawlability, indexing, and AI citations; none is proof of the next.
