/**
 * Site identity and metadata. SITE_URL is used for the canonical URL, Open
 * Graph tags and the sitemap; change it when the website's domain is final.
 */
export const SITE_URL = "https://vario.nedanwr.dev";

export const SITE = {
  name: "Vario",
  title: "Vario: Observe everything AI.",
  description:
    "Vario is an open-source CLI that collects your AI conversations, token usage and execution telemetry into your own ClickHouse database.",
} as const;
