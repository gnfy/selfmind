#!/usr/bin/env node

import { spawn } from "node:child_process";
import { createRequire } from "node:module";
import path from "node:path";
import process from "node:process";
import { fileURLToPath } from "node:url";

const require = createRequire(import.meta.url);

const targets = {
  "linux-x64": {
    packageName: "@selfmind/cli-linux-x64",
    triple: "x86_64-unknown-linux-gnu",
  },
  "linux-arm64": {
    packageName: "@selfmind/cli-linux-arm64",
    triple: "aarch64-unknown-linux-gnu",
  },
  "darwin-x64": {
    packageName: "@selfmind/cli-darwin-x64",
    triple: "x86_64-apple-darwin",
  },
  "darwin-arm64": {
    packageName: "@selfmind/cli-darwin-arm64",
    triple: "aarch64-apple-darwin",
  },
};

function packageManager() {
  const userAgent = process.env.npm_config_user_agent ?? "";
  if (userAgent.startsWith("pnpm/")) return "pnpm";
  if (userAgent.startsWith("yarn/")) return "yarn";
  if (userAgent.startsWith("bun/")) return "bun";
  return "npm";
}

function reinstallHint() {
  switch (packageManager()) {
    case "pnpm":
      return "pnpm add -g @selfmind/cli@latest";
    case "yarn":
      return "yarn global add @selfmind/cli@latest";
    case "bun":
      return "bun add -g @selfmind/cli@latest";
    default:
      return "npm install -g @selfmind/cli@latest";
  }
}

const targetKey = `${process.platform}-${process.arch}`;
const target = targets[targetKey];
if (!target) {
  console.error(
    `SelfMind does not currently support ${process.platform}/${process.arch}. ` +
      "The official npm release supports Linux x64/arm64, macOS x64/arm64, and WSL.",
  );
  process.exit(1);
}

let packageJsonPath;
try {
  packageJsonPath = require.resolve(`${target.packageName}/package.json`);
} catch {
  console.error(
    `The SelfMind platform package for ${targetKey} is missing. ` +
      `Reinstall with: ${reinstallHint()}`,
  );
  process.exit(1);
}

const packageRoot = path.dirname(packageJsonPath);
const binaryPath = path.join(
  packageRoot,
  "vendor",
  target.triple,
  "bin",
  "selfmind",
);
const launcherPath = fileURLToPath(import.meta.url);
const child = spawn(binaryPath, process.argv.slice(2), {
  stdio: "inherit",
  env: {
    ...process.env,
    SELFMIND_INSTALL_METHOD: packageManager(),
    SELFMIND_NPM_LAUNCHER: launcherPath,
    SELFMIND_NODE_PATH: process.execPath,
    SELFMIND_NPM_PACKAGE: "@selfmind/cli",
  },
});

const signalHandlers = new Map();
for (const signal of ["SIGINT", "SIGTERM", "SIGHUP"]) {
  const handler = () => {
    if (!child.killed) child.kill(signal);
  };
  signalHandlers.set(signal, handler);
  process.on(signal, handler);
}

child.on("error", (error) => {
  if (error?.code === "ENOENT") {
    console.error(
      `The SelfMind binary is missing from ${target.packageName}. ` +
        `Reinstall with: ${reinstallHint()}`,
    );
  } else {
    console.error(`Failed to start SelfMind: ${error.message}`);
  }
  process.exit(1);
});

child.on("exit", (code, signal) => {
  if (signal) {
    for (const [registeredSignal, handler] of signalHandlers) {
      process.off(registeredSignal, handler);
    }
    process.kill(process.pid, signal);
    return;
  }
  process.exit(code ?? 1);
});
