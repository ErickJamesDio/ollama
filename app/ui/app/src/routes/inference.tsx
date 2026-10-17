import { useEffect, useState } from "react";
import { createFileRoute, useRouter } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { CpuChipIcon, CogIcon } from "@heroicons/react/24/outline";
import { AppSidebar } from "@/components/AppSidebar";
import { SidebarLayout } from "@/components/layout/layout";
import { Button } from "@/components/ui/button";
import { Badge } from "@/components/ui/badge";
import { Slider } from "@/components/ui/slider";
import { Switch } from "@/components/ui/switch";
import { Field, Label, Description } from "@/components/ui/fieldset";
import { Text } from "@/components/ui/text";
import { getInferenceCompute, getRunningModels, type RunningModel } from "@/api";
import { useSettings } from "@/hooks/useSettings";

const runtimeOptions = [
  {
    value: "auto",
    label: "Auto",
    description: "Choose the best available runtime for this machine.",
  },
  {
    value: "cuda_legacy",
    label: "CUDA Legacy (experimental)",
    description: "Compatibility runtime for older NVIDIA GPUs.",
  },
  {
    value: "cuda_v12",
    label: "CUDA 12",
    description: "Modern NVIDIA GPUs and CUDA 12 drivers.",
  },
  {
    value: "cuda_v13",
    label: "CUDA 13",
    description: "Latest NVIDIA GPUs and CUDA 13 drivers.",
  },
  {
    value: "rocm_v7_2",
    label: "ROCm",
    description: "Supported AMD GPUs.",
  },
  {
    value: "vulkan",
    label: "Vulkan",
    description: "Cross-vendor GPU backend, useful as a fallback.",
  },
  {
    value: "cpu",
    label: "CPU",
    description: "Run without GPU acceleration.",
  },
];

export const Route = createFileRoute("/inference")({
  component: InferenceRoute,
});

function InferenceRoute() {
  const router = useRouter();
  const { settings, setSettings } = useSettings();
  const [showSaved, setShowSaved] = useState(false);
  const [restartMessage, setRestartMessage] = useState(false);
  const [manualLayers, setManualLayers] = useState<Record<string, number>>({});

  const { data: inferenceComputeResponse } = useQuery({
    queryKey: ["inferenceCompute"],
    queryFn: getInferenceCompute,
    enabled: settings.inferenceEnabled,
  });
  const { data: runningModelsResponse } = useQuery({
    queryKey: ["runningModels"],
    queryFn: getRunningModels,
    enabled: settings.inferenceEnabled,
    refetchInterval: 3000,
  });

  const defaultContextLength = inferenceComputeResponse?.defaultContextLength;
  const loadedModel = runningModelsResponse?.models?.[0];
  const savedManualPlan = parseManualLayerPlan(settings.inferenceManualLayers);
  const placement = buildPlacementRows(
    inferenceComputeResponse?.inferenceComputes ?? [],
    loadedModel,
    settings.cpuFallbackEnabled,
    savedManualPlan,
  );
  const observedPlacementKey = `${placement.totalLayers}:${placement.rows
    .map((row) => `${row.name}:${row.layers}`)
    .join("|")}`;
  const displayedRows = settings.inferenceAutoFitEnabled
    ? placement.rows
    : placement.rows.map((row) => ({
        ...row,
        layers:
          row.kind === "host" && !settings.cpuFallbackEnabled
            ? 0
            : (manualLayers[row.name] ?? row.layers),
      }));
  const allocatedLayers = displayedRows.reduce((sum, row) => sum + row.layers, 0);
  const canEditManualLayers = placement.totalLayers > 0;
  const selectedRuntime =
    runtimeOptions.find((option) => option.value === settings.inferenceRuntime) ??
    runtimeOptions[0];

  useEffect(() => {
    const savedLayers = savedManualPlan?.layers ?? null;
    const observedLayers = Object.fromEntries(
      placement.rows.map((row) => [
        row.name,
        row.kind === "host" && !settings.cpuFallbackEnabled ? 0 : row.layers,
      ]),
    );

    if (settings.inferenceAutoFitEnabled) {
      setManualLayers(savedLayers ?? observedLayers);
      return;
    }

    setManualLayers((current) => {
      const hasAnyCurrentValue = placement.rows.some(
        (row) => current[row.name] !== undefined,
      );

      if (!hasAnyCurrentValue) {
        return savedLayers ?? observedLayers;
      }

      if (
        !settings.cpuFallbackEnabled &&
        current["CPU / Host"] !== undefined &&
        current["CPU / Host"] !== 0
      ) {
        return { ...current, "CPU / Host": 0 };
      }

      return current;
    });
  }, [
    observedPlacementKey,
    settings.cpuFallbackEnabled,
    settings.inferenceAutoFitEnabled,
    settings.inferenceManualLayers,
  ]);

  const persistSetting = async (
    updates: Parameters<typeof setSettings>[0],
    options?: {
      showRestart?: boolean;
      changed?: boolean;
    },
  ) => {
    await setSettings(updates);

    if (options?.showRestart && options.changed) {
      setRestartMessage(true);
      setTimeout(() => setRestartMessage(false), 3000);
    }

    setShowSaved(true);
    setTimeout(() => setShowSaved(false), 1500);
  };

  return (
    <SidebarLayout
      title="Inference"
      sidebar={<AppSidebar current="inference" />}
    >
      <div className="flex h-full flex-col overflow-y-auto px-6 py-4">
        <div className="mx-auto flex w-full max-w-4xl flex-1 flex-col gap-4">
          <Button
            outline
            className="self-start"
            onClick={() => router.history.back()}
          >
            Back
          </Button>

          {!settings.inferenceEnabled ? (
            <div className="rounded-xl bg-white p-4 dark:bg-neutral-800">
              <Text className="text-sm text-neutral-600 dark:text-neutral-300">
                Inference is off. Enable it in Settings to show this section and
                manage advanced local model controls.
              </Text>
            </div>
          ) : (
            <div className="relative overflow-hidden rounded-xl bg-white dark:bg-neutral-800">
              <div className="space-y-4 p-4">
                <Field>
                  <div className="flex items-start space-x-3">
                    <CpuChipIcon className="mt-1 h-5 w-5 flex-shrink-0 text-black dark:text-neutral-100" />
                    <div className="w-full">
                      <Label>Runtime</Label>
                      <Description>{selectedRuntime.description}</Description>
                      <select
                        value={settings.inferenceRuntime}
                        onChange={(event) => {
                          void persistSetting(
                            { InferenceRuntime: event.target.value },
                            {
                              showRestart: true,
                              changed:
                                event.target.value !==
                                settings.inferenceRuntime,
                            },
                          );
                        }}
                        className="mt-3 w-full rounded-lg border border-neutral-300 bg-white px-3 py-2 text-sm text-neutral-900 outline-none focus:border-blue-500 dark:border-neutral-600 dark:bg-neutral-700 dark:text-neutral-100"
                      >
                        {runtimeOptions.map((option) => (
                          <option key={option.value} value={option.value}>
                            {option.label}
                          </option>
                        ))}
                      </select>
                    </div>
                  </div>
                </Field>

                <Field>
                  <div className="flex items-start space-x-3">
                    <CogIcon className="mt-1 h-5 w-5 flex-shrink-0 text-black dark:text-neutral-100" />
                    <div className="w-full">
                      <Label>Context length</Label>
                      <Description>
                        Context length determines how much of your conversation
                        local LLMs can remember and use to generate responses.
                      </Description>
                      <div className="mt-3">
                        <Slider
                          value={
                            settings.contextLength || defaultContextLength || 0
                          }
                          onChange={(value) => {
                            void persistSetting(
                              { ContextLength: value },
                              {
                                showRestart: true,
                                changed: value !== settings.contextLength,
                              },
                            );
                          }}
                          disabled={!defaultContextLength}
                          options={[
                            { value: 4096, label: "4k" },
                            { value: 8192, label: "8k" },
                            { value: 16384, label: "16k" },
                            { value: 32768, label: "32k" },
                            { value: 65536, label: "64k" },
                            { value: 131072, label: "128k" },
                            { value: 262144, label: "256k" },
                          ]}
                        />
                      </div>
                    </div>
                  </div>
                </Field>

                <Field>
                  <div className="flex items-start justify-between gap-4">
                    <div className="flex items-start space-x-3 flex-1">
                      <CpuChipIcon className="mt-1 h-5 w-5 flex-shrink-0 text-black dark:text-neutral-100" />
                      <div>
                        <Label>CPU fallback</Label>
                        <Description>
                          Allow larger local models to remain eligible when VRAM
                          is limited but CPU and RAM can help carry the load.
                        </Description>
                      </div>
                    </div>
                    <div className="flex-shrink-0">
                      <Switch
                        checked={settings.cpuFallbackEnabled}
                        onChange={(checked) => {
                          void persistSetting({ CPUFallbackEnabled: checked });
                        }}
                      />
                    </div>
                  </div>
                </Field>

                <div className="border-t border-neutral-200 pt-4 dark:border-neutral-700">
                  <div className="mb-4">
                    <div className="text-base/6 font-medium text-zinc-950 sm:text-sm/6 dark:text-white">
                      GPU allocation
                    </div>
                    <p className="mt-1 text-base/6 text-zinc-500 sm:text-sm/6 dark:text-zinc-400">
                      Sliders show the layer placement currently reported by the
                      runtime. Disable automatic fit to make future placement
                      explicit instead of silently falling back.
                    </p>
                  </div>

                  <Field>
                    <div className="flex items-start justify-between gap-4">
                      <div>
                        <Label>Automatic fit</Label>
                        <Description>
                          Let llama.cpp automatically determine layer placement.
                        </Description>
                      </div>
                      <Switch
                        checked={settings.inferenceAutoFitEnabled}
                        onChange={(checked) => {
                          void persistSetting(
                            { InferenceAutoFitEnabled: checked },
                            {
                              showRestart: true,
                              changed:
                                checked !== settings.inferenceAutoFitEnabled,
                            },
                          );
                        }}
                      />
                    </div>
                  </Field>

                  <div className="mt-4 space-y-4">
                    {displayedRows.map((row) => (
                      <LayerAllocationSlider
                        key={row.name}
                        label={row.label}
                        value={row.layers}
                        max={placement.totalLayers}
                        disabled={
                          settings.inferenceAutoFitEnabled ||
                          (row.kind === "host" && !settings.cpuFallbackEnabled)
                        }
                        reason={
                          settings.inferenceAutoFitEnabled
                            ? "Controlled automatically"
                            : row.kind === "host" &&
                                !settings.cpuFallbackEnabled
                              ? "CPU fallback is off"
                              : !loadedModel && savedManualPlan
                                ? "Using saved manual plan"
                              : undefined
                        }
                        onChange={(layers) => {
                          if (
                            settings.inferenceAutoFitEnabled ||
                            !canEditManualLayers ||
                            (row.kind === "host" && !settings.cpuFallbackEnabled)
                          ) {
                            return;
                          }

                          setManualLayers((current) => ({
                            ...current,
                            [row.name]: layers,
                          }));
                        }}
                        onCommit={(layers) => {
                          if (
                            settings.inferenceAutoFitEnabled ||
                            !canEditManualLayers ||
                            (row.kind === "host" && !settings.cpuFallbackEnabled)
                          ) {
                            return;
                          }

                          const nextRows = displayedRows.map((candidate) => ({
                            ...candidate,
                            layers:
                              candidate.name === row.name
                                ? layers
                                : candidate.layers,
                          }));

                          void persistSetting(
                            {
                              InferenceManualLayers:
                                serializeManualLayers(nextRows, placement.totalLayers),
                            },
                            {
                              showRestart: true,
                              changed:
                                serializeManualLayers(nextRows, placement.totalLayers) !==
                                settings.inferenceManualLayers,
                            },
                          );
                        }}
                      />
                    ))}
                  </div>

                  <div className="mt-4 flex items-center justify-between text-sm">
                    <span className="text-neutral-500 dark:text-neutral-400">
                      {loadedModel
                        ? `Loaded model: ${loadedModel.name || loadedModel.model}`
                        : "No loaded model yet"}
                    </span>
                    <span
                      className={
                        allocatedLayers === placement.totalLayers
                          ? "text-neutral-700 dark:text-neutral-200"
                          : "text-red-600 dark:text-red-400"
                      }
                    >
                      Allocated {allocatedLayers} / {placement.totalLayers}
                    </span>
                  </div>
                </div>
              </div>
            </div>
          )}
        </div>

        {(showSaved || restartMessage) && (
          <div className="fixed bottom-4 left-1/2 z-50 -translate-x-1/2 transform transition-opacity duration-300">
            <Badge
              color="green"
              className="!bg-green-500 !text-white dark:!bg-green-600"
            >
              Saved
            </Badge>
          </div>
        )}
      </div>
    </SidebarLayout>
  );
}

type PlacementRow = {
  kind: "gpu" | "host";
  name: string;
  label: string;
  layers: number;
};

type ManualLayerPlan = {
  layers: Record<string, number>;
  totalLayers: number;
};

function parseManualLayerPlan(raw: string): ManualLayerPlan | null {
  if (!raw.trim()) {
    return null;
  }

  try {
    const parsed = JSON.parse(raw) as
      | Partial<PlacementRow>[]
      | { totalLayers?: number; rows?: Partial<PlacementRow>[] };
    const rows = Array.isArray(parsed) ? parsed : parsed.rows;
    if (!Array.isArray(rows)) {
      return null;
    }

    const layers = Object.fromEntries(
      rows
        .filter((row) => typeof row.name === "string")
        .map((row) => [row.name as string, Math.max(0, Number(row.layers) || 0)]),
    );
    const rowTotal = Object.values(layers).reduce((sum, value) => sum + value, 0);
    const totalLayers = Math.max(
      0,
      Math.round(
        Array.isArray(parsed) ? rowTotal : Number(parsed.totalLayers) || rowTotal,
      ),
    );

    return { layers, totalLayers };
  } catch {
    return null;
  }
}

function serializeManualLayers(rows: PlacementRow[], totalLayers: number): string {
  return JSON.stringify({
    totalLayers: Math.max(0, Math.round(totalLayers)),
    rows: rows.map((row) => ({
      kind: row.kind,
      name: row.name,
      label: row.label,
      layers: Math.max(0, Math.round(row.layers)),
    })),
  });
}

function buildPlacementRows(
  computes: { name: string; description?: string }[],
  model: RunningModel | undefined,
  cpuFallbackEnabled: boolean,
  savedPlan: ManualLayerPlan | null,
): { rows: PlacementRow[]; allocated: number; totalLayers: number } {
  const totalLayers = Math.max(
    0,
    model?.total_layers ?? savedPlan?.totalLayers ?? 0,
  );
  const gpuLayers = Math.max(0, model?.gpu_layers ?? 0);
  const devices = computes.map((compute) => compute.name);
  const vramByDevice = new Map(
    (model?.placement ?? []).map((device) => [device.name, device.vram]),
  );
  const activeDevices = devices.filter((name) => (vramByDevice.get(name) ?? 0) > 0);
  const totalActiveVRAM = activeDevices.reduce(
    (sum, name) => sum + (vramByDevice.get(name) ?? 0),
    0,
  );

  let assignedGPU = 0;
  const rows: PlacementRow[] = computes.map((compute, index) => {
    const name = compute.name;
    const description = compute.description?.trim();
    let layers = Math.max(0, savedPlan?.layers[name] ?? 0);
    if (gpuLayers > 0 && activeDevices.includes(name)) {
      if (activeDevices.length === 1 || totalActiveVRAM <= 0) {
        layers = gpuLayers;
      } else {
        const raw = (gpuLayers * (vramByDevice.get(name) ?? 0)) / totalActiveVRAM;
        layers = index === devices.length - 1 ? gpuLayers - assignedGPU : Math.round(raw);
      }
    }
    assignedGPU += layers;
    return {
      kind: "gpu" as const,
      name,
      label: description ? `${name} - ${description}` : name,
      layers,
    };
  });

  const hostLayers = cpuFallbackEnabled
    ? Math.max(
        0,
        savedPlan?.layers["CPU / Host"] ??
          totalLayers - Math.min(gpuLayers, totalLayers),
      )
    : 0;
  rows.push({ kind: "host", name: "CPU / Host", label: "CPU / Host", layers: hostLayers });

  return {
    rows,
    allocated: rows.reduce((sum, row) => sum + row.layers, 0),
    totalLayers,
  };
}

function LayerAllocationSlider({
  label,
  value,
  max,
  disabled,
  reason,
  onChange,
  onCommit,
}: {
  label: string;
  value: number;
  max: number;
  disabled: boolean;
  reason?: string;
  onChange?: (value: number) => void;
  onCommit?: (value: number) => void;
}) {
  const pct = max > 0 ? Math.min(100, Math.max(0, (value / max) * 100)) : 0;

  return (
    <div className={disabled ? "opacity-50" : ""}>
      <div className="mb-2 flex items-center justify-between gap-3">
        <div>
          <div className="text-sm font-medium text-neutral-900 dark:text-neutral-100">
            {label}
          </div>
          {reason && (
            <div className="text-xs text-neutral-500 dark:text-neutral-400">
              {reason}
            </div>
          )}
        </div>
        <div className="text-sm text-neutral-700 dark:text-neutral-200">
          {value} layers
        </div>
      </div>
      <div className="relative h-5">
        <div className="absolute top-2 left-0 right-0 h-1 rounded-full bg-neutral-200 dark:bg-neutral-700" />
        <div
          className="absolute top-2 left-0 h-1 rounded-full bg-neutral-500 dark:bg-neutral-300"
          style={{ width: `${pct}%` }}
        />
        <div
          className="absolute top-0 h-5 w-5 -translate-x-1/2 rounded-full border border-neutral-400 bg-white dark:border-neutral-500 dark:bg-neutral-100"
          style={{ left: `${pct}%` }}
        />
        <input
          type="range"
          min={0}
          max={max}
          step={1}
          value={value}
          disabled={disabled || max <= 0}
          onChange={(event) => onChange?.(Number(event.target.value))}
          onPointerUp={(event) =>
            onCommit?.(Number((event.currentTarget as HTMLInputElement).value))
          }
          onKeyUp={(event) => {
            if (
              event.key === "ArrowLeft" ||
              event.key === "ArrowRight" ||
              event.key === "Home" ||
              event.key === "End"
            ) {
              onCommit?.(Number(event.currentTarget.value));
            }
          }}
          className={`absolute inset-x-0 top-0 h-5 w-full opacity-0 ${
            disabled || max <= 0 ? "cursor-not-allowed" : "cursor-pointer"
          }`}
        />
      </div>
    </div>
  );
}
