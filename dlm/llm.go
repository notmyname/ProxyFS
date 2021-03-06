package dlm

import (
	"container/list"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/swiftstack/ProxyFS/blunder"
)

// This struct is used by LLM to track a lock.
type localLockTrack struct {
	lockId string // For debugging use
	sync.Mutex
	owners       uint64 // Count of threads which own lock
	waiters      uint64 // Count of threads which want to own the lock (either shared or exclusive)
	state        lockState
	listOfOwners []CallerID
	waitReqQ     *list.List // List of requests waiting for lock
}

type localLockRequest struct {
	requestedState lockState
	*sync.Cond
	wakeUp       bool
	LockCallerID CallerID
}

type lockState int

const (
	nilType lockState = iota
	shared
	exclusive
	stale
)

// NOTE: This is a test-only interface used for unit tests.
//
// This function assumes that globals.Lock() is held.
// TODO - can this be used in more cases without creating entry it if does not exist?
func getTrack(lockId string) (track *localLockTrack, ok bool) {
	track, ok = globals.localLockMap[lockId]
	if !ok {
		return track, ok
	}
	return track, ok
}

// NOTE: This is a test-only interface used for unit tests.
func waitCountWaiters(lockId string, count uint64) {
	for {
		globals.Lock()
		track, ok := getTrack(lockId)

		// If the tracking object has not been created yet, sleep and retry.
		if !ok {
			// Sleep 5 milliseconds and test again
			globals.Unlock()
			time.Sleep(5 * time.Millisecond)
			break
		}

		track.Mutex.Lock()

		globals.Unlock()

		waiters := track.waiters
		track.Mutex.Unlock()

		if waiters == count {
			return
		} else {
			// Sleep 5 milliseconds and test again
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// NOTE: This is a test-only interface used for unit tests.
func waitCountOwners(lockId string, count uint64) {
	for {
		globals.Lock()
		track, ok := getTrack(lockId)

		// If the tracking object has not been created yet, sleep and retry.
		if !ok {
			// Sleep 5 milliseconds and test again
			globals.Unlock()
			time.Sleep(5 * time.Millisecond)
			break
		}

		track.Mutex.Lock()

		globals.Unlock()

		owners := track.owners
		track.Mutex.Unlock()

		if owners == count {
			return
		} else {
			// Sleep 5 milliseconds and test again
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// This function assumes the mutex is held on the tracker structure
func removeFromListOfOwners(listOfOwners []CallerID, callerID CallerID) {
	// Find Position
	for i, id := range listOfOwners {
		if id == callerID {
			listOfOwners = append(listOfOwners[:i], listOfOwners[i+1:]...)
			return
		}
	}

	panic(fmt.Sprintf("Can't find CallerID: %v in list of lock owners!", callerID))
}

// This function assumes the mutex is held on the tracker structure
func callerInListOfOwners(listOfOwners []CallerID, callerID CallerID) (amOwner bool) {
	// Find Position
	for _, id := range listOfOwners {
		if id == callerID {
			return true
		}
	}

	return false
}

func isLockHeld(lockID string, callerID CallerID, lockHeldType LockHeldType) (held bool) {
	globals.Lock()
	// NOTE: Not doing a defer globals.Unlock() here since grabbing another lock below.

	track, ok := globals.localLockMap[lockID]
	if !ok {

		// Lock does not exist in map
		globals.Unlock()
		return false
	}

	track.Mutex.Lock()

	globals.Unlock()

	defer track.Mutex.Unlock()

	switch lockHeldType {
	case READLOCK:
		return (track.state == shared) && (callerInListOfOwners(track.listOfOwners, callerID))
	case WRITELOCK:
		return (track.state == exclusive) && (callerInListOfOwners(track.listOfOwners, callerID))
	case ANYLOCK:
		return ((track.state == exclusive) || (track.state == shared)) && (callerInListOfOwners(track.listOfOwners, callerID))
	}
	return false
}

func grantAndSignal(track *localLockTrack, localQRequest *localLockRequest) {
	track.state = localQRequest.requestedState
	track.listOfOwners = append(track.listOfOwners, localQRequest.LockCallerID)
	track.owners++
	localQRequest.wakeUp = true
	localQRequest.Cond.Broadcast()
}

// Process the waitReqQ and see if any locks can be granted.
//
// This function assumes that the tracking mutex is held.
func processLocalQ(track *localLockTrack) {

	// If nothing on queue then return
	if track.waitReqQ.Len() == 0 {
		return
	}

	// If the lock is already held exclusively then nothing to do.
	if track.state == exclusive {
		return
	}

	// At this point, the lock is either stale or shared
	//
	// Loop through Q and see if a request can be granted.  If it can then pop it off the Q.
	for track.waitReqQ.Len() > 0 {
		elem := track.waitReqQ.Remove(track.waitReqQ.Front())
		var localQRequest *localLockRequest
		var ok bool
		if localQRequest, ok = elem.(*localLockRequest); !ok {
			panic("Remove of elem failed!!!")
		}

		// If the lock is already free and then want it exclusive
		if (localQRequest.requestedState == exclusive) && (track.state == stale) {
			grantAndSignal(track, localQRequest)
			return
		}

		// If want exclusive and not free, we can't grant so push on front and break from loop.
		if localQRequest.requestedState == exclusive {
			track.waitReqQ.PushFront(localQRequest)
			return
		}

		// At this point we know the Q entry is shared.  Grant it now.
		grantAndSignal(track, localQRequest)
	}
}

func (l *RWLockStruct) commonLock(requestedState lockState, try bool) (err error) {

	globals.Lock()
	track, ok := globals.localLockMap[l.LockID]
	if !ok {
		// TODO - handle blocking waiting for lock from DLM

		// Lock does not exist in map, create one
		track = &localLockTrack{lockId: l.LockID, state: stale}
		track.waitReqQ = list.New()
		globals.localLockMap[l.LockID] = track

	}

	track.Mutex.Lock()
	defer track.Mutex.Unlock()

	globals.Unlock()

	// If we are doing a TryWriteLock or TryReadLock, see if we could
	// grab the lock before putting on queue.
	if try {
		if (requestedState == exclusive) && (track.state != stale) {
			err = errors.New("Lock is busy - try again!")
			return blunder.AddError(err, blunder.TryAgainError)
		} else {
			if track.state == exclusive {
				err = errors.New("Lock is busy - try again!")
				return blunder.AddError(err, blunder.TryAgainError)
			}
		}
	}
	localRequest := localLockRequest{requestedState: requestedState, LockCallerID: l.LockCallerID, wakeUp: false}
	localRequest.Cond = sync.NewCond(&track.Mutex)
	track.waitReqQ.PushBack(&localRequest)

	track.waiters++

	// See if any locks can be granted
	processLocalQ(track)

	// wakeUp will already be true if processLocalQ() signaled this thread to wakeup.
	for localRequest.wakeUp == false {
		localRequest.Cond.Wait()
	}

	// At this point, we got the lock either by the call to processLocalQ() above
	// or as a result of processLocalQ() being called from the unlock() path.

	// We decrement waiters here instead of in processLocalQ() so that other threads do not
	// assume there are no waiters between the time the Cond is signaled and we wakeup this thread.
	track.waiters--

	return nil
}

// unlock() releases the lock and signals any waiters that the lock is free.
func (l *RWLockStruct) unlock() (err error) {

	// TODO - assert not stale and if shared that count != 0
	globals.Lock()
	track, ok := globals.localLockMap[l.LockID]
	if !ok {
		panic(fmt.Sprintf("Trying to Unlock() inode: %v and lock not found in localLockMap()!", l.LockID))
	}

	track.Mutex.Lock()

	// Remove lock from localLockMap if no other thread using.
	//
	// We have track structure for lock.  While holding mutex on localLockMap, remove
	// lock from map if we are the last holder of the lock.
	// TODO - does this handle revoke case and any others?
	if (track.owners == 1) && (track.waiters == 0) {
		delete(globals.localLockMap, l.LockID)
	}

	globals.Unlock()

	// TODO - handle release of lock back to DLM and delete from localLockMap
	// Set stale and signal any waiters
	track.owners--
	removeFromListOfOwners(track.listOfOwners, l.LockCallerID)
	if track.owners == 0 {
		track.state = stale
	} else {
		if track.owners < 0 {
			panic("track.owners < 0!!!")
		}
	}

	// See if any locks can be granted
	processLocalQ(track)

	track.Mutex.Unlock()

	// TODO what error is possible?
	return nil
}
