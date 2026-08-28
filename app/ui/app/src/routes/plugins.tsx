import { useMemo, useState } from "react";
import { createFileRoute } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { TrashIcon, PlusIcon } from "@heroicons/react/24/outline";
import { AppSidebar } from "@/components/AppSidebar";
import { SidebarLayout } from "@/components/layout/layout";
import { Button } from "@/components/ui/button";
import { Switch } from "@/components/ui/switch";
import { Text } from "@/components/ui/text";
import {
  deletePlugin,
  getPlugins,
  importPlugins,
  updatePluginEnabled,
  type Plugin,
} from "@/api";
import { useSettings } from "@/hooks/useSettings";

const exampleConfig = `{
  "mcpServers": {
    "example": {
      "command": "example-command",
      "args": []
    }
  }
}`;

export const Route = createFileRoute("/plugins")({
  component: PluginsRoute,
});

function PluginsRoute() {
  const queryClient = useQueryClient();
  const { settings } = useSettings();
  const [showEditor, setShowEditor] = useState(false);
  const [jsonConfig, setJsonConfig] = useState(exampleConfig);
  const [formError, setFormError] = useState<string | null>(null);

  const pluginsQuery = useQuery({
    queryKey: ["plugins"],
    queryFn: getPlugins,
    enabled: settings.pluginsEnabled,
  });

  const importMutation = useMutation({
    mutationFn: importPlugins,
    onSuccess: async () => {
      setFormError(null);
      setShowEditor(false);
      setJsonConfig(exampleConfig);
      await queryClient.invalidateQueries({ queryKey: ["plugins"] });
    },
    onError: (error: Error) => {
      setFormError(error.message);
    },
  });

  const toggleMutation = useMutation({
    mutationFn: ({ id, enabled }: { id: string; enabled: boolean }) =>
      updatePluginEnabled(id, enabled),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["plugins"] });
    },
  });

  const deleteMutation = useMutation({
    mutationFn: deletePlugin,
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["plugins"] });
    },
  });

  const plugins = useMemo(
    () => pluginsQuery.data?.plugins ?? [],
    [pluginsQuery.data?.plugins],
  );

  return (
    <SidebarLayout title="Plugins" sidebar={<AppSidebar current="plugins" />}>
      <div className="flex h-full flex-col overflow-y-auto px-6 py-4">
        <div className="mx-auto flex w-full max-w-4xl flex-1 flex-col gap-4">
          <div className="flex items-start justify-between gap-4">
            <div>
              <h2 className="text-lg font-semibold text-neutral-900 dark:text-white">
                Plugins
              </h2>
              <p className="mt-1 text-sm text-neutral-500 dark:text-neutral-400">
                Saved plugin configurations stay here. This step only stores and
                manages config; it does not start MCP processes or register
                tools.
              </p>
            </div>

            {settings.pluginsEnabled && (
              <Button
                type="button"
                outline
                onClick={() => {
                  setFormError(null);
                  setShowEditor((current) => !current);
                }}
              >
                <PlusIcon data-slot="icon" />+ Add
              </Button>
            )}
          </div>

          {!settings.pluginsEnabled ? (
            <div className="rounded-xl bg-white p-4 dark:bg-neutral-800">
              <Text className="text-sm text-neutral-600 dark:text-neutral-300">
                Plugins are off. Enable them in Settings to show this section
                and manage saved configurations.
              </Text>
            </div>
          ) : (
            <>
              {showEditor && (
                <div className="rounded-xl bg-white p-4 dark:bg-neutral-800">
                  <div className="space-y-3">
                    <div>
                      <h3 className="text-sm font-medium text-neutral-900 dark:text-white">
                        Add plugin configuration
                      </h3>
                      <p className="mt-1 text-sm text-neutral-500 dark:text-neutral-400">
                        Paste MCP JSON directly. Each entry in{" "}
                        <code className="rounded bg-neutral-100 px-1 py-0.5 text-xs dark:bg-neutral-700">
                          mcpServers
                        </code>{" "}
                        is stored as one plugin.
                      </p>
                    </div>

                    <textarea
                      value={jsonConfig}
                      onChange={(event) => setJsonConfig(event.target.value)}
                      spellCheck={false}
                      className="min-h-72 w-full rounded-xl border border-neutral-200 bg-neutral-50 p-4 font-mono text-sm text-neutral-900 outline-none ring-0 placeholder:text-neutral-400 focus:border-neutral-400 dark:border-neutral-700 dark:bg-neutral-900 dark:text-white dark:placeholder:text-neutral-500"
                    />

                    {formError && (
                      <div className="rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-700 dark:border-red-900/60 dark:bg-red-950/30 dark:text-red-300">
                        {formError}
                      </div>
                    )}

                    <div className="flex items-center justify-end gap-2">
                      <Button
                        type="button"
                        outline
                        onClick={() => {
                          setShowEditor(false);
                          setFormError(null);
                        }}
                      >
                        Cancel
                      </Button>
                      <Button
                        type="button"
                        onClick={() => importMutation.mutate(jsonConfig)}
                        disabled={importMutation.isPending}
                      >
                        Save configuration
                      </Button>
                    </div>
                  </div>
                </div>
              )}

              <div className="rounded-xl bg-white dark:bg-neutral-800">
                {pluginsQuery.isLoading ? (
                  <div className="p-4 text-sm text-neutral-500 dark:text-neutral-400">
                    Loading plugins...
                  </div>
                ) : pluginsQuery.error ? (
                  <div className="p-4 text-sm text-red-600 dark:text-red-400">
                    {(pluginsQuery.error as Error).message}
                  </div>
                ) : plugins.length === 0 ? (
                  <div className="p-4 text-sm text-neutral-500 dark:text-neutral-400">
                    No saved plugins yet.
                  </div>
                ) : (
                  <div className="divide-y divide-neutral-200 dark:divide-neutral-700">
                    {plugins.map((plugin) => (
                      <PluginRow
                        key={plugin.id}
                        plugin={plugin}
                        onToggle={(enabled) =>
                          toggleMutation.mutate({ id: plugin.id, enabled })
                        }
                        onDelete={() => deleteMutation.mutate(plugin.id)}
                        disabled={
                          toggleMutation.isPending || deleteMutation.isPending
                        }
                      />
                    ))}
                  </div>
                )}
              </div>
            </>
          )}
        </div>
      </div>
    </SidebarLayout>
  );
}

function PluginRow({
  plugin,
  onToggle,
  onDelete,
  disabled,
}: {
  plugin: Plugin;
  onToggle: (enabled: boolean) => void;
  onDelete: () => void;
  disabled: boolean;
}) {
  return (
    <div className="space-y-3 p-4">
      <div className="flex items-start justify-between gap-4">
        <div className="min-w-0">
          <div className="flex items-center gap-2">
            <h3 className="truncate text-sm font-medium text-neutral-900 dark:text-white">
              {plugin.name}
            </h3>
            <span className="rounded-full bg-neutral-100 px-2 py-0.5 text-xs text-neutral-600 dark:bg-neutral-700 dark:text-neutral-200">
              {plugin.provider}
            </span>
          </div>
          <Text className="mt-1 text-sm text-neutral-500 dark:text-neutral-400">
            ID: {plugin.id}
          </Text>
        </div>

        <div className="flex items-center gap-3">
          <Switch
            checked={plugin.enabled}
            disabled={disabled}
            onChange={onToggle}
          />
          <Button type="button" plain onClick={onDelete} disabled={disabled}>
            <TrashIcon data-slot="icon" />
            Remove
          </Button>
        </div>
      </div>

      <pre className="overflow-x-auto rounded-xl bg-neutral-50 p-4 text-xs text-neutral-700 dark:bg-neutral-900 dark:text-neutral-200">
        {JSON.stringify(plugin.config, null, 2)}
      </pre>
    </div>
  );
}
