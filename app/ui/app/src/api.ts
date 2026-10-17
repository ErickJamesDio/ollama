import {
  ChatResponse,
  ChatsResponse,
  ChatEvent,
  DownloadEvent,
  ErrorEvent,
  InferenceComputeResponse,
  ModelCapabilitiesResponse,
  Model,
  ChatRequest,
  Settings,
  User,
} from "@/gotypes";
import { parseJsonlFromResponse } from "./util/jsonl-parsing";
import { ollamaClient as ollama } from "./lib/ollama-client";
import type { ModelResponse } from "ollama/browser";
import { API_BASE, OLLAMA_DOT_COM } from "./lib/config";

// Extend Model class with utility methods
declare module "@/gotypes" {
  interface Model {
    isCloud(): boolean;
  }
}

Model.prototype.isCloud = function (): boolean {
  return this.model.endsWith("cloud");
};

export type CloudStatusSource = "env" | "config" | "both" | "none";
export interface CloudStatusResponse {
  disabled: boolean;
  source: CloudStatusSource;
}

export interface IntegrationStatus {
  id: string;
  name: string;
  description: string;
  installed?: boolean;
  command?: string;
}

export type IntegrationStatuses = IntegrationStatus[];

export interface Plugin {
  id: string;
  name: string;
  provider: string;
  enabled: boolean;
  config: Record<string, unknown>;
  created_at: string;
  updated_at: string;
  runtime?: {
    status: "connected" | "error" | "disabled" | "starting";
    tools: Array<{
      name: string;
      description?: string;
      enabled: boolean;
    }>;
    toolCount: number;
    lastError?: string;
  };
}

export interface PluginsResponse {
  plugins: Plugin[];
}

export interface ManagedModel {
  name: string;
  size?: number;
  digest?: string;
  modified_at?: string | Date;
  details?: ModelResponse["details"];
  origin: "linked_gguf" | "ollama_storage";
  sourcePath?: string;
  displayName?: string;
  canRename: boolean;
  canUnlink: boolean;
  canDelete: boolean;
  canUpdate: boolean;
}

type ModelWithDisplayName = Model & { displayName?: string };

export async function getIntegrationStatuses(): Promise<IntegrationStatuses> {
  const response = await fetch(`${API_BASE}/api/v1/integrations`);
  if (!response.ok) {
    throw new Error(`Failed to fetch integration statuses: ${response.status}`);
  }
  return response.json();
}
// Helper function to convert Uint8Array to base64
function uint8ArrayToBase64(uint8Array: Uint8Array): string {
  const chunkSize = 0x8000; // 32KB chunks to avoid stack overflow
  let binary = "";

  for (let i = 0; i < uint8Array.length; i += chunkSize) {
    const chunk = uint8Array.subarray(i, i + chunkSize);
    binary += String.fromCharCode(...chunk);
  }

  return btoa(binary);
}

export async function fetchUser(): Promise<User | null> {
  const response = await fetch(`${API_BASE}/api/me`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
  });

  if (response.ok) {
    const userData: User = await response.json();

    if (userData.avatarurl && !userData.avatarurl.startsWith("http")) {
      userData.avatarurl = `${OLLAMA_DOT_COM}${userData.avatarurl}`;
    }

    return userData;
  }

  if (response.status === 401 || response.status === 403) {
    return null;
  }

  throw new Error(`Failed to fetch user: ${response.status}`);
}

export async function fetchConnectUrl(): Promise<string> {
  const response = await fetch(`${API_BASE}/api/me`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
  });

  if (response.status === 401) {
    const data = await response.json();
    if (data.signin_url) {
      const connectUrl = new URL(data.signin_url);
      connectUrl.searchParams.set("launch", "true");
      return connectUrl.toString();
    }
  }

  throw new Error("Failed to fetch connect URL");
}

export async function disconnectUser(): Promise<void> {
  const response = await fetch(`${API_BASE}/api/signout`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
  });

  if (!response.ok) {
    throw new Error("Failed to disconnect user");
  }
}

export async function getChats(): Promise<ChatsResponse> {
  const response = await fetch(`${API_BASE}/api/v1/chats`);
  const data = await response.json();
  return new ChatsResponse(data);
}

export async function getChat(chatId: string): Promise<ChatResponse> {
  const response = await fetch(`${API_BASE}/api/v1/chat/${chatId}`);
  const data = await response.json();
  return new ChatResponse(data);
}

export async function getModels(query?: string): Promise<Model[]> {
  try {
    const { models: modelsResponse } = await ollama.list();
    const displayNames = new Map(
      (await getManagedModels().catch(() => []))
        .filter((model) => model.displayName)
        .map((model) => [
          model.name.replace(/:latest$/, ""),
          model.displayName || "",
        ]),
    );

    let models: Model[] = modelsResponse
      .filter((m: ModelResponse) => {
        const families = m.details?.families;

        if (!families || families.length === 0) {
          return true;
        }

        const isBertOnly = families.every((family: string) =>
          family.toLowerCase().includes("bert"),
        );

        return !isBertOnly;
      })
      .map((m: ModelResponse) => {
        // Remove the latest tag from the returned model
        const modelName = m.name.replace(/:latest$/, "");

        return Object.assign(new Model({
          model: modelName,
          digest: m.digest,
          modified_at: m.modified_at ? new Date(m.modified_at) : undefined,
        }), {
          displayName: displayNames.get(modelName),
        });
      });

    // Filter by query if provided
    if (query) {
      const normalizedQuery = query.toLowerCase().trim();

      const filteredModels = models.filter((m: Model) => {
        const displayName = (m as ModelWithDisplayName).displayName || "";
        return (
          m.model.toLowerCase().startsWith(normalizedQuery) ||
          displayName.toLowerCase().startsWith(normalizedQuery)
        );
      });

      let exactMatch = false;
      for (const m of filteredModels) {
        if (m.model.toLowerCase() === normalizedQuery) {
          exactMatch = true;
          break;
        }
      }

      // Add query if it's in the registry and not already in the list
      if (!exactMatch) {
        const result = await getModelUpstreamInfo(new Model({ model: query }));
        const existsUpstream = result.exists;
        if (existsUpstream) {
          filteredModels.push(new Model({ model: query }));
        }
      }

      models = filteredModels;
    }

    return models;
  } catch (err) {
    throw new Error(`Failed to fetch models: ${err}`);
  }
}

export async function getManagedModels(): Promise<ManagedModel[]> {
  const response = await fetch(`${API_BASE}/api/v1/models/manage`);
  const contentType = response.headers.get("content-type") || "";
  if (response.ok && contentType.includes("application/json")) {
    const data = await response.json();
    return data.models || [];
  }

  const list = await ollama.list();
  return list.models
    .filter((model) => model.digest)
    .map((model) => {
      const remoteHost = (model as ModelResponse & { remote_host?: string })
        .remote_host;
      return {
        name: model.model || model.name,
        size: model.size,
        digest: model.digest,
        modified_at: model.modified_at,
        details: model.details,
        origin: "ollama_storage",
        canRename: true,
        canUnlink: false,
        canDelete: true,
        canUpdate: !!remoteHost,
      };
    });
}

export async function deleteModel(model: string): Promise<void> {
  const response = await fetch(`${API_BASE}/api/v1/models/delete`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ model }),
  });

  if (!response.ok) {
    const error = await response.text();
    throw new Error(error || "Failed to delete model");
  }
}

export async function unlinkModel(model: string): Promise<void> {
  const response = await fetch(`${API_BASE}/api/v1/models/unlink`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ model }),
  });

  if (!response.ok) {
    const error = await response.json().catch(() => null);
    throw new Error(error?.error || "Failed to unlink model");
  }
}

export async function setModelAlias(
  model: string,
  displayName: string,
): Promise<void> {
  const response = await fetch(`${API_BASE}/api/v1/models/alias`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ model, displayName }),
  });

  if (!response.ok) {
    const error = await response.json().catch(() => null);
    throw new Error(error?.error || "Failed to rename model");
  }
}

export async function getClaudeDesktopAvailableModels(): Promise<Model[]> {
  try {
    const { models: modelsResponse } = await ollama.list();

    const seen = new Set<string>();
    return modelsResponse
      .filter((model: ModelResponse) => {
        const response = model as ModelResponse & {
          remote_model?: string;
          remote_host?: string;
        };
        const name = model.name.replace(/:latest$/, "");
        return (
          !response.remote_model &&
          !response.remote_host &&
          !name.endsWith("cloud")
        );
      })
      .filter((model: ModelResponse) => {
        const base = model.name.replace(/:latest$/, "");
        if (!base || seen.has(base)) return false;

        const families = model.details?.families;
        const supported =
          !families ||
          families.length === 0 ||
          !families.every((family: string) =>
            family.toLowerCase().includes("bert"),
          );
        if (supported) seen.add(base);
        return supported;
      })
      .map(
        (model: ModelResponse) =>
          new Model({
            model: model.name.replace(/:latest$/, ""),
            digest: model.digest,
            modified_at: model.modified_at
              ? new Date(model.modified_at)
              : undefined,
          }),
      );
  } catch (err) {
    throw new Error(`Failed to fetch Ollama models: ${err}`);
  }
}

export async function getModelCapabilities(
  modelName: string,
): Promise<ModelCapabilitiesResponse> {
  try {
    const showResponse = await ollama.show({ model: modelName });

    return new ModelCapabilitiesResponse({
      capabilities: Array.isArray(showResponse.capabilities)
        ? showResponse.capabilities
        : [],
    });
  } catch (error) {
    // Model might not be downloaded yet, return empty capabilities
    console.error(`Failed to get capabilities for ${modelName}:`, error);
    return new ModelCapabilitiesResponse({ capabilities: [] });
  }
}

export type ChatEventUnion = ChatEvent | DownloadEvent | ErrorEvent;

export async function* sendMessage(
  chatId: string,
  message: string,
  model: Model,
  attachments?: Array<{ filename: string; data: Uint8Array }>,
  signal?: AbortSignal,
  index?: number,
  webSearch?: boolean,
  fileTools?: boolean,
  forceUpdate?: boolean,
  think?: boolean | string,
  uxEstimatedContextTokens?: number,
): AsyncGenerator<ChatEventUnion> {
  // Convert Uint8Array to base64 for JSON serialization
  const serializedAttachments = attachments?.map((att) => ({
    filename: att.filename,
    data: uint8ArrayToBase64(att.data),
  }));

  // Send think parameter when it's explicitly set (true, false, or a non-empty string).
  const shouldSendThink =
    think !== undefined &&
    (typeof think === "boolean" || (typeof think === "string" && think !== ""));

  const response = await fetch(`${API_BASE}/api/v1/chat/${chatId}`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify(
      new ChatRequest({
        model: model.model,
        prompt: message,
        ...(index !== undefined ? { index } : {}),
        ...(serializedAttachments !== undefined
          ? { attachments: serializedAttachments }
          : {}),
        // Always send web_search as a boolean value (default to false)
        web_search: webSearch ?? false,
        file_tools: fileTools ?? false,
        ...(forceUpdate !== undefined ? { forceUpdate } : {}),
        ...(shouldSendThink ? { think } : {}),
        ...(uxEstimatedContextTokens !== undefined
          ? { ux_estimated_context_tokens: uxEstimatedContextTokens }
          : {}),
      }),
    ),
    signal,
  });

  for await (const event of parseJsonlFromResponse<ChatEventUnion>(response)) {
    switch (event.eventName) {
      case "download":
        yield new DownloadEvent(event);
        break;
      case "error":
        yield new ErrorEvent(event);
        break;
      default:
        yield new ChatEvent(event);
        break;
    }
  }
}

export async function getSettings(): Promise<{
  settings: Settings;
}> {
  const response = await fetch(`${API_BASE}/api/v1/settings`);
  if (!response.ok) {
    throw new Error("Failed to fetch settings");
  }
  const data = await response.json();
  return {
    settings: new Settings(data.settings),
  };
}

export async function updateSettings(settings: Settings): Promise<{
  settings: Settings;
}> {
  const response = await fetch(`${API_BASE}/api/v1/settings`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify(settings),
  });
  if (!response.ok) {
    const error = await response.text();
    throw new Error(error || "Failed to update settings");
  }
  const data = await response.json();
  return {
    settings: new Settings(data.settings),
  };
}

export async function getPlugins(): Promise<PluginsResponse> {
  const response = await fetch(`${API_BASE}/api/v1/plugins`);
  if (!response.ok) {
    const error = await response.json().catch(() => null);
    throw new Error(error?.error || "Failed to fetch plugins");
  }
  return response.json();
}

export async function importPlugins(config: string): Promise<PluginsResponse> {
  const response = await fetch(`${API_BASE}/api/v1/plugins/import`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ config }),
  });
  if (!response.ok) {
    const error = await response.json().catch(() => null);
    throw new Error(error?.error || "Failed to import plugins");
  }
  return response.json();
}

export async function updatePluginEnabled(
  id: string,
  enabled: boolean,
): Promise<PluginsResponse> {
  const response = await fetch(`${API_BASE}/api/v1/plugins/${id}`, {
    method: "PUT",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ enabled }),
  });
  if (!response.ok) {
    const error = await response.json().catch(() => null);
    throw new Error(error?.error || "Failed to update plugin");
  }
  return response.json();
}

export async function updatePluginToolEnabled(
  id: string,
  toolName: string,
  enabled: boolean,
): Promise<PluginsResponse> {
  const response = await fetch(
    `${API_BASE}/api/v1/plugins/${encodeURIComponent(id)}/tools/${encodeURIComponent(toolName)}`,
    {
      method: "PUT",
      headers: {
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ enabled }),
    },
  );
  if (!response.ok) {
    const error = await response.json().catch(() => null);
    throw new Error(error?.error || "Failed to update plugin tool");
  }
  return response.json();
}

export async function deletePlugin(id: string): Promise<void> {
  const response = await fetch(`${API_BASE}/api/v1/plugins/${id}`, {
    method: "DELETE",
  });
  if (!response.ok) {
    const error = await response.json().catch(() => null);
    throw new Error(error?.error || "Failed to delete plugin");
  }
}

export async function importModelFiles(
  name: string,
  paths: string[],
): Promise<{ model: string }> {
  const response = await fetch(`${API_BASE}/api/v1/models/link`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ name, paths }),
  });
  if (!response.ok) {
    const error = await response.json().catch(() => null);
    throw new Error(error?.error || "Failed to link model");
  }
  return response.json();
}

export async function updateCloudSetting(
  enabled: boolean,
): Promise<CloudStatusResponse> {
  const response = await fetch(`${API_BASE}/api/v1/cloud`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ enabled }),
  });
  if (!response.ok) {
    const error = await response.text();
    throw new Error(error || "Failed to update cloud setting");
  }

  const data = await response.json();
  return {
    disabled: Boolean(data.disabled),
    source: (data.source as CloudStatusSource) || "none",
  };
}

export async function renameChat(chatId: string, title: string): Promise<void> {
  const response = await fetch(`${API_BASE}/api/v1/chat/${chatId}/rename`, {
    method: "PUT",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ title: title.trim() }),
  });
  if (!response.ok) {
    const error = await response.text();
    throw new Error(error || "Failed to rename chat");
  }
}

export async function deleteChat(chatId: string): Promise<void> {
  const response = await fetch(`${API_BASE}/api/v1/chat/${chatId}`, {
    method: "DELETE",
  });
  if (!response.ok) {
    const error = await response.text();
    throw new Error(error || "Failed to delete chat");
  }
}

// Get upstream information for model staleness checking
export async function getModelUpstreamInfo(
  model: Model,
): Promise<{ stale: boolean; exists: boolean; error?: string }> {
  try {
    const response = await fetch(`${API_BASE}/api/v1/model/upstream`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
      },
      body: JSON.stringify({
        model: model.model,
      }),
    });

    if (!response.ok) {
      console.warn(
        `Failed to check upstream for ${model.model}: ${response.status}`,
      );
      return { stale: false, exists: false };
    }

    const data = await response.json();

    if (data.error) {
      console.warn(`Upstream check: ${data.error}`);
      return { stale: false, exists: false, error: data.error };
    }

    return { stale: !!data.stale, exists: true };
  } catch (error) {
    console.warn(`Error checking model staleness:`, error);
    return { stale: false, exists: false };
  }
}

export async function* pullModel(
  modelName: string,
  signal?: AbortSignal,
): AsyncGenerator<{
  status: string;
  digest?: string;
  total?: number;
  completed?: number;
  done?: boolean;
}> {
  const response = await fetch(`${API_BASE}/api/v1/models/pull`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify({ name: modelName }),
    signal,
  });

  if (!response.ok) {
    throw new Error(`Failed to pull model: ${response.statusText}`);
  }

  for await (const event of parseJsonlFromResponse<{
    status: string;
    digest?: string;
    total?: number;
    completed?: number;
    done?: boolean;
  }>(response)) {
    yield event;
  }
}

export interface ModelRecommendation {
  model: string;
  description: string;
  context_length?: number;
  max_output_tokens?: number;
  vram_bytes?: number;
}

export interface ModelRecommendationsResponse {
  recommendations: ModelRecommendation[];
}

export async function getModelRecommendations(): Promise<
  ModelRecommendation[]
> {
  const response = await fetch(
    `${API_BASE}/api/experimental/model-recommendations`,
  );
  if (!response.ok) {
    throw new Error(
      `Failed to fetch model recommendations: ${response.statusText}`,
    );
  }
  const data: ModelRecommendationsResponse = await response.json();
  return data.recommendations || [];
}

export async function getInferenceCompute(): Promise<InferenceComputeResponse> {
  const response = await fetch(`${API_BASE}/api/v1/inference-compute`);
  if (!response.ok) {
    throw new Error(
      `Failed to fetch inference compute: ${response.statusText}`,
    );
  }

  const data = await response.json();
  return new InferenceComputeResponse(data);
}

export interface GPUPlacement {
  name: string;
  vram: number;
}

export interface RunningModel {
  name: string;
  model: string;
  size: number;
  size_vram: number;
  context_length: number;
  gpu_layers?: number;
  total_layers?: number;
  placement?: GPUPlacement[];
}

export interface RunningModelsResponse {
  models: RunningModel[];
}

export async function getRunningModels(): Promise<RunningModelsResponse> {
  const response = await fetch(`${API_BASE}/api/ps`);
  if (!response.ok) {
    throw new Error(`Failed to fetch running models: ${response.statusText}`);
  }

  return response.json();
}

export async function fetchHealth(): Promise<boolean> {
  try {
    // Use the /api/version endpoint as a health check
    const response = await fetch(`${API_BASE}/api/version`, {
      method: "GET",
      headers: {
        "Content-Type": "application/json",
      },
    });

    if (response.ok) {
      const data = await response.json();
      // If we get a version back, the server is healthy
      return !!data.version;
    }

    return false;
  } catch (error) {
    console.error("Error checking health:", error);
    return false;
  }
}

export async function getCloudStatus(): Promise<CloudStatusResponse | null> {
  const response = await fetch(`${API_BASE}/api/v1/cloud`);
  if (!response.ok) {
    throw new Error(`Failed to fetch cloud status: ${response.status}`);
  }

  const data = await response.json();
  return {
    disabled: Boolean(data.disabled),
    source: (data.source as CloudStatusSource) || "none",
  };
}
