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
  localModelsEnabled: boolean;
  pluginsEnabled: boolean;
}

// Type for partial settings updates
type SettingsUpdate = Partial<{
  TurboEnabled: boolean;
  WebSearchEnabled: boolean;
  ThinkEnabled: boolean;
  ThinkLevel: string;
  SelectedModel: string;
  SidebarOpen: boolean;
  LastHomeView: string;
  OnboardingVersion: number;
  LocalModelsEnabled: boolean;
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
      localModelsEnabled: settingsData?.settings?.LocalModelsEnabled ?? true,
      pluginsEnabled: settingsData?.settings?.PluginsEnabled ?? false,
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
