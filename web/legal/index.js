// witself-legal: serves the ratified legal pages from docs/legal/.
//
// The markdown files are bundled at deploy time (git is the source of
// truth). Each page is served as minimal server-rendered HTML; append
// ?format=md (or send Accept: text/markdown, as the CLI does) for the raw
// markdown. /legal/versions.json is the consent-version manifest: the
// version labels here are what signup consent records refer to.
import terms from "../../docs/legal/terms-of-service.md";
import privacy from "../../docs/legal/privacy-policy.md";
import aup from "../../docs/legal/acceptable-use.md";
import dpa from "../../docs/legal/data-processing-addendum.md";
import refunds from "../../docs/legal/refund-cancellation.md";

const DOCS = {
  terms: { title: "Terms of Service", md: terms },
  privacy: { title: "Privacy Policy", md: privacy },
  "acceptable-use": { title: "Acceptable Use Policy", md: aup },
  dpa: { title: "Data Processing Addendum", md: dpa },
  refunds: { title: "Refunds & Cancellation", md: refunds },
};

// The consent-version label is the "Version YYYY-MM-DD" line each ratified
// document carries; extraction failing loudly beats serving an unversioned
// legal page.
function versionOf(md) {
  const match = md.match(/\*\*Version (\d{4}-\d{2}-\d{2})/);
  if (!match) throw new Error("legal document is missing its version line");
  return match[1];
}

const VERSIONS = Object.fromEntries(
  Object.entries(DOCS).map(([slug, doc]) => [slug, {
    title: doc.title,
    version: versionOf(doc.md),
    path: `/legal/${slug}`,
  }]),
);

function escapeHTML(s) {
  return s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

// Minimal, dependency-free markdown rendering for the constructs these
// documents actually use: headings, lists, tables, links, bold, code, and
// paragraphs. Anything unrecognized falls through as an escaped paragraph.
function inline(s) {
  return escapeHTML(s)
    .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
    .replace(/`([^`]+)`/g, "<code>$1</code>")
    .replace(/\[([^\]]+)\]\(([^)\s]+)\)/g, (m, text, href) => {
      const safe = href.startsWith("http") || href.startsWith("/") ||
        href.endsWith(".md") ? href.replace(/\.md$/, "") : "#";
      return `<a href="${safe}">${text}</a>`;
    });
}

function render(md) {
  const out = [];
  const lines = md.split("\n");
  let list = false;
  let table = [];
  const closeList = () => { if (list) { out.push("</ul>"); list = false; } };
  const flushTable = () => {
    if (table.length === 0) return;
    const rows = table.filter((r) => !/^\|[\s|:-]+\|$/.test(r));
    out.push("<table>");
    rows.forEach((row, i) => {
      const cells = row.split("|").slice(1, -1).map((c) => inline(c.trim()));
      const tag = i === 0 ? "th" : "td";
      out.push(`<tr>${cells.map((c) => `<${tag}>${c}</${tag}>`).join("")}</tr>`);
    });
    out.push("</table>");
    table = [];
  };
  for (const line of lines) {
    if (/^\|.*\|$/.test(line.trim())) { closeList(); table.push(line.trim()); continue; }
    flushTable();
    const h = line.match(/^(#{1,3}) +(.*)$/);
    if (h) { closeList(); out.push(`<h${h[1].length}>${inline(h[2])}</h${h[1].length}>`); continue; }
    if (/^- /.test(line)) {
      if (!list) { out.push("<ul>"); list = true; }
      out.push(`<li>${inline(line.slice(2))}</li>`);
      continue;
    }
    if (/^ {2,}/.test(line) && list) {
      out[out.length - 1] = out[out.length - 1].replace(/<\/li>$/, ` ${inline(line.trim())}</li>`);
      continue;
    }
    closeList();
    if (line.trim() === "") { out.push(""); continue; }
    const prev = out[out.length - 1];
    if (prev !== undefined && prev.startsWith("<p>") && !prev.endsWith("</p>x")) {
      out[out.length - 1] = prev.replace(/<\/p>$/, ` ${inline(line.trim())}</p>`);
    } else {
      out.push(`<p>${inline(line.trim())}</p>`);
    }
  }
  closeList();
  flushTable();
  return out.join("\n");
}

const STYLE = `<style>
body{max-width:44rem;margin:2rem auto;padding:0 1rem;font:16px/1.6 system-ui,sans-serif;color:#1a1a1a;background:#fff}
@media (prefers-color-scheme:dark){body{color:#e6e6e6;background:#111}a{color:#8ab4f8}}
h1{font-size:1.6rem}h2{font-size:1.2rem;margin-top:2rem}code{background:rgba(128,128,128,.15);padding:.1em .3em;border-radius:3px}
table{border-collapse:collapse;margin:1rem 0;display:block;overflow-x:auto}th,td{border:1px solid rgba(128,128,128,.4);padding:.4rem .6rem;text-align:left;vertical-align:top}
nav{margin-bottom:2rem;font-size:.9rem}
</style>`;

function page(title, bodyHTML) {
  const nav = Object.entries(VERSIONS)
    .map(([slug, v]) => `<a href="${v.path}">${v.title}</a>`)
    .join(" · ");
  return `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>${escapeHTML(title)} — Witself</title>${STYLE}</head>
<body><nav>${nav}</nav>${bodyHTML}</body></html>`;
}

const CACHE = { "cache-control": "public, max-age=300" };

export default {
  fetch(request) {
    if (request.method !== "GET" && request.method !== "HEAD") {
      return new Response("method not allowed\n", { status: 405, headers: { allow: "GET, HEAD" } });
    }
    const url = new URL(request.url);
    const path = url.pathname.replace(/\/$/, "");
    if (path === "/legal/versions.json") {
      return new Response(JSON.stringify(VERSIONS, null, 2) + "\n", {
        headers: { "content-type": "application/json; charset=utf-8", "access-control-allow-origin": "*", ...CACHE },
      });
    }
    if (path === "/legal") {
      const index = Object.entries(VERSIONS)
        .map(([slug, v]) => `<li><a href="${v.path}">${v.title}</a> — version ${v.version}</li>`)
        .join("\n");
      return new Response(page("Legal", `<h1>Witself legal</h1><ul>${index}</ul>`), {
        headers: { "content-type": "text/html; charset=utf-8", ...CACHE },
      });
    }
    const slug = path.replace(/^\/legal\//, "");
    const doc = DOCS[slug];
    if (!doc) {
      return new Response("not found\n", { status: 404 });
    }
    const wantsMD = url.searchParams.get("format") === "md" ||
      (request.headers.get("accept") || "").includes("text/markdown");
    if (wantsMD) {
      return new Response(doc.md, {
        headers: { "content-type": "text/markdown; charset=utf-8", "access-control-allow-origin": "*", ...CACHE },
      });
    }
    return new Response(page(doc.title, render(doc.md)), {
      headers: { "content-type": "text/html; charset=utf-8", ...CACHE },
    });
  },
};
