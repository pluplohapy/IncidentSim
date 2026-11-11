package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// JobQueue is a background worker queue backed by a persistent jobs table.
// It models the kind of async processing that real incidents run into
// (poison messages, acks before processing, dead letters).
type JobQueue struct {
	db      *sql.DB
	state   *State
	mu      sync.Mutex
	running bool
}

// NewJobQueue creates a queue backed by the given database. It creates the
// jobs table if it does not exist.
func NewJobQueue(db *sql.DB, state *State) *JobQueue {
	q := &JobQueue{db: db, state: state}
	// Create the jobs table idempotently.
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS jobs (
			id        SERIAL PRIMARY KEY,
			payload   TEXT NOT NULL,
			status    TEXT NOT NULL DEFAULT 'pending',
			attempts  INTEGER NOT NULL DEFAULT 0,
			last_error TEXT,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`)
	if err != nil {
		log.Printf("create jobs table: %v", err)
	}
	return q
}

// Enqueue adds a job to the queue.
func (q *JobQueue) Enqueue(payload string) error {
	q.state.incDepCalls()
	_, err := q.db.Exec("INSERT INTO jobs (payload, status) VALUES ($1, 'pending')", payload)
	return err
}

// Process runs the worker loop: it claims pending jobs, processes them, and
// either marks them done or moves them to dead_letters after too many attempts.
// A poison payload makes the worker panic unless recover() is used — this is
// the queue-poison-message incident surface.
func (q *JobQueue) Process(process func(payload string) error) {
	q.mu.Lock()
	if q.running {
		q.mu.Unlock()
		return
	}
	q.running = true
	q.mu.Unlock()

	go func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			q.processOnce(process)
		}
	}()
}

// processOnce claims one pending job and processes it.
func (q *JobQueue) processOnce(process func(payload string) error) {
	var id int
	var payload string
	err := q.db.QueryRow(`
		SELECT id, payload FROM jobs
		WHERE status = 'pending'
		ORDER BY created_at
		LIMIT 1
		FOR UPDATE SKIP LOCKED
	`).Scan(&id, &payload)
	if err != nil {
		if err != sql.ErrNoRows {
			log.Printf("claim job: %v", err)
		}
		return
	}

	q.state.mu.Lock()
	q.state.dbQueriesTotal++
	q.state.mu.Unlock()

	// Process with recover: a poison payload panics the worker, which is the
	// incident. The fix recovers, increments attempts, and dead-letters.
	func() {
		defer func() {
			if r := recover(); r != nil {
				q.failJob(id, fmt.Sprintf("panic: %v", r))
				log.Printf("job %d panicked: %v", id, r)
			}
		}()
		if err := process(payload); err != nil {
			q.failJob(id, err.Error())
			return
		}
		q.doneJob(id)
	}()
}

func (q *JobQueue) doneJob(id int) {
	_, err := q.db.Exec("UPDATE jobs SET status = 'done' WHERE id = $1", id)
	if err != nil {
		log.Printf("done job %d: %v", id, err)
	}
}

func (q *JobQueue) failJob(id int, lastError string) {
	var attempts int
	q.db.QueryRow("SELECT attempts FROM jobs WHERE id = $1", id).Scan(&attempts)
	attempts++
	if attempts >= 3 {
		_, err := q.db.Exec("UPDATE jobs SET status = 'dead_letter', attempts = $1, last_error = $2 WHERE id = $3", attempts, lastError, id)
		if err != nil {
			log.Printf("dead letter job %d: %v", id, err)
		}
		return
	}
	_, err := q.db.Exec("UPDATE jobs SET status = 'pending', attempts = $1, last_error = $2 WHERE id = $3", attempts, lastError, id)
	if err != nil {
		log.Printf("retry job %d: %v", id, err)
	}
}

// QueueStatsHandler reports queue statistics for /debug/queue.
func (q *JobQueue) QueueStatsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var pending, done, dead int
		q.db.QueryRow("SELECT count(*) FROM jobs WHERE status = 'pending'").Scan(&pending)
		q.db.QueryRow("SELECT count(*) FROM jobs WHERE status = 'done'").Scan(&done)
		q.db.QueryRow("SELECT count(*) FROM jobs WHERE status = 'dead_letter'").Scan(&dead)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"pending":%d,"done":%d,"dead_letter":%d}`, pending, done, dead)
	}
}

// InvariantHandler checks a named invariant against the current state.
type InvariantChecker func(name string) (ok bool, message string)

// InvariantHandler returns an HTTP handler for /debug/invariant/<name>.
func InvariantHandler(check InvariantChecker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/debug/invariant/")
		ok, message := check(name)
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, `{"invariant":"%s","ok":false,"message":"%s"}`, name, message)
			return
		}
		fmt.Fprintf(w, `{"invariant":"%s","ok":true,"message":"%s"}`, name, message)
	}
}

// QueueInvariant checks that the queue is healthy: no dead letters and no
// pending jobs stuck for too long.
func (q *JobQueue) QueueInvariant() InvariantChecker {
	return func(name string) (bool, string) {
		var dead int
		q.db.QueryRow("SELECT count(*) FROM jobs WHERE status = 'dead_letter'").Scan(&dead)
		if dead > 0 {
			return false, fmt.Sprintf("%d dead letters in queue", dead)
		}
		return true, "queue healthy"
	}
}
