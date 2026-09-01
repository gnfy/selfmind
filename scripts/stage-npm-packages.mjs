#!/usr/bin/env node

import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

function fail(message) {
  console.error(message);
  process.exit(2);
}

function parseArgs(argv) {
  const result = {};
  for (let i = 0; i < argv.length; i += 2) {
    const key = argv[i];
    const value = argv[i + 1];
    if (!key?.startsWith("--") || value === undefined) {
      fail(
        "Usage: stage-npm-packages.mjs --version VERSION --out DIR " +
          "[--linux-x64 PATH] [--linux-arm64 PATH] [--darwin-x64 PATH] [--darwin-arm64 PATH] " +
          "(at least one platform)",
      );
    }
    result[key.slice(2)] = value;
  }
  return result;
}

function copyDir(source, destination) {
  fs.cpSync(source, destination, { recursive: true });
}

function rewritePackageJson(filePath, version) {
  const data = JSON.parse(fs.readFileSync(filePath, "utf8"));
  data.version = version;
  if (data.optionalDependencies) {
    for (const name of Object.keys(data.optionalDependencies)) {
      data.optionalDependencies[name] = version;
    }
  }
  fs.writeFileSync(filePath, `${JSON.stringify(data, null, 2)}${os.EOL}`);
}

const args = parseArgs(process.argv.slice(2));
for (const required of ["version", "out"]) {
  if (!args[required]) fail(`Missing --${required}`);
}
if (!/^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/.test(args.version)) {
  fail(`Invalid npm version: ${args.version}`);
}

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const outDir = path.resolve(args.out);
fs.rmSync(outDir, { recursive: true, force: true });
fs.mkdirSync(outDir, { recursive: true });

// The launcher is always staged. Platform packages are staged only for the
// binaries the caller provides: the release workflow passes all four, while
// the CI smoke passes just the platform it installs. The launcher's
// optionalDependencies still name every platform; installs that omit optional
// siblings (the smoke and the documented offline flow) are unaffected.
const platformPackages = [
  { flag: "linux-x64", template: "selfmind-linux-x64", triple: "x86_64-unknown-linux-gnu" },
  { flag: "linux-arm64", template: "selfmind-linux-arm64", triple: "aarch64-unknown-linux-gnu" },
  { flag: "darwin-x64", template: "selfmind-darwin-x64", triple: "x86_64-apple-darwin" },
  { flag: "darwin-arm64", template: "selfmind-darwin-arm64", triple: "aarch64-apple-darwin" },
];

const packages = [{ template: "selfmind" }];
for (const item of platformPackages) {
  if (!args[item.flag]) continue;
  packages.push({
    template: item.template,
    binary: path.resolve(args[item.flag]),
    triple: item.triple,
  });
}
if (packages.length === 1) {
  fail("At least one platform binary is required (--linux-x64/--linux-arm64/--darwin-x64/--darwin-arm64)");
}

for (const item of packages) {
  const packageDir = path.join(outDir, item.template);
  copyDir(path.join(repoRoot, "npm", item.template), packageDir);
  rewritePackageJson(path.join(packageDir, "package.json"), args.version);
  if (item.binary) {
    if (!fs.existsSync(item.binary)) fail(`Binary not found: ${item.binary}`);
    const binaryDir = path.join(packageDir, "vendor", item.triple, "bin");
    fs.mkdirSync(binaryDir, { recursive: true });
    const destination = path.join(binaryDir, "selfmind");
    fs.copyFileSync(item.binary, destination);
    fs.chmodSync(destination, 0o755);
  }
}

console.log(`Staged npm packages in ${outDir}`);
