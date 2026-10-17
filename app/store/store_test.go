//go:build windows || darwin

package store

import (
	"path/filepath"
	"testing"
)

func TestStore(t *testing.T) {
	s, cleanup := setupTestStore(t)
	defer cleanup()

	t.Run("default id", func(t *testing.T) {
		// ID should be automatically generated
		id, err := s.ID()
		if err != nil {
			t.Fatal(err)
		}
		if id == "" {
			t.Error("expected non-empty ID")
		}

		// Verify ID is persisted
		id2, err := s.ID()
		if err != nil {
			t.Fatal(err)
		}
		if id != id2 {
			t.Errorf("expected ID %s, got %s", id, id2)
		}
	})

	t.Run("has completed first run", func(t *testing.T) {
		// Default should be false (hasn't completed first run yet)
		hasCompleted, err := s.HasCompletedFirstRun()
		if err != nil {
			t.Fatal(err)
		}
		if hasCompleted {
			t.Error("expected has completed first run to be false by default")
		}

		if err := s.SetHasCompletedFirstRun(true); err != nil {
			t.Fatal(err)
		}

		hasCompleted, err = s.HasCompletedFirstRun()
		if err != nil {
			t.Fatal(err)
		}
		if !hasCompleted {
			t.Error("expected has completed first run to be true")
		}
	})

	t.Run("settings", func(t *testing.T) {
		sc := Settings{
			Expose:     true,
			Browser:    true,
			Survey:     true,
			Models:     "/tmp/models",
			Agent:      true,
			Tools:      false,
			WorkingDir: "/tmp/work",
		}

		if err := s.SetSettings(sc); err != nil {
			t.Fatal(err)
		}

		loaded, err := s.Settings()
		if err != nil {
			t.Fatal(err)
		}
		// Compare fields individually since Models might get a default
		if loaded.Expose != sc.Expose || loaded.Browser != sc.Browser ||
			loaded.Agent != sc.Agent || loaded.Survey != sc.Survey ||
			loaded.Tools != sc.Tools || loaded.WorkingDir != sc.WorkingDir {
			t.Errorf("expected %v, got %v", sc, loaded)
		}
	})

	t.Run("settings default home view is chat", func(t *testing.T) {
		loaded, err := s.Settings()
		if err != nil {
			t.Fatal(err)
		}

		if loaded.LastHomeView != "chat" {
			t.Fatalf("expected default LastHomeView to be chat, got %q", loaded.LastHomeView)
		}
	})

	t.Run("settings empty home view falls back to chat", func(t *testing.T) {
		if err := s.SetSettings(Settings{LastHomeView: ""}); err != nil {
			t.Fatal(err)
		}

		loaded, err := s.Settings()
		if err != nil {
			t.Fatal(err)
		}

		if loaded.LastHomeView != "chat" {
			t.Fatalf("expected empty LastHomeView to fall back to chat, got %q", loaded.LastHomeView)
		}
	})

	t.Run("settings retired home view falls back to chat", func(t *testing.T) {
		if err := s.SetSettings(Settings{LastHomeView: "claude-desktop"}); err != nil {
			t.Fatal(err)
		}

		loaded, err := s.Settings()
		if err != nil {
			t.Fatal(err)
		}

		if loaded.LastHomeView != "chat" {
			t.Fatalf("expected retired LastHomeView to fall back to chat, got %q", loaded.LastHomeView)
		}
	})

	t.Run("settings integration home view falls back to chat", func(t *testing.T) {
		if err := s.SetSettings(Settings{LastHomeView: "codex-app"}); err != nil {
			t.Fatal(err)
		}

		loaded, err := s.Settings()
		if err != nil {
			t.Fatal(err)
		}

		if loaded.LastHomeView != "chat" {
			t.Fatalf("expected integration LastHomeView to fall back to chat, got %q", loaded.LastHomeView)
		}
	})

	t.Run("window size", func(t *testing.T) {
		if err := s.SetWindowSize(1024, 768); err != nil {
			t.Fatal(err)
		}

		width, height, err := s.WindowSize()
		if err != nil {
			t.Fatal(err)
		}
		if width != 1024 || height != 768 {
			t.Errorf("expected 1024x768, got %dx%d", width, height)
		}
	})

	t.Run("create and retrieve chat", func(t *testing.T) {
		chat := NewChat("test-chat-1")
		chat.Title = "Test Chat"

		chat.Messages = append(chat.Messages, NewMessage("user", "Hello", nil))
		chat.Messages = append(chat.Messages, NewMessage("assistant", "Hi there!", &MessageOptions{
			Model: "llama4",
		}))

		if err := s.SetChat(*chat); err != nil {
			t.Fatalf("failed to save chat: %v", err)
		}

		retrieved, err := s.Chat("test-chat-1")
		if err != nil {
			t.Fatalf("failed to retrieve chat: %v", err)
		}

		if retrieved.ID != chat.ID {
			t.Errorf("expected ID %s, got %s", chat.ID, retrieved.ID)
		}
		if retrieved.Title != chat.Title {
			t.Errorf("expected title %s, got %s", chat.Title, retrieved.Title)
		}
		if len(retrieved.Messages) != 2 {
			t.Fatalf("expected 2 messages, got %d", len(retrieved.Messages))
		}
		if retrieved.Messages[0].Content != "Hello" {
			t.Errorf("expected first message 'Hello', got %s", retrieved.Messages[0].Content)
		}
		if retrieved.Messages[1].Content != "Hi there!" {
			t.Errorf("expected second message 'Hi there!', got %s", retrieved.Messages[1].Content)
		}
	})

	t.Run("list chats", func(t *testing.T) {
		chat2 := NewChat("test-chat-2")
		chat2.Title = "Another Chat"
		chat2.Messages = append(chat2.Messages, NewMessage("user", "Test", nil))

		if err := s.SetChat(*chat2); err != nil {
			t.Fatalf("failed to save chat: %v", err)
		}

		chats, err := s.Chats()
		if err != nil {
			t.Fatalf("failed to list chats: %v", err)
		}

		if len(chats) != 2 {
			t.Fatalf("expected 2 chats, got %d", len(chats))
		}
	})

	t.Run("delete chat", func(t *testing.T) {
		if err := s.DeleteChat("test-chat-1"); err != nil {
			t.Fatalf("failed to delete chat: %v", err)
		}

		// Verify it's gone
		_, err := s.Chat("test-chat-1")
		if err == nil {
			t.Error("expected error retrieving deleted chat")
		}

		// Verify other chat still exists
		chats, err := s.Chats()
		if err != nil {
			t.Fatalf("failed to list chats: %v", err)
		}
		if len(chats) != 1 {
			t.Fatalf("expected 1 chat after deletion, got %d", len(chats))
		}
	})
}

func TestToolsSettingSurvivesStoreRestart(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "db.sqlite")

	first := &Store{DBPath: dbPath}
	settings, err := first.Settings()
	if err != nil {
		t.Fatalf("Settings() before restart: %v", err)
	}
	settings.Tools = true
	if err := first.SetSettings(settings); err != nil {
		t.Fatalf("SetSettings(): %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close() first store: %v", err)
	}

	second := &Store{DBPath: dbPath}
	defer second.Close()
	reloaded, err := second.Settings()
	if err != nil {
		t.Fatalf("Settings() after restart: %v", err)
	}
	if !reloaded.Tools {
		t.Fatal("Tools setting was not preserved across store restart")
	}
}

func TestOnboardingVersionRoundTrip(t *testing.T) {
	s, cleanup := setupTestStore(t)
	defer cleanup()

	settings, err := s.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.OnboardingVersion != 0 {
		t.Fatalf("expected onboarding version 0 by default, got %d", settings.OnboardingVersion)
	}

	settings.OnboardingVersion = 1
	if err := s.SetSettings(settings); err != nil {
		t.Fatal(err)
	}

	loaded, err := s.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.OnboardingVersion != 1 {
		t.Fatalf("expected onboarding version 1, got %d", loaded.OnboardingVersion)
	}
}

func TestClaudeDesktopUsedRoundTrip(t *testing.T) {
	s, cleanup := setupTestStore(t)
	defer cleanup()

	settings, err := s.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.ClaudeDesktopUsed {
		t.Fatal("expected Claude Desktop history to be false by default")
	}

	settings.ClaudeDesktopUsed = true
	if err := s.SetSettings(settings); err != nil {
		t.Fatal(err)
	}

	loaded, err := s.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.ClaudeDesktopUsed {
		t.Fatal("expected Claude Desktop history to persist")
	}
}

func TestPluginsEnabledRoundTrip(t *testing.T) {
	s, cleanup := setupTestStore(t)
	defer cleanup()

	settings, err := s.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.PluginsEnabled {
		t.Fatal("expected PluginsEnabled to be false by default")
	}

	settings.PluginsEnabled = true
	if err := s.SetSettings(settings); err != nil {
		t.Fatal(err)
	}

	loaded, err := s.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.PluginsEnabled {
		t.Fatal("expected PluginsEnabled to persist")
	}
}

func TestLocalModelsEnabledRoundTrip(t *testing.T) {
	s, cleanup := setupTestStore(t)
	defer cleanup()

	settings, err := s.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if !settings.LocalModelsEnabled {
		t.Fatal("expected LocalModelsEnabled to be true by default")
	}

	settings.LocalModelsEnabled = false
	if err := s.SetSettings(settings); err != nil {
		t.Fatal(err)
	}

	loaded, err := s.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.LocalModelsEnabled {
		t.Fatal("expected LocalModelsEnabled to persist as false")
	}
}

func TestCPUFallbackEnabledRoundTrip(t *testing.T) {
	s, cleanup := setupTestStore(t)
	defer cleanup()

	settings, err := s.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.CPUFallbackEnabled {
		t.Fatal("expected CPUFallbackEnabled to be false by default")
	}

	settings.CPUFallbackEnabled = true
	if err := s.SetSettings(settings); err != nil {
		t.Fatal(err)
	}

	loaded, err := s.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.CPUFallbackEnabled {
		t.Fatal("expected CPUFallbackEnabled to persist")
	}
}

func TestInferenceEnabledRoundTrip(t *testing.T) {
	s, cleanup := setupTestStore(t)
	defer cleanup()

	settings, err := s.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.InferenceEnabled {
		t.Fatal("expected InferenceEnabled to be false by default")
	}

	settings.InferenceEnabled = true
	if err := s.SetSettings(settings); err != nil {
		t.Fatal(err)
	}

	loaded, err := s.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.InferenceEnabled {
		t.Fatal("expected InferenceEnabled to persist")
	}
}

func TestInferenceRuntimeRoundTrip(t *testing.T) {
	s, cleanup := setupTestStore(t)
	defer cleanup()

	settings, err := s.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.InferenceRuntime != "auto" {
		t.Fatalf("expected InferenceRuntime to default to auto, got %q", settings.InferenceRuntime)
	}

	settings.InferenceRuntime = "cuda_legacy"
	if err := s.SetSettings(settings); err != nil {
		t.Fatal(err)
	}

	loaded, err := s.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.InferenceRuntime != "cuda_legacy" {
		t.Fatalf("expected InferenceRuntime to persist, got %q", loaded.InferenceRuntime)
	}
}

func TestPluginCRUD(t *testing.T) {
	s, cleanup := setupTestStore(t)
	defer cleanup()

	plugin := Plugin{
		ID:       "mcp:example",
		Name:     "example",
		Provider: "mcp",
		Enabled:  true,
		Config:   []byte(`{"command":"example","args":[],"transport":"stdio"}`),
	}

	if err := s.UpsertPlugin(plugin); err != nil {
		t.Fatalf("UpsertPlugin() error = %v", err)
	}

	plugins, err := s.Plugins()
	if err != nil {
		t.Fatalf("Plugins() error = %v", err)
	}
	if len(plugins) != 1 {
		t.Fatalf("expected 1 plugin, got %d", len(plugins))
	}
	if plugins[0].ID != plugin.ID || plugins[0].Name != plugin.Name {
		t.Fatalf("unexpected plugin: %+v", plugins[0])
	}

	if err := s.SetPluginEnabled(plugin.ID, false); err != nil {
		t.Fatalf("SetPluginEnabled() error = %v", err)
	}

	plugins, err = s.Plugins()
	if err != nil {
		t.Fatalf("Plugins() after toggle error = %v", err)
	}
	if plugins[0].Enabled {
		t.Fatal("expected plugin to be disabled")
	}

	if err := s.DeletePlugin(plugin.ID); err != nil {
		t.Fatalf("DeletePlugin() error = %v", err)
	}

	plugins, err = s.Plugins()
	if err != nil {
		t.Fatalf("Plugins() after delete error = %v", err)
	}
	if len(plugins) != 0 {
		t.Fatalf("expected 0 plugins after delete, got %d", len(plugins))
	}
}

// setupTestStore creates a temporary store for testing
func setupTestStore(t *testing.T) (*Store, func()) {
	t.Helper()

	tmpDir := t.TempDir()

	// Override legacy config path to ensure no migration happens
	oldLegacyConfigPath := legacyConfigPath
	legacyConfigPath = filepath.Join(tmpDir, "config.json")

	s := &Store{DBPath: filepath.Join(tmpDir, "db.sqlite")}

	cleanup := func() {
		s.Close()
		legacyConfigPath = oldLegacyConfigPath
	}

	return s, cleanup
}
