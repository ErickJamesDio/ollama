import { useMemo, useState, type ReactNode } from "react";
import { createFileRoute } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  CheckCircleIcon,
  ExclamationCircleIcon,
  PlusIcon,
  TrashIcon,
} from "@heroicons/react/24/outline";
import { AppSidebar } from "@/components/AppSidebar";
import { SidebarLayout } from "@/components/layout/layout";
import { Button } from "@/components/ui/button";
import { Switch } from "@/components/ui/switch";
import { Text } from "@/components/ui/text";
import { Tooltip } from "@/components/ui/tooltip";
import {
  deletePlugin,
  getPlugins,
  importPlugins,
  updatePluginEnabled,
  updatePluginToolEnabled,
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

  const toolToggleMutation = useMutation({
    mutationFn: ({
      pluginId,
      toolName,
      enabled,
    }: {
      pluginId: string;
      toolName: string;
      enabled: boolean;
    }) => updatePluginToolEnabled(pluginId, toolName, enabled),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["plugins"] });
    },
  });

  const plugins = useMemo(
    () => pluginsQuery.data?.plugins ?? [],
    [pluginsQuery.data?.plugins],
  );

  const mcpPlugins = useMemo(
    () => plugins.filter((plugin) => plugin.provider === "mcp"),
    [plugins],
  );

  const otherPlugins = useMemo(
    () => plugins.filter((plugin) => plugin.provider !== "mcp"),
    [plugins],
  );

  const mutationsPending =
    toggleMutation.isPending ||
    deleteMutation.isPending ||
    toolToggleMutation.isPending;

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
                Saved MCP plugins stay here with runtime status and per-tool
                control. Disabled tools stay out of the chat tool registry.
              </p>
            </div>

            {settings.pluginsEnabled && (
              <Button
                type="button"
                outline
                onClick={() => {
                  setFormError(null);
                  document
                    .getElementById("mcp-configuration")
                    ?.scrollIntoView({ behavior: "smooth", block: "start" });
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
              <ProviderBlock
                title="MCP"
                description="Model Context Protocol servers and the tools they publish to Ollama."
              >
                {mcpPlugins.length === 0 ? (
                  <Text className="text-sm text-neutral-500 dark:text-neutral-400">
                    No MCP servers configured yet.
                  </Text>
                ) : (
                  <div className="divide-y divide-neutral-200 dark:divide-neutral-700">
                    {mcpPlugins.map((plugin) => (
                      <McpServerRow
                        key={plugin.id}
                        plugin={plugin}
                        onToggle={(enabled) =>
                          toggleMutation.mutate({ id: plugin.id, enabled })
                        }
                        onToolToggle={(toolName, enabled) =>
                          toolToggleMutation.mutate({
                            pluginId: plugin.id,
                            toolName,
                            enabled,
                          })
                        }
                        onDelete={() => deleteMutation.mutate(plugin.id)}
                        disabled={mutationsPending}
                      />
                    ))}
                  </div>
                )}

                <div
                  id="mcp-configuration"
                  className="border-t border-neutral-200 pt-4 dark:border-neutral-700"
                >
                  <div className="space-y-3">
                    <div>
                      <h3 className="text-sm font-medium text-neutral-900 dark:text-white">
                        MCP Configuration
                      </h3>
                      <p className="mt-1 text-sm text-neutral-500 dark:text-neutral-400">
                        Paste or replace the raw MCP configuration. Each entry in{" "}
                        <code className="rounded bg-neutral-100 px-1 py-0.5 text-xs dark:bg-neutral-700">
                          mcpServers
                        </code>{" "}
                        becomes one MCP server above.
                      </p>
                    </div>

                    <textarea
                      value={jsonConfig}
                      onChange={(event) => setJsonConfig(event.target.value)}
                      spellCheck={false}
                      className="min-h-56 w-full rounded-xl border border-neutral-200 bg-neutral-50 p-4 font-mono text-sm text-neutral-900 outline-none ring-0 placeholder:text-neutral-400 focus:border-neutral-400 dark:border-neutral-700 dark:bg-neutral-900 dark:text-white dark:placeholder:text-neutral-500"
                    />

                    {formError && (
                      <div className="rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-700 dark:border-red-900/60 dark:bg-red-950/30 dark:text-red-300">
                        {formError}
                      </div>
                    )}

                    <div className="flex items-center justify-end">
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
              </ProviderBlock>

              {otherPlugins.length > 0 && (
                <ProviderBlock
                  title="Other Plugins"
                  description="Additional plugin providers can live here without depending on MCP."
                >
                  <div className="divide-y divide-neutral-200 dark:divide-neutral-700">
                    {otherPlugins.map((plugin) => (
                      <GenericPluginRow
                        key={plugin.id}
                        plugin={plugin}
                        onToggle={(enabled) =>
                          toggleMutation.mutate({ id: plugin.id, enabled })
                        }
                        onDelete={() => deleteMutation.mutate(plugin.id)}
                        disabled={mutationsPending}
                      />
                    ))}
                  </div>
                </ProviderBlock>
              )}
            </>
          )}
        </div>
      </div>
    </SidebarLayout>
  );
}

function ProviderBlock({
  title,
  description,
  children,
}: {
  title: string;
  description: string;
  children: ReactNode;
}) {
  return (
    <section className="overflow-hidden rounded-xl bg-white dark:bg-neutral-800">
      <div className="border-b border-neutral-200 px-4 py-4 dark:border-neutral-700">
        <div className="flex items-center gap-2">
          <h2 className="text-base font-semibold text-neutral-900 dark:text-white">
            {title}
          </h2>
          {title === "MCP" && (
            <span className="rounded-full bg-neutral-100 px-2 py-0.5 text-xs font-medium text-neutral-600 dark:bg-neutral-700 dark:text-neutral-200">
              Provider
            </span>
          )}
        </div>
        <p className="mt-1 text-sm text-neutral-500 dark:text-neutral-400">
          {description}
        </p>
      </div>
      <div className="space-y-4 p-4">{children}</div>
    </section>
  );
}

function McpServerRow({
  plugin,
  onToggle,
  onToolToggle,
  onDelete,
  disabled,
}: {
  plugin: Plugin;
  onToggle: (enabled: boolean) => void;
  onToolToggle: (toolName: string, enabled: boolean) => void;
  onDelete: () => void;
  disabled: boolean;
}) {
  const runtime = plugin.runtime;
  const tools = runtime?.tools ?? [];
  const status = runtime?.status ?? "error";
  const enabledToolCount = tools.filter((tool) => tool.enabled).length;
  const statusStyles = {
    connected:
      "bg-emerald-50 text-emerald-700 dark:bg-emerald-950/40 dark:text-emerald-300",
    starting:
      "bg-amber-50 text-amber-700 dark:bg-amber-950/40 dark:text-amber-300",
    disabled:
      "bg-neutral-100 text-neutral-600 dark:bg-neutral-700 dark:text-neutral-200",
    error: "bg-red-50 text-red-700 dark:bg-red-950/40 dark:text-red-300",
  } as const;

  return (
    <div className="py-4 first:pt-0 last:pb-0">
      <div className="flex items-start justify-between gap-4">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            <h3 className="text-sm font-semibold text-neutral-900 dark:text-white">
              {humanizeName(plugin.name)}
            </h3>
            <span
              className={`rounded-full px-2 py-0.5 text-xs capitalize ${statusStyles[status]}`}
            >
              {status}
            </span>
          </div>
          <p className="mt-1 text-sm text-neutral-500 dark:text-neutral-400">
            {status === "connected"
              ? `${enabledToolCount} of ${tools.length} tools enabled`
              : `${runtime?.toolCount ?? 0} tools discovered`}
          </p>
        </div>

        <div className="flex items-center gap-3">
          <Switch
            checked={plugin.enabled}
            disabled={disabled}
            onChange={onToggle}
            className="shrink-0 self-center"
          />
          <Button type="button" plain onClick={onDelete} disabled={disabled}>
            <TrashIcon data-slot="icon" />
            Remove
          </Button>
        </div>
      </div>

      {runtime?.lastError && (
        <div className="mt-3 flex items-start gap-2 rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-700 dark:border-red-900/60 dark:bg-red-950/30 dark:text-red-300">
          <ExclamationCircleIcon className="mt-0.5 h-4 w-4 shrink-0" />
          <span>{runtime.lastError}</span>
        </div>
      )}

      <details open className="mt-4 rounded-xl border border-neutral-200 dark:border-neutral-700">
        <summary className="flex cursor-pointer list-none items-center justify-between gap-3 px-3 py-2.5 text-sm font-medium text-neutral-900 dark:text-white">
          <span className="flex items-center gap-2">
            <CheckCircleIcon className="h-4 w-4 text-neutral-400" />
            Tools
          </span>
          <span className="text-xs font-normal text-neutral-500 dark:text-neutral-400">
            {enabledToolCount}/{tools.length}
          </span>
        </summary>

        {tools.length === 0 ? (
          <div className="border-t border-neutral-200 px-3 py-3 dark:border-neutral-700">
            <Text className="text-sm text-neutral-500 dark:text-neutral-400">
              No discovered tools yet. If this server is enabled, check its runtime status above.
            </Text>
          </div>
        ) : (
          <div className="divide-y divide-neutral-200 border-t border-neutral-200 dark:divide-neutral-700 dark:border-neutral-700">
            {tools.map((tool) => (
              <div key={tool.name} className="flex items-start justify-between gap-4 px-3 py-3">
                <Tooltip
                  content={tool.description}
                  className="min-w-0 pr-2"
                  tooltipClassName="w-72 whitespace-normal text-left"
                >
                  <div className="text-sm font-medium text-neutral-900 dark:text-white">
                    {humanizeToolName(tool.name)}
                  </div>
                </Tooltip>
                <Tooltip
                  content={tool.description}
                  className="mt-0.5 shrink-0 self-start"
                  tooltipClassName="left-auto right-0 w-72 translate-x-0 whitespace-normal text-left"
                >
                  <Switch
                    checked={tool.enabled}
                    disabled={disabled || !plugin.enabled}
                    onChange={(enabled) => onToolToggle(tool.name, enabled)}
                    className="shrink-0 self-start"
                  />
                </Tooltip>
              </div>
            ))}
          </div>
        )}
      </details>
    </div>
  );
}

function GenericPluginRow({
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
    <div className="flex items-start justify-between gap-4 py-4 first:pt-0 last:pb-0">
      <div className="min-w-0">
        <div className="flex flex-wrap items-center gap-2">
          <h3 className="text-sm font-semibold text-neutral-900 dark:text-white">
            {humanizeName(plugin.name)}
          </h3>
          <span className="rounded-full bg-neutral-100 px-2 py-0.5 text-xs font-medium uppercase text-neutral-600 dark:bg-neutral-700 dark:text-neutral-200">
            {plugin.provider}
          </span>
        </div>
        <p className="mt-1 text-sm text-neutral-500 dark:text-neutral-400">Plugin provider</p>
      </div>
      <div className="flex items-center gap-3">
        <Switch
          checked={plugin.enabled}
          disabled={disabled}
          onChange={onToggle}
          className="shrink-0 self-center"
        />
        <Button type="button" plain onClick={onDelete} disabled={disabled}>
          <TrashIcon data-slot="icon" />
          Remove
        </Button>
      </div>
    </div>
  );
}

function humanizeToolName(toolName: string) {
  const shortName = toolName.split(".").pop() ?? toolName;
  return humanizeName(shortName);
}

function humanizeName(value: string) {
  return value
    .replace(/[-_]+/g, " ")
    .replace(/\b\w/g, (letter) => letter.toUpperCase());
}
