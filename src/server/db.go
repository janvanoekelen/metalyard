package main

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// DB wraps the SQLite database connection
type DB struct {
	*sql.DB
}

// OpenDB opens the SQLite database and runs migrations
func OpenDB(path string) (*DB, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// Test connection
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping database: %w", err)
	}

	// Run migrations
	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return &DB{db}, nil
}

// migrate runs the database migrations
func migrate(db *sql.DB) error {
	migrations := []string{
		`CREATE TABLE IF NOT EXISTS agents (
			agent_id        TEXT PRIMARY KEY,
			api_key_hash    TEXT NOT NULL,
			name            TEXT,
			status          TEXT NOT NULL DEFAULT 'offline',
			last_heartbeat  INTEGER NOT NULL,
			capabilities    TEXT NOT NULL DEFAULT '{}',
			current_load    INTEGER NOT NULL DEFAULT 0,
			created_at      INTEGER NOT NULL,
			updated_at      INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_agents_status ON agents(status)`,
		`CREATE INDEX IF NOT EXISTS idx_agents_last_heartbeat ON agents(last_heartbeat)`,
		`CREATE TABLE IF NOT EXISTS agent_models (
			agent_id        TEXT NOT NULL,
			model_name      TEXT NOT NULL,
			quantization    TEXT,
			max_context     INTEGER NOT NULL DEFAULT 4096,
			PRIMARY KEY (agent_id, model_name),
			FOREIGN KEY (agent_id) REFERENCES agents(agent_id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_agent_models_model ON agent_models(model_name)`,
	}

	for _, m := range migrations {
		if _, err := db.Exec(m); err != nil {
			return fmt.Errorf("execute migration: %w", err)
		}
	}

	log.Println("Database migrations completed")
	return nil
}

// Agent represents a registered GPU agent
type Agent struct {
	ID            string
	APIKeyHash    string
	Name          string
	Status        string
	LastHeartbeat time.Time
	Capabilities  string
	CurrentLoad   int
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// AgentModel represents a model an agent can serve
type AgentModel struct {
	AgentID      string
	ModelName    string
	Quantization string
	MaxContext   int
}

// RegisterAgent inserts or updates an agent in the database
func (db *DB) RegisterAgent(agent *Agent, models []AgentModel) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().Unix()

	// Upsert agent
	_, err = tx.Exec(`
		INSERT INTO agents (agent_id, api_key_hash, name, status, last_heartbeat, capabilities, current_load, created_at, updated_at)
		VALUES (?, ?, ?, 'online', ?, ?, 0, ?, ?)
		ON CONFLICT(agent_id) DO UPDATE SET
			name = excluded.name,
			status = 'online',
			last_heartbeat = excluded.last_heartbeat,
			capabilities = excluded.capabilities,
			updated_at = excluded.updated_at
	`, agent.ID, agent.APIKeyHash, agent.Name, now, agent.Capabilities, now, now)
	if err != nil {
		return fmt.Errorf("upsert agent: %w", err)
	}

	// Clear existing models
	_, err = tx.Exec(`DELETE FROM agent_models WHERE agent_id = ?`, agent.ID)
	if err != nil {
		return fmt.Errorf("delete old models: %w", err)
	}

	// Insert new models
	for _, m := range models {
		_, err = tx.Exec(`
			INSERT INTO agent_models (agent_id, model_name, quantization, max_context)
			VALUES (?, ?, ?, ?)
		`, agent.ID, m.ModelName, m.Quantization, m.MaxContext)
		if err != nil {
			return fmt.Errorf("insert model: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	return nil
}

// UpdateHeartbeat updates the agent's last heartbeat time and status
func (db *DB) UpdateHeartbeat(agentID string, capabilities string) error {
	now := time.Now().Unix()
	result, err := db.Exec(`
		UPDATE agents
		SET last_heartbeat = ?, capabilities = ?, status = 'online', updated_at = ?
		WHERE agent_id = ?
	`, now, capabilities, now, agentID)
	if err != nil {
		return fmt.Errorf("update heartbeat: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if rows == 0 {
		return fmt.Errorf("agent not found: %s", agentID)
	}

	return nil
}

// MarkStaleAgentsOffline marks agents as offline if they haven't sent a heartbeat recently
func (db *DB) MarkStaleAgentsOffline(timeout time.Duration) (int64, error) {
	cutoff := time.Now().Add(-timeout).Unix()
	result, err := db.Exec(`
		UPDATE agents
		SET status = 'offline', updated_at = ?
		WHERE status = 'online' AND last_heartbeat < ?
	`, time.Now().Unix(), cutoff)
	if err != nil {
		return 0, fmt.Errorf("mark stale agents: %w", err)
	}

	return result.RowsAffected()
}

// GetAllAgents returns all agents for the admin endpoint
func (db *DB) GetAllAgents() ([]Agent, error) {
	rows, err := db.Query(`
		SELECT agent_id, api_key_hash, name, status, last_heartbeat, capabilities, current_load, created_at, updated_at
		FROM agents
		ORDER BY status DESC, last_heartbeat DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("query agents: %w", err)
	}
	defer rows.Close()

	var agents []Agent
	for rows.Next() {
		var a Agent
		var lastHB, createdAt, updatedAt int64
		err := rows.Scan(&a.ID, &a.APIKeyHash, &a.Name, &a.Status, &lastHB, &a.Capabilities, &a.CurrentLoad, &createdAt, &updatedAt)
		if err != nil {
			return nil, fmt.Errorf("scan agent: %w", err)
		}
		a.LastHeartbeat = time.Unix(lastHB, 0)
		a.CreatedAt = time.Unix(createdAt, 0)
		a.UpdatedAt = time.Unix(updatedAt, 0)
		agents = append(agents, a)
	}

	return agents, rows.Err()
}

// GetOnlineAgents returns only online agents
func (db *DB) GetOnlineAgents() ([]Agent, error) {
	rows, err := db.Query(`
		SELECT agent_id, api_key_hash, name, status, last_heartbeat, capabilities, current_load, created_at, updated_at
		FROM agents
		WHERE status = 'online'
		ORDER BY current_load ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query online agents: %w", err)
	}
	defer rows.Close()

	var agents []Agent
	for rows.Next() {
		var a Agent
		var lastHB, createdAt, updatedAt int64
		err := rows.Scan(&a.ID, &a.APIKeyHash, &a.Name, &a.Status, &lastHB, &a.Capabilities, &a.CurrentLoad, &createdAt, &updatedAt)
		if err != nil {
			return nil, fmt.Errorf("scan agent: %w", err)
		}
		a.LastHeartbeat = time.Unix(lastHB, 0)
		a.CreatedAt = time.Unix(createdAt, 0)
		a.UpdatedAt = time.Unix(updatedAt, 0)
		agents = append(agents, a)
	}

	return agents, rows.Err()
}

// GetAgentModels returns all models for an agent
func (db *DB) GetAgentModels(agentID string) ([]AgentModel, error) {
	rows, err := db.Query(`
		SELECT agent_id, model_name, quantization, max_context
		FROM agent_models
		WHERE agent_id = ?
	`, agentID)
	if err != nil {
		return nil, fmt.Errorf("query agent models: %w", err)
	}
	defer rows.Close()

	var models []AgentModel
	for rows.Next() {
		var m AgentModel
		err := rows.Scan(&m.AgentID, &m.ModelName, &m.Quantization, &m.MaxContext)
		if err != nil {
			return nil, fmt.Errorf("scan model: %w", err)
		}
		models = append(models, m)
	}

	return models, rows.Err()
}

// AgentExists checks if an agent exists by ID
func (db *DB) AgentExists(agentID string) (bool, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM agents WHERE agent_id = ?`, agentID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check agent exists: %w", err)
	}
	return count > 0, nil
}
