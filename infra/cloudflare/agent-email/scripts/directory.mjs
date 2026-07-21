#!/usr/bin/env node
import { CloudflareAPI, cloudflareEnvironment, EMAIL_DIRECTORY_TITLE } from "./cloudflare.mjs";

async function main(argv = process.argv.slice(2)) {
  const command = argv[0];
  if (argv.length !== 1 || (command !== "ensure" && command !== "show")) {
    throw new Error("usage: node scripts/directory.mjs <ensure|show>");
  }
  const config = cloudflareEnvironment();
  const api = new CloudflareAPI(config);
  if (command === "show") {
    if (!config.namespaceID) throw new Error("EMAIL_DIRECTORY_KV_ID is required for show");
    const namespace = await api.getNamespace();
    if (namespace?.title !== EMAIL_DIRECTORY_TITLE) throw new Error("configured namespace is not the isolated email directory");
    process.stdout.write(`${namespace.id}\n`);
    return;
  }
  const existing = (await api.listNamespaces()).filter((item) => item.title === EMAIL_DIRECTORY_TITLE);
  if (existing.length > 1) throw new Error("multiple isolated email-directory namespaces exist; resolve manually");
  const namespace = existing[0] ?? await api.createNamespace();
  process.stdout.write(`${namespace.id}\n`);
}

main().catch((error) => {
  process.stderr.write(`${error.message}\n`);
  process.exitCode = 1;
});
