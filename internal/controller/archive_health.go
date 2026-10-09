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

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	postgresv1alpha1 "github.com/ruckc/pgop/api/v1alpha1"
	"github.com/ruckc/pgop/internal/postgres"
)

const (
	// archiveHealthInterval is how often WAL archiving is checked.
	archiveHealthInterval = time.Minute

	reasonArchiving        = "Archiving"
	reasonArchiveFailing   = "ArchiveFailing"
	reasonNoWALArchivedYet = "NoWALArchivedYet"
	reasonArchiveUnknown   = "Unknown"
	// reasonWALDropped: pgBackRest dropped WAL (queue full); latched until a
	// backup that started afterwards succeeded.
	reasonWALDropped = "WALDropped"
)

// ArchiveServer reads WAL archiving statistics from the primary.
type ArchiveServer interface {
	ArchiverStats(ctx context.Context) (postgres.ArchiverStats, error)
	TailFile(ctx context.Context, name string) (string, error)
	Close() error
}

func connectArchiveServer(_ context.Context, cfg postgres.ConnectionConfig) (ArchiveServer, error) {
	return postgres.NewClient(cfg)
}

// archivingCondition turns pg_stat_archiver into the WALArchiving condition.
// Archiving is failing when the last failure is newer than the last success.
func archivingCondition(s postgres.ArchiverStats) (metav1.ConditionStatus, string, string) {
	if s.FailedCount > 0 && !s.LastFailedTime.IsZero() &&
		(s.LastArchivedTime.IsZero() || s.LastFailedTime.After(s.LastArchivedTime)) {
		return metav1.ConditionFalse, reasonArchiveFailing, fmt.Sprintf(
			"archiving WAL %s failed (%d failures, last at %s); see the postgresql container log. "+
				"WAL waits in pg_wal up to archive-push-queue-max; beyond it pgBackRest drops WAL, "+
				"and point-in-time recovery has a gap until the next full backup",
			s.LastFailedWAL, s.FailedCount, s.LastFailedTime.UTC().Format(time.RFC3339))
	}
	if s.ArchivedCount == 0 {
		return metav1.ConditionUnknown, reasonNoWALArchivedYet, "No WAL segment has been archived yet"
	}
	return metav1.ConditionTrue, reasonArchiving, fmt.Sprintf("Last archived WAL %s at %s",
		s.LastArchivedWAL, s.LastArchivedTime.UTC().Format(time.RFC3339))
}

// reconcileArchiveHealth sets the WALArchiving condition from the primary's
// pg_stat_archiver while the Cluster has a physical Backup (and removes it
// otherwise). Never fails the reconcile. Returns when to check again.
func (r *ClusterReconciler) reconcileArchiveHealth(ctx context.Context, cluster *postgresv1alpha1.Cluster,
	backup *postgresv1alpha1.Backup, ready bool) time.Duration {
	if backup == nil {
		meta.RemoveStatusCondition(&cluster.Status.Conditions, ConditionTypeWALArchiving)
		return 0
	}
	if !ready {
		return archiveHealthInterval
	}
	set := func(status metav1.ConditionStatus, reason, msg string) time.Duration {
		meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeWALArchiving,
			Status:             status,
			ObservedGeneration: cluster.Generation,
			Reason:             reason,
			Message:            msg,
		})
		return archiveHealthInterval
	}
	cluster.Status.SecretName = cluster.Name + "-credentials"
	cfg, err := operatorConnectionConfig(ctx, r.Client, cluster, defaultDatabaseName)
	if err != nil {
		return set(metav1.ConditionUnknown, reasonArchiveUnknown, err.Error())
	}
	connect := r.ConnectArchiveServer
	if connect == nil {
		connect = connectArchiveServer
	}
	srv, err := connect(ctx, cfg)
	if err != nil {
		return set(metav1.ConditionUnknown, reasonArchiveUnknown, fmt.Sprintf("Cannot connect to the primary: %v", err))
	}
	defer func() { _ = srv.Close() }()
	stats, err := srv.ArchiverStats(ctx)
	if err != nil {
		return set(metav1.ConditionUnknown, reasonArchiveUnknown, err.Error())
	}
	marker, err := srv.TailFile(ctx, walDropMarkerFile)
	if err != nil {
		return set(metav1.ConditionUnknown, reasonArchiveUnknown, err.Error())
	}
	r.recordWALDrop(cluster, marker)
	if drop := cluster.Status.LastWALDrop; drop != nil && drop.ClosedBy == "" {
		closedBy, err := r.backupStartedAfter(ctx, backup, drop.Time.Time)
		if err != nil {
			return set(metav1.ConditionUnknown, reasonArchiveUnknown, err.Error())
		}
		if closedBy == "" {
			// Latched until a backup started after the drop succeeds:
			// archiving itself may look healthy again.
			return set(metav1.ConditionFalse, reasonWALDropped, fmt.Sprintf(
				"pgBackRest dropped WAL segment %s at %s because the archive queue exceeded archive-push-queue-max; "+
					"point-in-time recovery across it is impossible. This stays False until a backup (full, "+
					"differential or incremental) that starts after the drop succeeds; take one now",
				drop.Segment, drop.Time.UTC().Format(time.RFC3339)))
		}
		drop.ClosedBy = closedBy
	}
	return set(archivingCondition(stats))
}

// recordWALDrop records the newest drop from the drop marker file
// ("<RFC3339 time> <segment>" lines) in status.lastWALDrop, with a Warning
// event, when it is newer than the one recorded.
func (r *ClusterReconciler) recordWALDrop(cluster *postgresv1alpha1.Cluster, marker string) {
	var at time.Time
	var segment string
	for line := range strings.Lines(marker) {
		ts, seg, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		if t, err := time.Parse(time.RFC3339, ts); err == nil && t.After(at) {
			at, segment = t, seg
		}
	}
	if at.IsZero() {
		return
	}
	if last := cluster.Status.LastWALDrop; last != nil && !at.After(last.Time.Time) {
		return
	}
	cluster.Status.LastWALDrop = &postgresv1alpha1.WALDropRecord{Time: metav1.NewTime(at), Segment: segment}
	if r.Recorder != nil {
		r.Recorder.Eventf(cluster, nil, corev1.EventTypeWarning, reasonWALDropped, "ArchiveWAL",
			"pgBackRest dropped WAL segment %s at %s (archive queue full); point-in-time recovery across it is impossible "+
				"until a new backup", segment, at.Format(time.RFC3339))
	}
}

// backupStartedAfter returns the name of a successful BackupRun of the
// Backup that started after t ("" if none).
func (r *ClusterReconciler) backupStartedAfter(ctx context.Context, backup *postgresv1alpha1.Backup, t time.Time) (string, error) {
	runs := &postgresv1alpha1.BackupRunList{}
	if err := r.List(ctx, runs, client.InNamespace(backup.Namespace), client.MatchingLabels{
		LabelAppName:     appNameBackup,
		LabelAppInstance: backup.Name,
	}); err != nil {
		return "", err
	}
	for _, run := range runs.Items {
		if run.Status.Phase == postgresv1alpha1.BackupRunPhaseSucceeded && run.Status.StartTime != nil &&
			run.Status.StartTime.After(t) {
			return run.Name, nil
		}
	}
	return "", nil
}
