package main

import (
	"context"
	"log"
	"time"
)

// Scheduler handles background tasks like stale agent cleanup
type Scheduler struct {
	db              *DB
	staleTimeout    time.Duration
	cleanupInterval time.Duration
}

// NewScheduler creates a new Scheduler
func NewScheduler(db *DB, staleTimeout, cleanupInterval time.Duration) *Scheduler {
	return &Scheduler{
		db:              db,
		staleTimeout:    staleTimeout,
		cleanupInterval: cleanupInterval,
	}
}

// Run starts the scheduler's background tasks
// It blocks until the context is cancelled
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.cleanupInterval)
	defer ticker.Stop()

	log.Printf("Scheduler started: cleanup every %v, stale timeout %v", s.cleanupInterval, s.staleTimeout)

	// Run immediately on start
	s.cleanupStaleAgents()

	for {
		select {
		case <-ctx.Done():
			log.Println("Scheduler stopped")
			return
		case <-ticker.C:
			s.cleanupStaleAgents()
		}
	}
}

// cleanupStaleAgents marks agents as offline if they haven't sent a heartbeat
func (s *Scheduler) cleanupStaleAgents() {
	count, err := s.db.MarkStaleAgentsOffline(s.staleTimeout)
	if err != nil {
		log.Printf("Error cleaning up stale agents: %v", err)
		return
	}

	if count > 0 {
		log.Printf("Marked %d stale agents as offline", count)
	}
}
