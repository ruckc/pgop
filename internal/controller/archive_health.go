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
	"time"

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
)

// ArchiveServer reads WAL archiving statistics from the primary.
type ArchiveServer interface {
	ArchiverStats(ctx context.Context) (postgres.ArchiverStats, error)
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
	return set(archivingCondition(stats))
}
