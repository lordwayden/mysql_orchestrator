/*
   Copyright 2014 Outbrain Inc.

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

package inst

import (
	"fmt"
	goos "os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/github/orchestrator/go/config"
	"github.com/github/orchestrator/go/os"
	"github.com/openark/golib/log"
	"github.com/openark/golib/math"
	"github.com/openark/golib/util"
)

type StopReplicationMethod string

const (
	NoStopReplication     StopReplicationMethod = "NoStopReplication"
	StopReplicationNormal                       = "StopReplicationNormal"
	StopReplicationNicely                       = "StopReplicationNicely"
)

var ReplicationNotRunningError = fmt.Errorf("Replication not running")

var asciiFillerCharacter = " "
var tabulatorScharacter = "|"

var countRetries = 5
var MaxConcurrentReplicaOperations = 5

// getASCIITopologyEntry will get an ascii topology tree rooted at given instance. Ir recursively
// draws the tree
func getASCIITopologyEntry(depth int, instance *Instance, replicationMap map[*Instance]([]*Instance), extendedOutput bool, fillerCharacter string, tabulated bool) []string {
	if instance == nil {
		return []string{}
	}
	if instance.IsCoMain && depth > 1 {
		return []string{}
	}
	prefix := ""
	if depth > 0 {
		prefix = strings.Repeat(fillerCharacter, (depth-1)*2)
		if instance.ReplicaRunning() && instance.IsLastCheckValid && instance.IsRecentlyChecked {
			prefix += "+" + fillerCharacter
		} else {
			prefix += "-" + fillerCharacter
		}
	}
	entry := fmt.Sprintf("%s%s", prefix, instance.Key.DisplayString())
	if extendedOutput {
		if tabulated {
			entry = fmt.Sprintf("%s%s%s", entry, tabulatorScharacter, instance.TabulatedDescription(tabulatorScharacter))
		} else {
			entry = fmt.Sprintf("%s%s%s", entry, fillerCharacter, instance.HumanReadableDescription())
		}
	}
	result := []string{entry}
	for _, replica := range replicationMap[instance] {
		replicasResult := getASCIITopologyEntry(depth+1, replica, replicationMap, extendedOutput, fillerCharacter, tabulated)
		result = append(result, replicasResult...)
	}
	return result
}

// ASCIITopology returns a string representation of the topology of given cluster.
func ASCIITopology(clusterName string, historyTimestampPattern string, tabulated bool) (result string, err error) {
	fillerCharacter := asciiFillerCharacter
	var instances [](*Instance)
	if historyTimestampPattern == "" {
		instances, err = ReadClusterInstances(clusterName)
	} else {
		instances, err = ReadHistoryClusterInstances(clusterName, historyTimestampPattern)
	}
	if err != nil {
		return "", err
	}

	instancesMap := make(map[InstanceKey](*Instance))
	for _, instance := range instances {
		log.Debugf("instanceKey: %+v", instance.Key)
		instancesMap[instance.Key] = instance
	}

	replicationMap := make(map[*Instance]([]*Instance))
	var mainInstance *Instance
	// Investigate replicas:
	for _, instance := range instances {
		main, ok := instancesMap[instance.MainKey]
		if ok {
			if _, ok := replicationMap[main]; !ok {
				replicationMap[main] = [](*Instance){}
			}
			replicationMap[main] = append(replicationMap[main], instance)
		} else {
			mainInstance = instance
		}
	}
	// Get entries:
	var entries []string
	if mainInstance != nil {
		// Single main
		entries = getASCIITopologyEntry(0, mainInstance, replicationMap, historyTimestampPattern == "", fillerCharacter, tabulated)
	} else {
		// Co-mains? For visualization we put each in its own branch while ignoring its other co-mains.
		for _, instance := range instances {
			if instance.IsCoMain {
				entries = append(entries, getASCIITopologyEntry(1, instance, replicationMap, historyTimestampPattern == "", fillerCharacter, tabulated)...)
			}
		}
	}
	// Beautify: make sure the "[...]" part is nicely aligned for all instances.
	if tabulated {
		entries = util.Tabulate(entries, "|", "|", util.TabulateLeft, util.TabulateRight)
	} else {
		indentationCharacter := "["
		maxIndent := 0
		for _, entry := range entries {
			maxIndent = math.MaxInt(maxIndent, strings.Index(entry, indentationCharacter))
		}
		for i, entry := range entries {
			entryIndent := strings.Index(entry, indentationCharacter)
			if maxIndent > entryIndent {
				tokens := strings.SplitN(entry, indentationCharacter, 2)
				newEntry := fmt.Sprintf("%s%s%s%s", tokens[0], strings.Repeat(fillerCharacter, maxIndent-entryIndent), indentationCharacter, tokens[1])
				entries[i] = newEntry
			}
		}
	}
	// Turn into string
	result = strings.Join(entries, "\n")
	return result, nil
}

func shouldPostponeRelocatingReplica(replica *Instance, postponedFunctionsContainer *PostponedFunctionsContainer) bool {
	if postponedFunctionsContainer == nil {
		return false
	}
	if config.Config.PostponeReplicaRecoveryOnLagMinutes > 0 &&
		replica.SQLDelay > config.Config.PostponeReplicaRecoveryOnLagMinutes*60 {
		// This replica is lagging very much, AND
		// we're configured to postpone operation on this replica so as not to delay everyone else.
		return true
	}
	if replica.LastDiscoveryLatency > ReasonableDiscoveryLatency {
		return true
	}
	return false
}

// GetInstanceMain synchronously reaches into the replication topology
// and retrieves main's data
func GetInstanceMain(instance *Instance) (*Instance, error) {
	main, err := ReadTopologyInstance(&instance.MainKey)
	return main, err
}

// InstancesAreSiblings checks whether both instances are replicating from same main
func InstancesAreSiblings(instance0, instance1 *Instance) bool {
	if !instance0.IsReplica() {
		return false
	}
	if !instance1.IsReplica() {
		return false
	}
	if instance0.Key.Equals(&instance1.Key) {
		// same instance...
		return false
	}
	return instance0.MainKey.Equals(&instance1.MainKey)
}

// InstanceIsMainOf checks whether an instance is the main of another
func InstanceIsMainOf(allegedMain, allegedReplica *Instance) bool {
	if !allegedReplica.IsReplica() {
		return false
	}
	if allegedMain.Key.Equals(&allegedReplica.Key) {
		// same instance...
		return false
	}
	return allegedMain.Key.Equals(&allegedReplica.MainKey)
}

// MoveEquivalent will attempt moving instance indicated by instanceKey below another instance,
// based on known main coordinates equivalence
func MoveEquivalent(instanceKey, otherKey *InstanceKey) (*Instance, error) {
	instance, found, err := ReadInstance(instanceKey)
	if err != nil || !found {
		return instance, err
	}
	if instance.Key.Equals(otherKey) {
		return instance, fmt.Errorf("MoveEquivalent: attempt to move an instance below itself %+v", instance.Key)
	}

	// Are there equivalent coordinates to this instance?
	instanceCoordinates := &InstanceBinlogCoordinates{Key: instance.MainKey, Coordinates: instance.ExecBinlogCoordinates}
	binlogCoordinates, err := GetEquivalentBinlogCoordinatesFor(instanceCoordinates, otherKey)
	if err != nil {
		return instance, err
	}
	if binlogCoordinates == nil {
		return instance, fmt.Errorf("No equivalent coordinates found for %+v replicating from %+v at %+v", instance.Key, instance.MainKey, instance.ExecBinlogCoordinates)
	}
	// For performance reasons, we did all the above before even checking the replica is stopped or stopping it at all.
	// This allows us to quickly skip the entire operation should there NOT be coordinates.
	// To elaborate: if the replica is actually running AND making progress, it is unlikely/impossible for it to have
	// equivalent coordinates, as the current coordinates are like to have never been seen.
	// This excludes the case, for example, that the main is itself not replicating.
	// Now if we DO get to happen on equivalent coordinates, we need to double check. For CHANGE MASTER to happen we must
	// stop the replica anyhow. But then let's verify the position hasn't changed.
	knownExecBinlogCoordinates := instance.ExecBinlogCoordinates
	instance, err = StopSubordinate(instanceKey)
	if err != nil {
		goto Cleanup
	}
	if !instance.ExecBinlogCoordinates.Equals(&knownExecBinlogCoordinates) {
		// Seems like things were still running... We don't have an equivalence point
		err = fmt.Errorf("MoveEquivalent(): ExecBinlogCoordinates changed after stopping replication on %+v; aborting", instance.Key)
		goto Cleanup
	}
	instance, err = ChangeMainTo(instanceKey, otherKey, binlogCoordinates, false, GTIDHintNeutral)

Cleanup:
	instance, _ = StartSubordinate(instanceKey)

	if err == nil {
		message := fmt.Sprintf("moved %+v via equivalence coordinates below %+v", *instanceKey, *otherKey)
		log.Debugf(message)
		AuditOperation("move-equivalent", instanceKey, message)
	}
	return instance, err
}

// MoveUp will attempt moving instance indicated by instanceKey up the topology hierarchy.
// It will perform all safety and sanity checks and will tamper with this instance's replication
// as well as its main.
func MoveUp(instanceKey *InstanceKey) (*Instance, error) {
	instance, err := ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, err
	}
	if !instance.IsReplica() {
		return instance, fmt.Errorf("instance is not a replica: %+v", instanceKey)
	}
	rinstance, _, _ := ReadInstance(&instance.Key)
	if canMove, merr := rinstance.CanMove(); !canMove {
		return instance, merr
	}
	main, err := GetInstanceMain(instance)
	if err != nil {
		return instance, log.Errorf("Cannot GetInstanceMain() for %+v. error=%+v", instance.Key, err)
	}

	if !main.IsReplica() {
		return instance, fmt.Errorf("main is not a replica itself: %+v", main.Key)
	}

	if canReplicate, err := instance.CanReplicateFrom(main); canReplicate == false {
		return instance, err
	}
	if main.IsBinlogServer() {
		// Quick solution via binlog servers
		return Repoint(instanceKey, &main.MainKey, GTIDHintDeny)
	}

	log.Infof("Will move %+v up the topology", *instanceKey)

	if maintenanceToken, merr := BeginMaintenance(instanceKey, GetMaintenanceOwner(), "move up"); merr != nil {
		err = fmt.Errorf("Cannot begin maintenance on %+v", *instanceKey)
		goto Cleanup
	} else {
		defer EndMaintenance(maintenanceToken)
	}
	if maintenanceToken, merr := BeginMaintenance(&main.Key, GetMaintenanceOwner(), fmt.Sprintf("child %+v moves up", *instanceKey)); merr != nil {
		err = fmt.Errorf("Cannot begin maintenance on %+v", main.Key)
		goto Cleanup
	} else {
		defer EndMaintenance(maintenanceToken)
	}

	if !instance.UsingMariaDBGTID {
		main, err = StopSubordinate(&main.Key)
		if err != nil {
			goto Cleanup
		}
	}

	instance, err = StopSubordinate(instanceKey)
	if err != nil {
		goto Cleanup
	}

	if !instance.UsingMariaDBGTID {
		instance, err = StartSubordinateUntilMainCoordinates(instanceKey, &main.SelfBinlogCoordinates)
		if err != nil {
			goto Cleanup
		}
	}

	// We can skip hostname unresolve; we just copy+paste whatever our main thinks of its main.
	instance, err = ChangeMainTo(instanceKey, &main.MainKey, &main.ExecBinlogCoordinates, true, GTIDHintDeny)
	if err != nil {
		goto Cleanup
	}

Cleanup:
	instance, _ = StartSubordinate(instanceKey)
	if !instance.UsingMariaDBGTID {
		main, _ = StartSubordinate(&main.Key)
	}
	if err != nil {
		return instance, log.Errore(err)
	}
	// and we're done (pending deferred functions)
	AuditOperation("move-up", instanceKey, fmt.Sprintf("moved up %+v. Previous main: %+v", *instanceKey, main.Key))

	return instance, err
}

// MoveUpReplicas will attempt moving up all replicas of a given instance, at the same time.
// Clock-time, this is fater than moving one at a time. However this means all replicas of the given instance, and the instance itself,
// will all stop replicating together.
func MoveUpReplicas(instanceKey *InstanceKey, pattern string) ([](*Instance), *Instance, error, []error) {
	res := [](*Instance){}
	errs := []error{}
	replicaMutex := make(chan bool, 1)
	var barrier chan *InstanceKey

	instance, err := ReadTopologyInstance(instanceKey)
	if err != nil {
		return res, nil, err, errs
	}
	if !instance.IsReplica() {
		return res, instance, fmt.Errorf("instance is not a replica: %+v", instanceKey), errs
	}
	_, err = GetInstanceMain(instance)
	if err != nil {
		return res, instance, log.Errorf("Cannot GetInstanceMain() for %+v. error=%+v", instance.Key, err), errs
	}

	if instance.IsBinlogServer() {
		replicas, err, errors := RepointReplicasTo(instanceKey, pattern, &instance.MainKey)
		// Bail out!
		return replicas, instance, err, errors
	}

	replicas, err := ReadReplicaInstances(instanceKey)
	if err != nil {
		return res, instance, err, errs
	}
	replicas = filterInstancesByPattern(replicas, pattern)
	if len(replicas) == 0 {
		return res, instance, nil, errs
	}
	log.Infof("Will move replicas of %+v up the topology", *instanceKey)

	if maintenanceToken, merr := BeginMaintenance(instanceKey, GetMaintenanceOwner(), "move up replicas"); merr != nil {
		err = fmt.Errorf("Cannot begin maintenance on %+v", *instanceKey)
		goto Cleanup
	} else {
		defer EndMaintenance(maintenanceToken)
	}
	for _, replica := range replicas {
		if maintenanceToken, merr := BeginMaintenance(&replica.Key, GetMaintenanceOwner(), fmt.Sprintf("%+v moves up", replica.Key)); merr != nil {
			err = fmt.Errorf("Cannot begin maintenance on %+v", replica.Key)
			goto Cleanup
		} else {
			defer EndMaintenance(maintenanceToken)
		}
	}

	instance, err = StopSubordinate(instanceKey)
	if err != nil {
		goto Cleanup
	}

	barrier = make(chan *InstanceKey)
	for _, replica := range replicas {
		replica := replica
		go func() {
			defer func() {
				defer func() { barrier <- &replica.Key }()
				StartSubordinate(&replica.Key)
			}()

			var replicaErr error
			ExecuteOnTopology(func() {
				if canReplicate, err := replica.CanReplicateFrom(instance); canReplicate == false || err != nil {
					replicaErr = err
					return
				}
				if instance.IsBinlogServer() {
					// Special case. Just repoint
					replica, err = Repoint(&replica.Key, instanceKey, GTIDHintDeny)
					if err != nil {
						replicaErr = err
						return
					}
				} else {
					// Normal case. Do the math.
					replica, err = StopSubordinate(&replica.Key)
					if err != nil {
						replicaErr = err
						return
					}
					replica, err = StartSubordinateUntilMainCoordinates(&replica.Key, &instance.SelfBinlogCoordinates)
					if err != nil {
						replicaErr = err
						return
					}

					replica, err = ChangeMainTo(&replica.Key, &instance.MainKey, &instance.ExecBinlogCoordinates, false, GTIDHintDeny)
					if err != nil {
						replicaErr = err
						return
					}
				}
			})

			func() {
				replicaMutex <- true
				defer func() { <-replicaMutex }()
				if replicaErr == nil {
					res = append(res, replica)
				} else {
					errs = append(errs, replicaErr)
				}
			}()
		}()
	}
	for range replicas {
		<-barrier
	}

Cleanup:
	instance, _ = StartSubordinate(instanceKey)
	if err != nil {
		return res, instance, log.Errore(err), errs
	}
	if len(errs) == len(replicas) {
		// All returned with error
		return res, instance, log.Error("Error on all operations"), errs
	}
	AuditOperation("move-up-replicas", instanceKey, fmt.Sprintf("moved up %d/%d replicas of %+v. New main: %+v", len(res), len(replicas), *instanceKey, instance.MainKey))

	return res, instance, err, errs
}

// MoveBelow will attempt moving instance indicated by instanceKey below its supposed sibling indicated by sinblingKey.
// It will perform all safety and sanity checks and will tamper with this instance's replication
// as well as its sibling.
func MoveBelow(instanceKey, siblingKey *InstanceKey) (*Instance, error) {
	instance, err := ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, err
	}
	sibling, err := ReadTopologyInstance(siblingKey)
	if err != nil {
		return instance, err
	}

	if sibling.IsBinlogServer() {
		// Binlog server has same coordinates as main
		// Easy solution!
		return Repoint(instanceKey, &sibling.Key, GTIDHintDeny)
	}

	rinstance, _, _ := ReadInstance(&instance.Key)
	if canMove, merr := rinstance.CanMove(); !canMove {
		return instance, merr
	}

	rinstance, _, _ = ReadInstance(&sibling.Key)
	if canMove, merr := rinstance.CanMove(); !canMove {
		return instance, merr
	}
	if !InstancesAreSiblings(instance, sibling) {
		return instance, fmt.Errorf("instances are not siblings: %+v, %+v", *instanceKey, *siblingKey)
	}

	if canReplicate, err := instance.CanReplicateFrom(sibling); !canReplicate {
		return instance, err
	}
	log.Infof("Will move %+v below %+v", instanceKey, siblingKey)

	if maintenanceToken, merr := BeginMaintenance(instanceKey, GetMaintenanceOwner(), fmt.Sprintf("move below %+v", *siblingKey)); merr != nil {
		err = fmt.Errorf("Cannot begin maintenance on %+v", *instanceKey)
		goto Cleanup
	} else {
		defer EndMaintenance(maintenanceToken)
	}
	if maintenanceToken, merr := BeginMaintenance(siblingKey, GetMaintenanceOwner(), fmt.Sprintf("%+v moves below this", *instanceKey)); merr != nil {
		err = fmt.Errorf("Cannot begin maintenance on %+v", *siblingKey)
		goto Cleanup
	} else {
		defer EndMaintenance(maintenanceToken)
	}

	instance, err = StopSubordinate(instanceKey)
	if err != nil {
		goto Cleanup
	}

	sibling, err = StopSubordinate(siblingKey)
	if err != nil {
		goto Cleanup
	}
	if instance.ExecBinlogCoordinates.SmallerThan(&sibling.ExecBinlogCoordinates) {
		instance, err = StartSubordinateUntilMainCoordinates(instanceKey, &sibling.ExecBinlogCoordinates)
		if err != nil {
			goto Cleanup
		}
	} else if sibling.ExecBinlogCoordinates.SmallerThan(&instance.ExecBinlogCoordinates) {
		sibling, err = StartSubordinateUntilMainCoordinates(siblingKey, &instance.ExecBinlogCoordinates)
		if err != nil {
			goto Cleanup
		}
	}
	// At this point both siblings have executed exact same statements and are identical

	instance, err = ChangeMainTo(instanceKey, &sibling.Key, &sibling.SelfBinlogCoordinates, false, GTIDHintDeny)
	if err != nil {
		goto Cleanup
	}

Cleanup:
	instance, _ = StartSubordinate(instanceKey)
	sibling, _ = StartSubordinate(siblingKey)

	if err != nil {
		return instance, log.Errore(err)
	}
	// and we're done (pending deferred functions)
	AuditOperation("move-below", instanceKey, fmt.Sprintf("moved %+v below %+v", *instanceKey, *siblingKey))

	return instance, err
}

func canReplicateAssumingOracleGTID(instance, mainInstance *Instance) (canReplicate bool, err error) {
	subtract, err := GTIDSubtract(&instance.Key, mainInstance.GtidPurged, instance.ExecutedGtidSet)
	if err != nil {
		return false, err
	}
	subtractGtidSet, err := NewOracleGtidSet(subtract)
	if err != nil {
		return false, err
	}
	return subtractGtidSet.IsEmpty(), nil
}

func instancesAreGTIDAndCompatible(instance, otherInstance *Instance) (isOracleGTID bool, isMariaDBGTID, compatible bool) {
	isOracleGTID = (instance.UsingOracleGTID && otherInstance.SupportsOracleGTID)
	isMariaDBGTID = (instance.UsingMariaDBGTID && otherInstance.IsMariaDB())
	compatible = isOracleGTID || isMariaDBGTID
	return isOracleGTID, isMariaDBGTID, compatible
}

func CheckMoveViaGTID(instance, otherInstance *Instance) (err error) {
	isOracleGTID, _, moveCompatible := instancesAreGTIDAndCompatible(instance, otherInstance)
	if !moveCompatible {
		return fmt.Errorf("Instances %+v, %+v not GTID compatible or not using GTID", instance.Key, otherInstance.Key)
	}
	if isOracleGTID {
		canReplicate, err := canReplicateAssumingOracleGTID(instance, otherInstance)
		if err != nil {
			return err
		}
		if !canReplicate {
			return fmt.Errorf("Instance %+v has purged GTID entries not found on %+v", otherInstance.Key, instance.Key)
		}
	}

	return nil
}

// moveInstanceBelowViaGTID will attempt moving given instance below another instance using either Oracle GTID or MariaDB GTID.
func moveInstanceBelowViaGTID(instance, otherInstance *Instance) (*Instance, error) {
	rinstance, _, _ := ReadInstance(&instance.Key)
	if canMove, merr := rinstance.CanMoveViaMatch(); !canMove {
		return instance, merr
	}

	if canReplicate, err := instance.CanReplicateFrom(otherInstance); !canReplicate {
		return instance, err
	}
	if err := CheckMoveViaGTID(instance, otherInstance); err != nil {
		return instance, err
	}
	log.Infof("Will move %+v below %+v via GTID", instance.Key, otherInstance.Key)

	instanceKey := &instance.Key
	otherInstanceKey := &otherInstance.Key

	var err error
	if maintenanceToken, merr := BeginMaintenance(instanceKey, GetMaintenanceOwner(), fmt.Sprintf("move below %+v", *otherInstanceKey)); merr != nil {
		err = fmt.Errorf("Cannot begin maintenance on %+v", *instanceKey)
		goto Cleanup
	} else {
		defer EndMaintenance(maintenanceToken)
	}

	instance, err = StopSubordinate(instanceKey)
	if err != nil {
		goto Cleanup
	}

	instance, err = ChangeMainTo(instanceKey, &otherInstance.Key, &otherInstance.SelfBinlogCoordinates, false, GTIDHintForce)
	if err != nil {
		goto Cleanup
	}
Cleanup:
	instance, _ = StartSubordinate(instanceKey)
	if err != nil {
		return instance, log.Errore(err)
	}
	// and we're done (pending deferred functions)
	AuditOperation("move-below-gtid", instanceKey, fmt.Sprintf("moved %+v below %+v", *instanceKey, *otherInstanceKey))

	return instance, err
}

// MoveBelowGTID will attempt moving instance indicated by instanceKey below another instance using either Oracle GTID or MariaDB GTID.
func MoveBelowGTID(instanceKey, otherKey *InstanceKey) (*Instance, error) {
	instance, err := ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, err
	}
	other, err := ReadTopologyInstance(otherKey)
	if err != nil {
		return instance, err
	}
	return moveInstanceBelowViaGTID(instance, other)
}

// moveReplicasViaGTID moves a list of replicas under another instance via GTID, returning those replicas
// that could not be moved (do not use GTID or had GTID errors)
func moveReplicasViaGTID(replicas [](*Instance), other *Instance, postponedFunctionsContainer *PostponedFunctionsContainer) (movedReplicas [](*Instance), unmovedReplicas [](*Instance), err error, errs []error) {
	replicas = RemoveNilInstances(replicas)
	replicas = RemoveInstance(replicas, &other.Key)
	if len(replicas) == 0 {
		// Nothing to do
		return movedReplicas, unmovedReplicas, nil, errs
	}

	log.Infof("moveReplicasViaGTID: Will move %+v replicas below %+v via GTID", len(replicas), other.Key)

	var waitGroup sync.WaitGroup
	var replicaMutex sync.Mutex

	var concurrencyChan = make(chan bool, MaxConcurrentReplicaOperations)

	for _, replica := range replicas {
		replica := replica

		waitGroup.Add(1)
		// Parallelize repoints
		go func() {
			defer waitGroup.Done()
			moveFunc := func() error {

				concurrencyChan <- true
				defer func() { recover(); <-concurrencyChan }()

				movedReplica, replicaErr := moveInstanceBelowViaGTID(replica, other)
				if replicaErr != nil && movedReplica != nil {
					replica = movedReplica
				}

				// After having moved replicas, update local shared variables:
				replicaMutex.Lock()
				defer replicaMutex.Unlock()

				if replicaErr == nil {
					movedReplicas = append(movedReplicas, replica)
				} else {
					unmovedReplicas = append(unmovedReplicas, replica)
					errs = append(errs, replicaErr)
				}
				return replicaErr
			}
			if shouldPostponeRelocatingReplica(replica, postponedFunctionsContainer) {
				postponedFunctionsContainer.AddPostponedFunction(moveFunc, fmt.Sprintf("move-replicas-gtid %+v", replica.Key))
				// We bail out and trust our invoker to later call upon this postponed function
			} else {
				ExecuteOnTopology(func() { moveFunc() })
			}
		}()
	}
	waitGroup.Wait()

	if len(errs) == len(replicas) {
		// All returned with error
		return movedReplicas, unmovedReplicas, fmt.Errorf("moveReplicasViaGTID: Error on all %+v operations", len(errs)), errs
	}
	AuditOperation("move-replicas-gtid", &other.Key, fmt.Sprintf("moved %d/%d replicas below %+v via GTID", len(movedReplicas), len(replicas), other.Key))

	return movedReplicas, unmovedReplicas, err, errs
}

// MoveReplicasGTID will (attempt to) move all replicas of given main below given instance.
func MoveReplicasGTID(mainKey *InstanceKey, belowKey *InstanceKey, pattern string) (movedReplicas [](*Instance), unmovedReplicas [](*Instance), err error, errs []error) {
	belowInstance, err := ReadTopologyInstance(belowKey)
	if err != nil {
		// Can't access "below" ==> can't move replicas beneath it
		return movedReplicas, unmovedReplicas, err, errs
	}

	// replicas involved
	replicas, err := ReadReplicaInstancesIncludingBinlogServerSubReplicas(mainKey)
	if err != nil {
		return movedReplicas, unmovedReplicas, err, errs
	}
	replicas = filterInstancesByPattern(replicas, pattern)
	movedReplicas, unmovedReplicas, err, errs = moveReplicasViaGTID(replicas, belowInstance, nil)
	if err != nil {
		log.Errore(err)
	}

	if len(unmovedReplicas) > 0 {
		err = fmt.Errorf("MoveReplicasGTID: only moved %d out of %d replicas of %+v; error is: %+v", len(movedReplicas), len(replicas), *mainKey, err)
	}

	return movedReplicas, unmovedReplicas, err, errs
}

// Repoint connects a replica to a main using its exact same executing coordinates.
// The given mainKey can be null, in which case the existing main is used.
// Two use cases:
// - mainKey is nil: use case is corrupted relay logs on replica
// - mainKey is not nil: using Binlog servers (coordinates remain the same)
func Repoint(instanceKey *InstanceKey, mainKey *InstanceKey, gtidHint OperationGTIDHint) (*Instance, error) {
	instance, err := ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, err
	}
	if !instance.IsReplica() {
		return instance, fmt.Errorf("instance is not a replica: %+v", *instanceKey)
	}

	if mainKey == nil {
		mainKey = &instance.MainKey
	}
	// With repoint we *prefer* the main to be alive, but we don't strictly require it.
	// The use case for the main being alive is with hostname-resolve or hostname-unresolve: asking the replica
	// to reconnect to its same main while changing the MASTER_HOST in CHANGE MASTER TO due to DNS changes etc.
	main, err := ReadTopologyInstance(mainKey)
	mainIsAccessible := (err == nil)
	if !mainIsAccessible {
		main, _, err = ReadInstance(mainKey)
		if err != nil {
			return instance, err
		}
	}
	if canReplicate, err := instance.CanReplicateFrom(main); !canReplicate {
		return instance, err
	}

	// if a binlog server check it is sufficiently up to date
	if main.IsBinlogServer() {
		// "Repoint" operation trusts the user. But only so much. Repoiting to a binlog server which is not yet there is strictly wrong.
		if !instance.ExecBinlogCoordinates.SmallerThanOrEquals(&main.SelfBinlogCoordinates) {
			return instance, fmt.Errorf("repoint: binlog server %+v is not sufficiently up to date to repoint %+v below it", *mainKey, *instanceKey)
		}
	}

	log.Infof("Will repoint %+v to main %+v", *instanceKey, *mainKey)

	if maintenanceToken, merr := BeginMaintenance(instanceKey, GetMaintenanceOwner(), "repoint"); merr != nil {
		err = fmt.Errorf("Cannot begin maintenance on %+v", *instanceKey)
		goto Cleanup
	} else {
		defer EndMaintenance(maintenanceToken)
	}

	instance, err = StopSubordinate(instanceKey)
	if err != nil {
		goto Cleanup
	}

	// See above, we are relaxed about the main being accessible/inaccessible.
	// If accessible, we wish to do hostname-unresolve. If inaccessible, we can skip the test and not fail the
	// ChangeMainTo operation. This is why we pass "!mainIsAccessible" below.
	if instance.ExecBinlogCoordinates.IsEmpty() {
		instance.ExecBinlogCoordinates.LogFile = "orchestrator-unknown-log-file"
	}
	instance, err = ChangeMainTo(instanceKey, mainKey, &instance.ExecBinlogCoordinates, !mainIsAccessible, gtidHint)
	if err != nil {
		goto Cleanup
	}

Cleanup:
	instance, _ = StartSubordinate(instanceKey)
	if err != nil {
		return instance, log.Errore(err)
	}
	// and we're done (pending deferred functions)
	AuditOperation("repoint", instanceKey, fmt.Sprintf("replica %+v repointed to main: %+v", *instanceKey, *mainKey))

	return instance, err

}

// RepointTo repoints list of replicas onto another main.
// Binlog Server is the major use case
func RepointTo(replicas [](*Instance), belowKey *InstanceKey) ([](*Instance), error, []error) {
	res := [](*Instance){}
	errs := []error{}

	replicas = RemoveInstance(replicas, belowKey)
	if len(replicas) == 0 {
		// Nothing to do
		return res, nil, errs
	}
	if belowKey == nil {
		return res, log.Errorf("RepointTo received nil belowKey"), errs
	}

	log.Infof("Will repoint %+v replicas below %+v", len(replicas), *belowKey)
	barrier := make(chan *InstanceKey)
	replicaMutex := make(chan bool, 1)
	for _, replica := range replicas {
		replica := replica

		// Parallelize repoints
		go func() {
			defer func() { barrier <- &replica.Key }()
			ExecuteOnTopology(func() {
				replica, replicaErr := Repoint(&replica.Key, belowKey, GTIDHintNeutral)

				func() {
					// Instantaneous mutex.
					replicaMutex <- true
					defer func() { <-replicaMutex }()
					if replicaErr == nil {
						res = append(res, replica)
					} else {
						errs = append(errs, replicaErr)
					}
				}()
			})
		}()
	}
	for range replicas {
		<-barrier
	}

	if len(errs) == len(replicas) {
		// All returned with error
		return res, log.Error("Error on all operations"), errs
	}
	AuditOperation("repoint-to", belowKey, fmt.Sprintf("repointed %d/%d replicas to %+v", len(res), len(replicas), *belowKey))

	return res, nil, errs
}

// RepointReplicasTo repoints replicas of a given instance (possibly filtered) onto another main.
// Binlog Server is the major use case
func RepointReplicasTo(instanceKey *InstanceKey, pattern string, belowKey *InstanceKey) ([](*Instance), error, []error) {
	res := [](*Instance){}
	errs := []error{}

	replicas, err := ReadReplicaInstances(instanceKey)
	if err != nil {
		return res, err, errs
	}
	replicas = RemoveInstance(replicas, belowKey)
	replicas = filterInstancesByPattern(replicas, pattern)
	if len(replicas) == 0 {
		// Nothing to do
		return res, nil, errs
	}
	if belowKey == nil {
		// Default to existing main. All replicas are of the same main, hence just pick one.
		belowKey = &replicas[0].MainKey
	}
	log.Infof("Will repoint replicas of %+v to %+v", *instanceKey, *belowKey)
	return RepointTo(replicas, belowKey)
}

// RepointReplicas repoints all replicas of a given instance onto its existing main.
func RepointReplicas(instanceKey *InstanceKey, pattern string) ([](*Instance), error, []error) {
	return RepointReplicasTo(instanceKey, pattern, nil)
}

// MakeCoMain will attempt to make an instance co-main with its main, by making its main a replica of its own.
// This only works out if the main is not replicating; the main does not have a known main (it may have an unknown main).
func MakeCoMain(instanceKey *InstanceKey) (*Instance, error) {
	instance, err := ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, err
	}
	if canMove, merr := instance.CanMove(); !canMove {
		return instance, merr
	}
	main, err := GetInstanceMain(instance)
	if err != nil {
		return instance, err
	}
	log.Debugf("Will check whether %+v's main (%+v) can become its co-main", instance.Key, main.Key)
	if canMove, merr := main.CanMoveAsCoMain(); !canMove {
		return instance, merr
	}
	if instanceKey.Equals(&main.MainKey) {
		return instance, fmt.Errorf("instance %+v is already co main of %+v", instance.Key, main.Key)
	}
	if !instance.ReadOnly {
		return instance, fmt.Errorf("instance %+v is not read-only; first make it read-only before making it co-main", instance.Key)
	}
	if main.IsCoMain {
		// We allow breaking of an existing co-main replication. Here's the breakdown:
		// Ideally, this would not eb allowed, and we would first require the user to RESET SLAVE on 'main'
		// prior to making it participate as co-main with our 'instance'.
		// However there's the problem that upon RESET SLAVE we lose the replication's user/password info.
		// Thus, we come up with the following rule:
		// If S replicates from M1, and M1<->M2 are co mains, we allow S to become co-main of M1 (S<->M1) if:
		// - M1 is writeable
		// - M2 is read-only or is unreachable/invalid
		// - S  is read-only
		// And so we will be replacing one read-only co-main with another.
		otherCoMain, found, _ := ReadInstance(&main.MainKey)
		if found && otherCoMain.IsLastCheckValid && !otherCoMain.ReadOnly {
			return instance, fmt.Errorf("main %+v is already co-main with %+v, and %+v is alive, and not read-only; cowardly refusing to demote it. Please set it as read-only beforehand", main.Key, otherCoMain.Key, otherCoMain.Key)
		}
		// OK, good to go.
	} else if _, found, _ := ReadInstance(&main.MainKey); found {
		return instance, fmt.Errorf("%+v is not a real main; it replicates from: %+v", main.Key, main.MainKey)
	}
	if canReplicate, err := main.CanReplicateFrom(instance); !canReplicate {
		return instance, err
	}
	log.Infof("Will make %+v co-main of %+v", instanceKey, main.Key)

	var gitHint OperationGTIDHint = GTIDHintNeutral
	if maintenanceToken, merr := BeginMaintenance(instanceKey, GetMaintenanceOwner(), fmt.Sprintf("make co-main of %+v", main.Key)); merr != nil {
		err = fmt.Errorf("Cannot begin maintenance on %+v", *instanceKey)
		goto Cleanup
	} else {
		defer EndMaintenance(maintenanceToken)
	}
	if maintenanceToken, merr := BeginMaintenance(&main.Key, GetMaintenanceOwner(), fmt.Sprintf("%+v turns into co-main of this", *instanceKey)); merr != nil {
		err = fmt.Errorf("Cannot begin maintenance on %+v", main.Key)
		goto Cleanup
	} else {
		defer EndMaintenance(maintenanceToken)
	}

	// the coMain used to be merely a replica. Just point main into *some* position
	// within coMain...
	if main.IsReplica() {
		// this is the case of a co-main. For mains, the StopSubordinate operation throws an error, and
		// there's really no point in doing it.
		main, err = StopSubordinate(&main.Key)
		if err != nil {
			goto Cleanup
		}
	}
	if !main.HasReplicationCredentials {
		// Let's try , if possible, to get credentials from replica. Best effort.
		if replicationUser, replicationPassword, credentialsErr := ReadReplicationCredentials(&instance.Key); credentialsErr == nil {
			log.Debugf("Got credentials from a replica. will now apply")
			_, err = ChangeMainCredentials(&main.Key, replicationUser, replicationPassword)
			if err != nil {
				goto Cleanup
			}
		}
	}

	if instance.AllowTLS {
		log.Debugf("Enabling SSL replication")
		_, err = EnableMainSSL(&main.Key)
		if err != nil {
			goto Cleanup
		}
	}

	if instance.UsingOracleGTID {
		gitHint = GTIDHintForce
	}
	main, err = ChangeMainTo(&main.Key, instanceKey, &instance.SelfBinlogCoordinates, false, gitHint)
	if err != nil {
		goto Cleanup
	}

Cleanup:
	main, _ = StartSubordinate(&main.Key)
	if err != nil {
		return instance, log.Errore(err)
	}
	// and we're done (pending deferred functions)
	AuditOperation("make-co-main", instanceKey, fmt.Sprintf("%+v made co-main of %+v", *instanceKey, main.Key))

	return instance, err
}

// ResetSubordinateOperation will reset a replica
func ResetSubordinateOperation(instanceKey *InstanceKey) (*Instance, error) {
	instance, err := ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, err
	}

	log.Infof("Will reset replica on %+v", instanceKey)

	if maintenanceToken, merr := BeginMaintenance(instanceKey, GetMaintenanceOwner(), "reset replica"); merr != nil {
		err = fmt.Errorf("Cannot begin maintenance on %+v", *instanceKey)
		goto Cleanup
	} else {
		defer EndMaintenance(maintenanceToken)
	}

	if instance.IsReplica() {
		instance, err = StopSubordinate(instanceKey)
		if err != nil {
			goto Cleanup
		}
	}

	instance, err = ResetSubordinate(instanceKey)
	if err != nil {
		goto Cleanup
	}

Cleanup:
	instance, _ = StartSubordinate(instanceKey)

	if err != nil {
		return instance, log.Errore(err)
	}

	// and we're done (pending deferred functions)
	AuditOperation("reset-subordinate", instanceKey, fmt.Sprintf("%+v replication reset", *instanceKey))

	return instance, err
}

// DetachReplicaMainHost detaches a replica from its main by corrupting the Main_Host (in such way that is reversible)
func DetachReplicaMainHost(instanceKey *InstanceKey) (*Instance, error) {
	instance, err := ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, err
	}
	if !instance.IsReplica() {
		return instance, fmt.Errorf("instance is not a replica: %+v", *instanceKey)
	}
	if instance.MainKey.IsDetached() {
		return instance, fmt.Errorf("instance already detached: %+v", *instanceKey)
	}
	detachedMainKey := instance.MainKey.DetachedKey()

	log.Infof("Will detach main host on %+v. Detached key is %+v", *instanceKey, *detachedMainKey)

	if maintenanceToken, merr := BeginMaintenance(instanceKey, GetMaintenanceOwner(), "detach-replica-main-host"); merr != nil {
		err = fmt.Errorf("Cannot begin maintenance on %+v", *instanceKey)
		goto Cleanup
	} else {
		defer EndMaintenance(maintenanceToken)
	}

	instance, err = StopSubordinate(instanceKey)
	if err != nil {
		goto Cleanup
	}

	instance, err = ChangeMainTo(instanceKey, detachedMainKey, &instance.ExecBinlogCoordinates, true, GTIDHintNeutral)
	if err != nil {
		goto Cleanup
	}

Cleanup:
	instance, _ = StartSubordinate(instanceKey)
	if err != nil {
		return instance, log.Errore(err)
	}
	// and we're done (pending deferred functions)
	AuditOperation("repoint", instanceKey, fmt.Sprintf("replica %+v detached from main into %+v", *instanceKey, *detachedMainKey))

	return instance, err
}

// ReattachReplicaMainHost reattaches a replica back onto its main by undoing a DetachReplicaMainHost operation
func ReattachReplicaMainHost(instanceKey *InstanceKey) (*Instance, error) {
	instance, err := ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, err
	}
	if !instance.IsReplica() {
		return instance, fmt.Errorf("instance is not a replica: %+v", *instanceKey)
	}
	if !instance.MainKey.IsDetached() {
		return instance, fmt.Errorf("instance does not seem to be detached: %+v", *instanceKey)
	}

	reattachedMainKey := instance.MainKey.ReattachedKey()

	log.Infof("Will reattach main host on %+v. Reattached key is %+v", *instanceKey, *reattachedMainKey)

	if maintenanceToken, merr := BeginMaintenance(instanceKey, GetMaintenanceOwner(), "reattach-replica-main-host"); merr != nil {
		err = fmt.Errorf("Cannot begin maintenance on %+v", *instanceKey)
		goto Cleanup
	} else {
		defer EndMaintenance(maintenanceToken)
	}

	instance, err = StopSubordinate(instanceKey)
	if err != nil {
		goto Cleanup
	}

	instance, err = ChangeMainTo(instanceKey, reattachedMainKey, &instance.ExecBinlogCoordinates, true, GTIDHintNeutral)
	if err != nil {
		goto Cleanup
	}
	// Just in case this instance used to be a main:
	ReplaceAliasClusterName(instanceKey.StringCode(), reattachedMainKey.StringCode())

Cleanup:
	instance, _ = StartSubordinate(instanceKey)
	if err != nil {
		return instance, log.Errore(err)
	}
	// and we're done (pending deferred functions)
	AuditOperation("repoint", instanceKey, fmt.Sprintf("replica %+v reattached to main %+v", *instanceKey, *reattachedMainKey))

	return instance, err
}

// EnableGTID will attempt to enable GTID-mode (either Oracle or MariaDB)
func EnableGTID(instanceKey *InstanceKey) (*Instance, error) {
	instance, err := ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, err
	}
	if instance.UsingGTID() {
		return instance, fmt.Errorf("%+v already uses GTID", *instanceKey)
	}

	log.Infof("Will attempt to enable GTID on %+v", *instanceKey)

	instance, err = Repoint(instanceKey, nil, GTIDHintForce)
	if err != nil {
		return instance, err
	}
	if !instance.UsingGTID() {
		return instance, fmt.Errorf("Cannot enable GTID on %+v", *instanceKey)
	}

	AuditOperation("enable-gtid", instanceKey, fmt.Sprintf("enabled GTID on %+v", *instanceKey))

	return instance, err
}

// DisableGTID will attempt to disable GTID-mode (either Oracle or MariaDB) and revert to binlog file:pos replication
func DisableGTID(instanceKey *InstanceKey) (*Instance, error) {
	instance, err := ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, err
	}
	if !instance.UsingGTID() {
		return instance, fmt.Errorf("%+v is not using GTID", *instanceKey)
	}

	log.Infof("Will attempt to disable GTID on %+v", *instanceKey)

	instance, err = Repoint(instanceKey, nil, GTIDHintDeny)
	if err != nil {
		return instance, err
	}
	if instance.UsingGTID() {
		return instance, fmt.Errorf("Cannot disable GTID on %+v", *instanceKey)
	}

	AuditOperation("disable-gtid", instanceKey, fmt.Sprintf("disabled GTID on %+v", *instanceKey))

	return instance, err
}

func LocateErrantGTID(instanceKey *InstanceKey) (errantBinlogs []string, err error) {
	instance, err := ReadTopologyInstance(instanceKey)
	if err != nil {
		return errantBinlogs, err
	}
	errantSearch := instance.GtidErrant
	if errantSearch == "" {
		return errantBinlogs, log.Errorf("locate-errant-gtid: no errant-gtid on %+v", *instanceKey)
	}
	subtract, err := GTIDSubtract(instanceKey, errantSearch, instance.GtidPurged)
	if err != nil {
		return errantBinlogs, err
	}
	if subtract != errantSearch {
		return errantBinlogs, fmt.Errorf("locate-errant-gtid: %+v is already purged on %+v", subtract, *instanceKey)
	}
	binlogs, err := ShowBinaryLogs(instanceKey)
	if err != nil {
		return errantBinlogs, err
	}
	previousGTIDs := make(map[string]*OracleGtidSet)
	for _, binlog := range binlogs {
		oracleGTIDSet, err := GetPreviousGTIDs(instanceKey, binlog)
		if err != nil {
			return errantBinlogs, err
		}
		previousGTIDs[binlog] = oracleGTIDSet
	}
	for i, binlog := range binlogs {
		if errantSearch == "" {
			break
		}
		previousGTID := previousGTIDs[binlog]
		subtract, err := GTIDSubtract(instanceKey, errantSearch, previousGTID.String())
		if err != nil {
			return errantBinlogs, err
		}
		if subtract != errantSearch {
			// binlogs[i-1] is safe to use when i==0. because that implies GTIDs have been purged,
			// which covered by an earlier assertion
			errantBinlogs = append(errantBinlogs, binlogs[i-1])
			errantSearch = subtract
		}
	}
	if errantSearch != "" {
		// then it's in the last binary log
		errantBinlogs = append(errantBinlogs, binlogs[len(binlogs)-1])
	}
	return errantBinlogs, err
}

// ErrantGTIDResetMain will issue a safe RESET MASTER on a replica that replicates via GTID:
// It will make sure the gtid_purged set matches the executed set value as read just before the RESET.
// this will enable new replicas to be attached to given instance without complaints about missing/purged entries.
// This function requires that the instance does not have replicas.
func ErrantGTIDResetMain(instanceKey *InstanceKey) (instance *Instance, err error) {
	instance, err = ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, err
	}
	if instance.GtidErrant == "" {
		return instance, log.Errorf("gtid-errant-reset-main will not operate on %+v because no errant GTID is found", *instanceKey)
	}
	if !instance.SupportsOracleGTID {
		return instance, log.Errorf("gtid-errant-reset-main requested for %+v but it is not using oracle-gtid", *instanceKey)
	}
	if len(instance.SubordinateHosts) > 0 {
		return instance, log.Errorf("gtid-errant-reset-main will not operate on %+v because it has %+v replicas. Expecting no replicas", *instanceKey, len(instance.SubordinateHosts))
	}

	gtidSubtract := ""
	executedGtidSet := ""
	mainStatusFound := false
	replicationStopped := false
	waitInterval := time.Second * 5

	if maintenanceToken, merr := BeginMaintenance(instanceKey, GetMaintenanceOwner(), "reset-main-gtid"); merr != nil {
		err = fmt.Errorf("Cannot begin maintenance on %+v", *instanceKey)
		goto Cleanup
	} else {
		defer EndMaintenance(maintenanceToken)
	}

	if instance.IsReplica() {
		instance, err = StopSubordinate(instanceKey)
		if err != nil {
			goto Cleanup
		}
		replicationStopped, err = waitForReplicationState(instanceKey, ReplicationThreadStateStopped)
		if err != nil {
			goto Cleanup
		}
		if !replicationStopped {
			err = fmt.Errorf("gtid-errant-reset-main: timeout while waiting for replication to stop on %+v", instance.Key)
			goto Cleanup
		}
	}

	gtidSubtract, err = GTIDSubtract(instanceKey, instance.ExecutedGtidSet, instance.GtidErrant)
	if err != nil {
		goto Cleanup
	}

	// We're about to perform a destructive operation. It is non transactional and cannot be rolled back.
	// The replica will be left in a broken state.
	// This is why we allow multiple attempts at the following:
	for i := 0; i < countRetries; i++ {
		instance, err = ResetMain(instanceKey)
		if err == nil {
			break
		}
		time.Sleep(waitInterval)
	}
	if err != nil {
		err = fmt.Errorf("gtid-errant-reset-main: error while resetting main on %+v, after which intended to set gtid_purged to: %s. Error was: %+v", instance.Key, gtidSubtract, err)
		goto Cleanup
	}

	mainStatusFound, executedGtidSet, err = ShowMainStatus(instanceKey)
	if err != nil {
		err = fmt.Errorf("gtid-errant-reset-main: error getting main status on %+v, after which intended to set gtid_purged to: %s. Error was: %+v", instance.Key, gtidSubtract, err)
		goto Cleanup
	}
	if !mainStatusFound {
		err = fmt.Errorf("gtid-errant-reset-main: cannot get main status on %+v, after which intended to set gtid_purged to: %s.", instance.Key, gtidSubtract)
		goto Cleanup
	}
	if executedGtidSet != "" {
		err = fmt.Errorf("gtid-errant-reset-main: Unexpected non-empty Executed_Gtid_Set found on %+v following RESET MASTER, after which intended to set gtid_purged to: %s. Executed_Gtid_Set found to be: %+v", instance.Key, gtidSubtract, executedGtidSet)
		goto Cleanup
	}

	// We've just made the destructive operation. Again, allow for retries:
	for i := 0; i < countRetries; i++ {
		err = setGTIDPurged(instance, gtidSubtract)
		if err == nil {
			break
		}
		time.Sleep(waitInterval)
	}
	if err != nil {
		err = fmt.Errorf("gtid-errant-reset-main: error setting gtid_purged on %+v to: %s. Error was: %+v", instance.Key, gtidSubtract, err)
		goto Cleanup
	}

Cleanup:
	var startSubordinateErr error
	instance, startSubordinateErr = StartSubordinate(instanceKey)
	log.Errore(startSubordinateErr)

	if err != nil {
		return instance, log.Errore(err)
	}

	// and we're done (pending deferred functions)
	AuditOperation("gtid-errant-reset-main", instanceKey, fmt.Sprintf("%+v main reset", *instanceKey))

	return instance, err
}

// ErrantGTIDInjectEmpty will inject an empty transaction on the main of an instance's cluster in order to get rid
// of an errant transaction observed on the instance.
func ErrantGTIDInjectEmpty(instanceKey *InstanceKey) (instance *Instance, clusterMain *Instance, countInjectedTransactions int64, err error) {
	instance, err = ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, clusterMain, countInjectedTransactions, err
	}
	if instance.GtidErrant == "" {
		return instance, clusterMain, countInjectedTransactions, log.Errorf("gtid-errant-inject-empty will not operate on %+v because no errant GTID is found", *instanceKey)
	}
	if !instance.SupportsOracleGTID {
		return instance, clusterMain, countInjectedTransactions, log.Errorf("gtid-errant-inject-empty requested for %+v but it does not support oracle-gtid", *instanceKey)
	}

	mains, err := ReadClusterWriteableMain(instance.ClusterName)
	if err != nil {
		return instance, clusterMain, countInjectedTransactions, err
	}
	if len(mains) == 0 {
		return instance, clusterMain, countInjectedTransactions, log.Errorf("gtid-errant-inject-empty found no writabel main for %+v cluster", instance.ClusterName)
	}
	clusterMain = mains[0]

	if !clusterMain.SupportsOracleGTID {
		return instance, clusterMain, countInjectedTransactions, log.Errorf("gtid-errant-inject-empty requested for %+v but the cluster's main %+v does not support oracle-gtid", *instanceKey, clusterMain.Key)
	}

	gtidSet, err := NewOracleGtidSet(instance.GtidErrant)
	if err != nil {
		return instance, clusterMain, countInjectedTransactions, err
	}
	explodedEntries := gtidSet.Explode()
	log.Infof("gtid-errant-inject-empty: about to inject %+v empty transactions %+v on cluster main %+v", len(explodedEntries), gtidSet.String(), clusterMain.Key)
	for _, entry := range explodedEntries {
		if err := injectEmptyGTIDTransaction(&clusterMain.Key, entry); err != nil {
			return instance, clusterMain, countInjectedTransactions, err
		}
		countInjectedTransactions++
	}

	// and we're done (pending deferred functions)
	AuditOperation("gtid-errant-inject-empty", instanceKey, fmt.Sprintf("injected %+v empty transactions on %+v", countInjectedTransactions, clusterMain.Key))

	return instance, clusterMain, countInjectedTransactions, err
}

// FindLastPseudoGTIDEntry will search an instance's binary logs or relay logs for the last pseudo-GTID entry,
// and return found coordinates as well as entry text
func FindLastPseudoGTIDEntry(instance *Instance, recordedInstanceRelayLogCoordinates BinlogCoordinates, maxBinlogCoordinates *BinlogCoordinates, exhaustiveSearch bool, expectedBinlogFormat *string) (instancePseudoGtidCoordinates *BinlogCoordinates, instancePseudoGtidText string, err error) {

	if config.Config.PseudoGTIDPattern == "" {
		return instancePseudoGtidCoordinates, instancePseudoGtidText, fmt.Errorf("PseudoGTIDPattern not configured; cannot use Pseudo-GTID")
	}

	if instance.LogBinEnabled && instance.LogSubordinateUpdatesEnabled && !*config.RuntimeCLIFlags.SkipBinlogSearch && (expectedBinlogFormat == nil || instance.Binlog_format == *expectedBinlogFormat) {
		minBinlogCoordinates, _, _ := GetHeuristiclyRecentCoordinatesForInstance(&instance.Key)
		// Well no need to search this instance's binary logs if it doesn't have any...
		// With regard log-subordinate-updates, some edge cases are possible, like having this instance's log-subordinate-updates
		// enabled/disabled (of course having restarted it)
		// The approach is not to take chances. If log-subordinate-updates is disabled, fail and go for relay-logs.
		// If log-subordinate-updates was just enabled then possibly no pseudo-gtid is found, and so again we will go
		// for relay logs.
		// Also, if main has STATEMENT binlog format, and the replica has ROW binlog format, then comparing binlog entries would urely fail if based on the replica's binary logs.
		// Instead, we revert to the relay logs.
		instancePseudoGtidCoordinates, instancePseudoGtidText, err = getLastPseudoGTIDEntryInInstance(instance, minBinlogCoordinates, maxBinlogCoordinates, exhaustiveSearch)
	}
	if err != nil || instancePseudoGtidCoordinates == nil {
		minRelaylogCoordinates, _ := GetPreviousKnownRelayLogCoordinatesForInstance(instance)
		// Unable to find pseudo GTID in binary logs.
		// Then MAYBE we are lucky enough (chances are we are, if this replica did not crash) that we can
		// extract the Pseudo GTID entry from the last (current) relay log file.
		instancePseudoGtidCoordinates, instancePseudoGtidText, err = getLastPseudoGTIDEntryInRelayLogs(instance, minRelaylogCoordinates, recordedInstanceRelayLogCoordinates, exhaustiveSearch)
	}
	return instancePseudoGtidCoordinates, instancePseudoGtidText, err
}

// CorrelateBinlogCoordinates find out, if possible, the binlog coordinates of given otherInstance that correlate
// with given coordinates of given instance.
func CorrelateBinlogCoordinates(instance *Instance, binlogCoordinates *BinlogCoordinates, otherInstance *Instance) (*BinlogCoordinates, int, error) {
	// We record the relay log coordinates just after the instance stopped since the coordinates can change upon
	// a FLUSH LOGS/FLUSH RELAY LOGS (or a START SLAVE, though that's an altogether different problem) etc.
	// We want to be on the safe side; we don't utterly trust that we are the only ones playing with the instance.
	recordedInstanceRelayLogCoordinates := instance.RelaylogCoordinates
	instancePseudoGtidCoordinates, instancePseudoGtidText, err := FindLastPseudoGTIDEntry(instance, recordedInstanceRelayLogCoordinates, binlogCoordinates, true, &otherInstance.Binlog_format)

	if err != nil {
		return nil, 0, err
	}
	entriesMonotonic := (config.Config.PseudoGTIDMonotonicHint != "") && strings.Contains(instancePseudoGtidText, config.Config.PseudoGTIDMonotonicHint)
	minBinlogCoordinates, _, err := GetHeuristiclyRecentCoordinatesForInstance(&otherInstance.Key)
	otherInstancePseudoGtidCoordinates, err := SearchEntryInInstanceBinlogs(otherInstance, instancePseudoGtidText, entriesMonotonic, minBinlogCoordinates)
	if err != nil {
		return nil, 0, err
	}

	// We've found a match: the latest Pseudo GTID position within instance and its identical twin in otherInstance
	// We now iterate the events in both, up to the completion of events in instance (recall that we looked for
	// the last entry in instance, hence, assuming pseudo GTID entries are frequent, the amount of entries to read
	// from instance is not long)
	// The result of the iteration will be either:
	// - bad conclusion that instance is actually more advanced than otherInstance (we find more entries in instance
	//   following the pseudo gtid than we can match in otherInstance), hence we cannot ask instance to replicate
	//   from otherInstance
	// - good result: both instances are exactly in same shape (have replicated the exact same number of events since
	//   the last pseudo gtid). Since they are identical, it is easy to point instance into otherInstance.
	// - good result: the first position within otherInstance where instance has not replicated yet. It is easy to point
	//   instance into otherInstance.
	nextBinlogCoordinatesToMatch, countMatchedEvents, err := GetNextBinlogCoordinatesToMatch(instance, *instancePseudoGtidCoordinates,
		recordedInstanceRelayLogCoordinates, binlogCoordinates, otherInstance, *otherInstancePseudoGtidCoordinates)
	if err != nil {
		return nil, 0, err
	}
	if countMatchedEvents == 0 {
		err = fmt.Errorf("Unexpected: 0 events processed while iterating logs. Something went wrong; aborting. nextBinlogCoordinatesToMatch: %+v", nextBinlogCoordinatesToMatch)
		return nil, 0, err
	}
	return nextBinlogCoordinatesToMatch, countMatchedEvents, nil
}

func CorrelateRelaylogCoordinates(instance *Instance, relaylogCoordinates *BinlogCoordinates, otherInstance *Instance) (instanceCoordinates, correlatedCoordinates, nextCoordinates *BinlogCoordinates, found bool, err error) {
	// The two servers are expected to have the same main, or this doesn't work
	if !instance.MainKey.Equals(&otherInstance.MainKey) {
		return instanceCoordinates, correlatedCoordinates, nextCoordinates, found, log.Errorf("CorrelateRelaylogCoordinates requires sibling instances, however %+v has main %+v, and %+v has main %+v", instance.Key, instance.MainKey, otherInstance.Key, otherInstance.MainKey)
	}
	var binlogEvent *BinlogEvent
	if relaylogCoordinates == nil {
		instanceCoordinates = &instance.RelaylogCoordinates
		if minCoordinates, err := GetPreviousKnownRelayLogCoordinatesForInstance(instance); err != nil {
			return instanceCoordinates, correlatedCoordinates, nextCoordinates, found, err
		} else if binlogEvent, err = GetLastExecutedEntryInRelayLogs(instance, minCoordinates, instance.RelaylogCoordinates); err != nil {
			return instanceCoordinates, correlatedCoordinates, nextCoordinates, found, err
		}
	} else {
		instanceCoordinates = relaylogCoordinates
		relaylogCoordinates.Type = RelayLog
		if binlogEvent, err = ReadBinlogEventAtRelayLogCoordinates(&instance.Key, relaylogCoordinates); err != nil {
			return instanceCoordinates, correlatedCoordinates, nextCoordinates, found, err
		}
	}

	_, minCoordinates, err := GetHeuristiclyRecentCoordinatesForInstance(&otherInstance.Key)
	if err != nil {
		return instanceCoordinates, correlatedCoordinates, nextCoordinates, found, err
	}
	correlatedCoordinates, nextCoordinates, found, err = SearchEventInRelayLogs(binlogEvent, otherInstance, minCoordinates, otherInstance.RelaylogCoordinates)
	return instanceCoordinates, correlatedCoordinates, nextCoordinates, found, err
}

// MatchBelow will attempt moving instance indicated by instanceKey below its the one indicated by otherKey.
// The refactoring is based on matching binlog entries, not on "classic" positions comparisons.
// The "other instance" could be the sibling of the moving instance any of its ancestors. It may actually be
// a cousin of some sort (though unlikely). The only important thing is that the "other instance" is more
// advanced in replication than given instance.
func MatchBelow(instanceKey, otherKey *InstanceKey, requireInstanceMaintenance bool) (*Instance, *BinlogCoordinates, error) {
	instance, err := ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, nil, err
	}
	if config.Config.PseudoGTIDPattern == "" {
		return instance, nil, fmt.Errorf("PseudoGTIDPattern not configured; cannot use Pseudo-GTID")
	}
	if instanceKey.Equals(otherKey) {
		return instance, nil, fmt.Errorf("MatchBelow: attempt to match an instance below itself %+v", *instanceKey)
	}
	otherInstance, err := ReadTopologyInstance(otherKey)
	if err != nil {
		return instance, nil, err
	}

	rinstance, _, _ := ReadInstance(&instance.Key)
	if canMove, merr := rinstance.CanMoveViaMatch(); !canMove {
		return instance, nil, merr
	}

	if canReplicate, err := instance.CanReplicateFrom(otherInstance); !canReplicate {
		return instance, nil, err
	}
	var nextBinlogCoordinatesToMatch *BinlogCoordinates
	var countMatchedEvents int

	if otherInstance.IsBinlogServer() {
		// A Binlog Server does not do all the SHOW BINLOG EVENTS stuff
		err = fmt.Errorf("Cannot use PseudoGTID with Binlog Server %+v", otherInstance.Key)
		goto Cleanup
	}

	log.Infof("Will match %+v below %+v", *instanceKey, *otherKey)

	if requireInstanceMaintenance {
		if maintenanceToken, merr := BeginMaintenance(instanceKey, GetMaintenanceOwner(), fmt.Sprintf("match below %+v", *otherKey)); merr != nil {
			err = fmt.Errorf("Cannot begin maintenance on %+v", *instanceKey)
			goto Cleanup
		} else {
			defer EndMaintenance(maintenanceToken)
		}

		// We don't require grabbing maintenance lock on otherInstance, but we do request
		// that it is not already under maintenance.
		if inMaintenance, merr := InMaintenance(&otherInstance.Key); merr != nil {
			err = merr
			goto Cleanup
		} else if inMaintenance {
			err = fmt.Errorf("Cannot match below %+v; it is in maintenance", otherInstance.Key)
			goto Cleanup
		}
	}

	log.Debugf("Stopping replica on %+v", *instanceKey)
	instance, err = StopSubordinate(instanceKey)
	if err != nil {
		goto Cleanup
	}

	nextBinlogCoordinatesToMatch, countMatchedEvents, err = CorrelateBinlogCoordinates(instance, nil, otherInstance)

	if countMatchedEvents == 0 {
		err = fmt.Errorf("Unexpected: 0 events processed while iterating logs. Something went wrong; aborting. nextBinlogCoordinatesToMatch: %+v", nextBinlogCoordinatesToMatch)
		goto Cleanup
	}
	log.Debugf("%+v will match below %+v at %+v; validated events: %d", *instanceKey, *otherKey, *nextBinlogCoordinatesToMatch, countMatchedEvents)

	// Drum roll...
	instance, err = ChangeMainTo(instanceKey, otherKey, nextBinlogCoordinatesToMatch, false, GTIDHintDeny)
	if err != nil {
		goto Cleanup
	}

Cleanup:
	instance, _ = StartSubordinate(instanceKey)
	if err != nil {
		return instance, nextBinlogCoordinatesToMatch, log.Errore(err)
	}
	// and we're done (pending deferred functions)
	AuditOperation("match-below", instanceKey, fmt.Sprintf("matched %+v below %+v", *instanceKey, *otherKey))

	return instance, nextBinlogCoordinatesToMatch, err
}

// RematchReplica will re-match a replica to its main, using pseudo-gtid
func RematchReplica(instanceKey *InstanceKey, requireInstanceMaintenance bool) (*Instance, *BinlogCoordinates, error) {
	instance, err := ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, nil, err
	}
	mainInstance, found, err := ReadInstance(&instance.MainKey)
	if err != nil || !found {
		return instance, nil, err
	}
	return MatchBelow(instanceKey, &mainInstance.Key, requireInstanceMaintenance)
}

// MakeMain will take an instance, make all its siblings its replicas (via pseudo-GTID) and make it main
// (stop its replicaiton, make writeable).
func MakeMain(instanceKey *InstanceKey) (*Instance, error) {
	instance, err := ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, err
	}
	mainInstance, err := ReadTopologyInstance(&instance.MainKey)
	if err == nil {
		// If the read succeeded, check the main status.
		if mainInstance.IsReplica() {
			return instance, fmt.Errorf("MakeMain: instance's main %+v seems to be replicating", mainInstance.Key)
		}
		if mainInstance.IsLastCheckValid {
			return instance, fmt.Errorf("MakeMain: instance's main %+v seems to be accessible", mainInstance.Key)
		}
	}
	// Continue anyway if the read failed, because that means the main is
	// inaccessible... So it's OK to do the promotion.
	if !instance.SQLThreadUpToDate() {
		return instance, fmt.Errorf("MakeMain: instance's SQL thread must be up-to-date with I/O thread for %+v", *instanceKey)
	}
	siblings, err := ReadReplicaInstances(&mainInstance.Key)
	if err != nil {
		return instance, err
	}
	for _, sibling := range siblings {
		if instance.ExecBinlogCoordinates.SmallerThan(&sibling.ExecBinlogCoordinates) {
			return instance, fmt.Errorf("MakeMain: instance %+v has more advanced sibling: %+v", *instanceKey, sibling.Key)
		}
	}

	if maintenanceToken, merr := BeginMaintenance(instanceKey, GetMaintenanceOwner(), fmt.Sprintf("siblings match below this: %+v", *instanceKey)); merr != nil {
		err = fmt.Errorf("Cannot begin maintenance on %+v", *instanceKey)
		goto Cleanup
	} else {
		defer EndMaintenance(maintenanceToken)
	}

	_, _, err, _ = MultiMatchBelow(siblings, instanceKey, nil)
	if err != nil {
		goto Cleanup
	}

	SetReadOnly(instanceKey, false)

Cleanup:
	if err != nil {
		return instance, log.Errore(err)
	}
	// and we're done (pending deferred functions)
	AuditOperation("make-main", instanceKey, fmt.Sprintf("made main of %+v", *instanceKey))

	return instance, err
}

// TakeSiblings is a convenience method for turning siblings of a replica to be its subordinates.
// This operation is a syntatctic sugar on top relocate-replicas, which uses any available means to the objective:
// GTID, Pseudo-GTID, binlog servers, standard replication...
func TakeSiblings(instanceKey *InstanceKey) (instance *Instance, takenSiblings int, err error) {
	instance, err = ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, 0, err
	}
	if !instance.IsReplica() {
		return instance, takenSiblings, log.Errorf("take-siblings: instance %+v is not a replica.", *instanceKey)
	}
	relocatedReplicas, _, err, _ := RelocateReplicas(&instance.MainKey, instanceKey, "")

	return instance, len(relocatedReplicas), err
}

// Created this function to allow a hook to be called after a successful TakeMain event
func TakeMainHook(successor *Instance, demoted *Instance) {
	successorKey := successor.Key
	demotedKey := demoted.Key
	env := goos.Environ()

	env = append(env, fmt.Sprintf("ORC_SUCCESSOR_HOST=%s", successorKey))
	env = append(env, fmt.Sprintf("ORC_FAILED_HOST=%s", demotedKey))

	successorStr := fmt.Sprintf("%s", successorKey)
	demotedStr := fmt.Sprintf("%s", demotedKey)

	processCount := len(config.Config.PostTakeMainProcesses)
	for i, command := range config.Config.PostTakeMainProcesses {
		fullDescription := fmt.Sprintf("PostTakeMainProcesses hook %d of %d", i+1, processCount)
		log.Debugf("Take-Main: PostTakeMainProcesses: Calling %+s", fullDescription)
		start := time.Now()
		if err := os.CommandRun(command, env, successorStr, demotedStr); err == nil {
			info := fmt.Sprintf("Completed %s in %v", fullDescription, time.Since(start))
			log.Infof("Take-Main: %s", info)
		} else {
			info := fmt.Sprintf("Execution of PostTakeMainProcesses failed in %v with error: %v", time.Since(start), err)
			log.Errorf("Take-Main: %s", info)
		}
	}

}

// TakeMain will move an instance up the chain and cause its main to become its replica.
// It's almost a role change, just that other replicas of either 'instance' or its main are currently unaffected
// (they continue replicate without change)
// Note that the main must itself be a replica; however the grandparent does not necessarily have to be reachable
// and can in fact be dead.
func TakeMain(instanceKey *InstanceKey, allowTakingCoMain bool) (*Instance, error) {
	instance, err := ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, err
	}
	mainInstance, found, err := ReadInstance(&instance.MainKey)
	if err != nil || !found {
		return instance, err
	}
	if mainInstance.IsCoMain && !allowTakingCoMain {
		return instance, fmt.Errorf("%+v is co-main. Cannot take it.", mainInstance.Key)
	}
	log.Debugf("TakeMain: will attempt making %+v take its main %+v, now resolved as %+v", *instanceKey, instance.MainKey, mainInstance.Key)

	if canReplicate, err := mainInstance.CanReplicateFrom(instance); canReplicate == false {
		return instance, err
	}
	// We begin
	mainInstance, err = StopSubordinate(&mainInstance.Key)
	if err != nil {
		goto Cleanup
	}
	instance, err = StopSubordinate(&instance.Key)
	if err != nil {
		goto Cleanup
	}

	instance, err = StartSubordinateUntilMainCoordinates(&instance.Key, &mainInstance.SelfBinlogCoordinates)
	if err != nil {
		goto Cleanup
	}

	// instance and mainInstance are equal
	// We skip name unresolve. It is OK if the main's main is dead, unreachable, does not resolve properly.
	// We just copy+paste info from the main.
	// In particular, this is commonly calledin DeadMain recovery
	instance, err = ChangeMainTo(&instance.Key, &mainInstance.MainKey, &mainInstance.ExecBinlogCoordinates, true, GTIDHintNeutral)
	if err != nil {
		goto Cleanup
	}
	// instance is now sibling of main
	mainInstance, err = ChangeMainTo(&mainInstance.Key, &instance.Key, &instance.SelfBinlogCoordinates, false, GTIDHintNeutral)
	if err != nil {
		goto Cleanup
	}
	// swap is done!

Cleanup:
	instance, _ = StartSubordinate(&instance.Key)
	mainInstance, _ = StartSubordinate(&mainInstance.Key)
	if err != nil {
		return instance, err
	}
	AuditOperation("take-main", instanceKey, fmt.Sprintf("took main: %+v", mainInstance.Key))

	// Created this to enable a custom hook to be called after a TakeMain success.
	// This only runs if there is a hook configured in orchestrator.conf.json
	demoted := mainInstance
	successor := instance
	if config.Config.PostTakeMainProcesses != nil {
		TakeMainHook(successor, demoted)
	}

	return instance, err
}

// MakeLocalMain promotes a replica above its main, making it replica of its grandparent, while also enslaving its siblings.
// This serves as a convenience method to recover replication when a local main fails; the instance promoted is one of its replicas,
// which is most advanced among its siblings.
// This method utilizes Pseudo GTID
func MakeLocalMain(instanceKey *InstanceKey) (*Instance, error) {
	instance, err := ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, err
	}
	mainInstance, found, err := ReadInstance(&instance.MainKey)
	if err != nil || !found {
		return instance, err
	}
	grandparentInstance, err := ReadTopologyInstance(&mainInstance.MainKey)
	if err != nil {
		return instance, err
	}
	siblings, err := ReadReplicaInstances(&mainInstance.Key)
	if err != nil {
		return instance, err
	}
	for _, sibling := range siblings {
		if instance.ExecBinlogCoordinates.SmallerThan(&sibling.ExecBinlogCoordinates) {
			return instance, fmt.Errorf("MakeMain: instance %+v has more advanced sibling: %+v", *instanceKey, sibling.Key)
		}
	}

	instance, err = StopSubordinateNicely(instanceKey, 0)
	if err != nil {
		goto Cleanup
	}

	_, _, err = MatchBelow(instanceKey, &grandparentInstance.Key, true)
	if err != nil {
		goto Cleanup
	}

	_, _, err, _ = MultiMatchBelow(siblings, instanceKey, nil)
	if err != nil {
		goto Cleanup
	}

Cleanup:
	if err != nil {
		return instance, log.Errore(err)
	}
	// and we're done (pending deferred functions)
	AuditOperation("make-local-main", instanceKey, fmt.Sprintf("made main of %+v", *instanceKey))

	return instance, err
}

// sortInstances shuffles given list of instances according to some logic
func sortInstancesDataCenterHint(instances [](*Instance), dataCenterHint string) {
	sort.Sort(sort.Reverse(NewInstancesSorterByExec(instances, dataCenterHint)))
}

// sortInstances shuffles given list of instances according to some logic
func sortInstances(instances [](*Instance)) {
	sortInstancesDataCenterHint(instances, "")
}

// getReplicasForSorting returns a list of replicas of a given main potentially for candidate choosing
func getReplicasForSorting(mainKey *InstanceKey, includeBinlogServerSubReplicas bool) (replicas [](*Instance), err error) {
	if includeBinlogServerSubReplicas {
		replicas, err = ReadReplicaInstancesIncludingBinlogServerSubReplicas(mainKey)
	} else {
		replicas, err = ReadReplicaInstances(mainKey)
	}
	return replicas, err
}

func sortedReplicas(replicas [](*Instance), stopReplicationMethod StopReplicationMethod) [](*Instance) {
	return sortedReplicasDataCenterHint(replicas, stopReplicationMethod, "")
}

// sortedReplicas returns the list of replicas of some main, sorted by exec coordinates
// (most up-to-date replica first).
// This function assumes given `replicas` argument is indeed a list of instances all replicating
// from the same main (the result of `getReplicasForSorting()` is appropriate)
func sortedReplicasDataCenterHint(replicas [](*Instance), stopReplicationMethod StopReplicationMethod, dataCenterHint string) [](*Instance) {
	if len(replicas) == 0 {
		return replicas
	}
	replicas = StopSubordinates(replicas, stopReplicationMethod, time.Duration(config.Config.InstanceBulkOperationsWaitTimeoutSeconds)*time.Second)
	replicas = RemoveNilInstances(replicas)

	sortInstancesDataCenterHint(replicas, dataCenterHint)
	for _, replica := range replicas {
		log.Debugf("- sorted replica: %+v %+v", replica.Key, replica.ExecBinlogCoordinates)
	}

	return replicas
}

// GetSortedReplicas reads list of replicas of a given main, and returns them sorted by exec coordinates
// (most up-to-date replica first).
func GetSortedReplicas(mainKey *InstanceKey, stopReplicationMethod StopReplicationMethod) (replicas [](*Instance), err error) {
	if replicas, err = getReplicasForSorting(mainKey, false); err != nil {
		return replicas, err
	}
	replicas = sortedReplicas(replicas, stopReplicationMethod)
	if len(replicas) == 0 {
		return replicas, fmt.Errorf("No replicas found for %+v", *mainKey)
	}
	return replicas, err
}

// MultiMatchBelow will efficiently match multiple replicas below a given instance.
// It is assumed that all given replicas are siblings
func MultiMatchBelow(replicas [](*Instance), belowKey *InstanceKey, postponedFunctionsContainer *PostponedFunctionsContainer) (matchedReplicas [](*Instance), belowInstance *Instance, err error, errs []error) {
	belowInstance, found, err := ReadInstance(belowKey)
	if err != nil || !found {
		return matchedReplicas, belowInstance, err, errs
	}

	replicas = RemoveInstance(replicas, belowKey)
	if len(replicas) == 0 {
		// Nothing to do
		return replicas, belowInstance, err, errs
	}

	log.Infof("Will match %+v replicas below %+v via Pseudo-GTID, independently", len(replicas), belowKey)

	barrier := make(chan *InstanceKey)
	replicaMutex := &sync.Mutex{}

	for _, replica := range replicas {
		replica := replica

		// Parallelize repoints
		go func() {
			defer func() { barrier <- &replica.Key }()
			matchFunc := func() error {
				replica, _, replicaErr := MatchBelow(&replica.Key, belowKey, true)

				replicaMutex.Lock()
				defer replicaMutex.Unlock()

				if replicaErr == nil {
					matchedReplicas = append(matchedReplicas, replica)
				} else {
					errs = append(errs, replicaErr)
				}
				return replicaErr
			}
			if shouldPostponeRelocatingReplica(replica, postponedFunctionsContainer) {
				postponedFunctionsContainer.AddPostponedFunction(matchFunc, fmt.Sprintf("multi-match-below-independent %+v", replica.Key))
				// We bail out and trust our invoker to later call upon this postponed function
			} else {
				ExecuteOnTopology(func() { matchFunc() })
			}
		}()
	}
	for range replicas {
		<-barrier
	}
	if len(errs) == len(replicas) {
		// All returned with error
		return matchedReplicas, belowInstance, fmt.Errorf("MultiMatchBelowIndependently: Error on all %+v operations", len(errs)), errs
	}
	AuditOperation("multi-match-below-independent", belowKey, fmt.Sprintf("matched %d/%d replicas below %+v via Pseudo-GTID", len(matchedReplicas), len(replicas), belowKey))

	return matchedReplicas, belowInstance, err, errs
}

// MultiMatchReplicas will match (via pseudo-gtid) all replicas of given main below given instance.
func MultiMatchReplicas(mainKey *InstanceKey, belowKey *InstanceKey, pattern string) ([](*Instance), *Instance, error, []error) {
	res := [](*Instance){}
	errs := []error{}

	belowInstance, err := ReadTopologyInstance(belowKey)
	if err != nil {
		// Can't access "below" ==> can't match replicas beneath it
		return res, nil, err, errs
	}

	mainInstance, found, err := ReadInstance(mainKey)
	if err != nil || !found {
		return res, nil, err, errs
	}

	// See if we have a binlog server case (special handling):
	binlogCase := false
	if mainInstance.IsBinlogServer() && mainInstance.MainKey.Equals(belowKey) {
		// repoint-up
		log.Debugf("MultiMatchReplicas: pointing replicas up from binlog server")
		binlogCase = true
	} else if belowInstance.IsBinlogServer() && belowInstance.MainKey.Equals(mainKey) {
		// repoint-down
		log.Debugf("MultiMatchReplicas: pointing replicas down to binlog server")
		binlogCase = true
	} else if mainInstance.IsBinlogServer() && belowInstance.IsBinlogServer() && mainInstance.MainKey.Equals(&belowInstance.MainKey) {
		// Both BLS, siblings
		log.Debugf("MultiMatchReplicas: pointing replicas to binlong sibling")
		binlogCase = true
	}
	if binlogCase {
		replicas, err, errors := RepointReplicasTo(mainKey, pattern, belowKey)
		// Bail out!
		return replicas, mainInstance, err, errors
	}

	// Not binlog server

	// replicas involved
	replicas, err := ReadReplicaInstancesIncludingBinlogServerSubReplicas(mainKey)
	if err != nil {
		return res, belowInstance, err, errs
	}
	replicas = filterInstancesByPattern(replicas, pattern)
	matchedReplicas, belowInstance, err, errs := MultiMatchBelow(replicas, &belowInstance.Key, nil)

	if len(matchedReplicas) != len(replicas) {
		err = fmt.Errorf("MultiMatchReplicas: only matched %d out of %d replicas of %+v; error is: %+v", len(matchedReplicas), len(replicas), *mainKey, err)
	}
	AuditOperation("multi-match-replicas", mainKey, fmt.Sprintf("matched %d replicas under %+v", len(matchedReplicas), *belowKey))

	return matchedReplicas, belowInstance, err, errs
}

// MatchUp will move a replica up the replication chain, so that it becomes sibling of its main, via Pseudo-GTID
func MatchUp(instanceKey *InstanceKey, requireInstanceMaintenance bool) (*Instance, *BinlogCoordinates, error) {
	instance, found, err := ReadInstance(instanceKey)
	if err != nil || !found {
		return nil, nil, err
	}
	if !instance.IsReplica() {
		return instance, nil, fmt.Errorf("instance is not a replica: %+v", instanceKey)
	}
	main, found, err := ReadInstance(&instance.MainKey)
	if err != nil || !found {
		return instance, nil, log.Errorf("Cannot get main for %+v. error=%+v", instance.Key, err)
	}

	if !main.IsReplica() {
		return instance, nil, fmt.Errorf("main is not a replica itself: %+v", main.Key)
	}

	return MatchBelow(instanceKey, &main.MainKey, requireInstanceMaintenance)
}

// MatchUpReplicas will move all replicas of given main up the replication chain,
// so that they become siblings of their main.
// This should be called when the local main dies, and all its replicas are to be resurrected via Pseudo-GTID
func MatchUpReplicas(mainKey *InstanceKey, pattern string) ([](*Instance), *Instance, error, []error) {
	res := [](*Instance){}
	errs := []error{}

	mainInstance, found, err := ReadInstance(mainKey)
	if err != nil || !found {
		return res, nil, err, errs
	}

	return MultiMatchReplicas(mainKey, &mainInstance.MainKey, pattern)
}

func isGenerallyValidAsBinlogSource(replica *Instance) bool {
	if !replica.IsLastCheckValid {
		// something wrong with this replica right now. We shouldn't hope to be able to promote it
		return false
	}
	if !replica.LogBinEnabled {
		return false
	}
	if !replica.LogSubordinateUpdatesEnabled {
		return false
	}

	return true
}

func isGenerallyValidAsCandidateReplica(replica *Instance) bool {
	if !isGenerallyValidAsBinlogSource(replica) {
		// does not have binary logs
		return false
	}
	if replica.IsBinlogServer() {
		// Can't regroup under a binlog server because it does not support pseudo-gtid related queries such as SHOW BINLOG EVENTS
		return false
	}

	return true
}

// isValidAsCandidateMainInBinlogServerTopology let's us know whether a given replica is generally
// valid to promote to be main.
func isValidAsCandidateMainInBinlogServerTopology(replica *Instance) bool {
	if !replica.IsLastCheckValid {
		// something wrong with this replica right now. We shouldn't hope to be able to promote it
		return false
	}
	if !replica.LogBinEnabled {
		return false
	}
	if replica.LogSubordinateUpdatesEnabled {
		// That's right: we *disallow* log-replica-updates
		return false
	}
	if replica.IsBinlogServer() {
		return false
	}

	return true
}

func IsBannedFromBeingCandidateReplica(replica *Instance) bool {
	if replica.PromotionRule == MustNotPromoteRule {
		log.Debugf("instance %+v is banned because of promotion rule", replica.Key)
		return true
	}
	for _, filter := range config.Config.PromotionIgnoreHostnameFilters {
		if matched, _ := regexp.MatchString(filter, replica.Key.Hostname); matched {
			return true
		}
	}
	return false
}

// getPriorityMajorVersionForCandidate returns the primary (most common) major version found
// among given instances. This will be used for choosing best candidate for promotion.
func getPriorityMajorVersionForCandidate(replicas [](*Instance)) (priorityMajorVersion string, err error) {
	if len(replicas) == 0 {
		return "", log.Errorf("empty replicas list in getPriorityMajorVersionForCandidate")
	}
	majorVersionsCount := make(map[string]int)
	for _, replica := range replicas {
		majorVersionsCount[replica.MajorVersionString()] = majorVersionsCount[replica.MajorVersionString()] + 1
	}
	if len(majorVersionsCount) == 1 {
		// all same version, simple case
		return replicas[0].MajorVersionString(), nil
	}
	sorted := NewMajorVersionsSortedByCount(majorVersionsCount)
	sort.Sort(sort.Reverse(sorted))
	return sorted.First(), nil
}

// getPriorityBinlogFormatForCandidate returns the primary (most common) binlog format found
// among given instances. This will be used for choosing best candidate for promotion.
func getPriorityBinlogFormatForCandidate(replicas [](*Instance)) (priorityBinlogFormat string, err error) {
	if len(replicas) == 0 {
		return "", log.Errorf("empty replicas list in getPriorityBinlogFormatForCandidate")
	}
	binlogFormatsCount := make(map[string]int)
	for _, replica := range replicas {
		binlogFormatsCount[replica.Binlog_format] = binlogFormatsCount[replica.Binlog_format] + 1
	}
	if len(binlogFormatsCount) == 1 {
		// all same binlog format, simple case
		return replicas[0].Binlog_format, nil
	}
	sorted := NewBinlogFormatSortedByCount(binlogFormatsCount)
	sort.Sort(sort.Reverse(sorted))
	return sorted.First(), nil
}

// chooseCandidateReplica
func chooseCandidateReplica(replicas [](*Instance)) (candidateReplica *Instance, aheadReplicas, equalReplicas, laterReplicas, cannotReplicateReplicas [](*Instance), err error) {
	if len(replicas) == 0 {
		return candidateReplica, aheadReplicas, equalReplicas, laterReplicas, cannotReplicateReplicas, fmt.Errorf("No replicas found given in chooseCandidateReplica")
	}
	priorityMajorVersion, _ := getPriorityMajorVersionForCandidate(replicas)
	priorityBinlogFormat, _ := getPriorityBinlogFormatForCandidate(replicas)

	for _, replica := range replicas {
		replica := replica
		if isGenerallyValidAsCandidateReplica(replica) &&
			!IsBannedFromBeingCandidateReplica(replica) &&
			!IsSmallerMajorVersion(priorityMajorVersion, replica.MajorVersionString()) &&
			!IsSmallerBinlogFormat(priorityBinlogFormat, replica.Binlog_format) {
			// this is the one
			candidateReplica = replica
			break
		}
	}
	if candidateReplica == nil {
		// Unable to find a candidate that will main others.
		// Instead, pick a (single) replica which is not banned.
		for _, replica := range replicas {
			replica := replica
			if !IsBannedFromBeingCandidateReplica(replica) {
				// this is the one
				candidateReplica = replica
				break
			}
		}
		if candidateReplica != nil {
			replicas = RemoveInstance(replicas, &candidateReplica.Key)
		}
		return candidateReplica, replicas, equalReplicas, laterReplicas, cannotReplicateReplicas, fmt.Errorf("chooseCandidateReplica: no candidate replica found")
	}
	replicas = RemoveInstance(replicas, &candidateReplica.Key)
	for _, replica := range replicas {
		replica := replica
		if canReplicate, _ := replica.CanReplicateFrom(candidateReplica); !canReplicate {
			// lost due to inability to replicate
			cannotReplicateReplicas = append(cannotReplicateReplicas, replica)
		} else if replica.ExecBinlogCoordinates.SmallerThan(&candidateReplica.ExecBinlogCoordinates) {
			laterReplicas = append(laterReplicas, replica)
		} else if replica.ExecBinlogCoordinates.Equals(&candidateReplica.ExecBinlogCoordinates) {
			equalReplicas = append(equalReplicas, replica)
		} else {
			// lost due to being more advanced/ahead of chosen replica.
			aheadReplicas = append(aheadReplicas, replica)
		}
	}
	return candidateReplica, aheadReplicas, equalReplicas, laterReplicas, cannotReplicateReplicas, err
}

// GetCandidateReplica chooses the best replica to promote given a (possibly dead) main
func GetCandidateReplica(mainKey *InstanceKey, forRematchPurposes bool) (*Instance, [](*Instance), [](*Instance), [](*Instance), [](*Instance), error) {
	var candidateReplica *Instance
	aheadReplicas := [](*Instance){}
	equalReplicas := [](*Instance){}
	laterReplicas := [](*Instance){}
	cannotReplicateReplicas := [](*Instance){}

	dataCenterHint := ""
	if main, _, _ := ReadInstance(mainKey); main != nil {
		dataCenterHint = main.DataCenter
	}
	replicas, err := getReplicasForSorting(mainKey, false)
	if err != nil {
		return candidateReplica, aheadReplicas, equalReplicas, laterReplicas, cannotReplicateReplicas, err
	}
	stopReplicationMethod := NoStopReplication
	if forRematchPurposes {
		stopReplicationMethod = StopReplicationNicely
	}
	replicas = sortedReplicasDataCenterHint(replicas, stopReplicationMethod, dataCenterHint)
	if err != nil {
		return candidateReplica, aheadReplicas, equalReplicas, laterReplicas, cannotReplicateReplicas, err
	}
	if len(replicas) == 0 {
		return candidateReplica, aheadReplicas, equalReplicas, laterReplicas, cannotReplicateReplicas, fmt.Errorf("No replicas found for %+v", *mainKey)
	}
	candidateReplica, aheadReplicas, equalReplicas, laterReplicas, cannotReplicateReplicas, err = chooseCandidateReplica(replicas)
	if err != nil {
		return candidateReplica, aheadReplicas, equalReplicas, laterReplicas, cannotReplicateReplicas, err
	}
	if candidateReplica != nil {
		mostUpToDateReplica := replicas[0]
		if candidateReplica.ExecBinlogCoordinates.SmallerThan(&mostUpToDateReplica.ExecBinlogCoordinates) {
			log.Warningf("GetCandidateReplica: chosen replica: %+v is behind most-up-to-date replica: %+v", candidateReplica.Key, mostUpToDateReplica.Key)
		}
	}
	log.Debugf("GetCandidateReplica: candidate: %+v, ahead: %d, equal: %d, late: %d, break: %d", candidateReplica.Key, len(aheadReplicas), len(equalReplicas), len(laterReplicas), len(cannotReplicateReplicas))
	return candidateReplica, aheadReplicas, equalReplicas, laterReplicas, cannotReplicateReplicas, nil
}

// GetCandidateReplicaOfBinlogServerTopology chooses the best replica to promote given a (possibly dead) main
func GetCandidateReplicaOfBinlogServerTopology(mainKey *InstanceKey) (candidateReplica *Instance, err error) {
	replicas, err := getReplicasForSorting(mainKey, true)
	if err != nil {
		return candidateReplica, err
	}
	replicas = sortedReplicas(replicas, NoStopReplication)
	if len(replicas) == 0 {
		return candidateReplica, fmt.Errorf("No replicas found for %+v", *mainKey)
	}
	for _, replica := range replicas {
		replica := replica
		if candidateReplica != nil {
			break
		}
		if isValidAsCandidateMainInBinlogServerTopology(replica) && !IsBannedFromBeingCandidateReplica(replica) {
			// this is the one
			candidateReplica = replica
		}
	}
	if candidateReplica != nil {
		log.Debugf("GetCandidateReplicaOfBinlogServerTopology: returning %+v as candidate replica for %+v", candidateReplica.Key, *mainKey)
	} else {
		log.Debugf("GetCandidateReplicaOfBinlogServerTopology: no candidate replica found for %+v", *mainKey)
	}
	return candidateReplica, err
}

// RegroupReplicasPseudoGTID will choose a candidate replica of a given instance, and take its siblings using pseudo-gtid
func RegroupReplicasPseudoGTID(
	mainKey *InstanceKey,
	returnReplicaEvenOnFailureToRegroup bool,
	onCandidateReplicaChosen func(*Instance),
	postponedFunctionsContainer *PostponedFunctionsContainer,
	postponeAllMatchOperations func(*Instance) bool,
) (
	aheadReplicas [](*Instance),
	equalReplicas [](*Instance),
	laterReplicas [](*Instance),
	cannotReplicateReplicas [](*Instance),
	candidateReplica *Instance,
	err error,
) {
	candidateReplica, aheadReplicas, equalReplicas, laterReplicas, cannotReplicateReplicas, err = GetCandidateReplica(mainKey, true)
	if err != nil {
		if !returnReplicaEvenOnFailureToRegroup {
			candidateReplica = nil
		}
		return aheadReplicas, equalReplicas, laterReplicas, cannotReplicateReplicas, candidateReplica, err
	}

	if config.Config.PseudoGTIDPattern == "" {
		return aheadReplicas, equalReplicas, laterReplicas, cannotReplicateReplicas, candidateReplica, fmt.Errorf("PseudoGTIDPattern not configured; cannot use Pseudo-GTID")
	}

	if onCandidateReplicaChosen != nil {
		onCandidateReplicaChosen(candidateReplica)
	}

	allMatchingFunc := func() error {
		log.Debugf("RegroupReplicas: working on %d equals replicas", len(equalReplicas))
		barrier := make(chan *InstanceKey)
		for _, replica := range equalReplicas {
			replica := replica
			// This replica has the exact same executing coordinates as the candidate replica. This replica
			// is *extremely* easy to attach below the candidate replica!
			go func() {
				defer func() { barrier <- &candidateReplica.Key }()
				ExecuteOnTopology(func() {
					ChangeMainTo(&replica.Key, &candidateReplica.Key, &candidateReplica.SelfBinlogCoordinates, false, GTIDHintDeny)
				})
			}()
		}
		for range equalReplicas {
			<-barrier
		}

		log.Debugf("RegroupReplicas: multi matching %d later replicas", len(laterReplicas))
		// As for the laterReplicas, we'll have to apply pseudo GTID
		laterReplicas, candidateReplica, err, _ = MultiMatchBelow(laterReplicas, &candidateReplica.Key, postponedFunctionsContainer)

		operatedReplicas := append(equalReplicas, candidateReplica)
		operatedReplicas = append(operatedReplicas, laterReplicas...)
		log.Debugf("RegroupReplicas: starting %d replicas", len(operatedReplicas))
		barrier = make(chan *InstanceKey)
		for _, replica := range operatedReplicas {
			replica := replica
			go func() {
				defer func() { barrier <- &candidateReplica.Key }()
				ExecuteOnTopology(func() {
					StartSubordinate(&replica.Key)
				})
			}()
		}
		for range operatedReplicas {
			<-barrier
		}
		AuditOperation("regroup-replicas", mainKey, fmt.Sprintf("regrouped %+v replicas below %+v", len(operatedReplicas), *mainKey))
		return err
	}
	if postponedFunctionsContainer != nil && postponeAllMatchOperations != nil && postponeAllMatchOperations(candidateReplica) {
		postponedFunctionsContainer.AddPostponedFunction(allMatchingFunc, fmt.Sprintf("regroup-replicas-pseudo-gtid %+v", candidateReplica.Key))
	} else {
		err = allMatchingFunc()
	}
	log.Debugf("RegroupReplicas: done")
	// aheadReplicas are lost (they were ahead in replication as compared to promoted replica)
	return aheadReplicas, equalReplicas, laterReplicas, cannotReplicateReplicas, candidateReplica, err
}

func getMostUpToDateActiveBinlogServer(mainKey *InstanceKey) (mostAdvancedBinlogServer *Instance, binlogServerReplicas [](*Instance), err error) {
	if binlogServerReplicas, err = ReadBinlogServerReplicaInstances(mainKey); err == nil && len(binlogServerReplicas) > 0 {
		// Pick the most advanced binlog sever that is good to go
		for _, binlogServer := range binlogServerReplicas {
			if binlogServer.IsLastCheckValid {
				if mostAdvancedBinlogServer == nil {
					mostAdvancedBinlogServer = binlogServer
				}
				if mostAdvancedBinlogServer.ExecBinlogCoordinates.SmallerThan(&binlogServer.ExecBinlogCoordinates) {
					mostAdvancedBinlogServer = binlogServer
				}
			}
		}
	}
	return mostAdvancedBinlogServer, binlogServerReplicas, err
}

// RegroupReplicasPseudoGTIDIncludingSubReplicasOfBinlogServers uses Pseugo-GTID to regroup replicas
// of given instance. The function also drill in to replicas of binlog servers that are replicating from given instance,
// and other recursive binlog servers, as long as they're in the same binlog-server-family.
func RegroupReplicasPseudoGTIDIncludingSubReplicasOfBinlogServers(
	mainKey *InstanceKey,
	returnReplicaEvenOnFailureToRegroup bool,
	onCandidateReplicaChosen func(*Instance),
	postponedFunctionsContainer *PostponedFunctionsContainer,
	postponeAllMatchOperations func(*Instance) bool,
) (
	aheadReplicas [](*Instance),
	equalReplicas [](*Instance),
	laterReplicas [](*Instance),
	cannotReplicateReplicas [](*Instance),
	candidateReplica *Instance,
	err error,
) {
	// First, handle binlog server issues:
	func() error {
		log.Debugf("RegroupReplicasIncludingSubReplicasOfBinlogServers: starting on replicas of %+v", *mainKey)
		// Find the most up to date binlog server:
		mostUpToDateBinlogServer, binlogServerReplicas, err := getMostUpToDateActiveBinlogServer(mainKey)
		if err != nil {
			return log.Errore(err)
		}
		if mostUpToDateBinlogServer == nil {
			log.Debugf("RegroupReplicasIncludingSubReplicasOfBinlogServers: no binlog server replicates from %+v", *mainKey)
			// No binlog server; proceed as normal
			return nil
		}
		log.Debugf("RegroupReplicasIncludingSubReplicasOfBinlogServers: most up to date binlog server of %+v: %+v", *mainKey, mostUpToDateBinlogServer.Key)

		// Find the most up to date candidate replica:
		candidateReplica, _, _, _, _, err := GetCandidateReplica(mainKey, true)
		if err != nil {
			return log.Errore(err)
		}
		if candidateReplica == nil {
			log.Debugf("RegroupReplicasIncludingSubReplicasOfBinlogServers: no candidate replica for %+v", *mainKey)
			// Let the followup code handle that
			return nil
		}
		log.Debugf("RegroupReplicasIncludingSubReplicasOfBinlogServers: candidate replica of %+v: %+v", *mainKey, candidateReplica.Key)

		if candidateReplica.ExecBinlogCoordinates.SmallerThan(&mostUpToDateBinlogServer.ExecBinlogCoordinates) {
			log.Debugf("RegroupReplicasIncludingSubReplicasOfBinlogServers: candidate replica %+v coordinates smaller than binlog server %+v", candidateReplica.Key, mostUpToDateBinlogServer.Key)
			// Need to align under binlog server...
			candidateReplica, err = Repoint(&candidateReplica.Key, &mostUpToDateBinlogServer.Key, GTIDHintDeny)
			if err != nil {
				return log.Errore(err)
			}
			log.Debugf("RegroupReplicasIncludingSubReplicasOfBinlogServers: repointed candidate replica %+v under binlog server %+v", candidateReplica.Key, mostUpToDateBinlogServer.Key)
			candidateReplica, err = StartSubordinateUntilMainCoordinates(&candidateReplica.Key, &mostUpToDateBinlogServer.ExecBinlogCoordinates)
			if err != nil {
				return log.Errore(err)
			}
			log.Debugf("RegroupReplicasIncludingSubReplicasOfBinlogServers: aligned candidate replica %+v under binlog server %+v", candidateReplica.Key, mostUpToDateBinlogServer.Key)
			// and move back
			candidateReplica, err = Repoint(&candidateReplica.Key, mainKey, GTIDHintDeny)
			if err != nil {
				return log.Errore(err)
			}
			log.Debugf("RegroupReplicasIncludingSubReplicasOfBinlogServers: repointed candidate replica %+v under main %+v", candidateReplica.Key, *mainKey)
			return nil
		}
		// Either because it _was_ like that, or we _made_ it so,
		// candidate replica is as/more up to date than all binlog servers
		for _, binlogServer := range binlogServerReplicas {
			log.Debugf("RegroupReplicasIncludingSubReplicasOfBinlogServers: matching replicas of binlog server %+v below %+v", binlogServer.Key, candidateReplica.Key)
			// Right now sequentially.
			// At this point just do what you can, don't return an error
			MultiMatchReplicas(&binlogServer.Key, &candidateReplica.Key, "")
			log.Debugf("RegroupReplicasIncludingSubReplicasOfBinlogServers: done matching replicas of binlog server %+v below %+v", binlogServer.Key, candidateReplica.Key)
		}
		log.Debugf("RegroupReplicasIncludingSubReplicasOfBinlogServers: done handling binlog regrouping for %+v; will proceed with normal RegroupReplicas", *mainKey)
		AuditOperation("regroup-replicas-including-bls", mainKey, fmt.Sprintf("matched replicas of binlog server replicas of %+v under %+v", *mainKey, candidateReplica.Key))
		return nil
	}()
	// Proceed to normal regroup:
	return RegroupReplicasPseudoGTID(mainKey, returnReplicaEvenOnFailureToRegroup, onCandidateReplicaChosen, postponedFunctionsContainer, postponeAllMatchOperations)
}

// RegroupReplicasGTID will choose a candidate replica of a given instance, and take its siblings using GTID
func RegroupReplicasGTID(
	mainKey *InstanceKey,
	returnReplicaEvenOnFailureToRegroup bool,
	onCandidateReplicaChosen func(*Instance),
	postponedFunctionsContainer *PostponedFunctionsContainer,
	postponeAllMatchOperations func(*Instance) bool,
) (
	lostReplicas [](*Instance),
	movedReplicas [](*Instance),
	cannotReplicateReplicas [](*Instance),
	candidateReplica *Instance,
	err error,
) {
	var emptyReplicas [](*Instance)
	var unmovedReplicas [](*Instance)
	candidateReplica, aheadReplicas, equalReplicas, laterReplicas, cannotReplicateReplicas, err := GetCandidateReplica(mainKey, true)
	if err != nil {
		if !returnReplicaEvenOnFailureToRegroup {
			candidateReplica = nil
		}
		return emptyReplicas, emptyReplicas, emptyReplicas, candidateReplica, err
	}

	if onCandidateReplicaChosen != nil {
		onCandidateReplicaChosen(candidateReplica)
	}
	moveGTIDFunc := func() error {
		replicasToMove := append(equalReplicas, laterReplicas...)
		log.Debugf("RegroupReplicasGTID: working on %d replicas", len(replicasToMove))

		movedReplicas, unmovedReplicas, err, _ = moveReplicasViaGTID(replicasToMove, candidateReplica, postponedFunctionsContainer)
		unmovedReplicas = append(unmovedReplicas, aheadReplicas...)
		return log.Errore(err)
	}
	if postponedFunctionsContainer != nil && postponeAllMatchOperations != nil && postponeAllMatchOperations(candidateReplica) {
		postponedFunctionsContainer.AddPostponedFunction(moveGTIDFunc, fmt.Sprintf("regroup-replicas-gtid %+v", candidateReplica.Key))
	} else {
		err = moveGTIDFunc()
	}

	StartSubordinate(&candidateReplica.Key)

	log.Debugf("RegroupReplicasGTID: done")
	AuditOperation("regroup-replicas-gtid", mainKey, fmt.Sprintf("regrouped replicas of %+v via GTID; promoted %+v", *mainKey, candidateReplica.Key))
	return unmovedReplicas, movedReplicas, cannotReplicateReplicas, candidateReplica, err
}

// RegroupReplicasBinlogServers works on a binlog-servers topology. It picks the most up-to-date BLS and repoints all other
// BLS below it
func RegroupReplicasBinlogServers(mainKey *InstanceKey, returnReplicaEvenOnFailureToRegroup bool) (repointedBinlogServers [](*Instance), promotedBinlogServer *Instance, err error) {
	var binlogServerReplicas [](*Instance)
	promotedBinlogServer, binlogServerReplicas, err = getMostUpToDateActiveBinlogServer(mainKey)

	resultOnError := func(err error) ([](*Instance), *Instance, error) {
		if !returnReplicaEvenOnFailureToRegroup {
			promotedBinlogServer = nil
		}
		return repointedBinlogServers, promotedBinlogServer, err
	}

	if err != nil {
		return resultOnError(err)
	}

	repointedBinlogServers, err, _ = RepointTo(binlogServerReplicas, &promotedBinlogServer.Key)

	if err != nil {
		return resultOnError(err)
	}
	AuditOperation("regroup-replicas-bls", mainKey, fmt.Sprintf("regrouped binlog server replicas of %+v; promoted %+v", *mainKey, promotedBinlogServer.Key))
	return repointedBinlogServers, promotedBinlogServer, nil
}

// RegroupReplicas is a "smart" method of promoting one replica over the others ("promoting" it on top of its siblings)
// This method decides which strategy to use: GTID, Pseudo-GTID, Binlog Servers.
func RegroupReplicas(mainKey *InstanceKey, returnReplicaEvenOnFailureToRegroup bool,
	onCandidateReplicaChosen func(*Instance),
	postponedFunctionsContainer *PostponedFunctionsContainer) (

	aheadReplicas [](*Instance),
	equalReplicas [](*Instance),
	laterReplicas [](*Instance),
	cannotReplicateReplicas [](*Instance),
	instance *Instance,
	err error,
) {
	//
	var emptyReplicas [](*Instance)

	replicas, err := ReadReplicaInstances(mainKey)
	if err != nil {
		return emptyReplicas, emptyReplicas, emptyReplicas, emptyReplicas, instance, err
	}
	if len(replicas) == 0 {
		return emptyReplicas, emptyReplicas, emptyReplicas, emptyReplicas, instance, err
	}
	if len(replicas) == 1 {
		return emptyReplicas, emptyReplicas, emptyReplicas, emptyReplicas, replicas[0], err
	}
	allGTID := true
	allBinlogServers := true
	allPseudoGTID := true
	for _, replica := range replicas {
		if !replica.UsingGTID() {
			allGTID = false
		}
		if !replica.IsBinlogServer() {
			allBinlogServers = false
		}
		if !replica.UsingPseudoGTID {
			allPseudoGTID = false
		}
	}
	if allGTID {
		log.Debugf("RegroupReplicas: using GTID to regroup replicas of %+v", *mainKey)
		unmovedReplicas, movedReplicas, cannotReplicateReplicas, candidateReplica, err := RegroupReplicasGTID(mainKey, returnReplicaEvenOnFailureToRegroup, onCandidateReplicaChosen, nil, nil)
		return unmovedReplicas, emptyReplicas, movedReplicas, cannotReplicateReplicas, candidateReplica, err
	}
	if allBinlogServers {
		log.Debugf("RegroupReplicas: using binlog servers to regroup replicas of %+v", *mainKey)
		movedReplicas, candidateReplica, err := RegroupReplicasBinlogServers(mainKey, returnReplicaEvenOnFailureToRegroup)
		return emptyReplicas, emptyReplicas, movedReplicas, cannotReplicateReplicas, candidateReplica, err
	}
	if allPseudoGTID {
		log.Debugf("RegroupReplicas: using Pseudo-GTID to regroup replicas of %+v", *mainKey)
		return RegroupReplicasPseudoGTID(mainKey, returnReplicaEvenOnFailureToRegroup, onCandidateReplicaChosen, postponedFunctionsContainer, nil)
	}
	// And, as last resort, we do PseudoGTID & binlog servers
	log.Warningf("RegroupReplicas: unsure what method to invoke for %+v; trying Pseudo-GTID+Binlog Servers", *mainKey)
	return RegroupReplicasPseudoGTIDIncludingSubReplicasOfBinlogServers(mainKey, returnReplicaEvenOnFailureToRegroup, onCandidateReplicaChosen, postponedFunctionsContainer, nil)
}

// relocateBelowInternal is a protentially recursive function which chooses how to relocate an instance below another.
// It may choose to use Pseudo-GTID, or normal binlog positions, or take advantage of binlog servers,
// or it may combine any of the above in a multi-step operation.
func relocateBelowInternal(instance, other *Instance) (*Instance, error) {
	if canReplicate, err := instance.CanReplicateFrom(other); !canReplicate {
		return instance, log.Errorf("%+v cannot replicate from %+v. Reason: %+v", instance.Key, other.Key, err)
	}
	// simplest:
	if InstanceIsMainOf(other, instance) {
		// already the desired setup.
		return Repoint(&instance.Key, &other.Key, GTIDHintNeutral)
	}
	// Do we have record of equivalent coordinates?
	if !instance.IsBinlogServer() {
		if movedInstance, err := MoveEquivalent(&instance.Key, &other.Key); err == nil {
			return movedInstance, nil
		}
	}
	// Try and take advantage of binlog servers:
	if InstancesAreSiblings(instance, other) && other.IsBinlogServer() {
		return MoveBelow(&instance.Key, &other.Key)
	}
	instanceMain, _, err := ReadInstance(&instance.MainKey)
	if err != nil {
		return instance, err
	}
	if instanceMain != nil && instanceMain.MainKey.Equals(&other.Key) && instanceMain.IsBinlogServer() {
		// Moving to grandparent via binlog server
		return Repoint(&instance.Key, &instanceMain.MainKey, GTIDHintDeny)
	}
	if other.IsBinlogServer() {
		if instanceMain != nil && instanceMain.IsBinlogServer() && InstancesAreSiblings(instanceMain, other) {
			// Special case: this is a binlog server family; we move under the uncle, in one single step
			return Repoint(&instance.Key, &other.Key, GTIDHintDeny)
		}

		// Relocate to its main, then repoint to the binlog server
		otherMain, found, err := ReadInstance(&other.MainKey)
		if err != nil {
			return instance, err
		}
		if !found {
			return instance, log.Errorf("Cannot find main %+v", other.MainKey)
		}
		if !other.IsLastCheckValid {
			return instance, log.Errorf("Binlog server %+v is not reachable. It would take two steps to relocate %+v below it, and I won't even do the first step.", other.Key, instance.Key)
		}

		log.Debugf("Relocating to a binlog server; will first attempt to relocate to the binlog server's main: %+v, and then repoint down", otherMain.Key)
		if _, err := relocateBelowInternal(instance, otherMain); err != nil {
			return instance, err
		}
		return Repoint(&instance.Key, &other.Key, GTIDHintDeny)
	}
	if instance.IsBinlogServer() {
		// Can only move within the binlog-server family tree
		// And these have been covered just now: move up from a main binlog server, move below a binling binlog server.
		// sure, the family can be more complex, but we keep these operations atomic
		return nil, log.Errorf("Relocating binlog server %+v below %+v turns to be too complex; please do it manually", instance.Key, other.Key)
	}
	// Next, try GTID
	if _, _, gtidCompatible := instancesAreGTIDAndCompatible(instance, other); gtidCompatible {
		return moveInstanceBelowViaGTID(instance, other)
	}

	// Next, try Pseudo-GTID
	if instance.UsingPseudoGTID && other.UsingPseudoGTID {
		// We prefer PseudoGTID to anything else because, while it takes longer to run, it does not issue
		// a STOP SLAVE on any server other than "instance" itself.
		instance, _, err := MatchBelow(&instance.Key, &other.Key, true)
		return instance, err
	}
	// No Pseudo-GTID; cehck simple binlog file/pos operations:
	if InstancesAreSiblings(instance, other) {
		// If comaining, only move below if it's read-only
		if !other.IsCoMain || other.ReadOnly {
			return MoveBelow(&instance.Key, &other.Key)
		}
	}
	// See if we need to MoveUp
	if instanceMain != nil && instanceMain.MainKey.Equals(&other.Key) {
		// Moving to grandparent--handles co-maining writable case
		return MoveUp(&instance.Key)
	}
	if instanceMain != nil && instanceMain.IsBinlogServer() {
		// Break operation into two: move (repoint) up, then continue
		if _, err := MoveUp(&instance.Key); err != nil {
			return instance, err
		}
		return relocateBelowInternal(instance, other)
	}
	// Too complex
	return nil, log.Errorf("Relocating %+v below %+v turns to be too complex; please do it manually", instance.Key, other.Key)
}

// RelocateBelow will attempt moving instance indicated by instanceKey below another instance.
// Orchestrator will try and figure out the best way to relocate the server. This could span normal
// binlog-position, pseudo-gtid, repointing, binlog servers...
func RelocateBelow(instanceKey, otherKey *InstanceKey) (*Instance, error) {
	instance, found, err := ReadInstance(instanceKey)
	if err != nil || !found {
		return instance, log.Errorf("Error reading %+v", *instanceKey)
	}
	other, found, err := ReadInstance(otherKey)
	if err != nil || !found {
		return instance, log.Errorf("Error reading %+v", *otherKey)
	}
	if other.IsDescendantOf(instance) {
		return instance, log.Errorf("relocate: %+v is a descendant of %+v", *otherKey, instance.Key)
	}
	instance, err = relocateBelowInternal(instance, other)
	if err == nil {
		AuditOperation("relocate-below", instanceKey, fmt.Sprintf("relocated %+v below %+v", *instanceKey, *otherKey))
	}
	return instance, err
}

// relocateReplicasInternal is a protentially recursive function which chooses how to relocate
// replicas of an instance below another.
// It may choose to use Pseudo-GTID, or normal binlog positions, or take advantage of binlog servers,
// or it may combine any of the above in a multi-step operation.
func relocateReplicasInternal(replicas [](*Instance), instance, other *Instance) ([](*Instance), error, []error) {
	errs := []error{}
	var err error
	// simplest:
	if instance.Key.Equals(&other.Key) {
		// already the desired setup.
		return RepointTo(replicas, &other.Key)
	}
	// Try and take advantage of binlog servers:
	if InstanceIsMainOf(other, instance) && instance.IsBinlogServer() {
		// Up from a binlog server
		return RepointTo(replicas, &other.Key)
	}
	if InstanceIsMainOf(instance, other) && other.IsBinlogServer() {
		// Down under a binlog server
		return RepointTo(replicas, &other.Key)
	}
	if InstancesAreSiblings(instance, other) && instance.IsBinlogServer() && other.IsBinlogServer() {
		// Between siblings
		return RepointTo(replicas, &other.Key)
	}
	if other.IsBinlogServer() {
		// Relocate to binlog server's parent (recursive call), then repoint down
		otherMain, found, err := ReadInstance(&other.MainKey)
		if err != nil || !found {
			return nil, err, errs
		}
		replicas, err, errs = relocateReplicasInternal(replicas, instance, otherMain)
		if err != nil {
			return replicas, err, errs
		}

		return RepointTo(replicas, &other.Key)
	}
	// GTID
	{
		movedReplicas, unmovedReplicas, err, errs := moveReplicasViaGTID(replicas, other, nil)

		if len(movedReplicas) == len(replicas) {
			// Moved (or tried moving) everything via GTID
			return movedReplicas, err, errs
		} else if len(movedReplicas) > 0 {
			// something was moved via GTID; let's try further on
			return relocateReplicasInternal(unmovedReplicas, instance, other)
		}
		// Otherwise nothing was moved via GTID. Maybe we don't have any GTIDs, we continue.
	}

	// Pseudo GTID
	if other.UsingPseudoGTID {
		// Which replicas are using Pseudo GTID?
		var pseudoGTIDReplicas [](*Instance)
		for _, replica := range replicas {
			_, _, hasToBeGTID := instancesAreGTIDAndCompatible(replica, other)
			if replica.UsingPseudoGTID && !hasToBeGTID {
				pseudoGTIDReplicas = append(pseudoGTIDReplicas, replica)
			}
		}
		pseudoGTIDReplicas, _, err, errs = MultiMatchBelow(pseudoGTIDReplicas, &other.Key, nil)
		return pseudoGTIDReplicas, err, errs
	}

	// Normal binlog file:pos
	if InstanceIsMainOf(other, instance) {
		// MoveUpReplicas -- but not supporting "replicas" argument at this time.
	}

	// Too complex
	return nil, log.Errorf("Relocating %+v replicas of %+v below %+v turns to be too complex; please do it manually", len(replicas), instance.Key, other.Key), errs
}

// RelocateReplicas will attempt moving replicas of an instance indicated by instanceKey below another instance.
// Orchestrator will try and figure out the best way to relocate the servers. This could span normal
// binlog-position, pseudo-gtid, repointing, binlog servers...
func RelocateReplicas(instanceKey, otherKey *InstanceKey, pattern string) (replicas [](*Instance), other *Instance, err error, errs []error) {

	instance, found, err := ReadInstance(instanceKey)
	if err != nil || !found {
		return replicas, other, log.Errorf("Error reading %+v", *instanceKey), errs
	}
	other, found, err = ReadInstance(otherKey)
	if err != nil || !found {
		return replicas, other, log.Errorf("Error reading %+v", *otherKey), errs
	}

	replicas, err = ReadReplicaInstances(instanceKey)
	if err != nil {
		return replicas, other, err, errs
	}
	replicas = RemoveInstance(replicas, otherKey)
	replicas = filterInstancesByPattern(replicas, pattern)
	if len(replicas) == 0 {
		// Nothing to do
		return replicas, other, nil, errs
	}
	for _, replica := range replicas {
		if other.IsDescendantOf(replica) {
			return replicas, other, log.Errorf("relocate-replicas: %+v is a descendant of %+v", *otherKey, replica.Key), errs
		}
	}
	replicas, err, errs = relocateReplicasInternal(replicas, instance, other)

	if err == nil {
		AuditOperation("relocate-replicas", instanceKey, fmt.Sprintf("relocated %+v replicas of %+v below %+v", len(replicas), *instanceKey, *otherKey))
	}
	return replicas, other, err, errs
}

// PurgeBinaryLogsTo attempts to 'PURGE BINARY LOGS' until given binary log is reached
func PurgeBinaryLogsTo(instanceKey *InstanceKey, logFile string, force bool) (*Instance, error) {
	replicas, err := ReadReplicaInstances(instanceKey)
	if err != nil {
		return nil, err
	}
	if !force {
		purgeCoordinates := &BinlogCoordinates{LogFile: logFile, LogPos: 0}
		for _, replica := range replicas {
			if !purgeCoordinates.SmallerThan(&replica.ExecBinlogCoordinates) {
				return nil, log.Errorf("Unsafe to purge binary logs on %+v up to %s because replica %+v has only applied up to %+v", *instanceKey, logFile, replica.Key, replica.ExecBinlogCoordinates)
			}
		}
	}
	return purgeBinaryLogsTo(instanceKey, logFile)
}

// PurgeBinaryLogsToLatest attempts to 'PURGE BINARY LOGS' until latest binary log
func PurgeBinaryLogsToLatest(instanceKey *InstanceKey, force bool) (*Instance, error) {
	instance, err := ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, log.Errore(err)
	}
	return PurgeBinaryLogsTo(instanceKey, instance.SelfBinlogCoordinates.LogFile, force)
}
