// Light/dark theme state. The <html data-theme> attribute is the single
// source of truth; index.html sets it before first paint so there is no
// flash, and this module just reads/writes it.
import { useEffect, useState } from "react";

export type Theme = "light" | "dark";

export function currentTheme(): Theme {
  return document.documentElement.dataset.theme === "dark" ? "dark" : "light";
}

/** React to theme changes from anywhere (ThemeToggle writes the attribute). */
export function useThemeAttr(): Theme {
  const [theme, setTheme] = useState<Theme>(currentTheme());
  useEffect(() => {
    const observer = new MutationObserver(() => setTheme(currentTheme()));
    observer.observe(document.documentElement, { attributeFilter: ["data-theme"] });
    return () => observer.disconnect();
  }, []);
  return theme;
}

export function applyTheme(theme: Theme) {
  document.documentElement.dataset.theme = theme;
  try {
    localStorage.setItem("theme", theme);
  } catch {
    // Private-mode storage failures just mean the choice isn't remembered.
  }
}
