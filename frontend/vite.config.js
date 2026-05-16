import { defineConfig, loadEnv } from "vite";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";

const __dirname = dirname(fileURLToPath(import.meta.url));

export default defineConfig(({ mode }) => {
  const env = {
    ...loadEnv(mode, resolve(__dirname, ".."), ""),
    CADDY_DOMAIN: process.env.CADDY_DOMAIN || "",
  };
  const allowedHosts = ["localhost", "127.0.0.1"];

  if (env.CADDY_DOMAIN) {
    allowedHosts.push(env.CADDY_DOMAIN);
  }

  return {
    envDir: resolve(__dirname, ".."),
    server: {
      allowedHosts,
    },
    preview: {
      allowedHosts,
    },
  };
});