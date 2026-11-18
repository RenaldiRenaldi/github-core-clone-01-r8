import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";

const executable = fileURLToPath(new URL("forge", import.meta.url));
const server = spawn(executable, { stdio: "inherit" });

function forward(signal: NodeJS.Signals) {
  if (!server.killed && server.exitCode === null && server.signalCode === null) {
    server.kill(signal);
  }
}

process.once("SIGINT", () => forward("SIGINT"));
process.once("SIGTERM", () => forward("SIGTERM"));
server.once("error", (cause) => {
  console.error("Could not start Go Forge:", cause.message);
  process.exitCode = 1;
});
server.once("exit", (status, signal) => {
  process.exitCode = status ?? (signal === "SIGINT" ? 130 : 143);
});
