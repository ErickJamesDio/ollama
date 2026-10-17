import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { Settings } from "@/gotypes";
import { getSettings, updateSettings } from "@/api";
import { useMemo, useCallback } from "react";

// TODO(hoyyeva): remove turboEnabled when we remove Migration logic in useSelectedModel.ts
interface SettingsState {
  turboEnabled: boolean;
  webSearchEnabled: boolean;
  selectedModel: string;
  sidebarOpen: boolean;
  lastHomeView: string;
  onboardingVersion: number;
  thinkEnabled: boolean;
  thinkLevel: string;
  contextLength: number;
  localModelsEnabled: boolean;
  cpuFallbackEnabled: boolean;
  inferenceEnabled: boolean;
  inferenceRuntime: string;
  inferenceAutoFitEnabled: boolean;
  inferenceManualLayers: string;
  pluginsEnabled: boolean;
  fileToolsEnabled: boolean;
}

// Type for partial settings updates
type SettingsUpdate = Partial<{
  Expose: boolean;
  Browser: boolean;
  Survey: boolean;
  Models: string;
  Agent: boolean;
  Tools: boolean;
  WorkingDir: string;
  ContextLength: number;
  TurboEnabled: boolean;
  WebSearchEnabled: boolean;
  ThinkEnabled: boolean;
  ThinkLevel: string;
  SelectedModel: string;
  SidebarOpen: boolean;
  LastHomeView: string;
  OnboardingVersion: number;
  AutoUpdateEnabled: boolean;
  ClaudeDesktopUsed: boolean;
  LocalModelsEnabled: boolean;
  CPUFallbackEnabled: boolean;
  InferenceEnabled: boolean;
  InferenceRuntime: string;
  InferenceAutoFitEnabled: boolean;
  InferenceManualLayers: string;
  PluginsEnabled: boolean;
}>;

export function useSettings() {
  const queryClient = useQueryClient();

  // Fetch settings with useQuery
  const { data: settingsData, error } = useQuery({
    queryKey: ["settings"],
    queryFn: getSettings,
  });

  // Update settings with useMutation
  const updateSettingsMutation = useMutation({
    mutationFn: updateSettings,
    onMutate: async (nextSettings) => {
      await queryClient.cancelQueries({ queryKey: ["settings"] });
      const previousSettings = queryClient.getQueryData(["settings"]);
      queryClient.setQueryData(["settings"], {
        settings: nextSettings,
      });
      return { previousSettings };
    },
    onError: (_error, _nextSettings, context) => {
      if (context?.previousSettings !== undefined) {
        queryClient.setQueryData(["settings"], context.previousSettings);
      }
    },
    onSuccess: (data) => {
      queryClient.setQueryData(["settings"], data);
      // Invalidate the query to ensure fresh data
      queryClient.invalidateQueries({ queryKey: ["settings"] });
    },
  });

  // Extract settings with defaults
  const settings: SettingsState = useMemo(
    () => ({
      turboEnabled: settingsData?.settings?.TurboEnabled ?? false,
      webSearchEnabled: settingsData?.settings?.WebSearchEnabled ?? false,
      thinkEnabled: settingsData?.settings?.ThinkEnabled ?? false,
      thinkLevel: settingsData?.settings?.ThinkLevel ?? "none",
      selectedModel: settingsData?.settings?.SelectedModel ?? "",
      sidebarOpen: settingsData?.settings?.SidebarOpen ?? false,
      lastHomeView: settingsData?.settings?.LastHomeView ?? "chat",
      onboardingVersion: settingsData?.settings?.OnboardingVersion ?? 0,
      contextLength: settingsData?.settings?.ContextLength ?? 0,
      localModelsEnabled: settingsData?.settings?.LocalModelsEnabled ?? true,
      cpuFallbackEnabled: settingsData?.settings?.CPUFallbackEnabled ?? false,
      inferenceEnabled: settingsData?.settings?.InferenceEnabled ?? false,
      inferenceRuntime: settingsData?.settings?.InferenceRuntime ?? "auto",
      inferenceAutoFitEnabled:
        settingsData?.settings?.InferenceAutoFitEnabled ?? true,
      inferenceManualLayers: settingsData?.settings?.InferenceManualLayers ?? "",
      pluginsEnabled: settingsData?.settings?.PluginsEnabled ?? false,
      fileToolsEnabled: settingsData?.settings?.Tools ?? false,
    }),
    [settingsData?.settings],
  );

  // Single function to update most settings
  const setSettings = useCallback(
    async (updates: SettingsUpdate) => {
      const currentSettings =
        queryClient.getQueryData<{ settings: Settings }>(["settings"])
          ?.settings ?? settingsData?.settings;
      if (!currentSettings) return;

      const updatedSettings = new Settings({
        ...currentSettings,
        ...updates,
      });

      await updateSettingsMutation.mutateAsync(updatedSettings);
    },
    [queryClient, settingsData?.settings, updateSettingsMutation],
  );

  return useMemo(
    () => ({
      settings,
      settingsData: settingsData?.settings,
      error,
      setSettings,
    }),
    [settings, settingsData?.settings, error, setSettings],
  );
}
