# Search and AI discovery runbook

This is the recovery and maintenance record for the public WitSelf brochure at
<https://witself.com/>. It covers the marketing site, not the authenticated WitSelf service.
Source stays in `web/product/`; generated output stays disposable in `web/product/dist/`.
Hosting and release instructions are in [README.md](README.md).

## What Git reproduces

| Component | Source and behavior |
| --- | --- |
| Product information | [content.mjs](content.mjs), setup examples in [build.mjs](build.mjs), and [plans.json](../plans/plans.json) generate static HTML and the corresponding `product.md`. Important information needs no JavaScript to read. |
| Search metadata | `build.mjs` generates the title, description, canonical URL, Open Graph metadata, and production `index, follow, max-snippet:-1`. |
| Structured data | JSON-LD describes Witwave, WitSelf, the website, and available monthly offers. Prices come from the shared plan catalog. Keep claims consistent with visible content; do not add invented reviews or ratings. |
| Crawl discovery | Production `robots.txt` allows crawling and names `https://witself.com/sitemap.xml`. The sitemap contains the canonical homepage; section fragments are not separate pages. |
| AI reading formats | `llms.txt` points to `product.md`, `plans.json`, GitHub, documentation, and Witwave. The HTML advertises the Markdown alternative. These formats help readers consume the same information; they do not register the site with an AI platform. |
| Preview protection | Default builds emit `noindex, nofollow`, disallow crawling, and omit the canonical URL and sitemap. Keep previews local; robots directives are not access control. |
| Hosting | [wrangler.toml](wrangler.toml) names `witself-product`, uploads static assets, and disables `workers.dev` and preview URLs. Custom domains and zone rules are managed separately. |

Build from the repository root with Node 22:

```sh
node web/product/build.mjs --site-url https://witself.com/ --production
```

This recreates the public files, not provider accounts, DNS verification, firewall settings, or search submissions.
Return a local preview to its safe default with `node web/product/build.mjs` after inspecting production output.
Never hand-edit `dist/` to make an SEO change.

## Existing automation and its limits

[CI](../../.github/workflows/ci.yml) validates affected website changes on pull requests and `main`;
[product-site.yml](../../.github/workflows/product-site.yml) handles publication separately. Both call
[product-site-checks.yml](../../.github/workflows/product-site-checks.yml), using the same local command:

```sh
npm ci --ignore-scripts --prefix infra/cloudflare/control-plane
make check-product-site
```

This checks source syntax, preview indexing restrictions, required production files, the exact canonical and robots
directives, sitemap URL, `llms.txt` guide link, plan-catalog equality, JSON-LD parsing, screenshot freshness and PNGs,
and a Cloudflare deployment dry run. The command restores the safe local preview afterward.

The complete Git diff determines the lane using the exact [brochure allowlist](../../scripts/ci-product-scope.mjs).
Brochure-only changes keep the six required merge checks but satisfy them through website validation, without the
full application suite. Application, mixed, shared dependency/pricing, hosting configuration, tooling, workflow,
and policy changes keep full application gates. Missing comparison evidence fails closed to the full path.
See [README.md](README.md#fast-brochure-checks) and [CLAUDE.md](../../CLAUDE.md) for the local pre-push policy.

A brochure-only push to `main` publishes automatically after website validation. The publication switch must be
exactly `true`; the deployment must use the current `main` commit and the protected `product-site` environment.
Other changes retain manual `publish=true` publication after full CI. Manual `publish=false` runs validate only.
No publishing path needs to wait for a server release, and pull requests never receive deployment credentials.

These checks do **not** verify live response headers, Cloudflare rules, external links, Google/Bing ownership or
indexing, or actual AI-provider fetches. Parsing JSON-LD does not prove schema validity or rich-result eligibility.
There is currently no automatic search-submission or IndexNow hook, no scheduled crawl monitor, and no checked-in
post-deployment HTTP verifier. The public checks below are the repeatable manual procedure.

## External settings to preserve or recreate

Use a company-controlled login with recoverable ownership. The launch used `scott@witwave.ai` for Google and Bing;
recover access through the company's account administration. Do not put passwords, deployment tokens, session
cookies, or recovery codes in Git. Provider-issued verification values must come from the selected property;
do not copy another company's example values.

| System | Required state and recovery action |
| --- | --- |
| Cloudflare hosting | Active `witself.com` zone, HTTPS certificate, and apex custom domain connected to `witself-product`. Restore through the hosting README. Retain DNS verification records when changing hosting. |
| GitHub deployment | Environment `product-site` restricted to `main`; repository variables `CLOUDFLARE_ACCOUNT_ID` and `WITSELF_PRODUCT_PUBLISH_ENABLED`; environment secret `CLOUDFLARE_API_TOKEN` scoped to editing the existing Worker. CI does not have zone-write access. |
| [Google Search Console](https://search.google.com/search-console) | Verify the **Domain property `witself.com`** using Google's DNS TXT record. Submit `https://witself.com/sitemap.xml`. Inspect the canonical homepage, run the live test, then request indexing when appropriate. Preserve property access and DNS verification. Check the property-level **Search generative AI** inclusion control where available; launch observation was **Include**. |
| [Bing Webmaster Tools](https://www.bing.com/webmasters/) | Add `https://witself.com/`. The launch used Bing's DNS CNAME verification, with Cloudflare proxying disabled for that verification record. Recreate the exact provider-issued name/target if recovery requires it. Submit the sitemap, inspect/test the homepage, and request indexing. |
| GitHub inbound links | Repository homepage/About URL should be `https://witself.com/`; retain the product link near the start of the root [README](../../README.md). |
| Witwave inbound links | Keep a visible product link from `https://witwave.ai/` and a link in its `llms.txt`. Product, installation, and pricing traffic goes to WitSelf; company/team-project content remains on Witwave. Its source and deployment belong to the Witwave repository. |

Sitemap submission instructions: [Google](https://developers.google.com/search/docs/crawling-indexing/sitemaps/build-sitemap)
and [Bing](https://www.bing.com/webmasters/help/sitemaps-3b5cf6ed). A submitted or successfully processed sitemap is
discovery evidence, not proof that its page is indexed. Do not repeatedly submit an unchanged URL as a ranking tactic.

## Cloudflare public-reader exception

The launch encountered HTTP 403 / error 1010 for ordinary `Python-urllib/3.14` requests even though browsers could
read the site. The approved Configuration Rule **Allow public brochure readers** disables **only Browser Integrity
Check** for this explicit public surface:

```text
(http.host eq "witself.com" and
 http.request.method in {"GET" "HEAD"} and
 http.request.uri.path in {
   "/" "/index.html" "/robots.txt" "/sitemap.xml" "/llms.txt"
   "/product.md" "/plans.json" "/styles.css" "/app.js" "/favicon.svg"
   "/assets/screenshots/agent-console.png" "/assets/screenshots/agent-tui.png"
 })
```

Restore it as a Cloudflare **Configuration Rule**, enabled, with Browser Integrity Check **Off** (`bic: false`).
As verified on 2026-10-14, the existing rule ID is `6635688790c142b2ac92d5823f1b3181`, at order **1 (First)**.
This is not a blanket WAF skip or an exception for the authenticated application. Inspect other matching rules and
their order before applying; a later conflicting setting can change the result. Record any deliberate scope change
here. See [Cloudflare's configuration settings](https://developers.cloudflare.com/rules/configuration-rules/settings/).

This exception is operator-managed and is **not** applied by Wrangler or GitHub CI. When transferring the zone,
retain a reviewed export of the rule's condition, action, enabled state, and ordering alongside the operational
handoff; this document records the intended condition, not a live export of the whole zone.

**Screenshot gap resolved 2026-10-14:** the two newer screenshot PNGs initially returned 403 for the generic Python
client while the identified client succeeded. After explicit approval, the existing rule was extended with only
the two exact screenshot paths shown above. Its hostname, GET/HEAD restriction, sole BIC setting, enabled state,
and first position were retained. After propagation, generic GET and HEAD returned 200 across all twelve named
paths; both screenshots had `image/png` responses and their GET bodies matched the committed PNG files. This is
public-client access evidence, not proof of a visit by an AI provider. Do not test only a browser-like user agent.

Also inspect Cloudflare AI-crawler blocking, managed `robots.txt`, bot rules, and challenges during recovery.
The original limited BIC exception does not establish what those separate controls allow.

## AI platforms: access, not a universal registration

- **Google AI search:** preserve Googlebot access, index eligibility, and snippet eligibility. Google says no special
  AI file or schema is required. `llms.txt` is supplementary. `Google-Extended` covers separate uses and is not the
  switch for Google Search inclusion. [Google guidance](https://developers.google.com/search/docs/appearance/ai-features).
- **ChatGPT search:** review `OAI-SearchBot` access. `ChatGPT-User` supports user-requested fetching; `GPTBot` concerns
  potential model-training use. Search and training controls are independent. Use current published IP information
  when investigating a genuine provider block. [OpenAI crawler documentation](https://developers.openai.com/api/docs/bots).
- **Perplexity:** review `PerplexityBot` for search and `Perplexity-User` for requested reads. Check both robots rules
  and the CDN; validate provider identity against its current published ranges if an exception is needed.
  [Perplexity crawler documentation](https://docs.perplexity.ai/docs/resources/perplexity-crawlers).

The generated robots policy currently allows all crawlers; no separate training opt-out is generated. An AI crawler's
name in a test User-Agent header does not authenticate that provider or prove it has visited. No separate Perplexity
or OpenAI website registration was performed. Successful fetching, search indexing, and being cited in an AI answer
are different outcomes; none guarantees ranking or citations.

## Recheck after publication or a hosting/rule change

1. Confirm the successful deployment used the intended `main` commit. Build and preview checks must pass before
   publishing; do not publish the default preview output.
2. Fetch the homepage, crawler files, text guide, plan catalog, styles/script/icon, and both screenshots without
   authentication. Check **GET and HEAD**, HTTPS, content types, actual bodies, and response headers. A challenge
   page with status 200 is not a pass. Public documents must not acquire `X-Robots-Tag: noindex` or restrictive
   snippet headers from the CDN.
3. Compare deployed content to the approved build, allowing only independently understood hosting transformations
   such as Cloudflare's analytics beacon. Confirm the canonical is `https://witself.com/`, robots permits crawling,
   the sitemap names the homepage, JSON-LD parses, and `product.md` agrees with visible content and current prices.
4. Repeat with an ordinary generic HTTP client. When a response differs, inspect Cloudflare security events for the
   exact path, method, time, and rule before changing protection. Verify real provider access with provider live
   tests or authenticated crawler evidence, not spoofed User-Agent strings alone.
5. Check Google/Bing sitemap processing and URL Inspection. Keep their reported status and last crawl date separate
   from the deployment date. Recheck GitHub and Witwave inbound links.

Quick public transport check (no credentials, no provider settings changed):

```sh
for ua in 'WitSelf-Discovery-Verification/1.0' 'Python-urllib/3.14'; do
  for path in / /index.html /robots.txt /sitemap.xml /llms.txt /product.md \
    /plans.json /styles.css /app.js /favicon.svg \
    /assets/screenshots/agent-console.png /assets/screenshots/agent-tui.png; do
    curl --silent --show-error --max-time 20 --user-agent "$ua" \
      --output /dev/null --write-out "GET %{http_code} %{content_type} $path [$ua]\n" \
      "https://witself.com$path"
    curl --silent --show-error --head --max-time 20 --user-agent "$ua" \
      --output /dev/null --write-out "HEAD %{http_code} %{content_type} $path [$ua]\n" \
      "https://witself.com$path"
  done
done
```

This prints diagnostics; it is not an automated pass/fail gate and does not inspect response bodies. Inspect bodies
and headers separately, for example `curl --include https://witself.com/robots.txt`. Follow the checklist even when
all statuses are 200. Restore the unindexed local build after local production inspection.

## Dated evidence and remaining work

Google and Bing observations below were retained from the launch session, not freshly rechecked in their
authenticated dashboards during this documentation audit. The Cloudflare update and public checks are dated
separately. Recheck provider state when restoring the site.

| Evidence | Last observed result |
| --- | --- |
| Google, 2026-10-14 launch session | Domain verified; sitemap submitted; homepage inspection reported **URL is on Google / Page is indexed**; **Search generative AI = Include**. |
| Bing, 2026-10-14 launch session | DNS verification completed; sitemap **Success**, one discovered URL; live test said the page could be indexed; indexing requested. Actual Bing inclusion was still pending at that observation. |
| Cloudflare, 2026-10-14 launch session | Limited public-reader rule active; generic GET/HEAD succeeded on its ten named paths. |
| Links | GitHub README link landed in [PR #729](https://github.com/witwave-ai/witself/pull/729). The Witwave product panel and `llms.txt` link were deployed separately. |
| Published brochure | [PR #733](https://github.com/witwave-ai/witself/pull/733), main `f03d54de`; [deployment](https://github.com/witwave-ai/witself/actions/runs/38596943619) and [post-merge CI](https://github.com/witwave-ai/witself/actions/runs/38596912868) passed. |
| Public recheck, 2026-10-14 documentation audit | Homepage plus nine named files returned 200 to identified and generic GETs, with no `X-Robots-Tag` restriction. Homepage had the correct canonical, indexable metadata, static commands, and three top-level JSON-LD types. Effective robots allowed crawling and named the sitemap. Both screenshots passed the identified client; generic requests returned 403. GitHub homepage metadata and the live Witwave homepage linked to WitSelf; Google verification TXT was present. This is public HTTP/DNS evidence, not a fresh provider-console audit. |
| Cloudflare screenshot fix, 2026-10-14 | Approved two-path extension saved to the existing rule; dashboard confirmed active at order 1. All 24 generic GET/HEAD checks passed across twelve public paths, without challenge or restrictive indexing headers. Screenshot responses were `image/png`; both GET bodies matched the committed PNGs. |

Follow-ups are explicit: verify Bing inclusion and recheck provider crawler traffic. The Witwave automatic publisher credential previously failed;
its product link was published manually, and that separate repository's publishing repair is not part of this setup.

The build assertions now share one local/CI command. A future read-only post-deployment HTTP check could cover
the public files. An optional
[IndexNow notification](https://www.indexnow.org/documentation) could notify participating
engines after a successful, verified deployment; it is not installed here and would not replace Google Search
Console. These are proposed hooks, not currently running jobs. Do not add recurring submissions or promise index
coverage. Record future changes here with the deployment commit, date, provider-reported result, and unresolved gaps.
