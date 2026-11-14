// @ts-check
import { defineConfig } from "astro/config";
import tailwindcss from "@tailwindcss/vite";
import { SITE_URL } from "./src/config/site.ts";

export default defineConfig({
  site: SITE_URL,
  output: "static",
  trailingSlash: "ignore",
  build: {
    inlineStylesheets: "always",
  },
  devToolbar: { enabled: false },
  vite: {
    plugins: [tailwindcss()],
  },
});
