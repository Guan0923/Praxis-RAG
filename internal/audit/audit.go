// Package audit records authenticated knowledge-base activity using explicit metadata.
package audit

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

type actorKey struct{}

func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// Actor returns the identity supplied by the authentication layer.
func Actor(ctx context.Context) string {
	actor, _ := ctx.Value(actorKey{}).(string)
	return actor
}

type Target struct {
	Name    string
	Outcome string
}
type Entry struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	Actor     string `json:"actor"`
	Channel   string `json:"channel"`
	Action    string `json:"action"`
	Namespace string `json:"namespace"`
	Target    string `json:"target"`
	Outcome   string `json:"outcome"`
}
type Filter struct{ Namespace, Actor, Action, Target, Before string }
type Writer interface {
	Append(context.Context, []Entry) error
}
type Repository interface {
	Writer
	List(context.Context, Filter, int) ([]Entry, error)
}
type Store struct{ DB *sql.DB }

// Record writes the outcomes of one operation together. A disconnected client
// cannot cancel the audit of a mutation that has already completed.
func Record(ctx context.Context, writer Writer, logger *slog.Logger, channel, action, namespace string, targets ...Target) {
	if writer == nil || len(targets) == 0 {
		return
	}
	now := time.Now().UTC()
	actor := Actor(ctx)
	entries := make([]Entry, 0, len(targets))
	for _, target := range targets {
		entries = append(entries, Entry{
			ID: fmt.Sprintf("%019d-%s", now.UnixNano(), rand.Text()), CreatedAt: now.Format(time.RFC3339Nano),
			Actor: actor, Channel: channel, Action: action, Namespace: namespace,
			Target: target.Name, Outcome: target.Outcome,
		})
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := writer.Append(writeCtx, entries); err != nil {
		logger.Error("操作审计写入失败", "channel", channel, "action", action, "namespace", namespace, "count", len(entries))
	}
}

func (s Store) Append(ctx context.Context, entries []Entry) error {
	placeholders := make([]string, 0, len(entries))
	args := make([]any, 0, len(entries)*8)
	for _, e := range entries {
		placeholders = append(placeholders, "(?,?,?,?,?,?,?,?)")
		args = append(args, e.ID, e.CreatedAt, e.Actor, e.Channel, e.Action, e.Namespace, e.Target, e.Outcome)
	}
	_, err := s.DB.ExecContext(ctx, "INSERT INTO audit_events (id,created_at,actor,channel,action,namespace,target,outcome) VALUES "+strings.Join(placeholders, ","), args...)
	return err
}

// List uses the time-ordered event ID as a stable cursor.
func (s Store) List(ctx context.Context, filter Filter, limit int) ([]Entry, error) {
	query := "SELECT id,created_at,actor,channel,action,namespace,target,outcome FROM audit_events WHERE 1=1"
	var args []any
	filters := []struct{ clause, value string }{
		{"namespace = ?", filter.Namespace},
		{"LOCATE(?,actor) > 0", filter.Actor},
		{"target = ?", filter.Target},
		{"action = ?", filter.Action},
		{"id < ?", filter.Before},
	}
	for _, f := range filters {
		if f.value != "" {
			query += " AND " + f.clause
			args = append(args, f.value)
		}
	}
	rows, err := s.DB.QueryContext(ctx, query+" ORDER BY id DESC LIMIT ?", append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Entry, 0, limit)
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.CreatedAt, &e.Actor, &e.Channel, &e.Action, &e.Namespace, &e.Target, &e.Outcome); err != nil {
			return nil, err
		}
		items = append(items, e)
	}
	return items, rows.Err()
}

// SearchTargets keeps only distinct source identifiers from the returned evidence.
func SearchTargets(payload map[string]any) []Target {
	seen := map[string]bool{}
	var targets []Target
	add := func(hit map[string]any) {
		name, _ := hit["source"].(string)
		if name == "" {
			name, _ = hit["document_id"].(string)
		}
		if name != "" && !seen[name] {
			targets = append(targets, Target{Name: name, Outcome: "returned"})
			seen[name] = true
		}
	}
	switch hits := payload["results"].(type) {
	case []map[string]any:
		for _, hit := range hits {
			add(hit)
		}
	case []any:
		for _, raw := range hits {
			hit, _ := raw.(map[string]any)
			add(hit)
		}
	}
	if len(targets) == 0 {
		return []Target{{Outcome: "success"}}
	}
	return targets
}
