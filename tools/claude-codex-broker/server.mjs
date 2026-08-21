#!/usr/bin/env node
import { startServer } from "./lib/broker.mjs";

await startServer(process.argv.slice(2));
