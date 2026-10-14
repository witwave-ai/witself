import { readFile, writeFile, mkdir, rm, copyFile } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import { product as p } from './content.mjs';
import { verifyScreenshots } from './screenshots/verify.mjs';

const root = new URL('./', import.meta.url);
const out = new URL('./dist/', root);
const args = process.argv.slice(2);
let production = false;
let siteURL;
for (let i = 0; i < args.length; i++) {
  if (args[i] === '--production') production = true;
  else if (args[i] === '--site-url' && args[i + 1]) siteURL = new URL(args[++i]);
  else throw new Error('Usage: node web/product/build.mjs [--site-url https://owned-domain.example/] [--production]');
}
if (siteURL && (siteURL.protocol !== 'https:' || siteURL.username || siteURL.password || siteURL.search || siteURL.hash)) {
  throw new Error('The site URL must be an HTTPS URL without credentials, query, or fragment.');
}
if (production && !siteURL) throw new Error('--production requires --site-url for the domain you control.');
if (siteURL && !siteURL.pathname.endsWith('/')) siteURL.pathname += '/';
const canonical = production ? siteURL.href : null;
const screenshotManifest = await verifyScreenshots();
const screenshotSize = (src) => screenshotManifest.images[src.split('/').at(-1)];
const absolute = (path) => canonical ? new URL(path, canonical).href : path;
const esc = (s) => String(s).replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;').replaceAll('"', '&quot;');
const catalog = JSON.parse(await readFile(new URL('../plans/plans.json', root), 'utf8'));
const plans = catalog.plans.filter(plan => plan.available);
const fmt = (n) => n == null ? 'Custom' : new Intl.NumberFormat('en-US').format(n);
const has = (plan, key) => plan.features.includes(key) && !catalog.coming_soon_features.includes(key);
const repo = 'https://github.com/witwave-ai/witself';
const docs = `${repo}/blob/main/docs`;
const professional = plans.find(plan => plan.id === 'standard');
if (!professional) throw new Error('The management example requires the available Professional plan.');
const managementExample = p.management.example.map(([speaker, text]) => [speaker, text.replaceAll('{price}', `$${fmt(professional.price_monthly)}`)]);
const code = (text, id, label = 'Homebrew install command', wrap = false) => `<div class="codebox"><pre><code${id ? ` id="${id}"` : ''}>${wrap ? text.split(' ').map(word => `<span>${esc(word)}</span>`).join(' ') : esc(text)}</code></pre>${id ? `<button type="button" data-copy="${id}" hidden aria-label="Copy ${esc(label)}">Copy</button>` : ''}</div>`;
const steps = [
  {title: 'Install the CLI', text: 'Use Homebrew above, or choose a native installer. Release installers verify checksums.', commands: ''},
  {title: 'Create your account and agent', text: 'Start free. Complete the browser challenge, retry with the challenge token, and verify your email. Then create a realm, agent, and local token.', commands: 'witself account create --name personal --email you@example.com --accept-terms\n# Complete the challenge; retry the same command with --challenge TOKEN.\n# Follow the verification link in your email, then check status:\nwitself account status --account personal\nwitself realm create --account personal main\n# Use the returned realm ID and agent ID in the next commands:\nwitself agent create --account personal --realm REALM_ID assistant\nwitself token create --account personal --agent AGENT_ID'},
  {title: 'Connect your AI tool', text: 'Install your preferred integration, restart the AI client, and start a new session. Then ask it to remember something useful. The example below connects Codex with user-scoped hooks.', commands: 'witself install codex --account personal --realm main --agent assistant --user-hooks\nwitself integrations --verify'},
];
const rows = [
  ['Realms (isolated workspaces)', plan => fmt(plan.limits.realms)],
  ['Agents per realm', plan => fmt(plan.limits.agents_per_realm)],
  ['Active memories per agent', plan => fmt(plan.limits.stored_memory)],
  ['Facts per agent', plan => fmt(plan.limits.stored_fact)],
  ['Secrets per agent', plan => plan.limits.stored_secret ? fmt(plan.limits.stored_secret) : 'Not included'],
  ['Transcript retention', plan => `${plan.policies.transcript_retention_days} days`],
  ['People on the account', plan => fmt(plan.limits.operator_seats)],
  ['Agent messaging & collaboration', plan => has(plan, 'messaging') && has(plan, 'collaboration') ? 'Included' : 'Not included'],
  ['Support', plan => has(plan, 'support') ? 'Included' : 'Not included'],
];
const summaries = {
  free: 'A lasting memory for your personal agents.',
  standard: 'More context. Secrets. Agents working together.',
  team: 'Separate workspaces for a larger agent team.',
};
const priceCards = plans.map(plan => `<article class="plan${plan.recommended ? ' featured' : ''}"><p class="plan-label">${esc(plan.name)}${plan.recommended ? '<span>For your daily work</span>' : ''}</p><p class="price">$${fmt(plan.price_monthly)}<span>/ month</span></p><p class="plan-description">${esc(summaries[plan.id] || plan.summary)}</p><ul><li>${fmt(plan.limits.realms)} ${plan.limits.realms === 1 ? 'realm' : 'realms'} · ${fmt(plan.limits.agents_per_realm)} agents ${plan.limits.realms === 1 ? 'total' : 'per realm'}</li><li>${fmt(plan.limits.stored_memory)} memories per agent</li><li>${fmt(plan.limits.stored_fact)} facts per agent</li><li>${plan.limits.stored_secret ? `${fmt(plan.limits.stored_secret)} secrets per agent` : 'Memory and facts included'}</li><li>${has(plan, 'collaboration') ? 'Messaging, collaboration & support' : `${plan.policies.transcript_retention_days}-day transcript retention`}</li></ul><a class="button ${plan.recommended ? 'primary' : 'secondary'}" href="#install">${plan.price_monthly === 0 ? 'Start free' : 'Get started'} <span aria-hidden="true">↗</span></a></article>`).join('\n');
const table = `<div class="table-scroll" tabindex="0" role="region" aria-label="Plan comparison; scroll horizontally on small screens"><table><caption>Managed-service plan comparison · ${esc(catalog.currency)}</caption><thead><tr><th scope="col">What’s included</th>${plans.map(plan => `<th scope="col">${esc(plan.name)}</th>`).join('')}</tr></thead><tbody>${rows.map(([label, value]) => `<tr><th scope="row">${esc(label)}</th>${plans.map(plan => `<td>${esc(value(plan))}</td>`).join('')}</tr>`).join('')}</tbody></table></div>`;
const schema = {
  '@context': 'https://schema.org', '@graph': [
    {'@type': 'Organization', '@id': 'https://witwave.ai/#organization', name: 'Witwave', url: 'https://witwave.ai/', sameAs: ['https://github.com/witwave-ai']},
    {'@type': 'SoftwareApplication', name: p.name, description: p.summary, applicationCategory: 'DeveloperApplication', operatingSystem: 'macOS, Linux, Windows', ...(canonical ? {url: canonical} : {}), publisher: {'@id': 'https://witwave.ai/#organization'}, sameAs: [repo],
      offers: plans.map(plan => ({'@type': 'Offer', name: `${plan.name} monthly plan`, price: plan.price_monthly, priceCurrency: catalog.currency, priceSpecification: {'@type': 'UnitPriceSpecification', price: plan.price_monthly, priceCurrency: catalog.currency, unitText: 'month'}, ...(canonical ? {url: `${canonical}#pricing`} : {})}))},
    ...(canonical ? [{'@type': 'WebSite', url: canonical, name: p.name, publisher: {'@id': 'https://witwave.ai/#organization'}}] : []),
  ],
};
const html = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>${esc(p.title)}</title>
<meta name="description" content="${esc(p.summary)}">
<meta name="robots" content="${production ? 'index, follow, max-snippet:-1' : 'noindex, nofollow'}">
${canonical ? `<link rel="canonical" href="${esc(canonical)}">\n<meta property="og:url" content="${esc(canonical)}">` : ''}
<meta property="og:type" content="website">
<meta property="og:site_name" content="WitSelf">
<meta property="og:title" content="${esc(p.title)}">
<meta property="og:description" content="${esc(p.summary)}">
<meta name="twitter:card" content="summary">
<meta name="theme-color" content="#f7f9f5">
<link rel="icon" href="favicon.svg" type="image/svg+xml">
<link rel="alternate" href="product.md" type="text/markdown" title="WitSelf product guide in Markdown">
<link rel="stylesheet" href="styles.css">
<script type="application/ld+json">${JSON.stringify(schema).replaceAll('<', '\\u003c')}</script>
<script defer src="app.js"></script>
</head>
<body>
<a class="skip-link" href="#main">Skip to content</a>
<header class="site-header wrap">
  <a class="brand" href="#" aria-label="WitSelf home"><span class="brand-mark" aria-hidden="true">›_</span> WitSelf<span class="brand-by">by Witwave</span></a>
  <nav aria-label="Main navigation"><a href="#inspect">Product</a><a href="#pricing">Pricing</a><a class="repo-link" href="${repo}">GitHub <span aria-hidden="true">↗</span></a></nav>
</header>
<main id="main">
  <section class="hero wrap" aria-labelledby="hero-title">
    <div class="hero-copy"><p class="eyebrow"><span class="status-dot"></span> Persistent context for AI agents</p>
      <h1 id="hero-title">Memory that moves<br>with your AI<span class="accent">.</span></h1>
      <p class="hero-description">${esc(p.summary)}</p>
      <div class="hero-actions"><a class="button primary" href="#install">Get started free <span aria-hidden="true">↗</span></a><a class="text-link" href="${repo}">Explore the source <span aria-hidden="true">↗</span></a></div>
      <div class="hero-install">${code(p.installers[0].command, 'brew-command')}<p>macOS · Linux · Windows <span>Free plan available</span></p></div>
    </div>
    <aside class="continuity" aria-label="Illustrative example: the same agent using two AI tools">
      <div class="demo-heading"><span class="mono">ONE AGENT. CONTINUOUS CONTEXT.</span><span class="demo-label">Example</span></div>
      <div class="conversation"><span class="tool-label"><span class="tool-initial">C</span> In Claude Code <span>Today</span></span><p>“Remember why we chose<br>PostgreSQL for this project.”</p></div>
      <div class="memory-link"><span></span><div><b class="brand-mark small" aria-hidden="true">›_</b><strong>WitSelf</strong><small>Identity · facts · memory</small></div><span></span></div>
      <div class="conversation"><span class="tool-label"><span class="tool-initial dark">↗</span> In Codex <span>Next session</span></span><p>“What did we decide<br>about the database?”</p></div>
      <p class="demo-footnote">Connect both tools to the same WitSelf agent.<br>Recall the saved decision and its context.</p>
    </aside>
  </section>
  <section class="overview wrap" aria-labelledby="overview-title">
    <div class="section-intro"><p class="eyebrow">Built to carry forward</p><h2 id="overview-title">${esc(p.intro)}</h2><p>${esc(p.introText)}</p></div>
    <div class="feature-list">${p.features.map(([title, text], i) => `<article><span class="feature-number">0${i + 1}</span><h3>${esc(title)}</h3><p>${esc(text)}</p></article>`).join('')}</div>
  </section>
  <section id="inspect" class="inspection wrap" aria-labelledby="inspection-title">
    <div class="section-intro"><p class="eyebrow">${esc(p.inspection.eyebrow)}</p><h2 id="inspection-title">${esc(p.inspection.title)}</h2><p>${esc(p.inspection.intro)}</p></div>
    <div class="inspection-grid">${p.inspection.screenshots.map(screenshot => `<figure class="inspection-card"><a class="inspection-image" href="${esc(screenshot.src)}" aria-label="View full-size ${esc(screenshot.name)} screenshot"><img src="${esc(screenshot.src)}" alt="${esc(screenshot.alt)}" width="${screenshotSize(screenshot.src).width}" height="${screenshotSize(screenshot.src).height}" loading="lazy" decoding="async"></a><figcaption><h3>${esc(screenshot.name)}</h3><p>${esc(screenshot.caption)}</p><p class="inspection-command-label">${esc(p.inspection.commandLabel)}</p>${code(screenshot.command, screenshot.commandId, `${screenshot.name} command`, true)}<p>${esc(screenshot.commandNote)}</p><a class="text-link" href="${docs}/${esc(screenshot.guide)}">Explore the ${screenshot.guide === 'agent-console.md' ? 'console' : 'terminal'} guide <span aria-hidden="true">↗</span></a></figcaption></figure>`).join('')}</div>
    <p class="fine-print inspection-caption">${esc(p.inspection.screenshotNote)}</p>
    <div class="inspection-footer"><p>${esc(p.inspection.boundary)}</p><p class="inspection-demo">${esc(p.inspection.demoLabel)} <code>${esc(p.inspection.demoCommand)}</code></p></div>
  </section>
  <section id="integrations" class="integrations wrap" aria-labelledby="integrations-title">
    <div class="section-heading"><div><p class="eyebrow">Works where you work</p><h2 id="integrations-title">One memory. Across your tools.</h2></div><a class="text-link" href="${docs}/cli-command-surface.md#witself-install">Integration guide <span aria-hidden="true">↗</span></a></div>
    <div class="runtime-grid">${p.runtimes.map(r => `<article class="runtime"><h3>${esc(r.name)}</h3><p>${esc(r.note)}</p><code>witself install ${esc(r.command)}</code></article>`).join('')}</div>
    <p class="fine-print">${esc(p.runtimeNote)} Commands shown assume a configured agent. <a href="${docs}/feature-status.md">Read the support details.</a></p>
  </section>
  <section id="pricing" class="pricing-band" aria-labelledby="pricing-title"><div class="wrap">
    <div class="section-heading"><div><p class="eyebrow">Start with the free plan</p><h2 id="pricing-title">A little memory. A lot of possibility.</h2></div><p>Flat monthly pricing in USD.<br>Your AI provider’s costs are separate.</p></div>
    <div class="plans">${priceCards}</div>
    <details class="comparison"><summary>Compare plans <span aria-hidden="true">+</span></summary>${table}</details>
    <p class="fine-print roadmap">Enterprise is coming soon. Agent email, email aliases, and custom email domains are coming soon and are not currently included as delivered features. <a href="plans.json">View the full plan catalog.</a></p>
  </div></section>
  <section id="manage" class="management wrap" aria-labelledby="management-title">
    <div class="management-copy"><p class="eyebrow">Managed in plain English</p><h2 id="management-title">${esc(p.management.title)}</h2><p class="management-promise">${esc(p.management.promise)}</p><p class="management-intro">${esc(p.management.intro)}</p>
      <dl class="management-prompts">${p.management.prompts.map(([label, prompt]) => `<div><dt>${esc(label)}</dt><dd>“${esc(prompt)}”</dd></div>`).join('')}</dl>
    </div>
    <aside class="upgrade-example" aria-label="Illustrative AI conversation about upgrading to Professional">
      <div class="demo-heading"><span class="mono">${esc(p.management.exampleTitle)}</span><span class="demo-label">Example conversation</span></div>
      <ol class="upgrade-messages">${managementExample.map(([speaker, text]) => `<li class="${speaker === 'You' ? 'from-you' : 'from-ai'}"><span>${esc(speaker)}</span><p>${esc(text)}</p></li>`).join('')}</ol>
      <p class="demo-footnote">${esc(p.management.exampleNote)}</p>
    </aside>
    <div class="management-notes"><p>${esc(p.management.requirement)}</p><p>${esc(p.management.support)}</p></div>
  </section>
  <section id="install" class="install wrap" aria-labelledby="install-title">
    <div class="section-intro"><p class="eyebrow">Start with a conversation</p><h2 id="install-title">Let your AI help you get set up.</h2><p>${esc(p.setupIntro)}</p><blockquote class="setup-prompt">“${esc(p.setupPrompt)}”</blockquote><p class="setup-note">Use an AI with local command access. You’ll complete identity verification and review the terms. The commands are here whenever you need them.</p></div>
    <ol class="setup-steps">${steps.map((step, i) => `<li><span class="step-index">${i + 1}</span><div><h3>${esc(step.title)}</h3><p>${esc(step.text)}</p>${i === 0 ? p.installers.map((installer, j) => j === 0 ? `<p class="command-label">${esc(installer.name)}</p>${code(installer.command)}` : `<details class="installer"><summary>${esc(installer.name)}</summary>${code(installer.command)}</details>`).join('') : i === 1 ? `<details class="installer"><summary>Account and agent setup commands</summary>${code(step.commands)}<p class="fine-print">Replace you@example.com, REALM_ID, and AGENT_ID with your own values. Read the <a href="https://self.witwave.ai/legal/terms">Terms</a> and <a href="https://self.witwave.ai/legal/privacy">Privacy Policy</a> before using --accept-terms.</p></details>` : code(step.commands)}</div></li>`).join('')}</ol>
    <div class="agent-callout"><span class="mono">PRODUCT AND SETUP GUIDE</span><p>Read the <a href="product.md">complete product and setup guide</a>.<br>Same information, in plain Markdown.</p><a class="text-link" href="llms.txt">AI reading index <span aria-hidden="true">↗</span></a></div>
  </section>
  <section class="faq wrap" aria-labelledby="faq-title"><div><p class="eyebrow">A few useful answers</p><h2 id="faq-title">Before you start.</h2></div><div>${p.questions.map(([q,a]) => `<details><summary>${esc(q)}<span aria-hidden="true">+</span></summary><p>${esc(a)}${q === 'Can I run it myself?' ? ` <a href="${docs}/runbooks.md">Self-hosting documentation.</a>` : ''}</p></details>`).join('')}</div></section>
</main>
<footer class="wrap"><div><a class="brand" href="#"><span class="brand-mark" aria-hidden="true">›_</span> WitSelf</a><p>A product of <a href="https://witwave.ai/">Witwave</a>.<br>Born from building agents that work together.</p></div><nav aria-label="Footer"><a href="${repo}">GitHub</a><a href="${repo}/tree/main/docs">Documentation</a><a href="https://witwave.ai/">Witwave</a><a href="https://self.witwave.ai/legal/terms">Terms</a><a href="https://self.witwave.ai/legal/privacy">Privacy</a><a href="product.md">Plain text</a></nav></footer>
<p id="copy-status" class="sr-only" role="status" aria-live="polite"></p>
</body></html>\n`;

const markdown = [
  `# ${p.name}\n\n> ${p.headline}\n\n${p.summary}`,
  `## ${p.intro}\n\n${p.introText}`,
  ...p.features.map(([title, text]) => `### ${title}\n\n${text}`),
  `## ${p.inspection.title}\n\n${p.inspection.intro}`,
  ...p.inspection.screenshots.map(screenshot => `### ${screenshot.name}\n\n${screenshot.caption}\n\n![${screenshot.alt}](${absolute(screenshot.src)})\n\n${p.inspection.commandLabel}:\n\n\`\`\`sh\n${screenshot.command}\n\`\`\`\n\n${screenshot.commandNote}\n\n[Read the guide](${docs}/${screenshot.guide}).`),
  `${p.inspection.screenshotNote}\n\n${p.inspection.boundary}\n\n${p.inspection.demoLabel}\n\n\`\`\`sh\n${p.inspection.demoCommand}\n\`\`\``,
  '## Supported integrations\n\nCommands assume a configured agent.',
  ...p.runtimes.map(r => `- **${r.name}**: ${r.note}. ${r.platforms}. Command: \`witself install ${r.command}\`.`),
  p.runtimeNote,
  '## Managed-service pricing\n\nFlat monthly pricing in USD. AI provider subscriptions and inference costs are separate.',
  `| Plan | ${plans.map(plan => plan.name).join(' | ')} |\n| --- | ${plans.map(() => '---').join(' | ')} |\n| Monthly USD | ${plans.map(plan => `$${plan.price_monthly}`).join(' | ')} |\n${rows.map(([label, value]) => `| ${label} | ${plans.map(value).join(' | ')} |`).join('\n')}`,
  'Enterprise is coming soon. Agent email, email aliases, and custom email domains are coming soon and are not currently delivered features.',
  `Full catalog: [plans.json](${absolute('plans.json')}). Catalog updated: ${catalog.updated}.`,
  `## Manage WitSelf through your AI\n\n${p.management.title}\n\n${p.management.promise}\n\n${p.management.intro}`,
  ...p.management.prompts.map(([label, prompt]) => `### ${label}\n\n“${prompt}”`),
  `### Example conversation: ${p.management.exampleTitle}\n\n${managementExample.map(([speaker, text]) => `**${speaker}:** ${text}`).join('\n\n')}\n\n${p.management.exampleNote}`,
  `${p.management.requirement}\n\n${p.management.support}`,
  `## Install and connect\n\n${p.setupIntro}\n\n“${p.setupPrompt}”\n\nUse an AI with local command access. You’ll complete identity verification and review the terms.`,
  ...p.installers.map(installer => `### ${installer.name}\n\n\`\`\`\n${installer.command}\n\`\`\``),
  ...steps.slice(1).map(step => `### ${step.title}\n\n${step.text}\n\n\`\`\`sh\n${step.commands}\n\`\`\``),
  'Replace you@example.com, REALM_ID, and AGENT_ID with your own values. Read the [Terms](https://self.witwave.ai/legal/terms) and [Privacy Policy](https://self.witwave.ai/legal/privacy) before using --accept-terms. Release installers verify checksums.',
  '## Questions', ...p.questions.map(([q, a]) => `### ${q}\n\n${a}`),
  `## Links\n\n- [Source repository](${repo})\n- [Documentation](${repo}/tree/main/docs)\n- [Feature status](${docs}/feature-status.md)\n- [CLI reference](${docs}/cli-command-surface.md)\n- [Witwave](https://witwave.ai/)\n\nWitSelf is a product of Witwave.`,
].join('\n\n') + '\n';

await rm(out, {recursive: true, force: true});
await mkdir(out, {recursive: true});
await writeFile(new URL('index.html', out), html);
await writeFile(new URL('product.md', out), markdown);
await writeFile(new URL('plans.json', out), JSON.stringify(catalog, null, 2) + '\n');
await writeFile(new URL('llms.txt', out), `# WitSelf\n\n> ${p.summary}\n\n## Product\n\n- [Complete product and setup guide](${absolute('product.md')}): features, supported AI tools, pricing, free plan limits, setup, and managing WitSelf through your AI using the CLI.\n- [Plan catalog](${absolute('plans.json')}): machine-readable pricing and availability.\n- [Source repository](${repo})\n- [Documentation](${repo}/tree/main/docs)\n- [Witwave](https://witwave.ai/): the company and project behind WitSelf.\n`);
await writeFile(new URL('robots.txt', out), production ? `User-agent: *\nAllow: /\n\nSitemap: ${absolute('sitemap.xml')}\n` : 'User-agent: *\nDisallow: /\n');
if (production) await writeFile(new URL('sitemap.xml', out), `<?xml version="1.0" encoding="UTF-8"?>\n<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9"><url><loc>${esc(canonical)}</loc></url></urlset>\n`);
await writeFile(new URL('.nojekyll', out), '');
for (const asset of ['styles.css', 'app.js', 'favicon.svg']) await copyFile(new URL(asset, root), new URL(asset, out));
await mkdir(new URL('assets/screenshots/', out), {recursive: true});
for (const {src} of p.inspection.screenshots) await copyFile(new URL(src, root), new URL(src, out));
console.log(`Built ${production ? 'production' : 'unindexed preview'} in ${fileURLToPath(out)}`);
