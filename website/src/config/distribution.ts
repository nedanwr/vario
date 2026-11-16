// Keep in sync with the "Download source" block of scripts/install.sh and
// scripts/install.ps1 when moving to Vario's own download domain.

import { resolveReleaseTag, type Channel } from "./release.ts";

export const GITHUB_REPO = "nedanwr/vario";
export const REPO_URL = `https://github.com/${GITHUB_REPO}`;

export const DOCS_URL = `${REPO_URL}#readme`;
export const LICENSE_URL = `${REPO_URL}/blob/master/LICENSE`;
export const LICENSE_NAME = "Apache-2.0";
export const RELEASES_URL = `${REPO_URL}/releases`;

const INSTALLER_BASE_URL = `https://raw.githubusercontent.com/${GITHUB_REPO}/master/scripts`;

const ASSET_BASE_URL = `${REPO_URL}/releases/download`;

const RELEASE_CHANNEL: Channel = "beta";
export const RELEASE = {
  tag: await resolveReleaseTag(GITHUB_REPO, RELEASE_CHANNEL),
  channel: RELEASE_CHANNEL,
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

export function archivesFor(platform: PlatformId): Archive[] {
  const version = RELEASE.tag.replace(/^v/, "");
  return ARCHIVE_TARGETS[platform].map(({ os, arch, label }) => {
    const ext = os === "windows" ? "zip" : "tar.gz";
    const file = `vario_${version}_${os}_${arch}.${ext}`;
    return { label, file, url: assetUrl(file) };
  });
}

export const CHECKSUMS_URL = assetUrl("checksums.txt");
