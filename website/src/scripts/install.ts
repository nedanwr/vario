/**
 * Platform tabs (WAI-ARIA tabs pattern) and copy buttons for the install
 * commands. Everything here is progressive: without JavaScript both commands
 * are shown and can be selected by hand.
 */

const COPIED_MS = 1800;

function prefersWindows(): boolean {
  const nav = navigator as Navigator & {
    userAgentData?: { platform?: string };
  };
  const platform = nav.userAgentData?.platform ?? navigator.platform ?? "";
  return /^win/i.test(platform) || /Windows NT/.test(navigator.userAgent);
}

async function writeClipboard(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // Older browsers, or a page served without a secure context.
    const area = document.createElement("textarea");
    area.value = text;
    area.setAttribute("readonly", "");
    area.style.position = "fixed";
    area.style.opacity = "0";
    document.body.append(area);
    area.select();
    let ok = false;
    try {
      ok = document.execCommand("copy");
    } catch {
      ok = false;
    }
    area.remove();
    return ok;
  }
}

function updateFade(scroller: HTMLElement): void {
  const hidden =
    scroller.scrollLeft + scroller.clientWidth < scroller.scrollWidth - 1;
  scroller.toggleAttribute("data-fade", hidden);
}

export function initInstall(): void {
  const root = document.querySelector<HTMLElement>("[data-install]");
  if (!root) return;

  const tabs = Array.from(
    root.querySelectorAll<HTMLButtonElement>('[role="tab"]'),
  );
  const panels = Array.from(
    root.querySelectorAll<HTMLElement>('[role="tabpanel"]'),
  );
  const status = root.querySelector<HTMLElement>("[data-copy-status]");
  const scrollers = Array.from(
    root.querySelectorAll<HTMLElement>("[data-scroll]"),
  );

  const refreshFades = () => scrollers.forEach(updateFade);

  function select(tab: HTMLButtonElement, focus = false): void {
    for (const t of tabs) {
      const selected = t === tab;
      t.setAttribute("aria-selected", String(selected));
      t.tabIndex = selected ? 0 : -1;
    }
    for (const panel of panels) {
      panel.hidden = panel.dataset["panel"] !== tab.dataset["tab"];
    }
    if (focus) tab.focus();
    refreshFades();
  }

  tabs.forEach((tab, index) => {
    tab.addEventListener("click", () => select(tab));
    tab.addEventListener("keydown", (event) => {
      let next: number | undefined;
      if (event.key === "ArrowRight") next = (index + 1) % tabs.length;
      else if (event.key === "ArrowLeft")
        next = (index - 1 + tabs.length) % tabs.length;
      else if (event.key === "Home") next = 0;
      else if (event.key === "End") next = tabs.length - 1;
      if (next === undefined) return;
      event.preventDefault();
      const target = tabs[next];
      if (target) select(target, true);
    });
  });

  if (prefersWindows()) {
    const windows = tabs.find((t) => t.dataset["tab"] === "windows");
    if (windows) select(windows);
  }

  const timers = new WeakMap<HTMLElement, number>();
  root.querySelectorAll<HTMLButtonElement>("[data-copy]").forEach((button) => {
    const label = button.querySelector<HTMLElement>("[data-copy-label]");
    button.addEventListener("click", async () => {
      const text = button.dataset["copy"] ?? "";
      const ok = await writeClipboard(text);
      if (label) label.textContent = ok ? "Copied" : "Selected";
      button.toggleAttribute("data-copied", ok);
      if (status) {
        status.textContent = ok
          ? "Install command copied to the clipboard."
          : "Could not copy. Select the command and copy it manually.";
      }
      if (!ok) {
        const code = button.parentElement?.querySelector("code");
        if (code) window.getSelection()?.selectAllChildren(code);
      }
      window.clearTimeout(timers.get(button));
      timers.set(
        button,
        window.setTimeout(() => {
          if (label) label.textContent = "Copy";
          button.removeAttribute("data-copied");
        }, COPIED_MS),
      );
    });
  });

  for (const scroller of scrollers) {
    scroller.addEventListener("scroll", () => updateFade(scroller), {
      passive: true,
    });
  }
  new ResizeObserver(refreshFades).observe(root);
  refreshFades();
}
