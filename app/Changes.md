OPEN OLLAMA - CHANGES

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
- Cloud and Local Models remain independent controls.
- Model location remains configurable and persistent.
- Local Ollama inference verified successfully with a downloaded Gemma model.
- ROCm GPU detection confirmed on the local runtime.

Current limitations / next work

- Existing external GGUF files are not yet imported automatically.
- Import GGUF button is not implemented yet.
- MCP plugin configurations can be stored, but MCP tool publication/runtime integration is still under investigation.
- Local GGUF import will reuse Ollama's native blob/create pipeline rather than introduce a separate loader.