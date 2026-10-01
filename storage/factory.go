package storage

import (
	"log"
	"os"
	"strings"

	jsonstore "openrouter-bot/storage/json"
	pgstore "openrouter-bot/storage/postgres"
)

// Open initialises a Store from the requested storageType ("json" or "postgres").
// If PostgreSQL is requested but unavailable, it logs a warning and falls back
// to JSON storage so the bot continues running for simple deployments.
func Open(storageType, baseDir, postgresDSN string) Store {
	kind := strings.ToLower(strings.TrimSpace(storageType))
	if kind == "" {
		kind = strings.ToLower(strings.TrimSpace(os.Getenv("STORAGE_TYPE")))
	}
	if postgresDSN == "" {
		postgresDSN = strings.TrimSpace(os.Getenv("POSTGRES_DSN"))
		if postgresDSN == "" {
			postgresDSN = strings.TrimSpace(os.Getenv("DATABASE_URL"))
		}
	}

	if kind == "postgres" || kind == "postgresql" {
		store, err := pgstore.New("postgres", postgresDSN)
		if err == nil {
			log.Printf("Storage initialised: PostgreSQL")
			return store
		}
		log.Printf("Warning: PostgreSQL storage failed (%v), falling back to JSON storage in %s", err, baseDir)
	}

	return jsonstore.New(baseDir)
}
