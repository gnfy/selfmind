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
        "Usage: stage-npm-packages.mjs --version VERSION --linux-x64 PATH " +
          "--linux-arm64 PATH --darwin-x64 PATH --darwin-arm64 PATH --out DIR",
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
for (const required of [
  "version",
  "linux-x64",
  "linux-arm64",
  "darwin-x64",
  "darwin-arm64",
  "out",
]) {
  if (!args[required]) fail(`Missing --${required}`);
}
if (!/^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?$/.test(args.version)) {
  fail(`Invalid npm version: ${args.version}`);
}

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const outDir = path.resolve(args.out);
fs.rmSync(outDir, { recursive: true, force: true });
fs.mkdirSync(outDir, { recursive: true });

const packages = [
  {
    template: "selfmind",
  },
  {
    template: "selfmind-linux-x64",
    binary: path.resolve(args["linux-x64"]),
    triple: "x86_64-unknown-linux-gnu",
  },
  {
    template: "selfmind-linux-arm64",
    binary: path.resolve(args["linux-arm64"]),
    triple: "aarch64-unknown-linux-gnu",
  },
  {
    template: "selfmind-darwin-x64",
    binary: path.resolve(args["darwin-x64"]),
    triple: "x86_64-apple-darwin",
  },
  {
    template: "selfmind-darwin-arm64",
    binary: path.resolve(args["darwin-arm64"]),
    triple: "aarch64-apple-darwin",
  },
];

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
