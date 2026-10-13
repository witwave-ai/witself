# WitSelf product page

This directory owns the one-page product brochure for WitSelf: what it does, supported AI clients, installation,
free-plan limits, and paid plans. It is a static public-information page. Account setup and management stay in the
WitSelf CLI and AI integrations; the brochure has no login flow.

**Publishing is on hold until the product domain is acquired and launch is authorized.** The proposed `witself.ai`
domain is not assumed to be owned or configured. Committing this source does not deploy the site, configure DNS,
verify a search property, or submit anything for indexing. No automatic deployment workflow is enabled for it.

## Preview locally

Run these commands from the repository root:

```sh
node web/product/build.mjs
python3 -m http.server 4178 --bind 127.0.0.1 --directory web/product/dist
```

Open <http://127.0.0.1:4178/>. The build writes generated files to `web/product/dist/`.
The default preview emits `noindex` metadata and a `robots.txt` crawl disallow, with no production canonical URL or
domain-ownership claim. Keep this preview local; crawler directives are not access controls.

When a public domain is owned, configured, and approved for launch, the production build interface is:

```sh
node web/product/build.mjs --site-url https://owned-domain.example/ --production
```

Replace the example URL with the actual canonical origin. Production mode generates the canonical URL and sitemap,
removes the preview indexing restriction, and permits crawling. It does not create a `CNAME`, configure hosting, or
publish the output.

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

## Repository ownership and hosting recommendation

This source lives in the **WitSelf monorepo at `web/product/`** so product behavior, install instructions, pricing,
and the brochure can change together. A separate marketing-source repository is not needed.
Give the site an independent deployment workflow and, later, CI scoped to this directory and its content dependencies
such as `web/plans/plans.json`. Publishing a brochure should not require a server release.

Use **GitHub for source and review, with Cloudflare static hosting for the public site**. GitHub's current
[Pages limits](https://docs.github.com/en/pages/getting-started-with-github-pages/github-pages-limits) prohibit using
Pages as free hosting for sites primarily facilitating commercial transactions or providing commercial SaaS.
Because this brochure markets paid SaaS plans, do not assume GitHub Pages is an appropriate production host. This is
a hosting recommendation; no deployment is included in the prototype.

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
