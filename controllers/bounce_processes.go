/*
 * bounce_processes.go
 *
 * This source file is part of the FoundationDB open source project
 *
 * Copyright 2019-2021 Apple Inc. and the FoundationDB project authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package controllers

import (
	"context"
	"fmt"
	"time"

	"github.com/FoundationDB/fdb-kubernetes-operator/internal/restarts"

	fdbv1beta2 "github.com/FoundationDB/fdb-kubernetes-operator/api/v1beta2"
	"github.com/FoundationDB/fdb-kubernetes-operator/internal"
	"github.com/FoundationDB/fdb-kubernetes-operator/pkg/fdbadminclient"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/pointer"
)

// bounceProcesses provides a reconciliation step for bouncing fdbserver
// processes.
type bounceProcesses struct{}

// reconcile runs the reconciler's work.
func (bounceProcesses) reconcile(ctx context.Context, r *FoundationDBClusterReconciler, cluster *fdbv1beta2.FoundationDBCluster) *requeue {
	if !pointer.BoolDeref(cluster.Spec.AutomationOptions.KillProcesses, true) {
		return nil
	}

	logger := log.WithValues("namespace", cluster.Namespace, "cluster", cluster.Name, "reconciler", "bounceProcesses")
	adminClient, err := r.getDatabaseClientProvider().GetAdminClient(cluster, r)
	if err != nil {
		return &requeue{curError: err}
	}
	defer adminClient.Close()

	status, err := adminClient.GetStatus()
	if err != nil {
		return &requeue{curError: err}
	}

	minimumUptime, addressMap, err := internal.GetMinimumUptimeAndAddressMap(cluster, status, r.EnableRecoveryState)
	if err != nil {
		return &requeue{curError: err}
	}

	addresses, req := getProcessesReadyForRestart(logger, cluster, addressMap)
	if req != nil {
		return req
	}

	if len(addresses) == 0 {
		return nil
	}

	if minimumUptime < float64(cluster.GetMinimumUptimeSecondsForBounce()) {
		r.Recorder.Event(cluster, corev1.EventTypeNormal, "NeedsBounce",
			fmt.Sprintf("Spec require a bounce of some processes, but the cluster has only been up for %f seconds", minimumUptime))
		cluster.Status.Generations.NeedsBounce = cluster.ObjectMeta.Generation
		err = r.updateOrApply(ctx, cluster)
		if err != nil {
			logger.Error(err, "Error updating cluster status")
		}

		// Retry after we waited the minimum uptime
		return &requeue{
			message: "Cluster needs to stabilize before bouncing",
			delay:   time.Second * time.Duration(cluster.GetMinimumUptimeSecondsForBounce()-int(minimumUptime)),
		}
	}

	var lockClient fdbadminclient.LockClient
	useLocks := cluster.ShouldUseLocks()
	if useLocks {
		lockClient, err = r.getLockClient(cluster)
		if err != nil {
			return &requeue{curError: err}
		}
	}
	version, err := fdbv1beta2.ParseFdbVersion(cluster.Spec.Version)
	if err != nil {
		return &requeue{curError: err}
	}

	upgrading := cluster.Status.RunningVersion != cluster.Spec.Version

	if useLocks && upgrading {
		processGroupIDs := make([]string, 0, len(cluster.Status.ProcessGroups))
		for _, processGroup := range cluster.Status.ProcessGroups {
			processGroupIDs = append(processGroupIDs, processGroup.ProcessGroupID)
		}
		err = lockClient.AddPendingUpgrades(version, processGroupIDs)
		if err != nil {
			return &requeue{curError: err}
		}
	}

	hasLock, err := r.takeLock(cluster, fmt.Sprintf("bouncing processes: %v", addresses))
	if !hasLock {
		return &requeue{curError: err}
	}

	if useLocks && upgrading {
		var req *requeue
		addresses, req = getAddressesForUpgrade(logger, r, status, lockClient, cluster, version)
		if req != nil {
			return req
		}
		if addresses == nil {
			return &requeue{curError: fmt.Errorf("unknown error when getting addresses that are ready for upgrade")}
		}
	}

	logger.Info("Bouncing processes", "addresses", addresses, "upgrading", upgrading)
	r.Recorder.Event(cluster, corev1.EventTypeNormal, "BouncingProcesses", fmt.Sprintf("Bouncing processes: %v", addresses))
	err = adminClient.KillProcesses(addresses)
	if err != nil {
		return &requeue{curError: err}
	}

	// If the cluster was upgraded we will requeue and let the update_status command set the correct version.
	// Updating the version in this method has the drawback that we upgrade the version independent of the success
	// of the kill command. The kill command is not reliable, which means that some kill request might not be
	// delivered and the return value will still not contain any error.
	if upgrading {
		return &requeue{message: "fetch latest status after upgrade"}
	}

	return nil
}

// getProcessesReadyForRestart returns a slice of process addresses that can be restarted. If addresses are missing or not all processes
// have the latest configuration this method will return a requeue struct with more details.
func getProcessesReadyForRestart(logger logr.Logger, cluster *fdbv1beta2.FoundationDBCluster, addressMap map[string][]fdbv1beta2.ProcessAddress) ([]fdbv1beta2.ProcessAddress, *requeue) {
	processesToBounce := fdbv1beta2.FilterByConditions(cluster.Status.ProcessGroups, restarts.GetFilterConditions(cluster), true)
	addresses := make([]fdbv1beta2.ProcessAddress, 0, len(processesToBounce))
	allSynced := true
	var missingAddress []string

	for _, process := range processesToBounce {
		processGroup := fdbv1beta2.FindProcessGroupByID(cluster.Status.ProcessGroups, process)
		if cluster.SkipProcessGroup(processGroup) {
			continue
		}

		// Ignore processes that are missing for more than 30 seconds e.g. if the process is network partitioned.
		// This is required since the update status will not update the SidecarUnreachable setting if a process is
		// missing in the status.
		if missingTime := processGroup.GetConditionTime(fdbv1beta2.MissingProcesses); missingTime != nil {
			if time.Unix(*missingTime, 0).Add(cluster.GetIgnoreMissingProcessesSeconds()).Before(time.Now()) {
				logger.Info("ignore process group with missing process", "processGroupID", processGroup.ProcessGroupID)
				continue
			}
		}

		if addressMap[process] == nil {
			missingAddress = append(missingAddress, process)
			continue
		}

		addresses = append(addresses, addressMap[process]...)

		if processGroup.GetConditionTime(fdbv1beta2.IncorrectConfigMap) != nil {
			allSynced = false
			logger.Info("Waiting for dynamic Pod config update", "processGroupID", processGroup.ProcessGroupID)
		}
	}

	if len(missingAddress) > 0 {
		return nil, &requeue{curError: fmt.Errorf("could not find address for processes: %s", missingAddress), delayedRequeue: true}
	}

	if !allSynced {
		return nil, &requeue{message: "Waiting for config map to sync to all pods", delayedRequeue: true}
	}

	counts, err := cluster.GetProcessCountsWithDefaults()
	if err != nil {
		return nil, &requeue{
			curError: err,
		}
	}

	// If we upgrade the cluster wait until all processes are ready for the restart. In the future we can adjust this
	// to only be a requirement for version incompatible upgrades. In addition we probably only want to block for a
	// certain threshold either as a percentage or as a fixed number (which could also be 0).
	if cluster.IsBeingUpgraded() && counts.Total() != len(addresses) {
		return nil, &requeue{
			message:        fmt.Sprintf("expected %d processes, got %d processes ready to restart", counts.Total(), len(addresses)),
			delayedRequeue: true}
	}

	return addresses, nil
}

// getAddressesForUpgrade checks that all processes in a cluster are ready to be
// upgraded and returns the full list of addresses.
func getAddressesForUpgrade(logger logr.Logger, r *FoundationDBClusterReconciler, databaseStatus *fdbv1beta2.FoundationDBStatus, lockClient fdbadminclient.LockClient, cluster *fdbv1beta2.FoundationDBCluster, version fdbv1beta2.Version) ([]fdbv1beta2.ProcessAddress, *requeue) {
	pendingUpgrades, err := lockClient.GetPendingUpgrades(version)
	if err != nil {
		return nil, &requeue{curError: err}
	}

	if !databaseStatus.Client.DatabaseStatus.Available {
		r.Recorder.Event(cluster, corev1.EventTypeNormal, "UpgradeRequeued", "Database is unavailable")
		return nil, &requeue{message: "Deferring upgrade until database is available"}
	}

	notReadyProcesses := make([]string, 0)
	addresses := make([]fdbv1beta2.ProcessAddress, 0, len(databaseStatus.Cluster.Processes))
	for _, process := range databaseStatus.Cluster.Processes {
		processID := process.Locality[fdbv1beta2.FDBLocalityInstanceIDKey]
		if process.Version == version.String() {
			continue
		}
		if pendingUpgrades[processID] {
			addresses = append(addresses, process.Address)
		} else {
			notReadyProcesses = append(notReadyProcesses, processID)
		}
	}

	if len(notReadyProcesses) > 0 {
		logger.Info("Deferring upgrade until all processes are ready to be upgraded", "remainingProcesses", notReadyProcesses)
		message := fmt.Sprintf("Waiting for processes to be updated: %v", notReadyProcesses)
		r.Recorder.Event(cluster, corev1.EventTypeNormal, "UpgradeRequeued", message)
		return nil, &requeue{message: message}
	}
	err = lockClient.ClearPendingUpgrades()
	if err != nil {
		return nil, &requeue{curError: err}
	}

	return addresses, nil
}
