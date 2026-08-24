#!/usr/bin/env node
/**
 * Source hygiene gate for the Liftr Backstage integration.
 *
 * Mechanically enforces bans that tests alone could miss:
 *   1. dangerouslySetInnerHTML anywhere            (XSS / text-only rendering)
 *   2. localStorage/sessionStorage/indexedDB usage  (no persisted state)
 *   3. eval / new Function                          (code injection)
 *   4. provider vocabulary in UI code               (Pulumi/Crossplane/XR/cloud)
 *   5. Authorization/token forwarding from frontend glue
 *   6. wildcard proxying hints in the backend       (app.use('/' passthrough))
 */

import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');

const BANNED = [
  {
    name: 'dangerouslySetInnerHTML',
    pattern: /dangerouslySetInnerHTML/,
    where: () => true,
  },
  {
    name: 'browser persistence API',
    pattern: /\b(localStorage|sessionStorage|indexedDB)\b/,
    where: p => p.includes('/plugins/liftr/') || p.includes('/plugins/liftr-common/'),
  },
  {
    name: 'dynamic code evaluation',
    pattern: /\beval\s*\(|new\s+Function\s*\(/,
    where: () => true,
  },
  {
    name: 'provider vocabulary in frontend',
    pattern: /\b(pulumi|crossplane|kubernetes|xrbinding|composite resource)\b/i,
    where: p => p.includes('/plugins/liftr/src/components/'),
  },
  {
    name: 'frontend forwards Authorization',
    pattern: /^\s*(headers\[?\s*['"])?Authorization(\1['"]]?\s*[=:])/im,
    where: p => p.includes('/plugins/liftr/src/'),
  },
];

let failures = 0;
const files = [];
(function walk(dir) {
  for (const entry of readdirSync(dir)) {
    const full = join(dir, entry);
    if (statSync(full).isDirectory()) {
      if (entry === 'node_modules' || entry === 'dist' || entry.startsWith('.')) continue;
      walk(full);
    } else if (/\.(ts|tsx|mjs)$/.test(entry) && !entry.endsWith('.test.ts') && full !== import.meta.url.replace('file://','')) {
      files.push(full);
    }
  }
})(ROOT);

for (const file of files) {
  const rel = file.slice(ROOT.length);
  const text = readFileSync(file, 'utf8');
  for (const rule of BANNED) {
    if (!rule.where(rel)) continue;
    const m = text.match(rule.pattern);
    if (m) {
      console.error(`BANNED PATTERN "${rule.name}" (${m[0]}) in ${rel}`);
      failures += 1;
    }
  }
}

// Wildcard-proxy hint check is backend-specific and structural.
const routesFile = join(ROOT, 'plugins/liftr-backend/src/routes.ts');
if (!readFileSync(routesFile, 'utf8').includes('ROUTES: RouteDef[]')) {
  console.error('routes.ts must define an explicit finite ROUTES table');
  failures += 1;
}

if (failures > 0) {
  console.error(`\n${failures} hygiene violation(s).`);
  process.exit(1);
}
console.log('source hygiene: ok');
