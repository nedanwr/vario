// Same channel and SemVer rules as scripts/install.sh and install.ps1.
// VARIO_RELEASE_TAG skips the GitHub lookup; GITHUB_TOKEN avoids its anonymous rate limit.

export type Channel = "beta" | "stable";

// No Node type definitions in this project.
const env: Record<string, string | undefined> =
  (globalThis as { process?: { env: Record<string, string | undefined> } })
    .process?.env ?? {};

const TAG_PATTERN =
  /^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$/;

function compareDigits(x: string, y: string): number {
  if (x.length !== y.length) return Math.sign(x.length - y.length);
  return x < y ? -1 : x > y ? 1 : 0;
}

export function compareSemVer(a: string, b: string): number {
  const [aCore, aPre = ""] = splitOnce(a.replace(/^v/, ""), "-");
  const [bCore, bPre = ""] = splitOnce(b.replace(/^v/, ""), "-");
  const ac = aCore.split(".");
  const bc = bCore.split(".");
  for (let i = 0; i < 3; i++) {
    const c = compareDigits(ac[i] ?? "", bc[i] ?? "");
    if (c !== 0) return c;
  }
  if (!aPre && !bPre) return 0;
  if (!aPre) return 1;
  if (!bPre) return -1;
  const ai = aPre.split(".");
  const bi = bPre.split(".");
  for (let i = 0; i < Math.min(ai.length, bi.length); i++) {
    const x = ai[i] ?? "";
    const y = bi[i] ?? "";
    const xn = /^[0-9]+$/.test(x);
    const yn = /^[0-9]+$/.test(y);
    const c =
      xn && yn
        ? compareDigits(x, y)
        : xn
          ? -1
          : yn
            ? 1
            : x < y
              ? -1
              : x > y
                ? 1
                : 0;
    if (c !== 0) return c;
  }
  return Math.sign(ai.length - bi.length);
}

function splitOnce(s: string, sep: string): [string, string?] {
  const i = s.indexOf(sep);
  return i < 0 ? [s] : [s.slice(0, i), s.slice(i + 1)];
}

export function inChannel(channel: Channel, tag: string): boolean {
  const dash = tag.indexOf("-");
  if (dash < 0) return true;
  if (channel !== "beta") return false;
  return /^(beta|rc)(\..*)?$/.test(tag.slice(dash + 1));
}

export function newestOnChannel(
  channel: Channel,
  tags: readonly string[],
): string | undefined {
  let newest: string | undefined;
  for (const tag of tags) {
    if (!TAG_PATTERN.test(tag) || !inChannel(channel, tag)) continue;
    if (!newest || compareSemVer(tag, newest) > 0) newest = tag;
  }
  return newest;
}

export async function resolveReleaseTag(
  repo: string,
  channel: Channel,
): Promise<string> {
  const override = env.VARIO_RELEASE_TAG?.trim();
  if (override) {
    if (!TAG_PATTERN.test(override)) {
      throw new Error(
        `VARIO_RELEASE_TAG='${override}' is not a release tag (e.g. v0.1.0-beta.1)`,
      );
    }
    return override;
  }

  const url = `https://api.github.com/repos/${repo}/releases?per_page=100`;
  const headers: Record<string, string> = {
    Accept: "application/vnd.github+json",
    "User-Agent": "vario-website-build",
  };
  const token = env.GITHUB_TOKEN?.trim();
  if (token) headers.Authorization = `Bearer ${token}`;

  let tags: string[];
  try {
    const res = await fetch(url, {
      headers,
      signal: AbortSignal.timeout(30_000),
    });
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const releases = (await res.json()) as {
      tag_name?: unknown;
      draft?: unknown;
    }[];
    tags = releases
      .filter((r) => r.draft !== true && typeof r.tag_name === "string")
      .map((r) => r.tag_name as string);
  } catch (err) {
    throw new Error(
      `could not list releases from ${url} (${(err as Error).message}); ` +
        "set GITHUB_TOKEN to avoid the anonymous rate limit, or VARIO_RELEASE_TAG to skip the lookup",
    );
  }

  const tag = newestOnChannel(channel, tags);
  if (!tag) {
    throw new Error(`no ${channel} release of ${repo} is published yet`);
  }
  return tag;
}
