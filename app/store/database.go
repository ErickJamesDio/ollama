//go:build windows || darwin

package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// currentSchemaVersion defines the current database schema version.
// Increment this when making schema changes that require migrations.
const currentSchemaVersion = 28

// database wraps the SQLite connection.
// SQLite handles its own locking for concurrent access:
// - Multiple readers can access the database simultaneously
// - Writers are serialized (only one writer at a time)
// - WAL mode allows readers to not block writers
// This means we don't need application-level locks for database operations.
type database struct {
	conn *sql.DB
}

func newDatabase(dbPath string) (*database, error) {
	// Open database connection
	conn, err := sql.Open("sqlite3", dbPath+"?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=5000&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// Test the connection
	if err := conn.Ping(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	db := &database{conn: conn}

	// Initialize schema
	if err := db.init(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("initialize database: %w", err)
	}

	return db, nil
}

func (db *database) Close() error {
	_, _ = db.conn.Exec("PRAGMA wal_checkpoint(TRUNCATE);")

	return db.conn.Close()
}

func (db *database) init() error {
	if _, err := db.conn.Exec("PRAGMA foreign_keys = ON"); err != nil {
		return fmt.Errorf("enable foreign keys: %w", err)
	}

	schema := fmt.Sprintf(`
	CREATE TABLE IF NOT EXISTS settings (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		device_id TEXT NOT NULL DEFAULT '',
		has_completed_first_run BOOLEAN NOT NULL DEFAULT 0,
		expose BOOLEAN NOT NULL DEFAULT 0,
		survey BOOLEAN NOT NULL DEFAULT TRUE,
		browser BOOLEAN NOT NULL DEFAULT 0,
		models TEXT NOT NULL DEFAULT '',
		agent BOOLEAN NOT NULL DEFAULT 0,
		tools BOOLEAN NOT NULL DEFAULT 0,
		working_dir TEXT NOT NULL DEFAULT '',
		context_length INTEGER NOT NULL DEFAULT 0,
		window_width INTEGER NOT NULL DEFAULT 0,
		window_height INTEGER NOT NULL DEFAULT 0,
		config_migrated BOOLEAN NOT NULL DEFAULT 0,
		airplane_mode BOOLEAN NOT NULL DEFAULT 0,
		turbo_enabled BOOLEAN NOT NULL DEFAULT 0,
		websearch_enabled BOOLEAN NOT NULL DEFAULT 0,
		selected_model TEXT NOT NULL DEFAULT '',
		sidebar_open BOOLEAN NOT NULL DEFAULT 0,
		last_home_view TEXT NOT NULL DEFAULT 'chat',
		onboarding_version INTEGER NOT NULL DEFAULT 0,
		think_enabled BOOLEAN NOT NULL DEFAULT 0,
		think_level TEXT NOT NULL DEFAULT '',
		cloud_setting_migrated BOOLEAN NOT NULL DEFAULT 0,
		remote TEXT NOT NULL DEFAULT '', -- deprecated
		auto_update_enabled BOOLEAN NOT NULL DEFAULT 1,
		claude_desktop_used BOOLEAN NOT NULL DEFAULT 0,
		local_models_enabled BOOLEAN NOT NULL DEFAULT 1,
		cpu_fallback_enabled BOOLEAN NOT NULL DEFAULT 0,
		inference_enabled BOOLEAN NOT NULL DEFAULT 0,
		inference_runtime TEXT NOT NULL DEFAULT 'auto',
		inference_auto_fit_enabled BOOLEAN NOT NULL DEFAULT 1,
		inference_manual_layers TEXT NOT NULL DEFAULT '',
		plugins_enabled BOOLEAN NOT NULL DEFAULT 0,
		schema_version INTEGER NOT NULL DEFAULT %d
	);

	-- Insert default settings row if it doesn't exist
	INSERT OR IGNORE INTO settings (id) VALUES (1);

	CREATE TABLE IF NOT EXISTS chats (
		id TEXT PRIMARY KEY,
		title TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		browser_state TEXT
	);

	CREATE TABLE IF NOT EXISTS messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		chat_id TEXT NOT NULL,
		role TEXT NOT NULL,
		content TEXT NOT NULL DEFAULT '',
		thinking TEXT NOT NULL DEFAULT '',
		stream BOOLEAN NOT NULL DEFAULT 0,
		model_name TEXT,
		model_cloud BOOLEAN, -- deprecated
		model_ollama_host BOOLEAN, -- deprecated
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		thinking_time_start TIMESTAMP,
		thinking_time_end TIMESTAMP,
		tool_result TEXT,
		FOREIGN KEY (chat_id) REFERENCES chats(id) ON DELETE CASCADE
	);

	CREATE INDEX IF NOT EXISTS idx_messages_chat_id ON messages(chat_id);

	CREATE TABLE IF NOT EXISTS tool_calls (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		message_id INTEGER NOT NULL,
		type TEXT NOT NULL,
		function_name TEXT NOT NULL,
		function_arguments TEXT NOT NULL,
		function_result TEXT,
		FOREIGN KEY (message_id) REFERENCES messages(id) ON DELETE CASCADE
	);

	CREATE INDEX IF NOT EXISTS idx_tool_calls_message_id ON tool_calls(message_id);

	CREATE TABLE IF NOT EXISTS attachments (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		message_id INTEGER NOT NULL,
		filename TEXT NOT NULL,
		data BLOB NOT NULL,
		FOREIGN KEY (message_id) REFERENCES messages(id) ON DELETE CASCADE
	);

	CREATE INDEX IF NOT EXISTS idx_attachments_message_id ON attachments(message_id);

	CREATE TABLE IF NOT EXISTS users (
		name TEXT NOT NULL DEFAULT '',
		email TEXT NOT NULL DEFAULT '',
		plan TEXT NOT NULL DEFAULT '',
		cached_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS plugins (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL,
		provider TEXT NOT NULL,
		enabled BOOLEAN NOT NULL DEFAULT 1,
		config_json TEXT NOT NULL DEFAULT '{}',
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(provider, name)
	);

	CREATE INDEX IF NOT EXISTS idx_plugins_provider_name ON plugins(provider, name);

	CREATE TABLE IF NOT EXISTS plugin_tool_preferences (
		plugin_id TEXT NOT NULL,
		tool_name TEXT NOT NULL,
		enabled BOOLEAN NOT NULL DEFAULT 1,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (plugin_id, tool_name),
		FOREIGN KEY (plugin_id) REFERENCES plugins(id) ON DELETE CASCADE
	);

	CREATE INDEX IF NOT EXISTS idx_plugin_tool_preferences_plugin_id ON plugin_tool_preferences(plugin_id);

	CREATE TABLE IF NOT EXISTS model_metadata (
		model_name TEXT PRIMARY KEY,
		origin TEXT NOT NULL,
		source_path TEXT NOT NULL DEFAULT '',
		display_name TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS context_checkpoints (
		chat_id TEXT PRIMARY KEY,
		covered_message_count INTEGER NOT NULL,
		covered_fingerprint TEXT NOT NULL,
		content TEXT NOT NULL,
		estimated_tokens_before INTEGER NOT NULL DEFAULT 0,
		estimated_tokens_after INTEGER NOT NULL DEFAULT 0,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (chat_id) REFERENCES chats(id) ON DELETE CASCADE
	);
	`, currentSchemaVersion)

	_, err := db.conn.Exec(schema)
	if err != nil {
		return err
	}

	// Check and upgrade schema version if needed
	if err := db.migrate(); err != nil {
		return fmt.Errorf("migrate schema: %w", err)
	}

	// Clean up orphaned records created before foreign key constraints were properly enforced
	// TODO: Can eventually be removed - cleans up data from foreign key bug (ollama/ollama#11785, ollama/app#476)
	if err := db.cleanupOrphanedData(); err != nil {
		return fmt.Errorf("cleanup orphaned data: %w", err)
	}

	return nil
}

// migrate handles database schema migrations
func (db *database) migrate() error {
	// Get current schema version
	version, err := db.getSchemaVersion()
	if err != nil {
		return fmt.Errorf("get schema version after migration attempt: %w", err)
	}

	// Run migrations for each version
	for version < currentSchemaVersion {
		switch version {
		case 1:
			// Migrate from version 1 to 2: add context_length column
			if err := db.migrateV1ToV2(); err != nil {
				return fmt.Errorf("migrate v1 to v2: %w", err)
			}
			version = 2
		case 2:
			// Migrate from version 2 to 3: create attachments table
			if err := db.migrateV2ToV3(); err != nil {
				return fmt.Errorf("migrate v2 to v3: %w", err)
			}
			version = 3
		case 3:
			// Migrate from version 3 to 4: add tool_result column to messages table
			if err := db.migrateV3ToV4(); err != nil {
				return fmt.Errorf("migrate v3 to v4: %w", err)
			}
			version = 4
		case 4:
			// add airplane_mode column to settings table
			if err := db.migrateV4ToV5(); err != nil {
				return fmt.Errorf("migrate v4 to v5: %w", err)
			}
			version = 5
		case 5:
			// add turbo_enabled column to settings table
			if err := db.migrateV5ToV6(); err != nil {
				return fmt.Errorf("migrate v5 to v6: %w", err)
			}
			version = 6
		case 6:
			// add missing index for attachments table
			if err := db.migrateV6ToV7(); err != nil {
				return fmt.Errorf("migrate v6 to v7: %w", err)
			}
			version = 7
		case 7:
			// add think_enabled and think_level columns to settings table
			if err := db.migrateV7ToV8(); err != nil {
				return fmt.Errorf("migrate v7 to v8: %w", err)
			}
			version = 8
		case 8:
			// add browser_state column to chats table
			if err := db.migrateV8ToV9(); err != nil {
				return fmt.Errorf("migrate v8 to v9: %w", err)
			}
			version = 9
		case 9:
			// add cached user table
			if err := db.migrateV9ToV10(); err != nil {
				return fmt.Errorf("migrate v9 to v10: %w", err)
			}
			version = 10
		case 10:
			// remove remote column from settings table
			if err := db.migrateV10ToV11(); err != nil {
				return fmt.Errorf("migrate v10 to v11: %w", err)
			}
			version = 11
		case 11:
			// bring back remote column for backwards compatibility (deprecated)
			if err := db.migrateV11ToV12(); err != nil {
				return fmt.Errorf("migrate v11 to v12: %w", err)
			}
			version = 12
		case 12:
			// add cloud_setting_migrated column to settings table
			if err := db.migrateV12ToV13(); err != nil {
				return fmt.Errorf("migrate v12 to v13: %w", err)
			}
			version = 13
		case 13:
			// change default context_length from 4096 to 0 (VRAM-based tiered defaults)
			if err := db.migrateV13ToV14(); err != nil {
				return fmt.Errorf("migrate v13 to v14: %w", err)
			}
			version = 14
		case 14:
			// add auto_update_enabled column to settings table
			if err := db.migrateV14ToV15(); err != nil {
				return fmt.Errorf("migrate v14 to v15: %w", err)
			}
			version = 15
		case 15:
			// add last_home_view column to settings table
			if err := db.migrateV15ToV16(); err != nil {
				return fmt.Errorf("migrate v15 to v16: %w", err)
			}
			version = 16
		case 16:
			// Existing users should not be shown onboarding after an upgrade.
			if err := db.migrateV16ToV17(); err != nil {
				return fmt.Errorf("migrate v16 to v17: %w", err)
			}
			version = 17
		case 17:
			// Remember that Claude Desktop has been connected at least once.
			if err := db.migrateV17ToV18(); err != nil {
				return fmt.Errorf("migrate v17 to v18: %w", err)
			}
			version = 18
		case 18:
			// Add plugin settings and dedicated plugin storage.
			if err := db.migrateV18ToV19(); err != nil {
				return fmt.Errorf("migrate v18 to v19: %w", err)
			}
			version = 19
		case 19:
			// Preserve existing behavior by defaulting local models to visible.
			if err := db.migrateV19ToV20(); err != nil {
				return fmt.Errorf("migrate v19 to v20: %w", err)
			}
			version = 20
		case 20:
			// Store per-tool plugin preferences separately from MCP JSON config.
			if err := db.migrateV20ToV21(); err != nil {
				return fmt.Errorf("migrate v20 to v21: %w", err)
			}
			version = 21
		case 21:
			// Allow model recommendations to consider CPU/RAM fallback when enabled.
			if err := db.migrateV21ToV22(); err != nil {
				return fmt.Errorf("migrate v21 to v22: %w", err)
			}
			version = 22
		case 22:
			// Keep advanced inference controls hidden until the user enables them.
			if err := db.migrateV22ToV23(); err != nil {
				return fmt.Errorf("migrate v22 to v23: %w", err)
			}
			version = 23
		case 23:
			// Store the selected local inference runtime.
			if err := db.migrateV23ToV24(); err != nil {
				return fmt.Errorf("migrate v23 to v24: %w", err)
			}
			version = 24
		case 24:
			// Store fork model provenance and display aliases.
			if err := db.migrateV24ToV25(); err != nil {
				return fmt.Errorf("migrate v24 to v25: %w", err)
			}
			version = 25
		case 25:
			// Store non-destructive context compaction checkpoints separately from chat messages.
			if err := db.migrateV25ToV26(); err != nil {
				return fmt.Errorf("migrate v25 to v26: %w", err)
			}
			version = 26
		case 26:
			// Allow users to disable llama.cpp automatic memory fit.
			if err := db.migrateV26ToV27(); err != nil {
				return fmt.Errorf("migrate v26 to v27: %w", err)
			}
			version = 27
		case 27:
			// Store manual GPU allocation intent from the inference page.
			if err := db.migrateV27ToV28(); err != nil {
				return fmt.Errorf("migrate v27 to v28: %w", err)
			}
			version = 28
		default:
			// If we have a version we don't recognize, just set it to current
			// This might happen during development
			version = currentSchemaVersion
		}
	}

	return nil
}

// migrateV1ToV2 adds the context_length column to the settings table
func (db *database) migrateV1ToV2() error {
	_, err := db.conn.Exec(`ALTER TABLE settings ADD COLUMN context_length INTEGER NOT NULL DEFAULT 4096;`)
	if err != nil && !duplicateColumnError(err) {
		return fmt.Errorf("add context_length column: %w", err)
	}

	_, err = db.conn.Exec(`ALTER TABLE settings ADD COLUMN survey BOOLEAN NOT NULL DEFAULT TRUE;`)
	if err != nil && !duplicateColumnError(err) {
		return fmt.Errorf("add survey column: %w", err)
	}

	_, err = db.conn.Exec(`UPDATE settings SET schema_version = 2;`)
	if err != nil {
		return fmt.Errorf("update schema version: %w", err)
	}
	return nil
}

// migrateV2ToV3 creates the attachments table
func (db *database) migrateV2ToV3() error {
	_, err := db.conn.Exec(`
		CREATE TABLE IF NOT EXISTS attachments (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			message_id INTEGER NOT NULL,
			filename TEXT NOT NULL,
			data BLOB NOT NULL,
			FOREIGN KEY (message_id) REFERENCES messages(id) ON DELETE CASCADE
		)
	`)
	if err != nil {
		return fmt.Errorf("create attachments table: %w", err)
	}

	_, err = db.conn.Exec(`UPDATE settings SET schema_version = 3`)
	if err != nil {
		return fmt.Errorf("update schema version: %w", err)
	}

	return nil
}

func (db *database) migrateV3ToV4() error {
	_, err := db.conn.Exec(`ALTER TABLE messages ADD COLUMN tool_result TEXT;`)
	if err != nil && !duplicateColumnError(err) {
		return fmt.Errorf("add tool_result column: %w", err)
	}

	_, err = db.conn.Exec(`UPDATE settings SET schema_version = 4;`)
	if err != nil {
		return fmt.Errorf("update schema version: %w", err)
	}

	return nil
}

// migrateV4ToV5 adds the airplane_mode column to the settings table
func (db *database) migrateV4ToV5() error {
	_, err := db.conn.Exec(`ALTER TABLE settings ADD COLUMN airplane_mode BOOLEAN NOT NULL DEFAULT 0;`)
	if err != nil && !duplicateColumnError(err) {
		return fmt.Errorf("add airplane_mode column: %w", err)
	}

	_, err = db.conn.Exec(`UPDATE settings SET schema_version = 5;`)
	if err != nil {
		return fmt.Errorf("update schema version: %w", err)
	}

	return nil
}

// migrateV5ToV6 adds the turbo_enabled, websearch_enabled, selected_model, sidebar_open columns to the settings table
func (db *database) migrateV5ToV6() error {
	_, err := db.conn.Exec(`ALTER TABLE settings ADD COLUMN turbo_enabled BOOLEAN NOT NULL DEFAULT 0;`)
	if err != nil && !duplicateColumnError(err) {
		return fmt.Errorf("add turbo_enabled column: %w", err)
	}

	_, err = db.conn.Exec(`ALTER TABLE settings ADD COLUMN websearch_enabled BOOLEAN NOT NULL DEFAULT 0;`)
	if err != nil && !duplicateColumnError(err) {
		return fmt.Errorf("add websearch_enabled column: %w", err)
	}

	_, err = db.conn.Exec(`ALTER TABLE settings ADD COLUMN selected_model TEXT NOT NULL DEFAULT '';`)
	if err != nil && !duplicateColumnError(err) {
		return fmt.Errorf("add selected_model column: %w", err)
	}

	_, err = db.conn.Exec(`ALTER TABLE settings ADD COLUMN sidebar_open BOOLEAN NOT NULL DEFAULT 0;`)
	if err != nil && !duplicateColumnError(err) {
		return fmt.Errorf("add sidebar_open column: %w", err)
	}

	_, err = db.conn.Exec(`UPDATE settings SET schema_version = 6;`)
	if err != nil {
		return fmt.Errorf("update schema version: %w", err)
	}

	return nil
}

// migrateV6ToV7 adds the missing index for the attachments table
func (db *database) migrateV6ToV7() error {
	_, err := db.conn.Exec(`CREATE INDEX IF NOT EXISTS idx_attachments_message_id ON attachments(message_id);`)
	if err != nil {
		return fmt.Errorf("create attachments index: %w", err)
	}

	_, err = db.conn.Exec(`UPDATE settings SET schema_version = 7;`)
	if err != nil {
		return fmt.Errorf("update schema version: %w", err)
	}

	return nil
}

// migrateV7ToV8 adds the think_enabled and think_level columns to the settings table
func (db *database) migrateV7ToV8() error {
	_, err := db.conn.Exec(`ALTER TABLE settings ADD COLUMN think_enabled BOOLEAN NOT NULL DEFAULT 0;`)
	if err != nil && !duplicateColumnError(err) {
		return fmt.Errorf("add think_enabled column: %w", err)
	}

	_, err = db.conn.Exec(`ALTER TABLE settings ADD COLUMN think_level TEXT NOT NULL DEFAULT '';`)
	if err != nil && !duplicateColumnError(err) {
		return fmt.Errorf("add think_level column: %w", err)
	}

	_, err = db.conn.Exec(`UPDATE settings SET schema_version = 8;`)
	if err != nil {
		return fmt.Errorf("update schema version: %w", err)
	}

	return nil
}

// migrateV8ToV9 adds browser_state to chats and bumps schema
func (db *database) migrateV8ToV9() error {
	_, err := db.conn.Exec(`
		ALTER TABLE chats ADD COLUMN browser_state TEXT;
		UPDATE settings SET schema_version = 9;
	`)

	if err != nil && !duplicateColumnError(err) {
		return fmt.Errorf("add browser_state column: %w", err)
	}

	return nil
}

// migrateV9ToV10 adds users table
func (db *database) migrateV9ToV10() error {
	_, err := db.conn.Exec(`
		CREATE TABLE IF NOT EXISTS users (
			name TEXT NOT NULL DEFAULT '',
			email TEXT NOT NULL DEFAULT '',
			plan TEXT NOT NULL DEFAULT '',
			cached_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
		UPDATE settings SET schema_version = 10;
	`)
	if err != nil {
		return fmt.Errorf("create users table: %w", err)
	}

	return nil
}

// migrateV10ToV11 removes the remote column from the settings table
func (db *database) migrateV10ToV11() error {
	_, err := db.conn.Exec(`ALTER TABLE settings DROP COLUMN remote`)
	if err != nil && !columnNotExists(err) {
		return fmt.Errorf("drop remote column: %w", err)
	}

	_, err = db.conn.Exec(`UPDATE settings SET schema_version = 11`)
	if err != nil {
		return fmt.Errorf("update schema version: %w", err)
	}

	return nil
}

// migrateV11ToV12 brings back the remote column for backwards compatibility (deprecated)
func (db *database) migrateV11ToV12() error {
	_, err := db.conn.Exec(`ALTER TABLE settings ADD COLUMN remote TEXT NOT NULL DEFAULT ''`)
	if err != nil && !duplicateColumnError(err) {
		return fmt.Errorf("add remote column: %w", err)
	}

	_, err = db.conn.Exec(`UPDATE settings SET schema_version = 12`)
	if err != nil {
		return fmt.Errorf("update schema version: %w", err)
	}

	return nil
}

// migrateV12ToV13 adds cloud_setting_migrated to settings.
func (db *database) migrateV12ToV13() error {
	_, err := db.conn.Exec(`ALTER TABLE settings ADD COLUMN cloud_setting_migrated BOOLEAN NOT NULL DEFAULT 0`)
	if err != nil && !duplicateColumnError(err) {
		return fmt.Errorf("add cloud_setting_migrated column: %w", err)
	}

	_, err = db.conn.Exec(`UPDATE settings SET schema_version = 13`)
	if err != nil {
		return fmt.Errorf("update schema version: %w", err)
	}

	return nil
}

// migrateV13ToV14 changes the default context_length from 4096 to 0.
// When context_length is 0, the ollama server uses VRAM-based tiered defaults.
func (db *database) migrateV13ToV14() error {
	_, err := db.conn.Exec(`UPDATE settings SET context_length = 0 WHERE context_length = 4096`)
	if err != nil {
		return fmt.Errorf("update context_length default: %w", err)
	}

	_, err = db.conn.Exec(`UPDATE settings SET schema_version = 14`)
	if err != nil {
		return fmt.Errorf("update schema version: %w", err)
	}

	return nil
}

// migrateV14ToV15 adds the auto_update_enabled column to the settings table
func (db *database) migrateV14ToV15() error {
	_, err := db.conn.Exec(`ALTER TABLE settings ADD COLUMN auto_update_enabled BOOLEAN NOT NULL DEFAULT 1`)
	if err != nil && !duplicateColumnError(err) {
		return fmt.Errorf("add auto_update_enabled column: %w", err)
	}

	_, err = db.conn.Exec(`UPDATE settings SET schema_version = 15`)
	if err != nil {
		return fmt.Errorf("update schema version: %w", err)
	}

	return nil
}

// migrateV15ToV16 adds the last_home_view column to the settings table
func (db *database) migrateV15ToV16() error {
	_, err := db.conn.Exec(`ALTER TABLE settings ADD COLUMN last_home_view TEXT NOT NULL DEFAULT 'chat'`)
	if err != nil && !duplicateColumnError(err) {
		return fmt.Errorf("add last_home_view column: %w", err)
	}

	_, err = db.conn.Exec(`UPDATE settings SET schema_version = 16`)
	if err != nil {
		return fmt.Errorf("update schema version: %w", err)
	}

	return nil
}

// migrateV16ToV17 adds versioned onboarding state. The schema default stays at
// zero for genuinely new installs, while all existing rows are marked complete
// and moved off the retired launch home view.
func (db *database) migrateV16ToV17() error {
	_, err := db.conn.Exec(`ALTER TABLE settings ADD COLUMN onboarding_version INTEGER NOT NULL DEFAULT 0`)
	if err != nil && !duplicateColumnError(err) {
		return fmt.Errorf("add onboarding_version column: %w", err)
	}

	_, err = db.conn.Exec(`UPDATE settings SET onboarding_version = 1, last_home_view = 'chat', schema_version = 17`)
	if err != nil {
		return fmt.Errorf("complete onboarding for existing users: %w", err)
	}

	return nil
}

// migrateV17ToV18 adds durable Claude Desktop integration history.
func (db *database) migrateV17ToV18() error {
	_, err := db.conn.Exec(`ALTER TABLE settings ADD COLUMN claude_desktop_used BOOLEAN NOT NULL DEFAULT 0`)
	if err != nil && !duplicateColumnError(err) {
		return fmt.Errorf("add claude_desktop_used column: %w", err)
	}

	_, err = db.conn.Exec(`UPDATE settings SET schema_version = 18`)
	if err != nil {
		return fmt.Errorf("update schema version: %w", err)
	}

	return nil
}

func (db *database) migrateV18ToV19() error {
	_, err := db.conn.Exec(`ALTER TABLE settings ADD COLUMN plugins_enabled BOOLEAN NOT NULL DEFAULT 0`)
	if err != nil && !duplicateColumnError(err) {
		return fmt.Errorf("add plugins_enabled column: %w", err)
	}

	_, err = db.conn.Exec(`
		CREATE TABLE IF NOT EXISTS plugins (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			provider TEXT NOT NULL,
			enabled BOOLEAN NOT NULL DEFAULT 1,
			config_json TEXT NOT NULL DEFAULT '{}',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(provider, name)
		)
	`)
	if err != nil {
		return fmt.Errorf("create plugins table: %w", err)
	}

	_, err = db.conn.Exec(`CREATE INDEX IF NOT EXISTS idx_plugins_provider_name ON plugins(provider, name)`)
	if err != nil {
		return fmt.Errorf("create plugins index: %w", err)
	}

	_, err = db.conn.Exec(`UPDATE settings SET schema_version = 19`)
	if err != nil {
		return fmt.Errorf("update schema version: %w", err)
	}

	return nil
}

func (db *database) migrateV19ToV20() error {
	_, err := db.conn.Exec(`ALTER TABLE settings ADD COLUMN local_models_enabled BOOLEAN NOT NULL DEFAULT 1`)
	if err != nil && !duplicateColumnError(err) {
		return fmt.Errorf("add local_models_enabled column: %w", err)
	}

	_, err = db.conn.Exec(`UPDATE settings SET local_models_enabled = 1, schema_version = 20`)
	if err != nil {
		return fmt.Errorf("update schema version: %w", err)
	}

	return nil
}

func (db *database) migrateV20ToV21() error {
	_, err := db.conn.Exec(`
		CREATE TABLE IF NOT EXISTS plugin_tool_preferences (
			plugin_id TEXT NOT NULL,
			tool_name TEXT NOT NULL,
			enabled BOOLEAN NOT NULL DEFAULT 1,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (plugin_id, tool_name),
			FOREIGN KEY (plugin_id) REFERENCES plugins(id) ON DELETE CASCADE
		)
	`)
	if err != nil {
		return fmt.Errorf("create plugin_tool_preferences table: %w", err)
	}

	_, err = db.conn.Exec(`CREATE INDEX IF NOT EXISTS idx_plugin_tool_preferences_plugin_id ON plugin_tool_preferences(plugin_id)`)
	if err != nil {
		return fmt.Errorf("create plugin_tool_preferences index: %w", err)
	}

	_, err = db.conn.Exec(`UPDATE settings SET schema_version = 21`)
	if err != nil {
		return fmt.Errorf("update schema version: %w", err)
	}

	return nil
}

// migrateV21ToV22 adds cpu_fallback_enabled to the settings table.
func (db *database) migrateV21ToV22() error {
	_, err := db.conn.Exec(`ALTER TABLE settings ADD COLUMN cpu_fallback_enabled BOOLEAN NOT NULL DEFAULT 0`)
	if err != nil && !duplicateColumnError(err) {
		return fmt.Errorf("add cpu_fallback_enabled column: %w", err)
	}

	_, err = db.conn.Exec(`UPDATE settings SET schema_version = 22`)
	if err != nil {
		return fmt.Errorf("update schema version: %w", err)
	}

	return nil
}

func (db *database) migrateV22ToV23() error {
	_, err := db.conn.Exec(`ALTER TABLE settings ADD COLUMN inference_enabled BOOLEAN NOT NULL DEFAULT 0`)
	if err != nil && !duplicateColumnError(err) {
		return fmt.Errorf("add inference_enabled column: %w", err)
	}

	_, err = db.conn.Exec(`UPDATE settings SET schema_version = 23`)
	if err != nil {
		return fmt.Errorf("update schema version: %w", err)
	}

	return nil
}

func (db *database) migrateV23ToV24() error {
	_, err := db.conn.Exec(`ALTER TABLE settings ADD COLUMN inference_runtime TEXT NOT NULL DEFAULT 'auto'`)
	if err != nil && !duplicateColumnError(err) {
		return fmt.Errorf("add inference_runtime column: %w", err)
	}

	_, err = db.conn.Exec(`UPDATE settings SET schema_version = 24`)
	if err != nil {
		return fmt.Errorf("update schema version: %w", err)
	}

	return nil
}

func (db *database) migrateV24ToV25() error {
	_, err := db.conn.Exec(`
		CREATE TABLE IF NOT EXISTS model_metadata (
			model_name TEXT PRIMARY KEY,
			origin TEXT NOT NULL,
			source_path TEXT NOT NULL DEFAULT '',
			display_name TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		return fmt.Errorf("create model metadata table: %w", err)
	}

	_, err = db.conn.Exec(`UPDATE settings SET schema_version = 25`)
	if err != nil {
		return fmt.Errorf("update schema version: %w", err)
	}

	return nil
}

func (db *database) migrateV25ToV26() error {
	_, err := db.conn.Exec(`
		CREATE TABLE IF NOT EXISTS context_checkpoints (
			chat_id TEXT PRIMARY KEY,
			covered_message_count INTEGER NOT NULL,
			covered_fingerprint TEXT NOT NULL,
			content TEXT NOT NULL,
			estimated_tokens_before INTEGER NOT NULL DEFAULT 0,
			estimated_tokens_after INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (chat_id) REFERENCES chats(id) ON DELETE CASCADE
		)
	`)
	if err != nil {
		return fmt.Errorf("create context checkpoints table: %w", err)
	}

	_, err = db.conn.Exec(`UPDATE settings SET schema_version = 26`)
	if err != nil {
		return fmt.Errorf("update schema version: %w", err)
	}

	return nil
}

// cleanupOrphanedData removes orphaned records that may exist due to the foreign key bug
func (db *database) cleanupOrphanedData() error {
	_, err := db.conn.Exec(`
		DELETE FROM tool_calls
		WHERE message_id NOT IN (SELECT id FROM messages)
	`)
	if err != nil {
		return fmt.Errorf("cleanup orphaned tool_calls: %w", err)
	}

	_, err = db.conn.Exec(`
		DELETE FROM attachments
		WHERE message_id NOT IN (SELECT id FROM messages)
	`)
	if err != nil {
		return fmt.Errorf("cleanup orphaned attachments: %w", err)
	}

	_, err = db.conn.Exec(`
		DELETE FROM messages
		WHERE chat_id NOT IN (SELECT id FROM chats)
	`)
	if err != nil {
		return fmt.Errorf("cleanup orphaned messages: %w", err)
	}

	return nil
}

func duplicateColumnError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate column name")
}

func columnNotExists(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such column")
}

func (db *database) getAllChats() ([]Chat, error) {
	// Query chats with their first user message and latest update time
	query := `
		SELECT 
			c.id, 
			c.title, 
			c.created_at,
			COALESCE(first_msg.content, '') as first_user_content,
			COALESCE(datetime(MAX(m.updated_at)), datetime(c.created_at)) as last_updated
		FROM chats c
		LEFT JOIN (
			SELECT chat_id, content, MIN(id) as min_id
			FROM messages
			WHERE role = 'user'
			GROUP BY chat_id
		) first_msg ON c.id = first_msg.chat_id
		LEFT JOIN messages m ON c.id = m.chat_id
		GROUP BY c.id, c.title, c.created_at, first_msg.content
		ORDER BY last_updated DESC
	`

	rows, err := db.conn.Query(query)
	if err != nil {
		return nil, fmt.Errorf("query chats: %w", err)
	}
	defer rows.Close()

	var chats []Chat
	for rows.Next() {
		var chat Chat
		var createdAt time.Time
		var firstUserContent string
		var lastUpdatedStr string

		err := rows.Scan(
			&chat.ID,
			&chat.Title,
			&createdAt,
			&firstUserContent,
			&lastUpdatedStr,
		)

		// Parse the last updated time
		lastUpdated, _ := time.Parse("2006-01-02 15:04:05", lastUpdatedStr)
		if err != nil {
			return nil, fmt.Errorf("scan chat: %w", err)
		}

		chat.CreatedAt = createdAt

		// Add a dummy first user message for the UI to display
		// This is just for the excerpt, full messages are loaded when needed
		chat.Messages = []Message{}
		if firstUserContent != "" {
			chat.Messages = append(chat.Messages, Message{
				Role:      "user",
				Content:   firstUserContent,
				UpdatedAt: lastUpdated,
			})
		}

		chats = append(chats, chat)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate chats: %w", err)
	}

	return chats, nil
}

func (db *database) getChatWithOptions(id string, loadAttachmentData bool) (*Chat, error) {
	query := `
		SELECT id, title, created_at, browser_state
		FROM chats
		WHERE id = ?
	`

	var chat Chat
	var createdAt time.Time
	var browserState sql.NullString

	err := db.conn.QueryRow(query, id).Scan(
		&chat.ID,
		&chat.Title,
		&createdAt,
		&browserState,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("chat not found")
		}
		return nil, fmt.Errorf("query chat: %w", err)
	}

	chat.CreatedAt = createdAt
	if browserState.Valid && browserState.String != "" {
		var raw json.RawMessage
		if err := json.Unmarshal([]byte(browserState.String), &raw); err == nil {
			chat.BrowserState = raw
		}
	}

	messages, err := db.getMessages(id, loadAttachmentData)
	if err != nil {
		return nil, fmt.Errorf("get messages: %w", err)
	}
	chat.Messages = messages

	return &chat, nil
}

func (db *database) saveChat(chat Chat) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Use COALESCE for browser_state to avoid wiping an existing
	// chat-level browser_state when saving a chat that doesn't include a new state payload.
	// Many code paths call SetChat to update metadata/messages only; without COALESCE the
	// UPSERT would overwrite browser_state with NULL, breaking revisit rendering that relies
	// on the last persisted full tool state.
	query := `
		INSERT INTO chats (id, title, created_at, browser_state)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			title = excluded.title,
			browser_state = COALESCE(excluded.browser_state, chats.browser_state)
	`

	var browserState sql.NullString
	if chat.BrowserState != nil {
		browserState = sql.NullString{String: string(chat.BrowserState), Valid: true}
	}

	_, err = tx.Exec(query,
		chat.ID,
		chat.Title,
		chat.CreatedAt,
		browserState,
	)
	if err != nil {
		return fmt.Errorf("save chat: %w", err)
	}

	var checkpoint ContextCheckpoint
	err = tx.QueryRow(`
		SELECT chat_id, covered_message_count, covered_fingerprint, content, estimated_tokens_before, estimated_tokens_after, created_at
		FROM context_checkpoints
		WHERE chat_id = ?
	`, chat.ID).Scan(
		&checkpoint.ChatID,
		&checkpoint.CoveredMessageCount,
		&checkpoint.CoveredFingerprint,
		&checkpoint.Content,
		&checkpoint.EstimatedTokensBefore,
		&checkpoint.EstimatedTokensAfter,
		&checkpoint.CreatedAt,
	)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("get context checkpoint: %w", err)
	}
	if err == nil && (checkpoint.CoveredMessageCount > len(chat.Messages) ||
		checkpoint.CoveredFingerprint != MessagePrefixFingerprint(chat.Messages, checkpoint.CoveredMessageCount)) {
		if _, err := tx.Exec(`DELETE FROM context_checkpoints WHERE chat_id = ?`, chat.ID); err != nil {
			return fmt.Errorf("delete stale context checkpoint: %w", err)
		}
	}

	// Delete existing messages (we'll re-insert all)
	_, err = tx.Exec("DELETE FROM messages WHERE chat_id = ?", chat.ID)
	if err != nil {
		return fmt.Errorf("delete messages: %w", err)
	}

	// Insert messages
	for _, msg := range chat.Messages {
		messageID, err := db.insertMessage(tx, chat.ID, msg)
		if err != nil {
			return fmt.Errorf("insert message: %w", err)
		}

		// Insert tool calls if any
		for _, toolCall := range msg.ToolCalls {
			err := db.insertToolCall(tx, messageID, toolCall)
			if err != nil {
				return fmt.Errorf("insert tool call: %w", err)
			}
		}
	}

	return tx.Commit()
}

// updateChatBrowserState updates only the browser_state for a chat
func (db *database) updateChatBrowserState(chatID string, state json.RawMessage) error {
	_, err := db.conn.Exec(`UPDATE chats SET browser_state = ? WHERE id = ?`, string(state), chatID)
	if err != nil {
		return fmt.Errorf("update chat browser state: %w", err)
	}
	return nil
}

func (db *database) getContextCheckpoint(chatID string) (*ContextCheckpoint, error) {
	var checkpoint ContextCheckpoint
	err := db.conn.QueryRow(`
		SELECT chat_id, covered_message_count, covered_fingerprint, content, estimated_tokens_before, estimated_tokens_after, created_at
		FROM context_checkpoints
		WHERE chat_id = ?
	`, chatID).Scan(
		&checkpoint.ChatID,
		&checkpoint.CoveredMessageCount,
		&checkpoint.CoveredFingerprint,
		&checkpoint.Content,
		&checkpoint.EstimatedTokensBefore,
		&checkpoint.EstimatedTokensAfter,
		&checkpoint.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get context checkpoint: %w", err)
	}
	return &checkpoint, nil
}

func (db *database) saveContextCheckpoint(checkpoint ContextCheckpoint) error {
	if checkpoint.CreatedAt.IsZero() {
		checkpoint.CreatedAt = time.Now()
	}
	_, err := db.conn.Exec(`
		INSERT INTO context_checkpoints (chat_id, covered_message_count, covered_fingerprint, content, estimated_tokens_before, estimated_tokens_after, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(chat_id) DO UPDATE SET
			covered_message_count = excluded.covered_message_count,
			covered_fingerprint = excluded.covered_fingerprint,
			content = excluded.content,
			estimated_tokens_before = excluded.estimated_tokens_before,
			estimated_tokens_after = excluded.estimated_tokens_after,
			created_at = excluded.created_at
	`, checkpoint.ChatID, checkpoint.CoveredMessageCount, checkpoint.CoveredFingerprint, checkpoint.Content, checkpoint.EstimatedTokensBefore, checkpoint.EstimatedTokensAfter, checkpoint.CreatedAt)
	if err != nil {
		return fmt.Errorf("save context checkpoint: %w", err)
	}
	return nil
}

func (db *database) deleteContextCheckpoint(chatID string) error {
	_, err := db.conn.Exec(`DELETE FROM context_checkpoints WHERE chat_id = ?`, chatID)
	if err != nil {
		return fmt.Errorf("delete context checkpoint: %w", err)
	}
	return nil
}

func (db *database) deleteChat(id string) error {
	_, err := db.conn.Exec("DELETE FROM chats WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete chat: %w", err)
	}

	_, _ = db.conn.Exec("PRAGMA wal_checkpoint(TRUNCATE);")

	return nil
}

func (db *database) updateLastMessage(chatID string, msg Message) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// Get the ID of the last message
	var messageID int64
	err = tx.QueryRow(`
		SELECT MAX(id) FROM messages WHERE chat_id = ?
	`, chatID).Scan(&messageID)
	if err != nil {
		return fmt.Errorf("get last message id: %w", err)
	}

	query := `
		UPDATE messages 
		SET content = ?, thinking = ?, model_name = ?, updated_at = ?, thinking_time_start = ?, thinking_time_end = ?, tool_result = ?
		WHERE id = ?
	`

	var thinkingTimeStart, thinkingTimeEnd sql.NullTime
	if msg.ThinkingTimeStart != nil {
		thinkingTimeStart = sql.NullTime{Time: *msg.ThinkingTimeStart, Valid: true}
	}
	if msg.ThinkingTimeEnd != nil {
		thinkingTimeEnd = sql.NullTime{Time: *msg.ThinkingTimeEnd, Valid: true}
	}

	var modelName sql.NullString
	if msg.Model != "" {
		modelName = sql.NullString{String: msg.Model, Valid: true}
	}

	var toolResultJSON sql.NullString
	if msg.ToolResult != nil {
		resultBytes, err := json.Marshal(msg.ToolResult)
		if err != nil {
			return fmt.Errorf("marshal tool result: %w", err)
		}
		toolResultJSON = sql.NullString{String: string(resultBytes), Valid: true}
	}

	result, err := tx.Exec(query,
		msg.Content,
		msg.Thinking,
		modelName,
		msg.UpdatedAt,
		thinkingTimeStart,
		thinkingTimeEnd,
		toolResultJSON,
		messageID,
	)
	if err != nil {
		return fmt.Errorf("update last message: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("get rows affected: %w", err)
	}
	if rowsAffected == 0 {
		return fmt.Errorf("no message found to update")
	}

	_, err = tx.Exec("DELETE FROM attachments WHERE message_id = ?", messageID)
	if err != nil {
		return fmt.Errorf("delete existing attachments: %w", err)
	}
	for _, att := range msg.Attachments {
		err := db.insertAttachment(tx, messageID, att)
		if err != nil {
			return fmt.Errorf("insert attachment: %w", err)
		}
	}

	_, err = tx.Exec("DELETE FROM tool_calls WHERE message_id = ?", messageID)
	if err != nil {
		return fmt.Errorf("delete existing tool calls: %w", err)
	}
	for _, toolCall := range msg.ToolCalls {
		err := db.insertToolCall(tx, messageID, toolCall)
		if err != nil {
			return fmt.Errorf("insert tool call: %w", err)
		}
	}

	return tx.Commit()
}

func (db *database) appendMessage(chatID string, msg Message) error {
	tx, err := db.conn.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	messageID, err := db.insertMessage(tx, chatID, msg)
	if err != nil {
		return fmt.Errorf("insert message: %w", err)
	}

	// Insert tool calls if any
	for _, toolCall := range msg.ToolCalls {
		err := db.insertToolCall(tx, messageID, toolCall)
		if err != nil {
			return fmt.Errorf("insert tool call: %w", err)
		}
	}

	return tx.Commit()
}

func (db *database) getMessages(chatID string, loadAttachmentData bool) ([]Message, error) {
	query := `
		SELECT id, role, content, thinking, stream, model_name, created_at, updated_at, thinking_time_start, thinking_time_end, tool_result
		FROM messages
		WHERE chat_id = ?
		ORDER BY id ASC
	`

	rows, err := db.conn.Query(query, chatID)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var msg Message
		var messageID int64
		var thinkingTimeStart, thinkingTimeEnd sql.NullTime
		var modelName sql.NullString
		var toolResult sql.NullString

		err := rows.Scan(
			&messageID,
			&msg.Role,
			&msg.Content,
			&msg.Thinking,
			&msg.Stream,
			&modelName,
			&msg.CreatedAt,
			&msg.UpdatedAt,
			&thinkingTimeStart,
			&thinkingTimeEnd,
			&toolResult,
		)
		if err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}

		attachments, err := db.getAttachments(messageID, loadAttachmentData)
		if err != nil {
			return nil, fmt.Errorf("get attachments: %w", err)
		}
		msg.Attachments = attachments

		if thinkingTimeStart.Valid {
			msg.ThinkingTimeStart = &thinkingTimeStart.Time
		}
		if thinkingTimeEnd.Valid {
			msg.ThinkingTimeEnd = &thinkingTimeEnd.Time
		}

		// Parse tool result from JSON if present
		if toolResult.Valid && toolResult.String != "" {
			var result json.RawMessage
			if err := json.Unmarshal([]byte(toolResult.String), &result); err == nil {
				msg.ToolResult = &result
			}
		}

		// Set model if present
		if modelName.Valid && modelName.String != "" {
			msg.Model = modelName.String
		}

		// Get tool calls for this message
		toolCalls, err := db.getToolCalls(messageID)
		if err != nil {
			return nil, fmt.Errorf("get tool calls: %w", err)
		}
		msg.ToolCalls = toolCalls

		messages = append(messages, msg)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}

	return messages, nil
}

func (db *database) insertMessage(tx *sql.Tx, chatID string, msg Message) (int64, error) {
	query := `
		INSERT INTO messages (chat_id, role, content, thinking, stream, model_name, created_at, updated_at, thinking_time_start, thinking_time_end, tool_result)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`

	var thinkingTimeStart, thinkingTimeEnd sql.NullTime
	if msg.ThinkingTimeStart != nil {
		thinkingTimeStart = sql.NullTime{Time: *msg.ThinkingTimeStart, Valid: true}
	}
	if msg.ThinkingTimeEnd != nil {
		thinkingTimeEnd = sql.NullTime{Time: *msg.ThinkingTimeEnd, Valid: true}
	}

	var modelName sql.NullString
	if msg.Model != "" {
		modelName = sql.NullString{String: msg.Model, Valid: true}
	}

	var toolResultJSON sql.NullString
	if msg.ToolResult != nil {
		resultBytes, err := json.Marshal(msg.ToolResult)
		if err != nil {
			return 0, fmt.Errorf("marshal tool result: %w", err)
		}
		toolResultJSON = sql.NullString{String: string(resultBytes), Valid: true}
	}

	result, err := tx.Exec(query,
		chatID,
		msg.Role,
		msg.Content,
		msg.Thinking,
		msg.Stream,
		modelName,
		msg.CreatedAt,
		msg.UpdatedAt,
		thinkingTimeStart,
		thinkingTimeEnd,
		toolResultJSON,
	)
	if err != nil {
		return 0, err
	}

	messageID, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}

	for _, att := range msg.Attachments {
		err := db.insertAttachment(tx, messageID, att)
		if err != nil {
			return 0, fmt.Errorf("insert attachment: %w", err)
		}
	}

	return messageID, nil
}

func (db *database) getAttachments(messageID int64, loadData bool) ([]File, error) {
	var query string
	if loadData {
		query = `
			SELECT filename, data
			FROM attachments
			WHERE message_id = ?
			ORDER BY id ASC
		`
	} else {
		query = `
			SELECT filename, '' as data
			FROM attachments
			WHERE message_id = ?
			ORDER BY id ASC
		`
	}

	rows, err := db.conn.Query(query, messageID)
	if err != nil {
		return nil, fmt.Errorf("query attachments: %w", err)
	}
	defer rows.Close()

	var attachments []File
	for rows.Next() {
		var file File
		err := rows.Scan(&file.Filename, &file.Data)
		if err != nil {
			return nil, fmt.Errorf("scan attachment: %w", err)
		}
		attachments = append(attachments, file)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate attachments: %w", err)
	}

	return attachments, nil
}

func (db *database) getToolCalls(messageID int64) ([]ToolCall, error) {
	query := `
		SELECT type, function_name, function_arguments, function_result
		FROM tool_calls
		WHERE message_id = ?
		ORDER BY id ASC
	`

	rows, err := db.conn.Query(query, messageID)
	if err != nil {
		return nil, fmt.Errorf("query tool calls: %w", err)
	}
	defer rows.Close()

	var toolCalls []ToolCall
	for rows.Next() {
		var tc ToolCall
		var functionResult sql.NullString

		err := rows.Scan(
			&tc.Type,
			&tc.Function.Name,
			&tc.Function.Arguments,
			&functionResult,
		)
		if err != nil {
			return nil, fmt.Errorf("scan tool call: %w", err)
		}

		if functionResult.Valid && functionResult.String != "" {
			// Parse the JSON result
			var result json.RawMessage
			if err := json.Unmarshal([]byte(functionResult.String), &result); err == nil {
				tc.Function.Result = &result
			}
		}

		toolCalls = append(toolCalls, tc)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tool calls: %w", err)
	}

	return toolCalls, nil
}

func (db *database) insertAttachment(tx *sql.Tx, messageID int64, file File) error {
	query := `
		INSERT INTO attachments (message_id, filename, data)
		VALUES (?, ?, ?)
	`
	_, err := tx.Exec(query, messageID, file.Filename, file.Data)
	return err
}

func (db *database) insertToolCall(tx *sql.Tx, messageID int64, tc ToolCall) error {
	query := `
		INSERT INTO tool_calls (message_id, type, function_name, function_arguments, function_result)
		VALUES (?, ?, ?, ?, ?)
	`

	var functionResult sql.NullString
	if tc.Function.Result != nil {
		// Convert result to JSON
		resultJSON, err := json.Marshal(tc.Function.Result)
		if err != nil {
			return fmt.Errorf("marshal tool result: %w", err)
		}
		functionResult = sql.NullString{String: string(resultJSON), Valid: true}
	}

	_, err := tx.Exec(query,
		messageID,
		tc.Type,
		tc.Function.Name,
		tc.Function.Arguments,
		functionResult,
	)
	return err
}

// Settings operations

func (db *database) getID() (string, error) {
	var id string
	err := db.conn.QueryRow("SELECT device_id FROM settings").Scan(&id)
	if err != nil {
		return "", fmt.Errorf("get device id: %w", err)
	}
	return id, nil
}

func (db *database) setID(id string) error {
	_, err := db.conn.Exec("UPDATE settings SET device_id = ?", id)
	if err != nil {
		return fmt.Errorf("set device id: %w", err)
	}
	return nil
}

func (db *database) getHasCompletedFirstRun() (bool, error) {
	var hasCompletedFirstRun bool
	err := db.conn.QueryRow("SELECT has_completed_first_run FROM settings").Scan(&hasCompletedFirstRun)
	if err != nil {
		return false, fmt.Errorf("get has completed first run: %w", err)
	}
	return hasCompletedFirstRun, nil
}

func (db *database) setHasCompletedFirstRun(hasCompletedFirstRun bool) error {
	_, err := db.conn.Exec("UPDATE settings SET has_completed_first_run = ?", hasCompletedFirstRun)
	if err != nil {
		return fmt.Errorf("set has completed first run: %w", err)
	}
	return nil
}

func (db *database) getSettings() (Settings, error) {
	var s Settings

	err := db.conn.QueryRow(`
		SELECT expose, survey, browser, models, agent, tools, working_dir, context_length, turbo_enabled, websearch_enabled, selected_model, sidebar_open, last_home_view, onboarding_version, think_enabled, think_level, auto_update_enabled, claude_desktop_used, local_models_enabled, cpu_fallback_enabled, inference_enabled, inference_runtime, inference_auto_fit_enabled, inference_manual_layers, plugins_enabled
		FROM settings
	`).Scan(&s.Expose, &s.Survey, &s.Browser, &s.Models, &s.Agent, &s.Tools, &s.WorkingDir, &s.ContextLength, &s.TurboEnabled, &s.WebSearchEnabled, &s.SelectedModel, &s.SidebarOpen, &s.LastHomeView, &s.OnboardingVersion, &s.ThinkEnabled, &s.ThinkLevel, &s.AutoUpdateEnabled, &s.ClaudeDesktopUsed, &s.LocalModelsEnabled, &s.CPUFallbackEnabled, &s.InferenceEnabled, &s.InferenceRuntime, &s.InferenceAutoFitEnabled, &s.InferenceManualLayers, &s.PluginsEnabled)
	if err != nil {
		return Settings{}, fmt.Errorf("get settings: %w", err)
	}
	s.InferenceRuntime = normalizeInferenceRuntime(s.InferenceRuntime)

	return s, nil
}

func (db *database) setSettings(s Settings) error {
	lastHomeView := strings.ToLower(strings.TrimSpace(s.LastHomeView))
	if lastHomeView != "chat" {
		lastHomeView = "chat"
	}

	_, err := db.conn.Exec(`
		UPDATE settings
		SET expose = ?, survey = ?, browser = ?, models = ?, agent = ?, tools = ?, working_dir = ?, context_length = ?, turbo_enabled = ?, websearch_enabled = ?, selected_model = ?, sidebar_open = ?, last_home_view = ?, onboarding_version = ?, think_enabled = ?, think_level = ?, auto_update_enabled = ?, claude_desktop_used = ?, local_models_enabled = ?, cpu_fallback_enabled = ?, inference_enabled = ?, inference_runtime = ?, inference_auto_fit_enabled = ?, inference_manual_layers = ?, plugins_enabled = ?
	`, s.Expose, s.Survey, s.Browser, s.Models, s.Agent, s.Tools, s.WorkingDir, s.ContextLength, s.TurboEnabled, s.WebSearchEnabled, s.SelectedModel, s.SidebarOpen, lastHomeView, s.OnboardingVersion, s.ThinkEnabled, s.ThinkLevel, s.AutoUpdateEnabled, s.ClaudeDesktopUsed, s.LocalModelsEnabled, s.CPUFallbackEnabled, s.InferenceEnabled, normalizeInferenceRuntime(s.InferenceRuntime), s.InferenceAutoFitEnabled, strings.TrimSpace(s.InferenceManualLayers), s.PluginsEnabled)
	if err != nil {
		return fmt.Errorf("set settings: %w", err)
	}
	return nil
}

func normalizeInferenceRuntime(runtime string) string {
	switch strings.TrimSpace(runtime) {
	case "", "auto":
		return "auto"
	case "cpu", "cuda_legacy", "cuda_v12", "cuda_v13", "rocm_v7_1", "rocm_v7_2", "vulkan":
		return strings.TrimSpace(runtime)
	default:
		return "auto"
	}
}

func (db *database) getPlugins() ([]Plugin, error) {
	rows, err := db.conn.Query(`
		SELECT id, name, provider, enabled, config_json, created_at, updated_at
		FROM plugins
		ORDER BY provider ASC, name ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query plugins: %w", err)
	}
	defer rows.Close()

	var plugins []Plugin
	for rows.Next() {
		var plugin Plugin
		var configText string
		if err := rows.Scan(
			&plugin.ID,
			&plugin.Name,
			&plugin.Provider,
			&plugin.Enabled,
			&configText,
			&plugin.CreatedAt,
			&plugin.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan plugin: %w", err)
		}
		plugin.Config = json.RawMessage(configText)
		plugins = append(plugins, plugin)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate plugins: %w", err)
	}

	return plugins, nil
}

func (db *database) upsertPlugin(plugin Plugin) error {
	_, err := db.conn.Exec(`
		INSERT INTO plugins (id, name, provider, enabled, config_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			provider = excluded.provider,
			enabled = excluded.enabled,
			config_json = excluded.config_json,
			updated_at = CURRENT_TIMESTAMP
	`, plugin.ID, plugin.Name, plugin.Provider, plugin.Enabled, string(plugin.Config))
	if err != nil {
		return fmt.Errorf("upsert plugin: %w", err)
	}
	return nil
}

func (db *database) setPluginEnabled(id string, enabled bool) error {
	result, err := db.conn.Exec(`
		UPDATE plugins
		SET enabled = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?
	`, enabled, id)
	if err != nil {
		return fmt.Errorf("set plugin enabled: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("plugin rows affected: %w", err)
	}
	if rowsAffected == 0 {
		return fmt.Errorf("plugin not found")
	}

	return nil
}

func (db *database) deletePlugin(id string) error {
	result, err := db.conn.Exec(`DELETE FROM plugins WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete plugin: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("plugin rows affected: %w", err)
	}
	if rowsAffected == 0 {
		return fmt.Errorf("plugin not found")
	}

	return nil
}

func (db *database) getPluginToolPreferences() ([]PluginToolPreference, error) {
	rows, err := db.conn.Query(`
		SELECT plugin_id, tool_name, enabled, updated_at
		FROM plugin_tool_preferences
		ORDER BY plugin_id ASC, tool_name ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query plugin tool preferences: %w", err)
	}
	defer rows.Close()

	var prefs []PluginToolPreference
	for rows.Next() {
		var pref PluginToolPreference
		if err := rows.Scan(&pref.PluginID, &pref.ToolName, &pref.Enabled, &pref.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan plugin tool preference: %w", err)
		}
		prefs = append(prefs, pref)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate plugin tool preferences: %w", err)
	}

	return prefs, nil
}

func (db *database) setPluginToolEnabled(pluginID, toolName string, enabled bool) error {
	if !enabled {
		result, err := db.conn.Exec(`
			INSERT INTO plugin_tool_preferences (plugin_id, tool_name, enabled, updated_at)
			SELECT id, ?, 0, CURRENT_TIMESTAMP
			FROM plugins
			WHERE id = ?
			ON CONFLICT(plugin_id, tool_name) DO UPDATE SET
				enabled = excluded.enabled,
				updated_at = CURRENT_TIMESTAMP
		`, toolName, pluginID)
		if err != nil {
			return fmt.Errorf("disable plugin tool: %w", err)
		}

		rowsAffected, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("plugin tool rows affected: %w", err)
		}
		if rowsAffected == 0 {
			return fmt.Errorf("plugin not found")
		}

		return nil
	}

	if _, err := db.conn.Exec(`
		DELETE FROM plugin_tool_preferences
		WHERE plugin_id = ? AND tool_name = ?
	`, pluginID, toolName); err != nil {
		return fmt.Errorf("enable plugin tool: %w", err)
	}

	var exists bool
	if err := db.conn.QueryRow(`SELECT EXISTS(SELECT 1 FROM plugins WHERE id = ?)`, pluginID).Scan(&exists); err != nil {
		return fmt.Errorf("check plugin exists: %w", err)
	}
	if !exists {
		return fmt.Errorf("plugin not found")
	}

	return nil
}

func (db *database) getModelMetadata() ([]ModelMetadata, error) {
	rows, err := db.conn.Query(`
		SELECT model_name, origin, source_path, display_name, created_at, updated_at
		FROM model_metadata
		ORDER BY model_name ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query model metadata: %w", err)
	}
	defer rows.Close()

	var metadata []ModelMetadata
	for rows.Next() {
		var item ModelMetadata
		if err := rows.Scan(&item.ModelName, &item.Origin, &item.SourcePath, &item.DisplayName, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan model metadata: %w", err)
		}
		metadata = append(metadata, item)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate model metadata: %w", err)
	}

	return metadata, nil
}

func (db *database) upsertModelMetadata(metadata ModelMetadata) error {
	_, err := db.conn.Exec(`
		INSERT INTO model_metadata (model_name, origin, source_path, display_name, created_at, updated_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(model_name) DO UPDATE SET
			origin = excluded.origin,
			source_path = excluded.source_path,
			display_name = excluded.display_name,
			updated_at = CURRENT_TIMESTAMP
	`, metadata.ModelName, metadata.Origin, metadata.SourcePath, metadata.DisplayName)
	if err != nil {
		return fmt.Errorf("upsert model metadata: %w", err)
	}
	return nil
}

func (db *database) setModelDisplayName(modelName, displayName string) error {
	_, err := db.conn.Exec(`
		INSERT INTO model_metadata (model_name, origin, display_name, created_at, updated_at)
		VALUES (?, 'ollama_storage', ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT(model_name) DO UPDATE SET
			display_name = excluded.display_name,
			updated_at = CURRENT_TIMESTAMP
	`, modelName, displayName)
	if err != nil {
		return fmt.Errorf("set model display name: %w", err)
	}
	return nil
}

func (db *database) deleteModelMetadata(modelName string) error {
	if _, err := db.conn.Exec(`DELETE FROM model_metadata WHERE model_name = ?`, modelName); err != nil {
		return fmt.Errorf("delete model metadata: %w", err)
	}
	return nil
}

func (db *database) isCloudSettingMigrated() (bool, error) {
	var migrated bool
	err := db.conn.QueryRow("SELECT cloud_setting_migrated FROM settings").Scan(&migrated)
	if err != nil {
		return false, fmt.Errorf("get cloud setting migration status: %w", err)
	}
	return migrated, nil
}

func (db *database) setCloudSettingMigrated(migrated bool) error {
	_, err := db.conn.Exec("UPDATE settings SET cloud_setting_migrated = ?", migrated)
	if err != nil {
		return fmt.Errorf("set cloud setting migration status: %w", err)
	}
	return nil
}

func (db *database) getAirplaneMode() (bool, error) {
	var airplaneMode bool
	err := db.conn.QueryRow("SELECT airplane_mode FROM settings").Scan(&airplaneMode)
	if err != nil {
		return false, fmt.Errorf("get airplane_mode: %w", err)
	}
	return airplaneMode, nil
}

func (db *database) getWindowSize() (int, int, error) {
	var width, height int
	err := db.conn.QueryRow("SELECT window_width, window_height FROM settings").Scan(&width, &height)
	if err != nil {
		return 0, 0, fmt.Errorf("get window size: %w", err)
	}
	return width, height, nil
}

func (db *database) setWindowSize(width, height int) error {
	_, err := db.conn.Exec("UPDATE settings SET window_width = ?, window_height = ?", width, height)
	if err != nil {
		return fmt.Errorf("set window size: %w", err)
	}
	return nil
}

func (db *database) isConfigMigrated() (bool, error) {
	var migrated bool
	err := db.conn.QueryRow("SELECT config_migrated FROM settings").Scan(&migrated)
	if err != nil {
		return false, fmt.Errorf("get config migrated: %w", err)
	}
	return migrated, nil
}

func (db *database) setConfigMigrated(migrated bool) error {
	_, err := db.conn.Exec("UPDATE settings SET config_migrated = ?", migrated)
	if err != nil {
		return fmt.Errorf("set config migrated: %w", err)
	}
	return nil
}

func (db *database) migrateV26ToV27() error {
	_, err := db.conn.Exec(`ALTER TABLE settings ADD COLUMN inference_auto_fit_enabled BOOLEAN NOT NULL DEFAULT 1;`)
	if err != nil && !duplicateColumnError(err) {
		return fmt.Errorf("add inference_auto_fit_enabled column: %w", err)
	}

	_, err = db.conn.Exec(`UPDATE settings SET schema_version = 27`)
	if err != nil {
		return fmt.Errorf("update schema version: %w", err)
	}

	return nil
}

func (db *database) migrateV27ToV28() error {
	_, err := db.conn.Exec(`ALTER TABLE settings ADD COLUMN inference_manual_layers TEXT NOT NULL DEFAULT '';`)
	if err != nil && !duplicateColumnError(err) {
		return fmt.Errorf("add inference_manual_layers column: %w", err)
	}

	_, err = db.conn.Exec(`UPDATE settings SET schema_version = 28`)
	if err != nil {
		return fmt.Errorf("update schema version: %w", err)
	}

	return nil
}

func (db *database) getSchemaVersion() (int, error) {
	var version int
	err := db.conn.QueryRow("SELECT schema_version FROM settings").Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("get schema version: %w", err)
	}
	return version, nil
}

func (db *database) setSchemaVersion(version int) error {
	_, err := db.conn.Exec("UPDATE settings SET schema_version = ?", version)
	if err != nil {
		return fmt.Errorf("set schema version: %w", err)
	}
	return nil
}

func (db *database) getUser() (*User, error) {
	var user User
	err := db.conn.QueryRow(`
		SELECT name, email, plan, cached_at
		FROM users
		LIMIT 1
	`).Scan(&user.Name, &user.Email, &user.Plan, &user.CachedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil // No user cached yet
		}
		return nil, fmt.Errorf("get user: %w", err)
	}

	return &user, nil
}

func (db *database) setUser(user User) error {
	if err := db.clearUser(); err != nil {
		return fmt.Errorf("before set: %w", err)
	}

	_, err := db.conn.Exec(`
		INSERT INTO users (name, email, plan, cached_at)
		VALUES (?, ?, ?, ?)
	`, user.Name, user.Email, user.Plan, user.CachedAt)
	if err != nil {
		return fmt.Errorf("set user: %w", err)
	}

	return nil
}

func (db *database) clearUser() error {
	_, err := db.conn.Exec("DELETE FROM users")
	if err != nil {
		return fmt.Errorf("clear user: %w", err)
	}
	return nil
}
