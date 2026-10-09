/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package postgres

import (
	"context"
	"database/sql"
	"fmt"
)

// ReplicationSlot is a row of pg_replication_slots.
type ReplicationSlot struct {
	Name     string
	Physical bool
	Active   bool
	// WALStatus is pg_replication_slots.wal_status: reserved, extended,
	// unreserved or lost (the WAL the slot needs was removed; the slot is
	// unusable).
	WALStatus string
}

// WALStatusLost is the wal_status of an invalidated slot.
const WALStatusLost = "lost"

// StandbyStatus is a row of pg_stat_replication: a WAL sender serving a
// standby (or a base backup).
type StandbyStatus struct {
	// ApplicationName is the standby's application_name (pgop sets the pod
	// name).
	ApplicationName string
	// State is the WAL sender state (startup, catchup, streaming, backup,
	// stopping).
	State string
}

// InRecovery reports whether the server is a standby (pg_is_in_recovery()).
func (c *Client) InRecovery(ctx context.Context) (bool, error) {
	var in bool
	if err := c.db.QueryRowContext(ctx, "SELECT pg_is_in_recovery()").Scan(&in); err != nil {
		return false, fmt.Errorf("failed to read pg_is_in_recovery(): %w", err)
	}
	return in, nil
}

// ReplicationSlots returns all replication slots.
func (c *Client) ReplicationSlots(ctx context.Context) ([]ReplicationSlot, error) {
	rows, err := c.db.QueryContext(ctx,
		"SELECT slot_name, slot_type = 'physical', active, COALESCE(wal_status, '') FROM pg_replication_slots ORDER BY slot_name")
	if err != nil {
		return nil, fmt.Errorf("failed to read pg_replication_slots: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ReplicationSlot
	for rows.Next() {
		var s ReplicationSlot
		if err := rows.Scan(&s.Name, &s.Physical, &s.Active, &s.WALStatus); err != nil {
			return nil, fmt.Errorf("failed to scan pg_replication_slots: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read pg_replication_slots: %w", err)
	}
	return out, nil
}

// CreatePhysicalReplicationSlot creates a physical replication slot. With
// reserve it reserves WAL right away, so a standby cloned from this point on
// finds all the WAL it needs; otherwise it only starts reserving WAL once a
// standby streams over it.
func (c *Client) CreatePhysicalReplicationSlot(ctx context.Context, name string, reserve bool) error {
	if _, err := c.db.ExecContext(ctx, "SELECT pg_create_physical_replication_slot($1, $2)", name, reserve); err != nil {
		return fmt.Errorf("failed to create replication slot %q: %w", name, err)
	}
	return nil
}

// DropReplicationSlot drops a replication slot. It fails while the slot is in
// use.
func (c *Client) DropReplicationSlot(ctx context.Context, name string) error {
	if _, err := c.db.ExecContext(ctx, "SELECT pg_drop_replication_slot($1)", name); err != nil {
		return fmt.Errorf("failed to drop replication slot %q: %w", name, err)
	}
	return nil
}

// ReplicationStatus returns pg_stat_replication. Requires a superuser (or
// pg_read_all_stats) to see the state of every WAL sender.
func (c *Client) ReplicationStatus(ctx context.Context) ([]StandbyStatus, error) {
	rows, err := c.db.QueryContext(ctx,
		"SELECT application_name, state FROM pg_stat_replication ORDER BY application_name")
	if err != nil {
		return nil, fmt.Errorf("failed to read pg_stat_replication: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []StandbyStatus
	for rows.Next() {
		var name, state sql.NullString
		if err := rows.Scan(&name, &state); err != nil {
			return nil, fmt.Errorf("failed to scan pg_stat_replication: %w", err)
		}
		out = append(out, StandbyStatus{ApplicationName: name.String, State: state.String})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read pg_stat_replication: %w", err)
	}
	return out, nil
}
