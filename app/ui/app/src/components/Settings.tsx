import { useEffect, useState, useCallback } from "react";
import { Switch } from "@/components/ui/switch";
import { Text } from "@/components/ui/text";
import { Input } from "@/components/ui/input";
import { Field, Label, Description } from "@/components/ui/fieldset";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { ClaudeDesktopModelsSettings } from "@/components/ClaudeDesktopModelsSettings";
import {
  WifiIcon,
  FolderIcon,
  BoltIcon,
  WrenchIcon,
  CloudIcon,
  CogIcon,
  ArrowDownTrayIcon,
  Squares2X2Icon,
  TrashIcon,
} from "@heroicons/react/20/solid";
import { CpuChipIcon, PuzzlePieceIcon } from "@heroicons/react/24/outline";
import { Model, Settings as SettingsType } from "@/gotypes";
import { isWindowsPlatform } from "@/lib/platform";
import { useSettings } from "@/hooks/useSettings";
import { useUser } from "@/hooks/useUser";
import { useCloudStatus } from "@/hooks/useCloudStatus";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  deleteModel,
  getManagedModels,
  getModelUpstreamInfo,
  importModelFiles,
  setModelAlias,
  type CloudStatusResponse,
  type ManagedModel,
  unlinkModel,
  updateCloudSetting,
} from "@/api";

type Appearance = "system" | "light" | "dark";

const APPEARANCE_KEY = "ollama-appearance";

function AnimatedDots() {
  return (
    <span className="inline-flex">
      <span className="animate-pulse">.</span>
      <span className="animate-pulse" style={{ animationDelay: "0.2s" }}>
        .
      </span>
      <span className="animate-pulse" style={{ animationDelay: "0.4s" }}>
        .
      </span>
    </span>
  );
}

function deriveImportedModelName(path: string) {
  const filename = path.split(/[\\/]/).pop() || "imported-model.gguf";
  const withoutExtension = filename.replace(/\.gguf$/i, "");
  const normalized = withoutExtension
    .trim()
    .toLowerCase()
    .replace(/[^a-z0-9._/-]+/g, "-")
    .replace(/^-+|-+$/g, "");

  return normalized ? `${normalized}:latest` : "imported-model:latest";
}

function formatModelSize(size?: number) {
  if (!size || size <= 0) {
    return "Unknown size";
  }

  const units = ["B", "KB", "MB", "GB", "TB"];
  let value = size;
  let unitIndex = 0;

  while (value >= 1024 && unitIndex < units.length - 1) {
    value /= 1024;
    unitIndex += 1;
  }

  const precision = value >= 10 || unitIndex === 0 ? 0 : 1;
  return `${value.toFixed(precision)} ${units[unitIndex]}`;
}

function modelDetails(model: ManagedModel) {
  const details = model.details;
  const parts = [
    details?.format?.toUpperCase(),
    details?.parameter_size,
    details?.quantization_level,
  ].filter(Boolean);

  return parts.length > 0 ? parts.join(" ") : "Installed model";
}

function modelOriginLabel(model: ManagedModel) {
  return model.origin === "linked_gguf" ? "Linked GGUF" : "Ollama storage";
}

function normalizeModelName(name: string) {
  return name.replace(/:latest$/, "");
}

export default function Settings() {
  const queryClient = useQueryClient();
  const [showSaved, setShowSaved] = useState(false);
  const [appearance, setAppearance] = useState<Appearance>(() => {
    const saved = window.localStorage.getItem(APPEARANCE_KEY);
    if (saved === "light" || saved === "dark" || saved === "system")
      return saved;
    return "system";
  });
  const [modelsPath, setModelsPath] = useState("");
  const [restartMessage, setRestartMessage] = useState(false);
  const [importModelError, setImportModelError] = useState<string | null>(null);
  const [importModelPaths, setImportModelPaths] = useState<string[] | null>(
    null,
  );
  const [importModelName, setImportModelName] = useState("");
  const [showModelManager, setShowModelManager] = useState(false);
  const [deleteModelError, setDeleteModelError] = useState<string | null>(null);
  const [modelManagerMessage, setModelManagerMessage] = useState<string | null>(
    null,
  );
  const [showAppsInMenu, setShowAppsInMenuState] = useState(true);
  const [showAppsInMenuPending, setShowAppsInMenuPending] = useState(false);
  const { settingsData, error, setSettings } = useSettings();
  const {
    user,
    isAuthenticated,
    refreshUser,
    isRefreshing,
    refetchUser,
    fetchConnectUrl,
    isLoading,
    disconnectUser,
  } = useUser();
  const [isAwaitingConnection, setIsAwaitingConnection] = useState(false);
  const [connectionError, setConnectionError] = useState<string | null>(null);
  const [pollingInterval, setPollingInterval] = useState<number | null>(null);
  const {
    cloudDisabled,
    cloudStatus,
    isLoading: cloudStatusLoading,
  } = useCloudStatus();
  const settings = settingsData || null;
  const installedModelsQuery = useQuery({
    queryKey: ["managedModels"],
    queryFn: getManagedModels,
    enabled: showModelManager,
  });

  const applyAppearance = (value: Appearance) => {
    const systemDark = window.matchMedia(
      "(prefers-color-scheme: dark)",
    ).matches;
    const useDark = value === "dark" || (value === "system" && systemDark);
    document.documentElement.classList.toggle("dark", useDark);
  };

  const changeAppearance = (value: Appearance) => {
    setAppearance(value);
    window.localStorage.setItem(APPEARANCE_KEY, value);
    applyAppearance(value);
  };

  useEffect(() => {
    applyAppearance(appearance);
    if (appearance !== "system") return;
    const media = window.matchMedia("(prefers-color-scheme: dark)");
    const handleSystemAppearanceChange = () => applyAppearance("system");
    media.addEventListener("change", handleSystemAppearanceChange);
    return () =>
      media.removeEventListener("change", handleSystemAppearanceChange);
  }, [appearance]);

  useEffect(() => {
    if (settings?.Models !== undefined) {
      setModelsPath(settings.Models || "");
    }
  }, [settings?.Models]);

  const updateCloudMutation = useMutation({
    mutationFn: (enabled: boolean) => updateCloudSetting(enabled),
    onMutate: async (enabled: boolean) => {
      await queryClient.cancelQueries({ queryKey: ["cloudStatus"] });

      const previous = queryClient.getQueryData<CloudStatusResponse | null>([
        "cloudStatus",
      ]);
      const envForcesDisabled =
        previous?.source === "env" || previous?.source === "both";

      queryClient.setQueryData<CloudStatusResponse | null>(
        ["cloudStatus"],
        previous
          ? {
              ...previous,
              disabled: !enabled || envForcesDisabled,
            }
          : {
              disabled: !enabled,
              source: "config",
            },
      );

      return { previous };
    },
    onError: (_error, _enabled, context) => {
      if (context?.previous !== undefined) {
        queryClient.setQueryData(["cloudStatus"], context.previous);
      }
    },
    onSuccess: (status) => {
      queryClient.setQueryData<CloudStatusResponse | null>(
        ["cloudStatus"],
        status,
      );
      queryClient.invalidateQueries({ queryKey: ["models"] });
      queryClient.invalidateQueries({ queryKey: ["cloudStatus"] });

      setShowSaved(true);
      setTimeout(() => setShowSaved(false), 1500);
    },
  });

  const importModelMutation = useMutation({
    mutationFn: ({ name, paths }: { name: string; paths: string[] }) =>
      importModelFiles(name, paths),
    onSuccess: () => {
      setImportModelError(null);
      setImportModelPaths(null);
      setImportModelName("");
      queryClient.invalidateQueries({ queryKey: ["models"] });
      setShowSaved(true);
      setTimeout(() => setShowSaved(false), 1500);
    },
    onError: (error) => {
      setImportModelError(
        error instanceof Error ? error.message : "Failed to import model",
      );
    },
  });

  const deleteModelMutation = useMutation({
    mutationFn: (model: string) => deleteModel(model),
    onSuccess: async (_result, model) => {
      setDeleteModelError(null);
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["models"] }),
        queryClient.invalidateQueries({ queryKey: ["managedModels"] }),
      ]);

      const selected = settings?.SelectedModel || "";
      if (
        selected === model ||
        normalizeModelName(selected) === normalizeModelName(model)
      ) {
        await setSettings({ SelectedModel: "" });
      }

      setShowSaved(true);
      setTimeout(() => setShowSaved(false), 1500);
    },
    onError: (error) => {
      setDeleteModelError(
        error instanceof Error ? error.message : "Failed to remove model",
      );
    },
  });

  const unlinkModelMutation = useMutation({
    mutationFn: (model: string) => unlinkModel(model),
    onSuccess: async (_result, model) => {
      setDeleteModelError(null);
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["models"] }),
        queryClient.invalidateQueries({ queryKey: ["managedModels"] }),
      ]);

      const selected = settings?.SelectedModel || "";
      if (
        selected === model ||
        normalizeModelName(selected) === normalizeModelName(model)
      ) {
        await setSettings({ SelectedModel: "" });
      }

      setShowSaved(true);
      setTimeout(() => setShowSaved(false), 1500);
    },
    onError: (error) => {
      setDeleteModelError(
        error instanceof Error ? error.message : "Failed to unlink model",
      );
    },
  });

  const renameModelMutation = useMutation({
    mutationFn: ({
      model,
      displayName,
    }: {
      model: string;
      displayName: string;
    }) => setModelAlias(model, displayName),
    onSuccess: async () => {
      setDeleteModelError(null);
      await queryClient.invalidateQueries({ queryKey: ["managedModels"] });
      setShowSaved(true);
      setTimeout(() => setShowSaved(false), 1500);
    },
    onError: (error) => {
      setDeleteModelError(
        error instanceof Error ? error.message : "Failed to rename model",
      );
    },
  });

  const checkModelUpdateMutation = useMutation({
    mutationFn: (model: string) => getModelUpstreamInfo(new Model({ model })),
    onSuccess: (result, model) => {
      setDeleteModelError(null);
      setModelManagerMessage(
        result.stale
          ? `${model} has an upstream update available.`
          : `${model} is up to date.`,
      );
    },
    onError: (error) => {
      setModelManagerMessage(null);
      setDeleteModelError(
        error instanceof Error ? error.message : "Failed to check update",
      );
    },
  });

  useEffect(() => {
    refetchUser();
  }, []); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    window
      .getShowAppsInMenu?.()
      .then(setShowAppsInMenuState)
      .catch((error) =>
        console.error("Failed to load menu app visibility:", error),
      );
  }, []);

  useEffect(() => {
    const handleFocus = () => {
      if (isAwaitingConnection && pollingInterval) {
        // Stop polling when window gets focus
        clearInterval(pollingInterval);
        setPollingInterval(null);
        // Reset awaiting connection state
        setIsAwaitingConnection(false);
        // Make one last refresh request
        refreshUser();
      }
    };

    window.addEventListener("focus", handleFocus);

    return () => {
      window.removeEventListener("focus", handleFocus);
    };
  }, [isAwaitingConnection, refreshUser, pollingInterval]);

  // Check if user is authenticated after refresh
  useEffect(() => {
    if (isAwaitingConnection && isAuthenticated) {
      setIsAwaitingConnection(false);
      setConnectionError(null);
      if (pollingInterval) {
        clearInterval(pollingInterval);
        setPollingInterval(null);
      }
    }
  }, [isAuthenticated, isAwaitingConnection, pollingInterval]);

  // Cleanup interval on unmount
  useEffect(() => {
    return () => {
      if (pollingInterval) {
        clearInterval(pollingInterval);
      }
    };
  }, [pollingInterval]);

  const handleChange = useCallback(
    async (field: keyof SettingsType, value: boolean | string | number) => {
      const currentSettings =
        queryClient.getQueryData<{ settings: SettingsType }>(["settings"])
          ?.settings ?? settings;
      if (!currentSettings) {
        return;
      }

      if (
        field === "ContextLength" &&
        value !== currentSettings.ContextLength
      ) {
        setRestartMessage(true);
        setTimeout(() => setRestartMessage(false), 3000);
      }

      await setSettings({ [field]: value } as Partial<SettingsType>);
      setShowSaved(true);
      setTimeout(() => setShowSaved(false), 1500);
    },
    [queryClient, setSettings, settings],
  );

  const handleResetToDefaults = async () => {
    if (!settings) {
      return;
    }

    await setSettings({
      Expose: false,
      Browser: false,
      Models: "",
      Agent: false,
      Tools: false,
      LocalModelsEnabled: true,
      CPUFallbackEnabled: false,
      InferenceEnabled: false,
      InferenceRuntime: "auto",
      InferenceAutoFitEnabled: true,
      InferenceManualLayers: "",
      PluginsEnabled: false,
      ContextLength: 0,
      AutoUpdateEnabled: true,
    });

    setShowSaved(true);
    setTimeout(() => setShowSaved(false), 1500);
  };

  const handleImportModel = async () => {
    setImportModelError(null);

    if (!window.webview?.selectModelFiles) {
      setImportModelError("Model import is only available in the desktop app.");
      return;
    }

    let paths: string[] | null;
    try {
      paths = await window.webview.selectModelFiles();
    } catch (error) {
      setImportModelError(
        error instanceof Error
          ? error.message
          : "Failed to select model files.",
      );
      return;
    }
    if (!paths || paths.length === 0) {
      return;
    }

    setImportModelPaths(paths);
    setImportModelName(deriveImportedModelName(paths[0]));
  };

  const handleConfirmImportModel = async () => {
    if (!importModelPaths || importModelPaths.length === 0) {
      setImportModelError("Choose at least one GGUF file first.");
      return;
    }

    const name = importModelName.trim();
    if (!name) {
      setImportModelError("Model name is required.");
      return;
    }

    importModelMutation.mutate({ name, paths: importModelPaths });
  };

  const handleShowAppsInMenu = async (checked: boolean) => {
    const previous = showAppsInMenu;
    setShowAppsInMenuState(checked);
    setShowAppsInMenuPending(true);
    try {
      await window.setShowAppsInMenu?.(checked);
      setShowSaved(true);
      setTimeout(() => setShowSaved(false), 1500);
    } catch (error) {
      setShowAppsInMenuState(previous);
      console.error("Failed to update menu app visibility:", error);
    } finally {
      setShowAppsInMenuPending(false);
    }
  };

  const cloudOverriddenByEnv =
    cloudStatus?.source === "env" || cloudStatus?.source === "both";
  const cloudToggleDisabled =
    cloudStatusLoading || updateCloudMutation.isPending || cloudOverriddenByEnv;

  const handleConnectOllamaAccount = async () => {
    setConnectionError(null);

    // If user is already authenticated, no need to connect
    if (isAuthenticated) {
      return;
    }

    try {
      // If we don't have a user or user has no name, get connect URL
      if (!user || !user?.name) {
        const { data: connectUrl } = await fetchConnectUrl();
        if (connectUrl) {
          window.open(connectUrl, "_blank");
          setIsAwaitingConnection(true);
          // Start polling every 5 seconds
          const interval = setInterval(() => {
            refreshUser();
          }, 5000);
          setPollingInterval(interval);
        } else {
          setConnectionError("Failed to get connect URL");
        }
      }
    } catch (error) {
      console.error("Error connecting to Ollama account:", error);
      setConnectionError(
        error instanceof Error
          ? error.message
          : "Failed to connect to Ollama account",
      );
      setIsAwaitingConnection(false);
    }
  };

  if (!settingsData && !error) {
    return null;
  }

  if (error || !settings) {
    return (
      <div className="flex flex-1 items-center justify-center">
        <div className="text-red-500">Failed to load settings</div>
      </div>
    );
  }

  const isWindows = isWindowsPlatform();

  return (
    <main className="flex min-h-0 w-full flex-1 flex-col select-none dark:bg-neutral-900">
      <div className="w-full px-6 pb-6 pt-4 overflow-y-auto flex-1 overscroll-contain">
        <div className="mx-auto max-w-4xl space-y-4">
          {/* Appearance */}
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

          {/* Connect Ollama Account */}
          <div className="overflow-hidden rounded-xl bg-white dark:bg-neutral-800">
            <div className="p-4">
              <Field>
                {isLoading ? (
                  // Loading skeleton, this will only happen if the app started recently
                  <div className="flex items-center justify-between">
                    <div className="space-y-2">
                      <div className="h-4 bg-neutral-200 dark:bg-neutral-700 rounded animate-pulse w-24"></div>
                      <div className="h-3 bg-neutral-200 dark:bg-neutral-700 rounded animate-pulse w-32"></div>
                    </div>
                    <div className="h-10 w-10 bg-neutral-200 dark:bg-neutral-700 rounded-full animate-pulse"></div>
                  </div>
                ) : user && user.name ? (
                  <div className="flex items-center justify-between">
                    <div>
                      <div className="flex items-center space-x-2">
                        <Label className="text-sm font-medium text-neutral-900 dark:text-white">
                          {user?.name}
                        </Label>
                      </div>
                      <Description className="text-sm text-neutral-500 dark:text-neutral-400">
                        {user?.email}
                      </Description>
                      <div className="flex items-center space-x-2 mt-2">
                        {user?.plan === "free" && (
                          <Button
                            type="button"
                            color="dark"
                            className="px-3 py-2 text-sm font-medium bg-black/90 backdrop-blur-sm text-white rounded-lg border border-white/10 shadow-2xl transition-all duration-300 ease-out relative overflow-hidden group"
                            onClick={() =>
                              window.open(
                                "https://ollama.com/upgrade",
                                "_blank",
                              )
                            }
                          >
                            <div className="absolute inset-0 bg-gradient-to-r from-cyan-500/20 via-purple-500/20 to-green-500/20 opacity-60 group-hover:opacity-80 transition-opacity duration-300"></div>
                            <div className="absolute inset-0 bg-gradient-to-r from-transparent via-white/5 to-transparent translate-x-[-100%] group-hover:translate-x-[100%] transition-transform duration-1000 ease-out"></div>
                            <span className="relative z-10 flex items-center space-x-2">
                              <span>Upgrade</span>
                            </span>
                          </Button>
                        )}
                        <Button
                          type="button"
                          outline
                          className="px-3 py-2 text-sm"
                          onClick={() =>
                            window.open("https://ollama.com/settings", "_blank")
                          }
                        >
                          Manage
                        </Button>
                        <Button
                          type="button"
                          color="zinc"
                          className="px-3 py-2 text-sm"
                          onClick={() => disconnectUser()}
                        >
                          Sign out
                        </Button>
                      </div>
                    </div>
                    {user?.avatarurl && (
                      <img
                        src={user.avatarurl}
                        alt={user?.name}
                        className="h-10 w-10 rounded-full bg-neutral-200 dark:bg-neutral-700 flex-shrink-0"
                        onError={(e) => {
                          const target = e.target as HTMLImageElement;
                          target.className = "hidden";
                        }}
                      />
                    )}
                  </div>
                ) : (
                  <div className="flex items-center justify-between">
                    <div>
                      <Label>Ollama account</Label>
                      <Description>Not connected</Description>
                    </div>
                    <Button
                      type="button"
                      outline
                      onClick={handleConnectOllamaAccount}
                      disabled={isRefreshing || isAwaitingConnection}
                    >
                      {isRefreshing || isAwaitingConnection ? (
                        <AnimatedDots />
                      ) : (
                        "Sign In"
                      )}
                    </Button>
                  </div>
                )}
              </Field>
              {connectionError && (
                <div className="mt-3 p-3 bg-red-50 dark:bg-red-900/20 border border-red-200 dark:border-red-800 rounded-lg">
                  <Text className="text-sm text-red-600 dark:text-red-400">
                    {connectionError}
                  </Text>
                </div>
              )}
            </div>
          </div>
          {/* Local Configuration */}
          <div className="relative overflow-hidden rounded-xl bg-white dark:bg-neutral-800">
            <div className="space-y-4 p-4">
              <Field>
                <div className="flex items-start justify-between gap-4">
                  <div className="flex items-start space-x-3 flex-1">
                    <CloudIcon className="mt-1 h-5 w-5 flex-shrink-0 text-black dark:text-neutral-100" />
                    <div>
                      <Label>Cloud</Label>
                      <Description>
                        {cloudOverriddenByEnv
                          ? "The OLLAMA_NO_CLOUD environment variable is currently forcing cloud off."
                          : "Enable cloud models and web search."}
                      </Description>
                    </div>
                  </div>
                  <div className="flex-shrink-0">
                    <Switch
                      checked={!cloudDisabled}
                      disabled={cloudToggleDisabled}
                      onChange={(checked) => {
                        if (cloudOverriddenByEnv) {
                          return;
                        }
                        updateCloudMutation.mutate(checked);
                      }}
                    />
                  </div>
                </div>
              </Field>
              {/* Local Models */}
              <Field>
                <div className="flex items-start justify-between gap-4">
                  <div className="flex items-start space-x-3 flex-1">
                    <CpuChipIcon className="mt-1 h-5 w-5 flex-shrink-0 text-black dark:text-neutral-100" />
                    <div>
                      <Label>Local Models</Label>
                      <Description>
                        Show imported local GGUF models in the model selector.
                      </Description>
                    </div>
                  </div>
                  <div className="flex-shrink-0">
                    <Switch
                      checked={settings.LocalModelsEnabled}
                      onChange={(checked) => {
                        void handleChange("LocalModelsEnabled", checked);
                      }}
                    />
                  </div>
                </div>
              </Field>
              {!isWindows && (
                <Field>
                  <div className="flex items-start justify-between gap-4">
                    <div className="flex flex-1 items-start space-x-3">
                      <Squares2X2Icon className="mt-1 h-5 w-5 flex-shrink-0 text-black dark:text-neutral-100" />
                      <div>
                        <Label>Show apps in menu</Label>
                        <Description>
                          Show connected apps at the top of the Ollama menu.
                        </Description>
                      </div>
                    </div>
                    <div className="flex-shrink-0">
                      <Switch
                        checked={showAppsInMenu}
                        disabled={showAppsInMenuPending}
                        onChange={handleShowAppsInMenu}
                      />
                    </div>
                  </div>
                </Field>
              )}

              {/* Auto Update */}
              <Field>
                <div className="flex items-start justify-between gap-4">
                  <div className="flex items-start space-x-3 flex-1">
                    <ArrowDownTrayIcon className="mt-1 h-5 w-5 flex-shrink-0 text-black dark:text-neutral-100" />
                    <div>
                      <Label>Auto-download updates</Label>
                      <Description>
                        {settings.AutoUpdateEnabled
                          ? "Automatically download updates when available."
                          : "Updates will not be downloaded automatically."}
                      </Description>
                    </div>
                  </div>
                  <div className="flex-shrink-0">
                    <Switch
                      checked={settings.AutoUpdateEnabled}
                      onChange={(checked) => {
                        void handleChange("AutoUpdateEnabled", checked);
                      }}
                    />
                  </div>
                </div>
              </Field>

              {/* Expose Ollama */}
              <Field>
                <div className="flex items-start justify-between gap-4">
                  <div className="flex items-start space-x-3 flex-1">
                    <WifiIcon className="mt-1 h-5 w-5 flex-shrink-0 text-black dark:text-neutral-100" />
                    <div>
                      <Label>Expose Ollama to the network</Label>
                      <Description>
                        Allow other devices or services to access Ollama.
                      </Description>
                    </div>
                  </div>
                  <div className="flex-shrink-0">
                    <Switch
                      checked={settings.Expose}
                      onChange={(checked) => {
                        void handleChange("Expose", checked);
                      }}
                    />
                  </div>
                </div>
              </Field>

              {/* Model Directory */}
              <Field>
                <div className="flex items-start space-x-3">
                  <FolderIcon className="mt-1 h-5 w-5 flex-shrink-0 text-black dark:text-neutral-100" />
                  <div className="w-full">
                    <Label>Model location</Label>
                    <Description>Location where models are stored.</Description>
                    <div className="mt-2 flex items-center space-x-2">
                      <Input
                        value={modelsPath}
                        onChange={(e) => setModelsPath(e.target.value)}
                        onBlur={() => {
                          if (modelsPath !== (settings.Models || "")) {
                            handleChange("Models", modelsPath);
                          }
                        }}
                        onKeyDown={(e) => {
                          if (e.key === "Enter") {
                            e.currentTarget.blur();
                          }
                        }}
                      />
                      <Button
                        type="button"
                        outline
                        className="px-2"
                        disabled={importModelMutation.isPending}
                        onClick={() => {
                          void handleImportModel();
                        }}
                      >
                        <ArrowDownTrayIcon className="w-4 h-4 mr-1" />
                        {importModelMutation.isPending
                          ? "Linking..."
                          : "Link GGUF"}
                      </Button>
                      <Button
                        type="button"
                        outline
                        className="px-2"
                        onClick={() => {
                          setShowModelManager((value) => !value);
                          setDeleteModelError(null);
                        }}
                      >
                        Manage
                      </Button>
                      <Button
                        type="button"
                        outline
                        className="px-2"
                        onClick={async () => {
                          if (window.webview?.selectModelsDirectory) {
                            try {
                              const directory =
                                await window.webview.selectModelsDirectory();
                              if (directory) {
                                setModelsPath(directory);
                                await handleChange("Models", directory);
                              }
                            } catch (error) {
                              console.error(
                                "Error selecting models directory:",
                                error,
                              );
                            }
                          }
                        }}
                      >
                        <FolderIcon className="w-4 h-4 mr-1" />
                        Browse
                      </Button>
                    </div>
                    {importModelPaths && (
                      <div className="mt-3 rounded-lg border border-neutral-200 bg-neutral-50 p-3 dark:border-neutral-700 dark:bg-neutral-900/40">
                        <div className="flex flex-col gap-3 sm:flex-row sm:items-end">
                          <div className="min-w-0 flex-1">
                            <Label>Linked model name</Label>
                            <Description>
                              {importModelPaths.length} GGUF file
                              {importModelPaths.length === 1 ? "" : "s"}{" "}
                              selected.
                            </Description>
                            <Input
                              className="mt-2"
                              value={importModelName}
                              onChange={(e) =>
                                setImportModelName(e.target.value)
                              }
                              onKeyDown={(e) => {
                                if (e.key === "Enter") {
                                  void handleConfirmImportModel();
                                }
                              }}
                            />
                          </div>
                          <div className="flex flex-shrink-0 gap-2">
                            <Button
                              type="button"
                              outline
                              disabled={importModelMutation.isPending}
                              onClick={() => {
                                void handleConfirmImportModel();
                              }}
                            >
                              {importModelMutation.isPending
                                ? "Linking..."
                                : "Link model"}
                            </Button>
                            <Button
                              type="button"
                              plain
                              disabled={importModelMutation.isPending}
                              onClick={() => {
                                setImportModelPaths(null);
                                setImportModelName("");
                                setImportModelError(null);
                              }}
                            >
                              Cancel
                            </Button>
                          </div>
                        </div>
                      </div>
                    )}
                    {importModelError && (
                      <p className="mt-2 text-sm text-red-600 dark:text-red-400">
                        {importModelError}
                      </p>
                    )}
                    {showModelManager && (
                      <div className="mt-3 rounded-lg border border-neutral-200 bg-neutral-50 p-3 dark:border-neutral-700 dark:bg-neutral-900/40">
                        <div className="mb-3 flex items-start justify-between gap-3">
                          <div>
                            <Label>Manage models</Label>
                            <Description>
                              Rename, unlink linked GGUFs, or delete models
                              stored by Ollama.
                            </Description>
                          </div>
                          <Button
                            type="button"
                            plain
                            className="px-2"
                            onClick={() => {
                              void installedModelsQuery.refetch();
                            }}
                          >
                            Refresh
                          </Button>
                        </div>
                        {installedModelsQuery.isLoading ? (
                          <p className="text-sm text-neutral-500 dark:text-neutral-400">
                            Loading models...
                          </p>
                        ) : installedModelsQuery.data?.length ? (
                          <div className="max-h-72 overflow-y-auto rounded-lg border border-neutral-200 dark:border-neutral-700">
                            {installedModelsQuery.data.map((model) => (
                              <div
                                key={`${model.name}-${model.digest}`}
                                className="flex items-center justify-between gap-3 border-b border-neutral-200 px-3 py-2 last:border-b-0 dark:border-neutral-700"
                              >
                                <div className="min-w-0 flex-1">
                                  <div className="truncate text-sm font-medium text-neutral-950 dark:text-white">
                                    {model.name}
                                  </div>
                                  <div className="mt-1">
                                    <Input
                                      className="h-8 text-sm"
                                      defaultValue={model.displayName || ""}
                                      placeholder="Display name"
                                      disabled={renameModelMutation.isPending}
                                      onBlur={(event) => {
                                        const displayName =
                                          event.currentTarget.value.trim();
                                        if (
                                          displayName !==
                                          (model.displayName || "")
                                        ) {
                                          renameModelMutation.mutate({
                                            model: model.name,
                                            displayName,
                                          });
                                        }
                                      }}
                                      onKeyDown={(event) => {
                                        if (event.key === "Enter") {
                                          event.currentTarget.blur();
                                        }
                                      }}
                                    />
                                  </div>
                                  <div className="mt-1 truncate text-xs text-neutral-500 dark:text-neutral-400">
                                    {modelOriginLabel(model)} ·{" "}
                                    {modelDetails(model)} ·{" "}
                                    {formatModelSize(model.size)}
                                  </div>
                                  {model.sourcePath && (
                                    <div className="mt-0.5 truncate text-xs text-neutral-500 dark:text-neutral-400">
                                      {model.sourcePath}
                                    </div>
                                  )}
                                </div>
                                <div className="flex flex-shrink-0 gap-1">
                                  {model.canUpdate && (
                                    <Button
                                      type="button"
                                      plain
                                      className="px-2"
                                      disabled={
                                        checkModelUpdateMutation.isPending
                                      }
                                      onClick={() =>
                                        checkModelUpdateMutation.mutate(
                                          model.name,
                                        )
                                      }
                                    >
                                      Check Update
                                    </Button>
                                  )}
                                  {model.canUnlink && (
                                    <Button
                                      type="button"
                                      plain
                                      className="px-2 text-red-600 dark:text-red-400"
                                      disabled={unlinkModelMutation.isPending}
                                      onClick={() => {
                                        unlinkModelMutation.mutate(model.name);
                                      }}
                                    >
                                      Unlink
                                    </Button>
                                  )}
                                  {model.canDelete && (
                                    <Button
                                      type="button"
                                      plain
                                      className="px-2 text-red-600 dark:text-red-400"
                                      disabled={deleteModelMutation.isPending}
                                      onClick={() => {
                                        deleteModelMutation.mutate(model.name);
                                      }}
                                    >
                                      <TrashIcon className="h-4 w-4" />
                                      Delete
                                    </Button>
                                  )}
                                </div>
                              </div>
                            ))}
                          </div>
                        ) : (
                          <p className="text-sm text-neutral-500 dark:text-neutral-400">
                            No installed models found.
                          </p>
                        )}
                        {deleteModelError && (
                          <p className="mt-2 text-sm text-red-600 dark:text-red-400">
                            {deleteModelError}
                          </p>
                        )}
                        {modelManagerMessage && (
                          <p className="mt-2 text-sm text-neutral-500 dark:text-neutral-400">
                            {modelManagerMessage}
                          </p>
                        )}
                      </div>
                    )}
                  </div>
                </div>
              </Field>
            </div>
          </div>

          <div className="overflow-hidden rounded-xl bg-white dark:bg-neutral-800">
            <div className="p-4">
              <Field>
                <div className="flex items-start justify-between gap-4">
                  <div className="flex items-start space-x-3 flex-1">
                    <CogIcon className="mt-1 h-5 w-5 flex-shrink-0 text-black dark:text-neutral-100" />
                    <div>
                      <Label>Inference</Label>
                      <Description>
                        Show the Inference section with advanced local model
                        controls.
                      </Description>
                    </div>
                  </div>
                  <div className="flex-shrink-0">
                    <Switch
                      checked={settings.InferenceEnabled}
                      onChange={(checked) => {
                        void handleChange("InferenceEnabled", checked);
                      }}
                    />
                  </div>
                </div>
              </Field>
            </div>
          </div>

          <div className="overflow-hidden rounded-xl bg-white dark:bg-neutral-800">
            <div className="p-4">
              <Field>
                <div className="flex items-start justify-between gap-4">
                  <div className="flex items-start space-x-3 flex-1">
                    <PuzzlePieceIcon className="mt-1 h-5 w-5 flex-shrink-0 text-black dark:text-neutral-100" />
                    <div>
                      <Label>Plugins</Label>
                      <Description>
                        Enable the Plugins section and saved plugin
                        configurations.
                      </Description>
                    </div>
                  </div>
                  <div className="flex-shrink-0">
                    <Switch
                      checked={settings.PluginsEnabled}
                      onChange={(checked) => {
                        void handleChange("PluginsEnabled", checked);
                      }}
                    />
                  </div>
                </div>
              </Field>
            </div>
          </div>

          <ClaudeDesktopModelsSettings />

          {/* Agent Mode */}
          {window.OLLAMA_TOOLS && (
            <div className="overflow-hidden rounded-xl bg-white dark:bg-neutral-800">
              <div className="space-y-4 p-4">
                <Field>
                  <div className="flex items-center justify-between">
                    <div className="flex items-start space-x-3">
                      <BoltIcon className="mt-1 h-5 w-5 flex-shrink-0 text-black dark:text-neutral-100" />
                      <div>
                        <Label>Enable Agent Mode</Label>
                        <Description>
                          Use multi-turn tools to fulfill user requests
                        </Description>
                      </div>
                    </div>
                    <Switch
                      checked={settings.Agent}
                      onChange={(checked) => {
                        void handleChange("Agent", checked);
                      }}
                    />
                  </div>
                </Field>

                {/* Tools Mode */}
                <Field>
                  <div className="flex items-center justify-between">
                    <div className="flex items-start space-x-3">
                      <WrenchIcon className="mt-1 h-5 w-5 flex-shrink-0 text-black dark:text-neutral-100" />
                      <div>
                        <Label>Enable Tools Mode</Label>
                        <Description>
                          Use single-turn tools to fulfill user requests
                        </Description>
                      </div>
                    </div>
                    <Switch
                      checked={settings.Tools}
                      onChange={(checked) => {
                        void handleChange("Tools", checked);
                      }}
                    />
                  </div>
                </Field>
              </div>
            </div>
          )}

          {/* Reset button */}
          <div className="mt-6 flex justify-end px-4">
            <Button
              type="button"
              outline
              className="px-3"
              onClick={handleResetToDefaults}
            >
              Reset to defaults
            </Button>
          </div>
        </div>

        {/* Saved indicator */}
        {(showSaved || restartMessage) && (
          <div className="fixed bottom-4 left-1/2 transform -translate-x-1/2 transition-opacity duration-300 z-50">
            <Badge
              color="green"
              className="!bg-green-500 !text-white dark:!bg-green-600"
            >
              Saved
            </Badge>
          </div>
        )}
      </div>
    </main>
  );
}
