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
	"time"
)

// ArchiverStats is the row of pg_stat_archiver.
type ArchiverStats struct {
	ArchivedCount    int64
	LastArchivedWAL  string
	LastArchivedTime time.Time
	FailedCount      int64
	LastFailedWAL    string
	LastFailedTime   time.Time
}

// ArchiverStats reads pg_stat_archiver.
func (c *Client) ArchiverStats(ctx context.Context) (ArchiverStats, error) {
	var s ArchiverStats
	var archivedWAL, failedWAL sql.NullString
	var archivedAt, failedAt sql.NullTime
	err := c.db.QueryRowContext(ctx, `SELECT archived_count, last_archived_wal, last_archived_time,
		failed_count, last_failed_wal, last_failed_time FROM pg_stat_archiver`).
		Scan(&s.ArchivedCount, &archivedWAL, &archivedAt, &s.FailedCount, &failedWAL, &failedAt)
	if err != nil {
		return ArchiverStats{}, fmt.Errorf("failed to read pg_stat_archiver: %w", err)
	}
	s.LastArchivedWAL, s.LastFailedWAL = archivedWAL.String, failedWAL.String
	s.LastArchivedTime, s.LastFailedTime = archivedAt.Time, failedAt.Time
	return s, nil
}
