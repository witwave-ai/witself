# WitSelf product page

This directory owns the one-page product brochure for WitSelf: what it does, supported AI clients, installation,
free-plan limits, and paid plans. It is a static public-information page. Account setup and management stay in the
WitSelf CLI and AI integrations; the brochure has no login flow.

**Publishing is on hold until launch is authorized.** The product domain is `witself.com`, registered with
Cloudflare. Committing this source does not deploy the site, configure its custom domain, verify a search property,
or submit anything for indexing. The product workflow validates changes automatically; publishing requires a
separate manual launch and an explicitly enabled repository variable.

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
or application runtime. Only `web/product/dist/` is uploaded. The intended custom domain is `witself.com`; public
`workers.dev` and preview URLs are disabled. Connecting the custom domain is a separate operator action at the
first authorized launch. The workflow does not create domain records, add a `www` hostname, or configure redirects.

Both `route` and `routes` are deliberately absent from the Wrangler configuration. With `workers_dev=false`, this
preserves operator-managed routes and domains on subsequent deployments, as described in
[Wrangler's route-management guidance](https://developers.cloudflare.com/workers/wrangler/configuration/#source-of-truth).
Do not add `routes=[]`: an explicit empty list asks Wrangler to clear routes instead of leaving them managed outside
the workflow. GitHub deployment credentials grant **Workers Editor for the existing `witself-product` Worker
only**; they do not grant zone-write or custom-domain management permissions.

The workflow checks source syntax, preview indexing restrictions, the production output, and a Wrangler dry-run on
pull requests and pushes to `main` affecting this directory, the plan catalog, the workflow, or its shared Wrangler
dependencies. It does not depend on a server release. Generated files stay excluded from Git.

The HTML build uses Node 22 without package installation. Deployment tooling reuses the committed dependency lock
in `infra/cloudflare/control-plane/`, with lifecycle scripts disabled, and the existing `assertWranglerVersion`
gate in `infra/cloudflare/wrangler-version.mjs`. There is no second Wrangler version or floating `npx` download.
Every Wrangler invocation passes the reviewed empty environment file to avoid loading local dotenv files.

To check the deployment locally without credentials or publication, install the locked dependencies and run:

```sh
npm ci --ignore-scripts --prefix infra/cloudflare/control-plane
node --test infra/cloudflare/control-plane/test/wrangler-version.test.mjs
node web/product/build.mjs --site-url https://witself.com/ --production
infra/cloudflare/control-plane/node_modules/.bin/wrangler deploy \
  --config web/product/wrangler.toml \
  --env-file infra/cloudflare/wrangler-production-empty.env \
  --dry-run
```

### Launch hold and manual publishing

Keep the repository Actions variable `WITSELF_PRODUCT_PUBLISH_ENABLED` **false or unset** until launch is authorized.
Every publishing attempt requires all four conditions:

- The workflow is started manually through `workflow_dispatch`.
- The selected branch is `main`.
- The `publish` input is `true` (its default is `false`).
- The repository variable `WITSELF_PRODUCT_PUBLISH_ENABLED` is exactly `true`.

The deployment job enforces these conditions before receiving deployment credentials in its final step. Pushes
and pull requests never publish, even after the launch variable is enabled. A manual run with `publish=false`
performs validation only.

Before the first authorized launch:

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
   `main` with `publish=true`. The deployment rebuilds the exact selected commit before publishing.
5. Verify `https://witself.com/`, its HTTPS certificate, canonical URL, public assets, `robots.txt`, `sitemap.xml`,
   Markdown guide, and plan catalog.

Remove or set the launch variable to `false` to block further workflow publishing. That hold does not unpublish an
already live site. Domain registration, GitHub settings, credentials, and launching are separate actions; committing
this workflow does not perform them. No Cloudflare automatic build integration or GitHub Pages site is required.

## Search and AI discovery

The discovery foundation is useful public text, semantic headings, descriptive links, metadata, an accurate canonical
URL, matching structured data, and a sitemap. Google states that its AI search features require no special AI files
or schema, and that crawling, indexing, and appearance are not guaranteed. `llms.txt` and Markdown are convenience
formats, not ranking mechanisms. See [Google's AI features guidance](https://developers.google.com/search/docs/appearance/ai-features).

After the domain and hosting are ready:

1. Build and publish using the final canonical URL; verify HTTPS, crawler access, public links, and production metadata.
2. Verify the domain property in Google Search Console through the domain owner's DNS access.
3. Submit the generated sitemap and use URL Inspection to check the live homepage and request indexing.
4. Add a clear product link from Witwave and monitor Search Console for crawl issues and search visibility.

These are future launch steps. None has been completed by creating this local prototype.
