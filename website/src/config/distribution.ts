/**
 * Every distribution URL the website shows is built here.
 *
 * Mirrors the "Download source" block of scripts/install.sh and
 * scripts/install.ps1 (see docs/distribution.md). When Vario moves to its own
 * download domain (Cloudflare R2), change INSTALLER_BASE_URL and
 * ASSET_BASE_URL below, together with the installers' DEFAULT_SOURCE, and the
 * whole site follows.
 */

export const GITHUB_REPO = "nedanwr/vario";
export const REPO_URL = `https://github.com/${GITHUB_REPO}`;

export const DOCS_URL = `${REPO_URL}#readme`;
export const LICENSE_URL = `${REPO_URL}/blob/master/LICENSE`;
export const LICENSE_NAME = "Apache-2.0";
export const RELEASES_URL = `${REPO_URL}/releases`;

/**
 * Where the installer scripts are served from. They follow `master`, so the
 * command never changes between releases.
 * Future: e.g. "https://install.<domain>".
 */
const INSTALLER_BASE_URL = `https://raw.githubusercontent.com/${GITHUB_REPO}/master/scripts`;

/**
 * Where a release's files live: `${ASSET_BASE_URL}/<tag>/<file>`.
 * Future (R2 mirror layout): "https://<download-domain>/releases".
 */
const ASSET_BASE_URL = `${REPO_URL}/releases/download`;

/**
 * The current release. Set `published` to true only once the GitHub Release
 * for `tag` is live with all its assets; until then the site links no
 * archives and offers only the installers.
 */
export const RELEASE = {
  tag: "v0.1.0-beta.1",
  channel: "beta",
  published: false,
} as const;

export type PlatformId = "unix" | "windows";

export interface Installer {
  id: PlatformId;
  label: string;
  shell: string;
  prompt: string;
  scriptUrl: string;
  command: string;
  note: string;
}

const installShUrl = `${INSTALLER_BASE_URL}/install.sh`;
const installPs1Url = `${INSTALLER_BASE_URL}/install.ps1`;

export const INSTALLERS: readonly Installer[] = [
  {
    id: "unix",
    label: "macOS / Linux",
    shell: "Terminal",
    prompt: "$",
    scriptUrl: installShUrl,
    command: `curl -fsSL ${installShUrl} | sh`,
    note: "Installs to ~/.local/bin. Verifies the checksum and needs no admin rights.",
  },
  {
    id: "windows",
    label: "Windows",
    shell: "PowerShell",
    prompt: ">",
    scriptUrl: installPs1Url,
    command: `irm ${installPs1Url} | iex`,
    note: "Installs to %LOCALAPPDATA%\\Programs\\Vario and adds it to your PATH. Verifies the checksum and needs no admin rights.",
  },
];

export interface Archive {
  label: string;
  file: string;
  url: string;
}

const ARCHIVE_TARGETS: Record<
  PlatformId,
  { os: string; arch: string; label: string }[]
> = {
  unix: [
    { os: "darwin", arch: "arm64", label: "macOS, Apple silicon" },
    { os: "darwin", arch: "amd64", label: "macOS, Intel" },
    { os: "linux", arch: "amd64", label: "Linux, x86-64" },
    { os: "linux", arch: "arm64", label: "Linux, ARM64" },
  ],
  windows: [
    { os: "windows", arch: "amd64", label: "Windows, x86-64" },
    { os: "windows", arch: "arm64", label: "Windows, ARM64" },
  ],
};

function assetUrl(file: string): string {
  return `${ASSET_BASE_URL}/${RELEASE.tag}/${file}`;
}

/** Release archives for a platform, or none while the release is unpublished. */
export function archivesFor(platform: PlatformId): Archive[] {
  if (!RELEASE.published) return [];
  const version = RELEASE.tag.replace(/^v/, "");
  return ARCHIVE_TARGETS[platform].map(({ os, arch, label }) => {
    const ext = os === "windows" ? "zip" : "tar.gz";
    const file = `vario_${version}_${os}_${arch}.${ext}`;
    return { label, file, url: assetUrl(file) };
  });
}

export const CHECKSUMS_URL = assetUrl("checksums.txt");
