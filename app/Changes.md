OPEN OLLAMA - CHANGES

2026-10-07

Future improvements - local-first model acquisition
- Study a model discovery and download experience similar to LM Studio, focused
  on browsing GGUF repositories, quantizations, file sizes and companion
  projectors such as mmproj.
- Download selected files into the configured user model library first; that
  library remains the physical source of truth rather than an opaque cache.
- After download, adapt the GGUF into Ollama's native catalog automatically by
  creating the required blob references, manifest and metadata.
- Reuse the existing same-volume hardlink flow where possible so cataloguing a
  downloaded GGUF does not duplicate the model bytes.
- Treat model discovery/download, local cataloguing and runtime loading as
  separate stages with visible errors and resumable state.
- Keep Ollama Cloud Models as an optional external inference provider, not as a
  prerequisite for local model discovery or installation.

Future improvements - composer capabilities
- Keep native vision and web access as explicit composer capabilities.
- Separate Web Access from the Ollama Cloud Models setting; a local model should
  be able to use a configured web provider without enabling cloud inference.
- Keep the Ollama-hosted web search provider optional behind account
  authentication, while allowing future local-first or user-configured search
  providers.

2026-08-22

Plugins
- Added Plugins section to sidebar.
- Added /plugins route.
- Plugins are kept separate from Apps and Settings.
- Planned master Plugins toggle in Settings.
- Plugin configuration will use direct JSON input.
- MCP will be the first supported plugin provider.
- MCP is not treated as synonymous with Plugin.

Tools
- Existing persistent ToolRegistry was identified in cmd/app/app.go.
- Chat currently creates a separate per-request Registry.
- Persistent ToolRegistry is not yet connected to chat execution.
- Registry integration intentionally postponed until plugin configuration
  and persistence are working.

Notes
- Preserve native Ollama tool flow.
- Avoid parallel tool execution architecture.
- Keep implementation simple and open-source friendly.
  
  Ollama Desktop Fork - Current Checkpoint
2026-08-23

Changes

- Added Back navigation to Settings.
- Added Plugins section to the sidebar and Settings.
- Added persistent plugin configuration management.
- Fixed Settings cache/persistence issues affecting saved toggles.
- Added Local Models toggle to Settings.
- Local Models toggle persists between navigation and app restarts.
- Local Models controls visibility of local models in the Chat model selector.
- Added CpuChip icon for Local Models.
- Added System, Light and Dark appearance controls to Settings.
- Cloud and Local Models remain independent controls.
- Model location remains configurable and persistent.
- Local Ollama inference verified successfully with a downloaded Gemma model.
- ROCm GPU detection confirmed on the local runtime.

Current limitations / next work

- Existing external GGUF files are not yet imported automatically.
- Import GGUF button is not implemented yet.
- MCP plugin configurations can be stored, but MCP tool publication/runtime integration is still under investigation.
- Local GGUF import will reuse Ollama's native blob/create pipeline rather than introduce a separate loader.

2026-09-09

Current Fork Checkpoint

Plugins and MCP
- Plugins are controlled by a master setting in Settings.
- When Plugins are disabled, the Plugins sidebar entry is hidden and plugin APIs are rejected.
- The Plugins page shows saved MCP plugin configurations, runtime status, discovered tools, tool count and connection errors.
- MCP plugin configuration remains raw JSON-based and is stored separately from runtime state.
- MCP is treated as the first plugin provider, not as the whole plugin architecture.
- A single in-memory MCP manager hydrates saved MCP plugins at startup and re-syncs after plugin import, update, deletion and tool preference changes.
- MCP stdio lifecycle now follows initialize, initialized notification, tools/list, tool execution and shutdown.
- MCP server stdout is reserved for JSON-RPC and stderr is captured separately for diagnostics.
- Discovered MCP tools are adapted into the native app/tools.Tool interface.
- MCP tools are registered in the persistent ToolRegistry used by the native chat tool flow.
- Per-tool enable/disable preferences are persisted separately from the MCP JSON config.
- Disabled tools are removed from the ToolRegistry and are not published in chat requests.
- Tool identity is namespaced by plugin origin plus public tool name to avoid collisions.

Settings and Model Visibility
- Settings persistence was extended for PluginsEnabled, LocalModelsEnabled, CPUFallbackEnabled, InferenceEnabled and InferenceRuntime.
- Partial settings updates preserve newly added fields when older UI snapshots omit them.
- Local Models controls whether local non-cloud models appear in the Chat model selector.
- Cloud and Local Models remain independent controls.
- Reset to defaults includes the new settings fields.

Composer Context Indicator
- The Chat composer send button now has a compact context halo.
- The halo uses the send/stop button outline as the context usage indicator.
- Hover shows a delayed token usage hint in the cleaner Ollama tooltip style.
- The hint uses graphite background and white text.
- The halo has theme-aware contrast so it remains visible in dark and light themes.
- The indicator is visual only and does not change chat sending behavior.

Inference Controls
- Added an Inference section controlled from Settings and shown in the sidebar when enabled.
- Added CPU/RAM fallback preference for local model availability decisions.
- Added a persisted runtime selector for local inference.
- Runtime options currently include Auto, CUDA Legacy (experimental), CUDA 12, CUDA 13, ROCm, Vulkan and CPU.
- Changing context length or runtime restarts the local Ollama server.
- The selected runtime is passed to the backend through OLLAMA_LLM_LIBRARY.

CUDA Legacy Runtime
- Added CUDA Legacy (experimental) as a compatibility runtime for older NVIDIA GPUs.
- The current local dev payload includes app/dist/windows-amd64/lib/ollama/cuda_legacy.
- The local CUDA Legacy backend was built with CUDA Toolkit 12.6 for CMAKE_CUDA_ARCHITECTURES=61;75.
- Direct llama-server device discovery using cuda_legacy detected both CUDA0 RTX 2060 SUPER and CUDA1 GTX 1050 Ti.
- This is intended to avoid relying on newer PTX that older drivers or Pascal-era GPUs may reject.
- A100 is Ampere, not Pascal, and should normally use a modern CUDA runtime rather than CUDA Legacy.

GGUF and Native Blob Experiments
- Confirmed that Ollama-managed model blobs can be GGUF files internally.
- Confirmed that llama-server receives and loads the blob path directly through the native flow.
- External GGUF files do not appear automatically just because they exist under Model location.
- The native recognition path depends on blobs plus manifests and descriptors, not a directory scan.
- A manual native-style manifest/blob experiment successfully made an existing GGUF appear through the normal model list path.
- This remains a lab technique only; no supported GUI importer has been added yet.

Validation Notes
- Frontend build passed with npm run build.
- Focused Go tests passed for app/store, app/server, app/ui and discover.
- Focused ESLint passed for the touched Settings, useSettings and Inference route files.
- Global npm run lint still reports pre-existing no-explicit-any errors in generated and older frontend files.
- Full model inference with CUDA Legacy has not yet been validated; only backend device discovery was validated.

Current limitations / next work
- CUDA Legacy is available as a dev runtime payload but production packaging has not been finalized.
- Auto runtime selection does not yet explicitly prefer CUDA Legacy when Pascal hardware is present.
- CPU fallback setting still needs real scheduler-placement validation with large models.
- GGUF import still needs a supported GUI flow around the native blob/create/manifest path.
- Inference tuning controls such as temperature, top-k and top-p have not been added yet.

2026-09-20

Link GGUF
- Added a Link GGUF button beside Model location in Settings.
- Link GGUF opens a native desktop file picker filtered to .gguf files.
- The frontend suggests a model name from the first selected filename and lets the user edit it inline before linking.
- The app backend validates the model name, validates selected files, computes SHA-256 digests and creates hardlinks at the native Ollama blob paths.
- No automatic copy fallback is used; if hardlink creation fails, the link operation fails and reports the error.
- The app backend writes a native Ollama manifest so /api/tags, the ModelPicker, scheduler and runner stay on the normal Ollama path.
- Fork metadata records model origin, source path for linked GGUFs and optional display aliases without changing model tags, manifests, digests or files.
- After linking, the frontend invalidates the model list cache so /api/tags can refresh the ModelPicker.

Model Manager
- Added a Manage button beside Model location in Settings.
- Manage lists installed models returned by the native Ollama model list with fork metadata merged in.
- Linked GGUF models can be renamed through a display alias or unlinked without deleting the original GGUF file.
- Ollama storage models can be renamed through a display alias or deleted through the native /api/delete flow.

2026-09-22

Inference Manual GPU Allocation
- The Inference page now exposes interactive per-device layer sliders when Automatic fit is disabled.
- Automatic fit keeps the sliders read-only as a live view of the placement reported by the runtime.
- CPU / Host remains locked to 0 layers while CPU fallback is disabled.
- Manual allocation is persisted as fork settings metadata and restarts the local Ollama server when committed.
- The desktop server converts the manual GPU layer intent into OLLAMA_NUM_GPU and OLLAMA_TENSOR_SPLIT.
- The Ollama server maps OLLAMA_NUM_GPU to the default runner num_gpu value and OLLAMA_TENSOR_SPLIT to llama-server --tensor-split.
- Explicit tensor split prevents the scheduler from collapsing placement to a single best-fit GPU.
- The layer count shown by the UI is model-defined: llama.cpp reports block_count + 1 offloadable layers, so the value changes by model, not by hardware.
- The user manages models by name and origin; blob SHA paths are not exposed as manual operations.
- After unlink or delete, model caches are invalidated and the selected model is cleared when it points to the removed model.

Current limitations / next work
- Link progress is currently a simple "Linking..." state, not a byte-level progress bar.
- Link GGUF uses a compact inline confirmation panel; a polished modal can replace it later.
- Template/parser overrides are not exposed in the GUI yet.
- Real multimodal model behavior with mmproj-style auxiliary files still needs runtime validation per model family.

Memory Architecture Blueprint
- Documented the proposed fork memory architecture in docs/fork-memory-architecture.md.
- The design separates INDEX, HOW TO, LEARNINGS, EVIDENCE and CANONICAL history.
- The goal is operational continuity without loading full raw history into every context window.
- Current implemented pieces include canonical chat storage, non-destructive context compaction checkpoints and passive context trace logging.
- Missing V1 pieces include a searchable memory index, reusable HOW TO records, structured LEARNINGS, evidence pointers, retrieval policy and invalidation rules.
- The document also records that Ollama has request/model/template-level system handling, not one universal hidden global system prompt for all chat requests.
