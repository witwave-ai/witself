// Deployment treats precisely these five legal documents as Text modules.
// Load their committed bytes; leave the legal handler and all other modules
// unchanged, including the existing containers resolution hook.
import { readFile } from "node:fs/promises";

const DOCUMENTS = new Set([
  "terms-of-service.md", "privacy-policy.md", "acceptable-use.md",
  "data-processing-addendum.md", "refund-cancellation.md",
].map((name) => new URL(`../../../../../docs/legal/${name}`, import.meta.url).href));

export async function load(url, context, nextLoad) {
  if (!DOCUMENTS.has(url)) return nextLoad(url, context);
  const markdown = await readFile(new URL(url), "utf8");
  return {
    format: "module", shortCircuit: true,
    source: `export default ${JSON.stringify(markdown)};`,
  };
}
