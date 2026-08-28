import { AppSidebar } from "@/components/AppSidebar";
import { SidebarLayout } from "@/components/layout/layout";
import { Button } from "@/components/ui/button";
import { createFileRoute, useRouter } from "@tanstack/react-router";
import Settings from "@/components/Settings";
import { useEffect, useState } from "react";

type Appearance = "system" | "light" | "dark";

const APPEARANCE_KEY = "ollama-appearance";

export const Route = createFileRoute("/settings")({
  component: SettingsRoute,
});

function SettingsRoute() {
  const router = useRouter();

  const [appearance, setAppearance] = useState<Appearance>(() => {
    const saved = window.localStorage.getItem(APPEARANCE_KEY);

    if (saved === "light" || saved === "dark" || saved === "system") {
      return saved;
    }

    return "system";
  });

  const applyAppearance = (value: Appearance) => {
    const systemDark = window.matchMedia(
      "(prefers-color-scheme: dark)",
    ).matches;

    const useDark =
      value === "dark" || (value === "system" && systemDark);

    document.documentElement.classList.toggle("dark", useDark);
  };

  const changeAppearance = (value: Appearance) => {
    setAppearance(value);
    window.localStorage.setItem(APPEARANCE_KEY, value);
    applyAppearance(value);
  };

  useEffect(() => {
    if (appearance !== "system") {
      return;
    }

    const media = window.matchMedia("(prefers-color-scheme: dark)");

    const handleSystemAppearanceChange = () => {
      applyAppearance("system");
    };

    media.addEventListener("change", handleSystemAppearanceChange);

    return () => {
      media.removeEventListener("change", handleSystemAppearanceChange);
    };
  }, [appearance]);

  return (
    <SidebarLayout
      title="Settings"
      sidebar={<AppSidebar current="settings" />}
    >
      <Button
        outline
        className="self-start ml-4"
        onClick={() => router.history.back()}
      >
        Back
      </Button>

      <div className="mx-auto mt-4 w-full max-w-[896px]">
        <div className="flex items-center justify-between gap-6 rounded-xl bg-white p-4 dark:bg-neutral-800">
          <div>
            <div className="text-sm font-medium text-neutral-950 dark:text-white">
              Appearance
            </div>
            <div className="mt-1 text-xs text-neutral-500 dark:text-neutral-400">
              Choose how Ollama looks.
            </div>
          </div>

          <div className="flex rounded-lg bg-neutral-100 p-1 dark:bg-neutral-800">
            {(["system", "light", "dark"] as Appearance[]).map((value) => (
              <button
                key={value}
                type="button"
                onClick={() => changeAppearance(value)}
                className={`rounded-md px-3 py-1.5 text-sm capitalize transition-colors ${
                  appearance === value
                    ? "bg-white text-neutral-950 shadow-sm dark:bg-neutral-700 dark:text-white"
                    : "text-neutral-500 hover:text-neutral-950 dark:text-neutral-400 dark:hover:text-white"
                }`}
              >
                {value}
              </button>
            ))}
          </div>
        </div>
      </div>

      <Settings />
    </SidebarLayout>
  );
}