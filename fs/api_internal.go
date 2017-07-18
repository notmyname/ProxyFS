// Package fs, sitting on top of the inode manager, defines the filesystem exposed by ProxyFS.
package fs

import (
	"bytes"
	"container/list"
	"fmt"
	"math"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/swiftstack/ProxyFS/blunder"
	"github.com/swiftstack/ProxyFS/dlm"
	"github.com/swiftstack/ProxyFS/inode"
	"github.com/swiftstack/ProxyFS/logger"
	"github.com/swiftstack/ProxyFS/stats"
	"github.com/swiftstack/ProxyFS/utils"
)

// Shorthand for our internal API debug log id; global to the package
var internalDebug = logger.DbgInternal

type symlinkFollowState struct {
	seen      map[inode.InodeNumber]bool
	traversed int
}

// Let us sort an array of directory and file names
type dirAndFileName struct {
	dirName  string
	fileName string
}

// this has to be a named type to be a method receiver
type dirAndFileNameSlice []dirAndFileName

func (coll dirAndFileNameSlice) Len() int {
	return len(coll)
}

func (coll dirAndFileNameSlice) Less(i int, j int) bool {
	return coll[i].dirName < coll[j].dirName
}

func (coll dirAndFileNameSlice) Swap(i int, j int) {
	coll[i], coll[j] = coll[j], coll[i]
}

func mount(volumeName string, mountOptions MountOptions) (mountHandle MountHandle, err error) {
	volumeHandle, err := inode.FetchVolumeHandle(volumeName)
	if nil != err {
		logger.ErrorWithError(err)
		return
	}

	globals.Lock()
	globals.lastMountID++
	mS := &mountStruct{
		id:           globals.lastMountID,
		userID:       inode.InodeRootUserID,  // TODO: Remove this
		groupID:      inode.InodeRootGroupID, // TODO: Remove this
		volumeName:   volumeName,
		options:      mountOptions,
		VolumeHandle: volumeHandle,
	}
	globals.mountMap[mS.id] = mS
	_, ok := globals.volumeMap[volumeName]
	if !ok {
		volStruct := &volumeStruct{}
		volStruct.FLockMap = make(map[inode.InodeNumber]*list.List)
		globals.volumeMap[volumeName] = volStruct
	}
	globals.Unlock()

	mountHandle = mS
	err = nil

	return
}

func (mS *mountStruct) Access(userID inode.InodeUserID, groupID inode.InodeGroupID, otherGroupIDs []inode.InodeGroupID, inodeNumber inode.InodeNumber, accessMode inode.InodeMode) (accessReturn bool) {
	accessReturn = mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, accessMode)
	return
}

func (mS *mountStruct) CallInodeToProvisionObject() (pPath string, err error) {
	pPath, err = mS.ProvisionObject()
	stats.IncrementOperations(&stats.FsProvisionObjOps)
	return
}

func (mS *mountStruct) Create(userID inode.InodeUserID, groupID inode.InodeGroupID, otherGroupIDs []inode.InodeGroupID, dirInodeNumber inode.InodeNumber, basename string, filePerm inode.InodeMode) (fileInodeNumber inode.InodeNumber, err error) {
	err = validateBaseName(basename)
	if err != nil {
		return 0, err
	}

	fileInodeNumber, err = mS.CreateFile(filePerm, userID, groupID)
	if err != nil {
		return 0, err
	}

	// Lock the directory inode before doing the link
	dirInodeLock, err := mS.initInodeLock(dirInodeNumber, nil)
	if err != nil {
		destroyErr := mS.Destroy(fileInodeNumber)
		if destroyErr != nil {
			logger.WarnfWithError(destroyErr, "couldn't destroy inode %v after failed initInodeLock() check in fs.Create", fileInodeNumber)
		}
		return 0, err
	}
	err = dirInodeLock.WriteLock()
	if err != nil {
		destroyErr := mS.Destroy(fileInodeNumber)
		if destroyErr != nil {
			logger.WarnfWithError(destroyErr, "couldn't destroy inode %v after failed WriteLock() in fs.Create", fileInodeNumber)
		}
		return 0, err
	}
	defer dirInodeLock.Unlock()

	if !mS.VolumeHandle.Access(dirInodeNumber, userID, groupID, otherGroupIDs, inode.F_OK) {
		destroyErr := mS.Destroy(fileInodeNumber)
		if destroyErr != nil {
			logger.WarnfWithError(destroyErr, "couldn't destroy inode %v after failed Access(F_OK) in fs.Create", fileInodeNumber)
		}
		return 0, blunder.NewError(blunder.NotFoundError, "ENOENT")
	}
	if !mS.VolumeHandle.Access(dirInodeNumber, userID, groupID, otherGroupIDs, inode.W_OK|inode.X_OK) {
		destroyErr := mS.Destroy(fileInodeNumber)
		if destroyErr != nil {
			logger.WarnfWithError(destroyErr, "couldn't destroy inode %v after failed Access(W_OK|X_OK) in fs.Create", fileInodeNumber)
		}
		return 0, blunder.NewError(blunder.PermDeniedError, "EACCES")
	}

	err = mS.VolumeHandle.Link(dirInodeNumber, basename, fileInodeNumber)
	if err != nil {
		destroyErr := mS.Destroy(fileInodeNumber)
		if destroyErr != nil {
			logger.WarnfWithError(destroyErr, "couldn't destroy inode %v after failed Link() in fs.Create", fileInodeNumber)
		}
		return 0, err
	}

	stats.IncrementOperations(&stats.FsCreateOps)
	return fileInodeNumber, nil
}

func (mS *mountStruct) Flush(userID inode.InodeUserID, groupID inode.InodeGroupID, otherGroupIDs []inode.InodeGroupID, inodeNumber inode.InodeNumber) (err error) {
	inodeLock, err := mS.initInodeLock(inodeNumber, nil)
	if err != nil {
		return
	}
	err = inodeLock.WriteLock()
	if err != nil {
		return
	}
	defer inodeLock.Unlock()

	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.F_OK) {
		return blunder.NewError(blunder.NotFoundError, "ENOENT")
	}
	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.W_OK) {
		return blunder.NewError(blunder.PermDeniedError, "EACCES")
	}

	stats.IncrementOperations(&stats.FsFlushOps)
	return mS.VolumeHandle.Flush(inodeNumber, false)
}

func (mS *mountStruct) getFileLockList(inodeNumber inode.InodeNumber) (fLocklist *list.List, err error) {
	globals.Lock()
	vol, ok := globals.volumeMap[mS.volumeName]
	globals.Unlock()

	if !ok {
		err = fmt.Errorf("Logic error... mS.volumeName == %v not found in globals.volumeMap", mS.volumeName)
		err = blunder.AddError(err, blunder.BadMountVolumeError)
		return
	}

	vol.Lock()
	defer vol.Unlock()

	fLocklist, ok = vol.FLockMap[inodeNumber]
	if !ok {
		fLocklist = list.New()
		vol.FLockMap[inodeNumber] = fLocklist
	}

	err = nil
	return
}

func (mS *mountStruct) Flock(userID inode.InodeUserID, groupID inode.InodeGroupID, otherGroupIDs []inode.InodeGroupID, inodeNumber inode.InodeNumber, lockCmd int32, inFlockStruct *FlockStruct) (outFlockStruct *FlockStruct, err error) {
	outFlockStruct = nil // default up front

	if lockCmd == syscall.F_SETLKW {
		err = blunder.AddError(nil, blunder.NotSupportedError)
		return
	}

	// Make sure the inode does not go away, while we are applying the flock.
	inodeLock, err := mS.initInodeLock(inodeNumber, nil)
	if err != nil {
		return
	}
	err = inodeLock.ReadLock()
	if err != nil {
		return
	}
	defer inodeLock.Unlock()

	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.F_OK) {
		err = blunder.NewError(blunder.NotFoundError, "ENOENT")
		return
	}
	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.R_OK) {
		err = blunder.NewError(blunder.PermDeniedError, "EACCES")
		return
	}

	flockList, err := mS.getFileLockList(inodeNumber)
	if err != nil {
		return
	}

	if inFlockStruct.Type == syscall.F_UNLCK {
		for e := flockList.Front(); e != nil; e = e.Next() {
			elm := e.Value.(*FlockStruct)

			if (elm.Pid == inFlockStruct.Pid) && (elm.Start == inFlockStruct.Start) && (elm.Len == inFlockStruct.Len) {
				flockList.Remove(e)
				return // err == nil already
			}
		}

		err = blunder.AddError(nil, blunder.NoDataError)
		return
	}

	var lockEnd uint64
	if inFlockStruct.Len == 0 {
		lockEnd = ^uint64(0)
	} else {
		lockEnd = inFlockStruct.Start + inFlockStruct.Len
	}

	var insertElm *list.Element

	// TBD: We are currently not handling overlapping locks more efficiently as described in fcntl(2) manpage.
	for e := flockList.Front(); e != nil; e = e.Next() {
		elm := e.Value.(*FlockStruct)
		var elmEnd uint64

		if elm.Len == 0 {
			elmEnd = ^uint64(0)
		} else {
			elmEnd = elm.Start + elm.Len
		}

		if elmEnd < inFlockStruct.Start {
			continue
		}

		if insertElm == nil && elm.Start >= inFlockStruct.Start {
			insertElm = e
		}

		if elm.Start > lockEnd {
			// No conflict insert the lock and return:
			flockList.InsertBefore(inFlockStruct, insertElm)
			outFlockStruct = inFlockStruct
			return // err == nil already
		}

		if reflect.DeepEqual(elm, inFlockStruct) {
			outFlockStruct = elm
			return // err == nil already
		}

		if (elm.Type == syscall.F_WRLCK) || (inFlockStruct.Type == syscall.F_WRLCK) {
			outFlockStruct = elm
			err = blunder.AddError(nil, blunder.TryAgainError)
			return
		}
	}

	newFlock := *inFlockStruct
	if insertElm != nil {
		flockList.InsertBefore(&newFlock, insertElm)
	} else {
		flockList.PushBack(&newFlock)
	}

	stats.IncrementOperations(&stats.FsFlockOps)

	outFlockStruct = inFlockStruct

	return // err == nil already
}

func (mS *mountStruct) getstatHelper(inodeNumber inode.InodeNumber, callerID dlm.CallerID) (stat Stat, err error) {
	lockID, err := mS.makeLockID(inodeNumber)
	if err != nil {
		return
	}
	if !dlm.IsLockHeld(lockID, callerID, dlm.ANYLOCK) {
		err = fmt.Errorf("%s: inode %v lock must be held before calling", utils.GetFnName(), inodeNumber)
		return nil, blunder.AddError(err, blunder.NotFoundError)
	}

	stat = make(map[StatKey]uint64)

	metadata, err := mS.GetMetadata(inodeNumber)

	if err != nil {
		return nil, err
	}

	stat[StatCRTime] = uint64(metadata.CreationTime.UnixNano())
	stat[StatMTime] = uint64(metadata.ModificationTime.UnixNano())
	stat[StatCTime] = uint64(metadata.AttrChangeTime.UnixNano())
	stat[StatATime] = uint64(metadata.AccessTime.UnixNano())
	stat[StatSize] = metadata.Size
	stat[StatNLink] = metadata.LinkCount
	stat[StatFType] = uint64(metadata.InodeType)
	stat[StatINum] = uint64(inodeNumber)
	stat[StatMode] = uint64(metadata.Mode)
	stat[StatUserID] = uint64(metadata.UserID)
	stat[StatGroupID] = uint64(metadata.GroupID)
	stat[StatNumWrites] = metadata.NumWrites

	return stat, nil
}

func (mS *mountStruct) Getstat(userID inode.InodeUserID, groupID inode.InodeGroupID, otherGroupIDs []inode.InodeGroupID, inodeNumber inode.InodeNumber) (stat Stat, err error) {
	inodeLock, err := mS.initInodeLock(inodeNumber, nil)
	if err != nil {
		return
	}
	err = inodeLock.ReadLock()
	if err != nil {
		return
	}
	defer inodeLock.Unlock()

	stats.IncrementOperations(&stats.FsGetstatOps)

	// Call getstat helper function to do the work
	return mS.getstatHelper(inodeNumber, inodeLock.GetCallerID())
}

func (mS *mountStruct) getTypeHelper(inodeNumber inode.InodeNumber, callerID dlm.CallerID) (inodeType inode.InodeType, err error) {
	lockID, err := mS.makeLockID(inodeNumber)
	if err != nil {
		return
	}
	if !dlm.IsLockHeld(lockID, callerID, dlm.ANYLOCK) {
		err = fmt.Errorf("%s: inode %v lock must be held before calling.", utils.GetFnName(), inodeNumber)
		err = blunder.AddError(err, blunder.NotFoundError)
		return
	}

	inodeType, err = mS.VolumeHandle.GetType(inodeNumber)
	if err != nil {
		logger.ErrorWithError(err, "couldn't get inode type")
		return inodeType, err
	}
	return inodeType, nil
}

func (mS *mountStruct) GetType(userID inode.InodeUserID, groupID inode.InodeGroupID, otherGroupIDs []inode.InodeGroupID, inodeNumber inode.InodeNumber) (inodeType inode.InodeType, err error) {
	inodeLock, err := mS.initInodeLock(inodeNumber, nil)
	if err != nil {
		return
	}
	err = inodeLock.ReadLock()
	if err != nil {
		return
	}
	defer inodeLock.Unlock()

	stats.IncrementOperations(&stats.FsGetTypeOps)
	return mS.getTypeHelper(inodeNumber, inodeLock.GetCallerID())
}

func (mS *mountStruct) GetXAttr(userID inode.InodeUserID, groupID inode.InodeGroupID, otherGroupIDs []inode.InodeGroupID, inodeNumber inode.InodeNumber, streamName string) (value []byte, err error) {
	inodeLock, err := mS.initInodeLock(inodeNumber, nil)
	if err != nil {
		return
	}
	err = inodeLock.ReadLock()
	if err != nil {
		return
	}
	defer inodeLock.Unlock()

	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.F_OK) {
		err = blunder.NewError(blunder.NotFoundError, "ENOENT")
		return
	}
	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.R_OK) {
		err = blunder.NewError(blunder.PermDeniedError, "EACCES")
		return
	}

	value, err = mS.GetStream(inodeNumber, streamName)
	if err != nil {
		// Did not find the requested stream. However this isn't really an error since
		// samba will ask for acl-related streams and is fine with not finding them.
		logger.TracefWithError(err, "Failed to get XAttr %v of inode %v", streamName, inodeNumber)
	}

	stats.IncrementOperations(&stats.FsGetXattrOps)
	return
}

func (mS *mountStruct) IsDir(userID inode.InodeUserID, groupID inode.InodeGroupID, otherGroupIDs []inode.InodeGroupID, inodeNumber inode.InodeNumber) (inodeIsDir bool, err error) {
	inodeLock, err := mS.initInodeLock(inodeNumber, nil)
	if err != nil {
		return
	}
	err = inodeLock.ReadLock()
	if err != nil {
		return
	}
	defer inodeLock.Unlock()

	stats.IncrementOperations(&stats.FsIsdirOps)

	lockID, err := mS.makeLockID(inodeNumber)
	if err != nil {
		return
	}
	if !dlm.IsLockHeld(lockID, inodeLock.GetCallerID(), dlm.ANYLOCK) {
		err = fmt.Errorf("%s: inode %v lock must be held before calling", utils.GetFnName(), inodeNumber)
		return false, blunder.AddError(err, blunder.NotFoundError)
	}

	inodeType, err := mS.VolumeHandle.GetType(inodeNumber)
	if err != nil {
		return false, err
	}
	return inodeType == inode.DirType, nil
}

func (mS *mountStruct) IsFile(userID inode.InodeUserID, groupID inode.InodeGroupID, otherGroupIDs []inode.InodeGroupID, inodeNumber inode.InodeNumber) (inodeIsFile bool, err error) {
	inodeLock, err := mS.initInodeLock(inodeNumber, nil)
	if err != nil {
		return
	}
	err = inodeLock.ReadLock()
	if err != nil {
		return
	}
	defer inodeLock.Unlock()

	inodeType, err := mS.VolumeHandle.GetType(inodeNumber)
	if err != nil {
		return false, err
	}
	stats.IncrementOperations(&stats.FsIsfileOps)
	return inodeType == inode.FileType, nil
}

func (mS *mountStruct) IsSymlink(userID inode.InodeUserID, groupID inode.InodeGroupID, otherGroupIDs []inode.InodeGroupID, inodeNumber inode.InodeNumber) (inodeIsSymlink bool, err error) {
	inodeLock, err := mS.initInodeLock(inodeNumber, nil)
	if err != nil {
		return
	}
	err = inodeLock.ReadLock()
	if err != nil {
		return
	}
	defer inodeLock.Unlock()

	inodeType, err := mS.VolumeHandle.GetType(inodeNumber)
	if err != nil {
		return false, err
	}
	stats.IncrementOperations(&stats.FsIssymlinkOps)
	return inodeType == inode.SymlinkType, nil
}

func (mS *mountStruct) Link(userID inode.InodeUserID, groupID inode.InodeGroupID, otherGroupIDs []inode.InodeGroupID, dirInodeNumber inode.InodeNumber, basename string, targetInodeNumber inode.InodeNumber) (err error) {
	// We need both dirInodelock and the targetInode lock to make sure they don't go away and linkCount is updated correctly.
	callerID := dlm.GenerateCallerID()
	dirInodeLock, err := mS.initInodeLock(dirInodeNumber, callerID)
	if err != nil {
		return
	}

	targetInodeLock, err := mS.initInodeLock(targetInodeNumber, callerID)
	if err != nil {
		return
	}

	err = dirInodeLock.WriteLock()
	if err != nil {
		return
	}
	defer dirInodeLock.Unlock()

	err = targetInodeLock.WriteLock()
	if err != nil {
		return
	}
	defer targetInodeLock.Unlock()

	// In the case of hardlink, make sure target inode is not a directory
	inodeType, err := mS.VolumeHandle.GetType(targetInodeNumber)
	if err != nil {
		// Because we know that GetMetadata has already "blunderized" the error, we just pass it on
		logger.ErrorfWithError(err, "couldn't get type for inode %v", targetInodeNumber)
		return err
	}
	if inodeType == inode.DirType {
		err = fmt.Errorf("%s: inode %v cannot be a dir inode", utils.GetFnName(), targetInodeNumber)
		logger.ErrorWithError(err)
		return blunder.AddError(err, blunder.LinkDirError)
	}

	if !mS.VolumeHandle.Access(dirInodeNumber, userID, groupID, otherGroupIDs, inode.F_OK) {
		err = blunder.NewError(blunder.NotFoundError, "ENOENT")
		return
	}
	if !mS.VolumeHandle.Access(targetInodeNumber, userID, groupID, otherGroupIDs, inode.F_OK) {
		err = blunder.NewError(blunder.NotFoundError, "ENOENT")
		return
	}
	if !mS.VolumeHandle.Access(dirInodeNumber, userID, groupID, otherGroupIDs, inode.W_OK|inode.X_OK) {
		err = blunder.NewError(blunder.PermDeniedError, "EACCES")
		return
	}
	if !mS.VolumeHandle.Access(targetInodeNumber, userID, groupID, otherGroupIDs, inode.W_OK) {
		err = blunder.NewError(blunder.PermDeniedError, "EACCES")
		return
	}

	err = mS.VolumeHandle.Link(dirInodeNumber, basename, targetInodeNumber)
	stats.IncrementOperations(&stats.FsLinkOps)
	return err
}

func (mS *mountStruct) ListXAttr(userID inode.InodeUserID, groupID inode.InodeGroupID, otherGroupIDs []inode.InodeGroupID, inodeNumber inode.InodeNumber) (streamNames []string, err error) {
	inodeLock, err := mS.initInodeLock(inodeNumber, nil)
	if err != nil {
		return
	}
	err = inodeLock.ReadLock()
	if err != nil {
		return
	}
	defer inodeLock.Unlock()

	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.F_OK) {
		err = blunder.NewError(blunder.NotFoundError, "ENOENT")
		return
	}
	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.R_OK) {
		err = blunder.NewError(blunder.PermDeniedError, "EACCES")
		return
	}

	metadata, err := mS.GetMetadata(inodeNumber)
	if err != nil {
		// Did not find the requested stream. However this isn't really an error since
		// samba will ask for acl-related streams and is fine with not finding them.
		logger.TracefWithError(err, "Failed to list XAttrs of inode %v", inodeNumber)
		return
	}

	streamNames = make([]string, len(metadata.InodeStreamNameSlice))
	copy(streamNames, metadata.InodeStreamNameSlice)
	stats.IncrementOperations(&stats.FsListXattrOps)
	return
}

func (mS *mountStruct) Lookup(userID inode.InodeUserID, groupID inode.InodeGroupID, otherGroupIDs []inode.InodeGroupID, dirInodeNumber inode.InodeNumber, basename string) (inodeNumber inode.InodeNumber, err error) {
	dirInodeLock, err := mS.initInodeLock(dirInodeNumber, nil)
	if err != nil {
		return
	}
	dirInodeLock.ReadLock()
	defer dirInodeLock.Unlock()

	if !mS.VolumeHandle.Access(dirInodeNumber, userID, groupID, otherGroupIDs, inode.F_OK) {
		err = blunder.NewError(blunder.NotFoundError, "ENOENT")
		return
	}
	if !mS.VolumeHandle.Access(dirInodeNumber, userID, groupID, otherGroupIDs, inode.X_OK) {
		err = blunder.NewError(blunder.PermDeniedError, "EACCES")
		return
	}

	inodeNumber, err = mS.VolumeHandle.Lookup(dirInodeNumber, basename)
	stats.IncrementOperations(&stats.FsLookupOps)
	return inodeNumber, err
}

func (mS *mountStruct) LookupPath(userID inode.InodeUserID, groupID inode.InodeGroupID, otherGroupIDs []inode.InodeGroupID, fullpath string) (inodeNumber inode.InodeNumber, err error) {
	stats.IncrementOperations(&stats.FsPathLookupOps)

	// In the special case of a fullpath starting with "/", the path segment splitting above
	// results in a first segment that still begins with "/". Because this is not recognized
	// as a real path segment, by the underlying code, we have trouble looking it up.
	//
	// This is a hack to work around this case until I figure out a better way.
	newfullpath := strings.TrimPrefix(fullpath, "/")
	if strings.Compare(fullpath, newfullpath) != 0 {
		fullpath = newfullpath
	}

	pathSegments := strings.Split(path.Clean(fullpath), "/")

	cursorInodeNumber := inode.RootDirInodeNumber
	for _, segment := range pathSegments {
		cursorInodeLock, err1 := mS.initInodeLock(cursorInodeNumber, nil)
		if err = err1; err != nil {
			return
		}
		err = cursorInodeLock.ReadLock()
		if err != nil {
			return
		}

		if !mS.VolumeHandle.Access(cursorInodeNumber, userID, groupID, otherGroupIDs, inode.X_OK) {
			cursorInodeLock.Unlock()
			err = blunder.NewError(blunder.PermDeniedError, "EACCES")
			return
		}

		cursorInodeNumber, err = mS.VolumeHandle.Lookup(cursorInodeNumber, segment)
		cursorInodeLock.Unlock()

		if err != nil {
			return cursorInodeNumber, err
		}
	}

	return cursorInodeNumber, nil
}

func (mS *mountStruct) MiddlewareCoalesce(destPath string, elementPaths []string) (ino uint64, numWrites uint64, modificationTime uint64, err error) {
	// it'll hold a dir lock and a file lock for each element path, plus a lock on the destination dir and the root dir
	heldLocks := make([]*dlm.RWLockStruct, 0, 2*len(elementPaths)+2)
	defer func() {
		for _, lock := range heldLocks {
			if lock != nil {
				lock.Unlock()
			}
		}
	}()

	elementDirAndFileNames := make(dirAndFileNameSlice, 0, len(elementPaths))
	coalesceElements := make([]inode.CoalesceElement, 0, len(elementPaths))

	for _, path := range elementPaths {
		dirName, fileName := filepath.Split(path)
		if dirName == "" {
			err = fmt.Errorf("Files to coalesce must not be in the root directory")
			return
		} else {
			// filepath.Split leaves the trailing slash on the directory file name; the only time it doesn't is if you
			// split something lacking slashes, e.g. "file.txt", in which case dirName is "" and we've already returned
			// an error.
			dirName = dirName[0 : len(dirName)-1]
		}

		elementDirAndFileNames = append(elementDirAndFileNames, dirAndFileName{
			dirName:  dirName,
			fileName: fileName,
		})
	}

	destDirName, destFileName := filepath.Split(destPath)
	if destDirName == "" {
		// NB: the middleware won't ever call us with a destination file in the root directory, as that would look like
		// a container path.
		err = fmt.Errorf("Coalesce target must not be in the root directory")
		return
	}
	destDirName = destDirName[0 : len(destDirName)-1]

	// We lock things in whatever order the caller provides them. To protect against deadlocks with other concurrent
	// calls to this function, we first obtain a write lock on the root inode.
	//
	// One might think there's some way to sort these paths and do something smarter, but then one would remember that
	// symlinks exist and so one cannot do anything useful with paths. For example, given two directory paths, which is
	// more deeply nested, a/b/c or d/e/f/g/h? It might be the first one, if b is a symlink to b1/b2/b3/b4/b5, or it
	// might not.
	//
	// This function is called infrequently enough that we can probably get away with the big heavy lock without causing
	// too much trouble.
	callerID := dlm.GenerateCallerID()

	rootDirInodeLock, err := mS.getWriteLock(inode.RootDirInodeNumber, callerID)
	if err != nil {
		return
	}
	heldLocks = append(heldLocks, rootDirInodeLock)

	destDirInodeNumber, destDirInodeType, destDirInodeLock, err := mS.resolvePathForWrite(destDirName, callerID)
	if err != nil {
		return
	}
	heldLocks = append(heldLocks, destDirInodeLock)
	if destDirInodeType != inode.DirType {
		err = blunder.NewError(blunder.NotDirError, "%s is not a directory", destDirName)
		return
	}

	for _, entry := range elementDirAndFileNames {
		dirInodeNumber, dirInodeType, dirInodeLock, err1 := mS.resolvePathForWrite(entry.dirName, callerID)
		if err1 != nil {
			err = err1
			return
		}
		if dirInodeLock != nil {
			heldLocks = append(heldLocks, dirInodeLock)
		}

		if dirInodeType != inode.DirType {
			err = blunder.NewError(blunder.NotDirError, "%s is not a directory", entry.dirName)
			return
		}

		fileInodeNumber, err1 := mS.VolumeHandle.Lookup(dirInodeNumber, entry.fileName)
		if err1 != nil {
			err = err1
			return
		}

		fileInodeLock, err1 := mS.initInodeLock(fileInodeNumber, callerID)
		if err1 != nil {
			err = err1
			return
		}
		if !fileInodeLock.IsWriteHeld() {
			err = fileInodeLock.WriteLock()
			if err != nil {
				return
			}
			heldLocks = append(heldLocks, fileInodeLock)
		}

		fileMetadata, err1 := mS.GetMetadata(fileInodeNumber)
		if err1 != nil {
			err = err1
			return
		}
		if fileMetadata.InodeType != inode.FileType {
			err = blunder.NewError(blunder.NotFileError, "%s/%s is not an ordinary file", entry.dirName, entry.fileName)
			return
		}

		coalesceElements = append(coalesceElements, inode.CoalesceElement{
			ContainingDirectoryInodeNumber: dirInodeNumber,
			ElementInodeNumber:             fileInodeNumber,
			ElementName:                    entry.fileName,
		})
	}

	// We've now jumped through all the requisite hoops to get the required locks, so now we can call inode.Coalesce and
	// do something useful
	destInodeNumber, mtime, numWrites, err := mS.Coalesce(destDirInodeNumber, destFileName, coalesceElements)
	ino = uint64(destInodeNumber)
	modificationTime = uint64(mtime.UnixNano())
	return
}

func (mS *mountStruct) MiddlewareDelete(parentDir string, baseName string) (err error) {
	// Get the inode, type, and lock for the parent directory
	parentInodeNumber, parentInodeType, parentDirLock, err := mS.resolvePathForWrite(parentDir, nil)
	if err != nil {
		return err
	}
	defer parentDirLock.Unlock()
	if parentInodeType != inode.DirType {
		err = blunder.NewError(blunder.NotDirError, "%s is a file", parentDir)
	}

	// We will need both parentDir lock to Unlink() and baseInode lock.
	baseNameInodeNumber, err := mS.VolumeHandle.Lookup(parentInodeNumber, baseName)
	if err != nil {
		return err
	}
	baseInodeLock, err := mS.getWriteLock(baseNameInodeNumber, parentDirLock.GetCallerID())
	if nil != err {
		return
	}
	defer baseInodeLock.Unlock()

	inodeType, err := mS.VolumeHandle.GetType(baseNameInodeNumber)
	if nil != err {
		return
	}

	var doDestroy bool

	if inodeType == inode.DirType {
		dirEntries, nonShadowingErr := mS.NumDirEntries(baseNameInodeNumber)
		if nil != nonShadowingErr {
			err = nonShadowingErr
			return
		}

		if 2 != dirEntries {
			err = fmt.Errorf("Directory not empty")
			err = blunder.AddError(err, blunder.NotEmptyError)
			return
		}

		// LinkCount must == 2 ("." and "..") since we don't allow hardlinks to DirInode's

		doDestroy = true
	} else { // inodeType != inode.DirType
		basenameLinkCount, nonShadowingErr := mS.GetLinkCount(baseNameInodeNumber)
		if nil != nonShadowingErr {
			err = nonShadowingErr
			return
		}

		doDestroy = (1 == basenameLinkCount)
	}

	// At this point, we *are* going to Unlink... and optionally Destroy... the inode

	err = mS.VolumeHandle.Unlink(parentInodeNumber, baseName)
	if nil != err {
		return
	}

	if doDestroy {
		err = mS.Destroy(baseNameInodeNumber)
		if nil != err {
			return err
		}
	}

	stats.IncrementOperations(&stats.FsMwDeleteOps)
	return
}

func (mS *mountStruct) MiddlewareGetAccount(maxEntries uint64, marker string) (accountEnts []AccountEntry, err error) {
	// List the root directory, starting at the marker, and keep only
	// the directories. The Swift API doesn't let you have objects in
	// an account, so files or symlinks don't belong in an account
	// listing.
	areMoreEntries := true
	lastBasename := marker
	for areMoreEntries && uint64(len(accountEnts)) < maxEntries {
		var dirEnts []inode.DirEntry
		dirEnts, _, areMoreEntries, err = mS.Readdir(inode.InodeRootUserID, inode.InodeRootGroupID, nil, inode.RootDirInodeNumber, lastBasename, maxEntries-uint64(len(accountEnts)), 0)
		if err != nil {
			if blunder.Is(err, blunder.NotFoundError) {
				// Readdir gives you a NotFoundError if you ask for a
				// lastBasename that's lexicographically greater than
				// the last entry in the directory.
				//
				// For account listings, it's not an error to set
				// marker=$PAST_END where $PAST_END is greater than
				// the last container in the account; you simply get
				// back an empty listing.
				//
				// Therefore, we treat this as though Readdir returned
				// 0 entries.
				err = nil
				break
			} else {
				return
			}
		}

		for _, dirEnt := range dirEnts {
			if dirEnt.Basename == "." || dirEnt.Basename == ".." {
				continue
			}

			var isItADir bool
			isItADir, err = mS.IsDir(inode.InodeRootUserID, inode.InodeRootGroupID, nil, dirEnt.InodeNumber)
			if err != nil {
				logger.ErrorfWithError(err, "MiddlewareGetAccount: error in IsDir(%v)", dirEnt.InodeNumber)
				return
			}

			if isItADir {
				accountEnts = append(accountEnts, AccountEntry{Basename: dirEnt.Basename})
			}
		}
		if len(dirEnts) == 0 {
			break
		} else {
			lastBasename = dirEnts[len(dirEnts)-1].Basename
		}
	}
	stats.IncrementOperations(&stats.FsMwGetAccountOps)
	return
}

func (mS *mountStruct) MiddlewareGetContainer(vContainerName string, maxEntries uint64, marker string, prefix string) (containerEnts []ContainerEntry, err error) {
	ino, _, inoLock, err := mS.resolvePathForRead(vContainerName, nil)
	if err != nil {
		return
	}
	// Because a container listing can take a long time to generate,
	// we don't hold locks for the whole time. While this might lead
	// to some small inconsistencies (like a new file created halfway
	// through a call to MiddlewareGetContainer being omitted from the
	// listing), this is mitigated by two things. First, this lets us
	// accept new PUTs and writes while generating a container
	// listing, and second, Swift container listings are subject to
	// all sorts of temporary inconsistencies, so this is no worse
	// than what a Swift client would normally have to put up with.
	inoLock.Unlock()

	containerEnts = make([]ContainerEntry, 0)
	var recursiveReaddirPlus func(dirName string, dirInode inode.InodeNumber) error
	recursiveReaddirPlus = func(dirName string, dirInode inode.InodeNumber) error {
		var dirEnts []inode.DirEntry
		var recursiveDescents []dirToDescend
		areMoreEntries := true
		lastBasename := ""

		// Note that we're taking advantage of the fact that
		// Readdir() returns things in lexicographic order, which
		// is the same as our desired order. This lets us avoid
		// reading the whole directory only to sort it.
		for (areMoreEntries || len(dirEnts) > 0 || len(recursiveDescents) > 0) && uint64(len(containerEnts)) < maxEntries {
			// If we've run out of real directory entries, load some more.
			if areMoreEntries && len(dirEnts) == 0 {
				dirEnts, _, areMoreEntries, err = mS.Readdir(inode.InodeRootUserID, inode.InodeRootGroupID, nil, dirInode, lastBasename, maxEntries-uint64(len(containerEnts)), 0)
				if len(dirEnts) > 0 {
					// If there's no dirEnts here, then areMoreEntries
					// is false, so we'll never call Readdir again,
					// and thus it doesn't matter what the value of
					// lastBasename is.
					lastBasename = dirEnts[len(dirEnts)-1].Basename
				}
			}
			if err != nil {
				logger.ErrorfWithError(err, "MiddlewareGetContainer: error reading directory %s (inode %v)", dirName, dirInode)
				return err
			}

			// Ignore these early so we can stop thinking about them
			if len(dirEnts) > 0 && (dirEnts[0].Basename == "." || dirEnts[0].Basename == "..") {
				dirEnts = dirEnts[1:]
				continue
			}

			// If we've got pending recursive descents that should go before the next dirEnt, handle them
			for len(recursiveDescents) > 0 && (len(dirEnts) == 0 || (recursiveDescents[0].name < dirEnts[0].Basename)) {
				err = recursiveReaddirPlus(recursiveDescents[0].name, recursiveDescents[0].ino)
				if err != nil {
					// already logged
					return err
				}
				if uint64(len(containerEnts)) >= maxEntries {
					// we're finished here
					return nil
				}
				recursiveDescents = recursiveDescents[1:]
			}

			// Handle just one dirEnt per loop iteration. That lets us
			// avoid having to refill dirEnts at more than one
			// location in the code.
			if !(len(dirEnts) > 0) {
				continue
			}

			dirEnt := dirEnts[0]
			dirEnts = dirEnts[1:]

			fileName := dirEnt.Basename
			if len(dirName) > 0 {
				fileName = dirName + dirEnt.Basename
			}

			if fileName > prefix && !strings.HasPrefix(fileName, prefix) {
				// Remember that we're going over these in order, so the first time we see something that's greater that
				// the prefix but doesn't start with it, we can skip the entire rest of the directory entries since they
				// are *also* greater than the prefix but don't start with it.
				return nil
			}

			// Swift container listings are paginated; you
			// retrieve the first page with a simple GET
			// <container>, then you retrieve each subsequent page
			// with a GET <container>?marker=<last-obj-returned>.
			//
			// If we were given a marker, then we can prune the
			// directory tree that we're walking.
			//
			// For a regular file, if its container-relative path
			// is lexicographically less than or equal to the
			// marker, we skip it.
			//
			// For a directory, if its container-relative path is
			// lexicographically less than or equal to the marker
			// and the marker does not begin with the directory's
			// path, we skip it.
			//
			// Since no regular file's container-relative path
			// starts with another regular file's
			// container-relative path, we can make the following
			// test prior to any Getstat() calls, avoiding
			// unneeded IO.
			if fileName <= marker && strings.Index(marker, fileName) != 0 {
				continue
			}

			statResult, err := mS.Getstat(inode.InodeRootUserID, inode.InodeRootGroupID, nil, dirEnt.InodeNumber) // TODO: fix this
			if err != nil {
				logger.ErrorfWithError(err, "MiddlewareGetContainer: error in Getstat of %s", fileName)
				return err
			}

			fileType := inode.InodeType(statResult[StatFType])

			if fileType == inode.FileType || fileType == inode.SymlinkType {
				if fileName <= marker {
					continue
				}
				if !strings.HasPrefix(fileName, prefix) {
					continue
				}
				containerEnt := ContainerEntry{
					Basename:         fileName,
					FileSize:         statResult[StatSize],
					ModificationTime: statResult[StatMTime],
					NumWrites:        statResult[StatNumWrites],
					InodeNumber:      statResult[StatINum],
					IsDir:            false,
				}
				containerEnts = append(containerEnts, containerEnt)
			} else {
				if !strings.HasPrefix(fileName, prefix) && !strings.HasPrefix(prefix, fileName) {
					continue
				}

				// Directories are handled specially. For a directory
				// "some-dir", we put an entry for "some-dir" in the
				// container listing, then put "some-dir/" into
				// recursiveDescents (note the trailing slash). This
				// lets us put off the descent until we have handled
				// all dirEnts coming before "some-dir/".
				//
				// For example, consider a filesystem with a dir "d",
				// a file "d/f", and a file "d-README".
				// Lexicographically, these would be ordered "d",
				// "d-README", "d/f" ("-" is ASCII 45, "/" is ASCII
				// 47). If we recursed into d immediately upon
				// encountering it, we would have "d/f" before
				// "d-README", which is not what the Swift API
				// demands.
				if fileName > marker && strings.HasPrefix(fileName, prefix) {
					containerEnt := ContainerEntry{
						Basename:         fileName,
						FileSize:         0,
						ModificationTime: statResult[StatMTime],
						NumWrites:        statResult[StatNumWrites],
						InodeNumber:      statResult[StatINum],
						IsDir:            true,
					}
					containerEnts = append(containerEnts, containerEnt)
				}
				recursiveDescents = append(recursiveDescents, dirToDescend{name: fileName + "/", ino: dirEnt.InodeNumber})
			}
		}
		return nil
	}

	err = recursiveReaddirPlus("", ino)
	if err != nil {
		// already logged
		return
	}
	stats.IncrementOperations(&stats.FsMwGetContainerOps)
	return
}

func (mS *mountStruct) MiddlewareGetObject(volumeName string, containerObjectPath string, readRangeIn []ReadRangeIn, readRangeOut *[]inode.ReadPlanStep) (fileSize uint64, lastModified uint64, ino uint64, numWrites uint64, serializedMetadata []byte, err error) {
	inodeNumber, inodeType, inodeLock, err := mS.resolvePathForRead(containerObjectPath, nil)
	ino = uint64(inodeNumber)
	if err != nil {
		return
	}
	defer inodeLock.Unlock()

	// If resolvePathForRead succeeded, then inodeType is either
	// inode.DirType or inode.FileType; if it was inode.SymlinkType,
	// then err was not nil, and we bailed out before reaching this
	// point.
	if inode.DirType == inodeType {
		err = blunder.NewError(blunder.IsDirError, "%s: inode %v is a directory.", utils.GetFnName(), inodeNumber)
		return
	}

	// Find file size
	metadata, err := mS.GetMetadata(inodeNumber)
	if err != nil {
		return
	}
	fileSize = metadata.Size
	lastModified = uint64(metadata.ModificationTime.UnixNano())
	numWrites = metadata.NumWrites

	// If no ranges are given then get range of whole file.  Otherwise, get ranges.
	if len(readRangeIn) == 0 {
		// Get ReadPlan for file
		volumeHandle, err1 := inode.FetchVolumeHandle(volumeName)
		if err1 != nil {
			err = err1
			logger.ErrorWithError(err)
			return
		}
		var offset uint64 = 0
		tmpReadEnt, err1 := volumeHandle.GetReadPlan(inodeNumber, &offset, &metadata.Size)
		if err1 != nil {
			err = err1
			return
		}
		appendReadPlanEntries(tmpReadEnt, readRangeOut)
	} else {
		volumeHandle, err1 := inode.FetchVolumeHandle(volumeName)
		if err1 != nil {
			err = err1
			logger.ErrorWithError(err)
			return
		}

		// Get ReadPlan for each range and append physical path ranges to result
		for i := range readRangeIn {
			// TODO - verify that range request is within file size
			tmpReadEnt, err1 := volumeHandle.GetReadPlan(inodeNumber, readRangeIn[i].Offset, readRangeIn[i].Len)
			if err1 != nil {
				err = err1
				return
			}
			appendReadPlanEntries(tmpReadEnt, readRangeOut)
		}
	}

	serializedMetadata, err = mS.GetStream(inodeNumber, MiddlewareStream)
	// If someone makes a directory or file via SMB/FUSE and then
	// accesses it via HTTP, we'll see StreamNotFound. We treat it as
	// though there is no metadata. The middleware is equipped to
	// handle receiving empty metadata.
	if err != nil && !blunder.Is(err, blunder.StreamNotFound) {
		return
	} else {
		err = nil
	}
	stats.IncrementOperations(&stats.FsMwGetObjOps)
	return
}

func (mS *mountStruct) MiddlewareHeadResponse(entityPath string) (response HeadResponse, err error) {
	ino, inoType, inoLock, err := mS.resolvePathForRead(entityPath, nil)
	if err != nil {
		return
	}
	defer inoLock.Unlock()

	statResult, err := mS.getstatHelper(ino, inoLock.GetCallerID())
	if err != nil {
		return
	}
	response.ModificationTime = statResult[StatMTime]
	response.FileSize = statResult[StatSize]
	response.IsDir = (inoType == inode.DirType)
	response.InodeNumber = ino
	response.NumWrites = statResult[StatNumWrites]

	response.Metadata, err = mS.GetStream(ino, MiddlewareStream)
	if err != nil {
		response.Metadata = []byte{}
		// If someone makes a directory or file via SMB/FUSE and then
		// HEADs it via HTTP, we'll see this error. We treat it as
		// though there is no metadata. The middleware is equipped to
		// handle this case.
		if blunder.Is(err, blunder.StreamNotFound) {
			err = nil
		}
		return
	}
	stats.IncrementOperations(&stats.FsMwHeadResponseOps)
	return
}

func (mS *mountStruct) MiddlewarePost(parentDir string, baseName string, newMetaData []byte, oldMetaData []byte) (err error) {
	// Find inode for container or object
	fullPathName := parentDir + "/" + baseName
	baseNameInodeNumber, _, baseInodeLock, err := mS.resolvePathForWrite(fullPathName, nil)
	if err != nil {
		return err
	}
	defer baseInodeLock.Unlock()

	// Compare oldMetaData to existing existingStreamData to make sure that the HTTP metadata has not changed.
	// If it has changed, then return an error since middleware has to handle it.
	existingStreamData, err := mS.GetStream(baseNameInodeNumber, MiddlewareStream)

	// GetStream() will return an error if there is no "middleware" stream
	if err != nil && blunder.IsNot(err, blunder.StreamNotFound) {
		return err
	}

	// Verify that the oldMetaData is the same as the one we think we are changing.
	if err == nil && !bytes.Equal(existingStreamData, oldMetaData) {
		return blunder.NewError(blunder.TryAgainError, "%s: MetaData different - existingStreamData: %v OldMetaData: %v.", utils.GetFnName(), existingStreamData, oldMetaData)
	}

	// Change looks okay so make it.
	err = mS.PutStream(baseNameInodeNumber, MiddlewareStream, newMetaData)

	stats.IncrementOperations(&stats.FsMwPostOps)
	return err
}

func (mS *mountStruct) MiddlewarePutComplete(vContainerName string, vObjectPath string, pObjectPaths []string, pObjectLengths []uint64, pObjectMetadata []byte) (mtime uint64, fileInodeNumber inode.InodeNumber, numWrites uint64, err error) {
	// Find the inode of the directory corresponding to the container
	dirInodeNumber, err := mS.Lookup(inode.InodeRootUserID, inode.InodeRootGroupID, nil, inode.RootDirInodeNumber, vContainerName)
	if err != nil {
		return
	}

	vObjectPathSegments := revSplitPath(vObjectPath)
	vObjectBaseName := vObjectPathSegments[0]
	dirs := vObjectPathSegments[1:]

	// Find all existing directories, taking locks as we go.
	//
	// We want to find the lowest existing subdirectory for this PUT
	// request. For example, if the FS tree has container/d1/d2, and a
	// PUT request comes in for container/d1/d2/d3/d4/file.bin, then
	// this loop will walk down to d2.
	//
	// We take locks as we go so that nobody can sneak d3 in ahead of
	// us.
	var dirEntInodeLock *dlm.RWLockStruct
	callerID := dlm.GenerateCallerID()
	dirInodeLock, err := mS.getWriteLock(dirInodeNumber, callerID)
	if err != nil {
		return
	}
	defer func() {
		if dirInodeLock != nil {
			dirInodeLock.Unlock()
		}
		if dirEntInodeLock != nil {
			dirEntInodeLock.Unlock()
		}
	}()

	for len(dirs) > 0 {
		thisDir := dirs[len(dirs)-1]
		if thisDir == "." {
			// Skip this early so we don't end up trying to write-lock
			// it and deadlocking with ourselves.
			dirs = dirs[0 : len(dirs)-1]
			continue
		}

		dirEntInodeNumber, err1 := mS.VolumeHandle.Lookup(dirInodeNumber, thisDir)
		if err1 != nil && blunder.Errno(err1) == int(blunder.NotFoundError) {
			// NotFoundError just means that it's time to start making
			// directories. We deliberately do not unlock dirInodeLock
			// here.
			break
		} else if err1 != nil {
			// Mysterious error; just bail
			err = err1
		} else {
			// There's a directory entry there; let's go see what it is
			dirs = dirs[0 : len(dirs)-1]
			dirEntInodeLock, err1 = mS.getWriteLock(dirEntInodeNumber, callerID)
			if err1 != nil {
				err = err1
				return
			}

			dirEntInodeType, err1 := mS.VolumeHandle.GetType(dirEntInodeNumber)
			if err1 != nil {
				err = err1
				return
			}

			if dirEntInodeType == inode.FileType {
				// We're processing the directory portion of the
				// object path, so if we run into a file, that's an
				// error
				err = blunder.NewError(blunder.NotDirError, "%s is a file, not a directory", thisDir)
				return
			} else if dirEntInodeType == inode.SymlinkType {
				target, err1 := mS.GetSymlink(dirEntInodeNumber)
				dirEntInodeLock.Unlock()
				dirEntInodeLock = nil
				if err1 != nil {
					err = err1
					return
				}

				if strings.HasPrefix(target, "/") {
					// Absolute symlink: restart traversal from the
					// root directory.
					dirInodeLock.Unlock()
					dirInodeNumber = inode.RootDirInodeNumber
					dirInodeLock, err1 = mS.getWriteLock(inode.RootDirInodeNumber, nil)
					if err1 != nil {
						err = err1
						return
					}
				}
				dirs = append(dirs, revSplitPath(target)...)
			} else {
				// There's actually a subdirectory here. Lock it
				// before unlocking the current directory so there's
				// no window for anyone else to sneak into.
				dirInodeLock.Unlock()

				dirInodeNumber = dirEntInodeNumber
				dirInodeLock = dirEntInodeLock
				dirEntInodeLock = nil // avoid double-cleanup in defer
			}
		}
	}

	// Now, dirInodeNumber is the inode of the lowest existing
	// directory. Anything else is created by us and isn't part of the
	// filesystem tree until we Link() it in, so we only need to hold
	// this one lock.
	//
	// Reify the Swift object into a ProxyFS file by making a new,
	// empty inode and then associating it with the log segment
	// written by the middleware.
	fileInodeNumber, err = mS.CreateFile(inode.PosixModePerm, 0, 0)
	if err != nil {
		logger.DebugfIDWithError(internalDebug, err, "fs.CreateFile(): %v dirInodeNumber: %v vContainerName: %v failed!",
			dirInodeNumber, vContainerName)
		return
	}

	// Associate fileInodeNumber with log segments written by Swift
	fileOffset := uint64(0) // Swift only writes whole files
	pObjectOffset := uint64(0)
	for i := 0; i < len(pObjectPaths); i++ {
		err = mS.Wrote(fileInodeNumber, fileOffset, pObjectPaths[i], pObjectOffset, pObjectLengths[i], i > 0)
		if err != nil {
			logger.DebugfIDWithError(internalDebug, err, "mount.Wrote() fileInodeNumber: %v fileOffset: %v pOjectPaths: %v pObjectOffset: %v pObjectLengths: %v i: %v failed!",
				fileInodeNumber, fileOffset, pObjectPaths, pObjectOffset, pObjectLengths, i)
			return
		}
		fileOffset += pObjectLengths[i]
	}

	// Set the metadata on the file
	err = mS.PutStream(fileInodeNumber, MiddlewareStream, pObjectMetadata)
	if err != nil {
		logger.DebugfIDWithError(internalDebug, err, "mount.PutStream fileInodeNumber: %v metadata: %v failed",
			fileInodeNumber, pObjectMetadata)
		return
	}

	// Build any missing-but-necessary directories
	highestUnlinkedInodeNumber := fileInodeNumber
	highestUnlinkedName := vObjectBaseName
	for i := 0; i < len(dirs); i++ {
		newDirInodeNumber, err1 := mS.CreateDir(inode.PosixModePerm, 0, 0)
		if err1 != nil {
			logger.DebugfIDWithError(internalDebug, err1, "mount.CreateDir(): %v failed!")
			err = err1
			return
		}

		err = mS.VolumeHandle.Link(newDirInodeNumber, highestUnlinkedName, highestUnlinkedInodeNumber)
		if err != nil {
			logger.DebugfIDWithError(internalDebug, err, "mount.Link(%v, %v, %v) failed",
				newDirInodeNumber, highestUnlinkedName, highestUnlinkedInodeNumber)
			return
		}

		highestUnlinkedInodeNumber = newDirInodeNumber
		highestUnlinkedName = dirs[i]
	}

	// Now we've got a pre-existing directory inode in dirInodeNumber,
	// and highestUnlinked(Name,InodeNumber) indicate a thing we need
	// to link into place. The last step is to make sure there's no
	// obstacle to us doing that. Note that this is only required when
	// all the necessary directories already exist; if we had to
	// create any directories, then the bottom directory is empty
	// because we just created it.
	haveObstacle := false
	var obstacleInodeNumber inode.InodeNumber
	if 0 == len(dirs) {
		obstacleInodeNumber, err1 := mS.VolumeHandle.Lookup(dirInodeNumber, vObjectBaseName)
		if err1 != nil && blunder.Errno(err1) == int(blunder.NotFoundError) {
			// File not found? Good!
		} else if err1 != nil {
			err = err1
			return
		} else {
			haveObstacle = true
			// Grab our own lock and call .getstatHelper() instead of
			// letting Getstat() do it for us;
			obstacleInodeLock, err1 := mS.getWriteLock(obstacleInodeNumber, callerID)
			if err1 != nil {
				err = err1
				return
			}
			defer obstacleInodeLock.Unlock()

			err = mS.removeObstacleToObjectPut(callerID, dirInodeNumber, vObjectBaseName, obstacleInodeNumber)
			if err != nil {
				return
			}
			// We're now responsible for destroying obstacleInode, but
			// we're not going to do it yet. We'll wait to actually
			// destroy the data until after we've linked in its
			// replacement.
		}
	}

	// If we got here, then there's no obstacle (any more). Link the thing into place.
	//
	// Note that we don't have a lock on highestUnlinkedInodeNumber.
	// That's because this inode was created in this function, so
	// nobody else knows it exists, so we don't have to worry about
	// anyone else accessing it.
	err = mS.VolumeHandle.Link(dirInodeNumber, highestUnlinkedName, highestUnlinkedInodeNumber)
	if err != nil {
		logger.ErrorfWithError(err, "MiddlewarePutComplete: failed final Link(%v, %v, %v)", dirInodeNumber, highestUnlinkedName, highestUnlinkedInodeNumber)

		// We can try to recover from a Link() failure here by putting
		// the old thing back. We're still holding locks, so it's safe
		// to try.
		if haveObstacle {
			relinkErr := mS.VolumeHandle.Link(dirInodeNumber, vObjectBaseName, obstacleInodeNumber)
			// the rest of the relevant variables were logged in the previous error-logging call
			logger.ErrorfWithError(relinkErr, "MiddlewarePutComplete: relink failed for inode=%v name=%v", obstacleInodeNumber, vObjectBaseName)
		}

		return
	}

	// Log errors from inode destruction, but don't let them cause the
	// RPC call to fail. As far as this function's caller is
	// concerned, everything worked as intended.
	if haveObstacle {
		destroyErr := mS.Destroy(obstacleInodeNumber)
		if destroyErr != nil {
			logger.ErrorfWithError(destroyErr, "MiddlewarePutComplete: error destroying inode %v", obstacleInodeNumber)
		}
	}

	metadata, err := mS.GetMetadata(fileInodeNumber) // not getstat() since we're already holding a lock on this inode
	if err != nil {
		return
	}

	stats.IncrementOperations(&stats.FsMwPutCompleteOps)

	mtime = uint64(metadata.ModificationTime.UnixNano())
	// fileInodeNumber set above
	numWrites = metadata.NumWrites
	return
}

func (mS *mountStruct) MiddlewarePutContainer(containerName string, oldMetadata []byte, newMetadata []byte) (err error) {
	var (
		containerInodeLock   *dlm.RWLockStruct
		containerInodeNumber inode.InodeNumber
		existingMetadata     []byte
		newDirInodeLock      *dlm.RWLockStruct
		newDirInodeNumber    inode.InodeNumber
	)

	// Yes, it's a heavy lock to hold on the root inode. However, we
	// might need to add a new directory entry there, so there's not
	// much else we can do.
	rootInodeLock, err := mS.getWriteLock(inode.RootDirInodeNumber, nil)
	if nil != err {
		return
	}
	defer rootInodeLock.Unlock()

	containerInodeNumber, err = mS.VolumeHandle.Lookup(inode.RootDirInodeNumber, containerName)
	if err != nil && blunder.IsNot(err, blunder.NotFoundError) {
		return
	} else if err != nil {
		// No such container, so we create it

		newDirInodeNumber, err = mS.CreateDir(inode.PosixModePerm, 0, 0)
		if err != nil {
			logger.ErrorWithError(err)
			return
		}

		newDirInodeLock, err = mS.getWriteLock(newDirInodeNumber, nil)
		defer newDirInodeLock.Unlock()

		err = mS.PutStream(newDirInodeNumber, MiddlewareStream, newMetadata)
		if err != nil {
			logger.ErrorWithError(err)
			return
		}

		err = mS.VolumeHandle.Link(inode.RootDirInodeNumber, containerName, newDirInodeNumber)

		return
	}

	containerInodeLock, err = mS.getWriteLock(containerInodeNumber, nil)
	if err != nil {
		return
	}
	defer containerInodeLock.Unlock()

	// Existing container: just update the metadata
	existingMetadata, err = mS.GetStream(containerInodeNumber, MiddlewareStream)

	// GetStream() will return an error if there is no "middleware" stream
	if err != nil && blunder.IsNot(err, blunder.StreamNotFound) {
		return
	} else if err != nil {
		existingMetadata = []byte{}
	}

	// Only change it if the caller sent the current value
	if !bytes.Equal(existingMetadata, oldMetadata) {
		err = blunder.NewError(blunder.TryAgainError, "Metadata differs - actual: %v request: %v", existingMetadata, oldMetadata)
		return
	}
	err = mS.PutStream(containerInodeNumber, MiddlewareStream, newMetadata)

	stats.IncrementOperations(&stats.FsMwPutContainerOps)
	return
}

func (mS *mountStruct) Mkdir(userID inode.InodeUserID, groupID inode.InodeGroupID, otherGroupIDs []inode.InodeGroupID, inodeNumber inode.InodeNumber, basename string, filePerm inode.InodeMode) (newDirInodeNumber inode.InodeNumber, err error) {
	// Make sure the file basename is not too long
	err = validateBaseName(basename)
	if err != nil {
		return 0, err
	}

	newDirInodeNumber, err = mS.CreateDir(filePerm, userID, groupID)
	if err != nil {
		logger.ErrorWithError(err)
		return 0, err
	}

	inodeLock, err := mS.initInodeLock(inodeNumber, nil)
	if err != nil {
		return
	}
	err = inodeLock.WriteLock()
	if err != nil {
		return
	}
	defer inodeLock.Unlock()

	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.F_OK) {
		destroyErr := mS.Destroy(newDirInodeNumber)
		if destroyErr != nil {
			logger.WarnfWithError(destroyErr, "couldn't destroy inode %v after failed Access(F_OK) in fs.Mkdir", newDirInodeNumber)
		}
		err = blunder.NewError(blunder.NotFoundError, "ENOENT")
		return 0, err
	}
	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.W_OK|inode.X_OK) {
		destroyErr := mS.Destroy(newDirInodeNumber)
		if destroyErr != nil {
			logger.WarnfWithError(destroyErr, "couldn't destroy inode %v after failed Access(W_OK|X_OK) in fs.Mkdir", newDirInodeNumber)
		}
		err = blunder.NewError(blunder.PermDeniedError, "EACCES")
		return 0, err
	}

	err = mS.VolumeHandle.Link(inodeNumber, basename, newDirInodeNumber)
	if err != nil {
		destroyErr := mS.Destroy(newDirInodeNumber)
		if destroyErr != nil {
			logger.WarnfWithError(destroyErr, "couldn't destroy inode %v after failed Link() in fs.Mkdir", newDirInodeNumber)
		}
		return 0, err
	}
	stats.IncrementOperations(&stats.FsMkdirOps)
	return newDirInodeNumber, nil
}

func (mS *mountStruct) RemoveXAttr(userID inode.InodeUserID, groupID inode.InodeGroupID, otherGroupIDs []inode.InodeGroupID, inodeNumber inode.InodeNumber, streamName string) (err error) {
	inodeLock, err := mS.initInodeLock(inodeNumber, nil)
	if err != nil {
		return
	}
	err = inodeLock.WriteLock()
	if err != nil {
		return
	}
	defer inodeLock.Unlock()

	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.F_OK) {
		err = blunder.NewError(blunder.NotFoundError, "ENOENT")
		return
	}
	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.W_OK) {
		err = blunder.NewError(blunder.PermDeniedError, "EACCES")
		return
	}

	err = mS.DeleteStream(inodeNumber, streamName)
	if err != nil {
		logger.ErrorfWithError(err, "Failed to delete XAttr %v of inode %v", streamName, inodeNumber)
	}
	stats.IncrementOperations(&stats.FsRemoveXattrOps)
	return
}

func (mS *mountStruct) Rename(userID inode.InodeUserID, groupID inode.InodeGroupID, otherGroupIDs []inode.InodeGroupID, srcDirInodeNumber inode.InodeNumber, srcBasename string, dstDirInodeNumber inode.InodeNumber, dstBasename string) (err error) {
	// Flag to tell us if there's only one directory to be locked
	srcAndDestDirsAreSame := srcDirInodeNumber == dstDirInodeNumber

	// Generate our calling context ID, so that the locks will have the same callerID
	callerID := dlm.GenerateCallerID()

	// Allocate the source dir lock
	srcDirLock, err := mS.initInodeLock(srcDirInodeNumber, callerID)
	if err != nil {
		return
	}

	// Allocate the dest dir lock
	dstDirLock, err := mS.initInodeLock(dstDirInodeNumber, callerID)
	if err != nil {
		return err
	}

retryLock:
	// Get the source directory's lock
	err = srcDirLock.WriteLock()
	if err != nil {
		return
	}

	if !mS.VolumeHandle.Access(srcDirInodeNumber, userID, groupID, otherGroupIDs, inode.F_OK) {
		err = blunder.NewError(blunder.NotFoundError, "ENOENT")
		return
	}
	if !mS.VolumeHandle.Access(srcDirInodeNumber, userID, groupID, otherGroupIDs, inode.W_OK|inode.X_OK) {
		err = blunder.NewError(blunder.PermDeniedError, "EACCES")
		return
	}

	// Try to get the destination directory's lock. If we can't get it, drop the
	// source dir lock and try the whole thing again.
	if !srcAndDestDirsAreSame {
		err = dstDirLock.TryWriteLock()
		if blunder.Is(err, blunder.TryAgainError) {
			srcDirLock.Unlock()
			goto retryLock
		} else if blunder.IsNotSuccess(err) {
			// This shouldn't happen...
			srcDirLock.Unlock()
			return err
		}

		if !mS.VolumeHandle.Access(dstDirInodeNumber, userID, groupID, otherGroupIDs, inode.F_OK) {
			srcDirLock.Unlock()
			err = blunder.NewError(blunder.NotFoundError, "ENOENT")
			return
		}
		if !mS.VolumeHandle.Access(dstDirInodeNumber, userID, groupID, otherGroupIDs, inode.W_OK|inode.X_OK) {
			srcDirLock.Unlock()
			err = blunder.NewError(blunder.PermDeniedError, "EACCES")
			return
		}
	}

	// Now we have the locks for both directories; we can do the move
	err = mS.Move(srcDirInodeNumber, srcBasename, dstDirInodeNumber, dstBasename)

	// Release our locks and return
	if !srcAndDestDirsAreSame {
		dstDirLock.Unlock()
	}
	srcDirLock.Unlock()

	stats.IncrementOperations(&stats.FsRenameOps)
	return err
}

func (mS *mountStruct) Read(userID inode.InodeUserID, groupID inode.InodeGroupID, otherGroupIDs []inode.InodeGroupID, inodeNumber inode.InodeNumber, offset uint64, length uint64, profiler *utils.Profiler) (buf []byte, err error) {
	inodeLock, err := mS.initInodeLock(inodeNumber, nil)
	if err != nil {
		return
	}
	err = inodeLock.ReadLock()
	if err != nil {
		return
	}
	defer inodeLock.Unlock()

	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.F_OK) {
		err = blunder.NewError(blunder.NotFoundError, "ENOENT")
		return
	}
	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.R_OK) {
		err = blunder.NewError(blunder.PermDeniedError, "EACCES")
		return
	}

	inodeType, err := mS.VolumeHandle.GetType(inodeNumber)
	if err != nil {
		logger.ErrorfWithError(err, "couldn't get type for inode %v", inodeNumber)
		return buf, err
	}
	// Make sure the inode number is for a file inode
	if inodeType != inode.FileType {
		err = fmt.Errorf("%s: expected inode %v to be a file inode, got %v", utils.GetFnName(), inodeNumber, inodeType)
		logger.ErrorWithError(err)
		return buf, blunder.AddError(err, blunder.NotFileError)
	}

	profiler.AddEventNow("before inode.Read()")
	buf, err = mS.VolumeHandle.Read(inodeNumber, offset, length, profiler)
	profiler.AddEventNow("after inode.Read()")
	if uint64(len(buf)) > length {
		err = fmt.Errorf("%s: Buf length %v is greater than supplied length %v", utils.GetFnName(), uint64(len(buf)), length)
		logger.ErrorWithError(err)
		return buf, blunder.AddError(err, blunder.IOError)
	}

	stats.IncrementOperations(&stats.FsReadOps)
	return buf, err
}

func (mS *mountStruct) Readdir(userID inode.InodeUserID, groupID inode.InodeGroupID, otherGroupIDs []inode.InodeGroupID, inodeNumber inode.InodeNumber, prevBasenameReturned string, maxEntries uint64, maxBufSize uint64) (entries []inode.DirEntry, numEntries uint64, areMoreEntries bool, err error) {
	inodeLock, err := mS.initInodeLock(inodeNumber, nil)
	if err != nil {
		return
	}
	err = inodeLock.ReadLock()
	if err != nil {
		return
	}
	defer inodeLock.Unlock()

	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.F_OK) {
		err = blunder.NewError(blunder.NotFoundError, "ENOENT")
		return
	}
	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.X_OK) {
		err = blunder.NewError(blunder.PermDeniedError, "EACCES")
		return
	}

	stats.IncrementOperations(&stats.FsReaddirOps)

	// Call readdir helper function to do the work
	return mS.readdirHelper(inodeNumber, prevBasenameReturned, maxEntries, maxBufSize, inodeLock.GetCallerID())
}

func (mS *mountStruct) ReaddirOne(userID inode.InodeUserID, groupID inode.InodeGroupID, otherGroupIDs []inode.InodeGroupID, inodeNumber inode.InodeNumber, prevDirLocation inode.InodeDirLocation) (entries []inode.DirEntry, err error) {
	inodeLock, err := mS.initInodeLock(inodeNumber, nil)
	if err != nil {
		return entries, err
	}
	err = inodeLock.ReadLock()
	if err != nil {
		return entries, err
	}
	defer inodeLock.Unlock()

	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.F_OK) {
		err = blunder.NewError(blunder.NotFoundError, "ENOENT")
		return
	}
	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.X_OK) {
		err = blunder.NewError(blunder.PermDeniedError, "EACCES")
		return
	}

	// Call readdirOne helper function to do the work
	entries, err = mS.readdirOneHelper(inodeNumber, prevDirLocation, inodeLock.GetCallerID())
	if err != nil {
		// When the client uses location-based readdir, it knows it is done when it reads beyond
		// the last entry and gets a not found error. Because of this, we don't log not found as an error.
		if blunder.IsNot(err, blunder.NotFoundError) {
			logger.ErrorWithError(err)
		}
	}
	stats.IncrementOperations(&stats.FsReaddirOneOps)
	return entries, err
}

func (mS *mountStruct) ReaddirPlus(userID inode.InodeUserID, groupID inode.InodeGroupID, otherGroupIDs []inode.InodeGroupID, inodeNumber inode.InodeNumber, prevBasenameReturned string, maxEntries uint64, maxBufSize uint64) (dirEntries []inode.DirEntry, statEntries []Stat, numEntries uint64, areMoreEntries bool, err error) {
	inodeLock, err := mS.initInodeLock(inodeNumber, nil)
	if err != nil {
		return
	}
	err = inodeLock.ReadLock()
	if err != nil {
		return
	}

	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.F_OK) {
		err = blunder.NewError(blunder.NotFoundError, "ENOENT")
		return
	}
	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.X_OK) {
		err = blunder.NewError(blunder.PermDeniedError, "EACCES")
		return
	}

	// Get dir entries; Call readdir helper function to do the work
	dirEntries, numEntries, areMoreEntries, err = mS.readdirHelper(inodeNumber, prevBasenameReturned, maxEntries, maxBufSize, inodeLock.GetCallerID())
	inodeLock.Unlock()

	if err != nil {
		// Not logging here, since Readdir will have done that for us already.
		return dirEntries, statEntries, numEntries, areMoreEntries, err
	}

	// Get stats
	statEntries = make([]Stat, numEntries)
	for i := range dirEntries {
		entryInodeLock, err1 := mS.initInodeLock(dirEntries[i].InodeNumber, nil)
		if err = err1; err != nil {
			return
		}
		err = entryInodeLock.ReadLock()
		if err != nil {
			return
		}

		// Fill in stats, calling getstat helper function to do the work
		statEntries[i], err = mS.getstatHelper(dirEntries[i].InodeNumber, entryInodeLock.GetCallerID())
		entryInodeLock.Unlock()

		if err != nil {
			logger.ErrorWithError(err)
			return dirEntries, statEntries, numEntries, areMoreEntries, err
		}
	}

	stats.IncrementOperations(&stats.FsReaddirPlusOps)
	return dirEntries, statEntries, numEntries, areMoreEntries, err
}

func (mS *mountStruct) ReaddirOnePlus(userID inode.InodeUserID, groupID inode.InodeGroupID, otherGroupIDs []inode.InodeGroupID, inodeNumber inode.InodeNumber, prevDirLocation inode.InodeDirLocation) (dirEntries []inode.DirEntry, statEntries []Stat, err error) {
	inodeLock, err := mS.initInodeLock(inodeNumber, nil)
	if err != nil {
		return
	}
	err = inodeLock.ReadLock()
	if err != nil {
		return
	}

	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.F_OK) {
		err = blunder.NewError(blunder.NotFoundError, "ENOENT")
		return
	}
	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.X_OK) {
		err = blunder.NewError(blunder.PermDeniedError, "EACCES")
		return
	}

	// Get dir entries; Call readdirOne helper function to do the work
	dirEntries, err = mS.readdirOneHelper(inodeNumber, prevDirLocation, inodeLock.GetCallerID())
	inodeLock.Unlock()

	if err != nil {
		// When the client uses location-based readdir, it knows it is done when it reads beyond
		// the last entry and gets a not found error. Because of this, we don't log not found as an error.
		if blunder.IsNot(err, blunder.NotFoundError) {
			logger.ErrorWithError(err)
		}
		return dirEntries, statEntries, err
	}

	// Always only one entry
	numEntries := 1

	// Get stats
	statEntries = make([]Stat, numEntries)
	for i := range dirEntries {
		entryInodeLock, err1 := mS.initInodeLock(dirEntries[i].InodeNumber, nil)
		if err = err1; err != nil {
			return
		}
		err = entryInodeLock.ReadLock()
		if err != nil {
			return
		}

		// Fill in stats, calling getstat helper function to do the work
		statEntries[i], err = mS.getstatHelper(dirEntries[i].InodeNumber, entryInodeLock.GetCallerID())
		entryInodeLock.Unlock()

		if err != nil {
			logger.ErrorWithError(err)
			return dirEntries, statEntries, err
		}
	}

	stats.IncrementOperations(&stats.FsReaddirOnePlusOps)
	return dirEntries, statEntries, err
}

func (mS *mountStruct) Readsymlink(userID inode.InodeUserID, groupID inode.InodeGroupID, otherGroupIDs []inode.InodeGroupID, inodeNumber inode.InodeNumber) (target string, err error) {
	inodeLock, err := mS.initInodeLock(inodeNumber, nil)
	if err != nil {
		return
	}
	err = inodeLock.ReadLock()
	if err != nil {
		return
	}
	defer inodeLock.Unlock()

	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.F_OK) {
		err = blunder.NewError(blunder.NotFoundError, "ENOENT")
		return
	}
	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.R_OK) {
		err = blunder.NewError(blunder.PermDeniedError, "EACCES")
		return
	}

	target, err = mS.GetSymlink(inodeNumber)
	stats.IncrementOperations(&stats.FsSymlinkReadOps)
	return target, err
}

func (mS *mountStruct) Resize(userID inode.InodeUserID, groupID inode.InodeGroupID, otherGroupIDs []inode.InodeGroupID, inodeNumber inode.InodeNumber, newSize uint64) (err error) {
	inodeLock, err := mS.initInodeLock(inodeNumber, nil)
	if err != nil {
		return
	}
	err = inodeLock.WriteLock()
	if err != nil {
		return
	}
	defer inodeLock.Unlock()

	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.F_OK) {
		err = blunder.NewError(blunder.NotFoundError, "ENOENT")
		return
	}
	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.W_OK) {
		err = blunder.NewError(blunder.PermDeniedError, "EACCES")
		return
	}

	err = mS.SetSize(inodeNumber, newSize)
	stats.IncrementOperations(&stats.FsSetsizeOps)
	return err
}

func (mS *mountStruct) Rmdir(userID inode.InodeUserID, groupID inode.InodeGroupID, otherGroupIDs []inode.InodeGroupID, inodeNumber inode.InodeNumber, basename string) (err error) {
	callerID := dlm.GenerateCallerID()
	inodeLock, err := mS.initInodeLock(inodeNumber, callerID)
	if err != nil {
		return
	}
	err = inodeLock.WriteLock()
	if err != nil {
		return
	}
	defer inodeLock.Unlock()

	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.F_OK) {
		err = blunder.NewError(blunder.NotFoundError, "ENOENT")
		return
	}
	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.W_OK|inode.X_OK) {
		err = blunder.NewError(blunder.PermDeniedError, "EACCES")
		return
	}

	basenameInodeNumber, err := mS.VolumeHandle.Lookup(inodeNumber, basename)
	if nil != err {
		return
	}

	basenameInodeLock, err := mS.initInodeLock(basenameInodeNumber, callerID)
	if err != nil {
		return
	}
	err = basenameInodeLock.WriteLock()
	if err != nil {
		return
	}
	defer basenameInodeLock.Unlock()

	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.W_OK|inode.X_OK) {
		err = blunder.NewError(blunder.PermDeniedError, "EACCES")
		return
	}

	basenameInodeType, err := mS.VolumeHandle.GetType(basenameInodeNumber)
	if nil != err {
		return
	}

	if inode.DirType != basenameInodeType {
		err = fmt.Errorf("Rmdir() called on non-Directory")
		err = blunder.AddError(err, blunder.NotDirError)
		return
	}

	dirEntries, err := mS.NumDirEntries(basenameInodeNumber)
	if nil != err {
		return
	}

	if 2 != dirEntries {
		err = fmt.Errorf("Directory not empty")
		err = blunder.AddError(err, blunder.NotEmptyError)
		return
	}

	err = mS.VolumeHandle.Unlink(inodeNumber, basename)
	if nil != err {
		return
	}

	err = mS.Destroy(basenameInodeNumber)
	if nil != err {
		return
	}

	stats.IncrementOperations(&stats.FsRmdirOps)
	return
}

func (mS *mountStruct) Setstat(userID inode.InodeUserID, groupID inode.InodeGroupID, otherGroupIDs []inode.InodeGroupID, inodeNumber inode.InodeNumber, stat Stat) (err error) {
	inodeLock, err := mS.initInodeLock(inodeNumber, nil)
	if err != nil {
		return
	}
	err = inodeLock.WriteLock()
	if err != nil {
		return
	}
	defer inodeLock.Unlock()

	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.P_OK) {
		err = blunder.NewError(blunder.NotPermError, "EPERM")
		return
	}

	// Set crtime, if present in the map
	crtime, ok := stat[StatCRTime]
	if ok {
		newCreationTime := time.Unix(0, int64(crtime))
		err = mS.SetCreationTime(inodeNumber, newCreationTime)
		if err != nil {
			logger.ErrorWithError(err)
			return err
		}
	}

	// Set mtime, if present in the map
	mtime, ok := stat[StatMTime]
	if ok {
		newModificationTime := time.Unix(0, int64(mtime))
		err = mS.SetModificationTime(inodeNumber, newModificationTime)
		if err != nil {
			logger.ErrorWithError(err)
			return err
		}
	}

	// Set atime, if present in the map
	atime, ok := stat[StatATime]
	if ok {
		newAccessTime := time.Unix(0, int64(atime))
		err = mS.SetAccessTime(inodeNumber, newAccessTime)
		if err != nil {
			logger.ErrorWithError(err)
			return err
		}
	}

	// Set ctime, if present in the map
	ctime, ok := stat[StatCTime]
	if ok {
		newAccessTime := time.Unix(0, int64(ctime))
		err = mS.SetAttrChangeTime(inodeNumber, newAccessTime)
		if err != nil {
			logger.ErrorWithError(err)
			return err
		}
	}

	// Set size, if present in the map
	size, ok := stat[StatSize]
	if ok {
		err = mS.SetSize(inodeNumber, size)
		if err != nil {
			logger.ErrorWithError(err)
			return err
		}
	}

	// Set userID, if present in the map
	newUserID, settingUserID := stat[StatUserID]
	if settingUserID {
		// Since we are using a uint64 to convey a uint32 value, make sure we didn't get something too big
		if newUserID > math.MaxUint32 {
			err = fmt.Errorf("%s: userID is too large - value is %d, max is %d.", utils.GetFnName(), newUserID, math.MaxUint32)
			return blunder.AddError(err, blunder.InvalidUserIDError)
		}
	}

	// Set groupID, if present in the map
	newGroupID, settingGroupID := stat[StatGroupID]
	if settingGroupID {
		// Since we are using a uint64 to convey a uint32 value, make sure we didn't get something too big
		if newGroupID > math.MaxUint32 {
			err = fmt.Errorf("%s: groupID is too large - value is %d, max is %d.", utils.GetFnName(), newGroupID, math.MaxUint32)
			return blunder.AddError(err, blunder.InvalidGroupIDError)
		}
	}

	if settingUserID || settingGroupID {
		if settingUserID {
			if settingGroupID {
				err = mS.SetOwnerUserIDGroupID(inodeNumber, inode.InodeUserID(newUserID), inode.InodeGroupID(newGroupID))
			} else { // only settingUserID is true
				err = mS.SetOwnerUserID(inodeNumber, inode.InodeUserID(newUserID))
			}
		} else { // only settingGroupID is true
			err = mS.SetOwnerGroupID(inodeNumber, inode.InodeGroupID(newGroupID))
		}
		if nil != err {
			logger.ErrorWithError(err)
			return err
		}
	}

	// Set mode, if present in the map
	filePerm, ok := stat[StatMode]
	if ok {
		// Since we are using a uint64 to convey a uint32 value, make sure we didn't get something too big
		if filePerm > math.MaxUint32 {
			err = fmt.Errorf("%s: filePerm is too large - value is %d, max is %d.", utils.GetFnName(), filePerm, math.MaxUint32)
			return blunder.AddError(err, blunder.InvalidFileModeError)
		}

		err = mS.SetPermMode(inodeNumber, inode.InodeMode(filePerm))
		if err != nil {
			logger.ErrorWithError(err)
			return err
		}
	}

	stats.IncrementOperations(&stats.FsSetstatOps)
	return
}

// TODO: XATTR_* values are obtained from </usr/include/attr/xattr.h>, remove constants with go equivalent.
const (
	xattr_create  = 1
	xattr_replace = 2
)

func (mS *mountStruct) SetXAttr(userID inode.InodeUserID, groupID inode.InodeGroupID, otherGroupIDs []inode.InodeGroupID, inodeNumber inode.InodeNumber, streamName string, value []byte, flags int) (err error) {
	inodeLock, err := mS.initInodeLock(inodeNumber, nil)
	if err != nil {
		return
	}
	err = inodeLock.WriteLock()
	if err != nil {
		return
	}
	defer inodeLock.Unlock()

	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.F_OK) {
		err = blunder.NewError(blunder.NotFoundError, "ENOENT")
		return
	}
	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.W_OK) {
		err = blunder.NewError(blunder.PermDeniedError, "EACCES")
		return
	}

	switch flags {
	case 0:
		break
	case xattr_create:
		_, err = mS.GetXAttr(userID, groupID, otherGroupIDs, inodeNumber, streamName)
		if err == nil {
			return blunder.AddError(err, blunder.FileExistsError)
		}
	case xattr_replace:
		_, err = mS.GetXAttr(userID, groupID, otherGroupIDs, inodeNumber, streamName)
		if err != nil {
			return blunder.AddError(err, blunder.StreamNotFound)
		}
	default:
		return blunder.AddError(err, blunder.InvalidArgError)
	}

	err = mS.PutStream(inodeNumber, streamName, value)
	if err != nil {
		logger.ErrorfWithError(err, "Failed to set XAttr %v to inode %v", streamName, inodeNumber)
	}

	stats.IncrementOperations(&stats.FsSetXattrOps)
	return
}

func (mS *mountStruct) StatVfs() (statVFS StatVFS, err error) {
	statVFS = make(map[StatVFSKey]uint64)

	statVFS[StatVFSFilesystemID] = mS.GetFSID()
	statVFS[StatVFSBlockSize] = FsBlockSize
	statVFS[StatVFSFragmentSize] = FsOptimalTransferSize
	statVFS[StatVFSTotalBlocks] = VolFakeTotalBlocks
	statVFS[StatVFSFreeBlocks] = VolFakeFreeBlocks
	statVFS[StatVFSAvailBlocks] = VolFakeAvailBlocks
	statVFS[StatVFSTotalInodes] = VolFakeTotalInodes
	statVFS[StatVFSFreeInodes] = VolFakeAvailInodes
	statVFS[StatVFSAvailInodes] = VolFakeAvailInodes
	statVFS[StatVFSMountFlags] = 0
	statVFS[StatVFSMaxFilenameLen] = FileNameMax

	stats.IncrementOperations(&stats.FsStatvfsOps)
	return statVFS, nil
}

func (mS *mountStruct) Symlink(userID inode.InodeUserID, groupID inode.InodeGroupID, otherGroupIDs []inode.InodeGroupID, inodeNumber inode.InodeNumber, basename string, target string) (symlinkInodeNumber inode.InodeNumber, err error) {
	err = validateBaseName(basename)
	if err != nil {
		return
	}

	err = validateFullPath(target)
	if err != nil {
		return
	}

	// Mode for symlinks defaults to rwxrwxrwx, i.e. inode.PosixModePerm
	symlinkInodeNumber, err = mS.CreateSymlink(target, inode.PosixModePerm, userID, groupID)
	if err != nil {
		return
	}

	inodeLock, err := mS.initInodeLock(inodeNumber, nil)
	if err != nil {
		return
	}
	err = inodeLock.WriteLock()
	if err != nil {
		return
	}
	defer inodeLock.Unlock()

	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.F_OK) {
		destroyErr := mS.Destroy(symlinkInodeNumber)
		if destroyErr != nil {
			logger.WarnfWithError(destroyErr, "couldn't destroy inode %v after failed Access(F_OK) in fs.Symlink", symlinkInodeNumber)
		}
		err = blunder.NewError(blunder.NotFoundError, "ENOENT")
		return
	}
	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.W_OK|inode.X_OK) {
		destroyErr := mS.Destroy(symlinkInodeNumber)
		if destroyErr != nil {
			logger.WarnfWithError(destroyErr, "couldn't destroy inode %v after failed Access(W_OK|X_OK) in fs.Symlink", symlinkInodeNumber)
		}
		err = blunder.NewError(blunder.PermDeniedError, "EACCES")
		return
	}

	err = mS.VolumeHandle.Link(inodeNumber, basename, symlinkInodeNumber)
	if err != nil {
		destroyErr := mS.Destroy(symlinkInodeNumber)
		if destroyErr != nil {
			logger.WarnfWithError(destroyErr, "couldn't destroy inode %v after failed Link() in fs.Symlink", symlinkInodeNumber)
		}
		return
	}

	stats.IncrementOperations(&stats.FsSymlinkOps)
	return
}

func (mS *mountStruct) Unlink(userID inode.InodeUserID, groupID inode.InodeGroupID, otherGroupIDs []inode.InodeGroupID, inodeNumber inode.InodeNumber, basename string) (err error) {
	callerID := dlm.GenerateCallerID()
	inodeLock, err := mS.initInodeLock(inodeNumber, callerID)
	if err != nil {
		return
	}
	err = inodeLock.WriteLock()
	if err != nil {
		return
	}
	defer inodeLock.Unlock()

	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.F_OK) {
		err = blunder.NewError(blunder.NotFoundError, "ENOENT")
		return
	}
	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.W_OK|inode.X_OK) {
		err = blunder.NewError(blunder.PermDeniedError, "EACCES")
		return
	}

	basenameInodeNumber, err := mS.VolumeHandle.Lookup(inodeNumber, basename)
	if nil != err {
		return
	}

	basenameInodeLock, err := mS.initInodeLock(basenameInodeNumber, callerID)
	if err != nil {
		return
	}
	err = basenameInodeLock.WriteLock()
	if err != nil {
		return
	}
	defer basenameInodeLock.Unlock()

	basenameInodeType, err := mS.VolumeHandle.GetType(basenameInodeNumber)
	if nil != err {
		return
	}

	if inode.DirType == basenameInodeType {
		err = fmt.Errorf("Unlink() called on a Directory")
		err = blunder.AddError(err, blunder.IsDirError)
		return
	}

	err = mS.VolumeHandle.Unlink(inodeNumber, basename)
	if nil != err {
		return
	}

	basenameLinkCount, err := mS.GetLinkCount(basenameInodeNumber)
	if nil != err {
		return
	}

	if 0 == basenameLinkCount {
		err = mS.Destroy(basenameInodeNumber)
		if nil != err {
			return
		}
	}

	stats.IncrementOperations(&stats.FsUnlinkOps)
	return
}

func (mS *mountStruct) Validate(inodeNumber inode.InodeNumber) (err error) {
	err = mS.Validate(inodeNumber)
	if err != nil {
		return err
	}

	stats.IncrementOperations(&stats.FsValidateOps)
	return nil
}

func (mS *mountStruct) VolumeName() (volumeName string) {
	volumeName = mS.volumeName
	return
}

func (mS *mountStruct) Write(userID inode.InodeUserID, groupID inode.InodeGroupID, otherGroupIDs []inode.InodeGroupID, inodeNumber inode.InodeNumber, offset uint64, buf []byte, profiler *utils.Profiler) (size uint64, err error) {
	inodeLock, err := mS.initInodeLock(inodeNumber, nil)
	if err != nil {
		return
	}
	err = inodeLock.WriteLock()
	if err != nil {
		return
	}
	defer inodeLock.Unlock()

	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.F_OK) {
		err = blunder.NewError(blunder.NotFoundError, "ENOENT")
		return
	}
	if !mS.VolumeHandle.Access(inodeNumber, userID, groupID, otherGroupIDs, inode.W_OK) {
		err = blunder.NewError(blunder.PermDeniedError, "EACCES")
		return
	}

	profiler.AddEventNow("before inode.Write()")
	err = mS.VolumeHandle.Write(inodeNumber, offset, buf, profiler)
	profiler.AddEventNow("after inode.Write()")
	// write to Swift presumably succeeds or fails as a whole
	if err != nil {
		return 0, err
	}
	size = uint64(len(buf))
	stats.IncrementOperations(&stats.FsWriteOps)
	return
}

func validateBaseName(baseName string) (err error) {
	// Make sure the file baseName is not too long
	baseLen := len(baseName)
	if baseLen > FileNameMax {
		err = fmt.Errorf("%s: basename is too long. Length %v, max %v", utils.GetFnName(), baseLen, FileNameMax)
		logger.ErrorWithError(err)
		return blunder.AddError(err, blunder.NameTooLongError)
	}
	stats.IncrementOperations(&stats.FsBasenameValidateOps)
	return
}

func validateFullPath(fullPath string) (err error) {
	pathLen := len(fullPath)
	if pathLen > FilePathMax {
		err = fmt.Errorf("%s: fullpath is too long. Length %v, max %v", utils.GetFnName(), pathLen, FilePathMax)
		logger.ErrorWithError(err)
		return blunder.AddError(err, blunder.NameTooLongError)
	}
	stats.IncrementOperations(&stats.FsFullpathValidateOps)
	return
}

func revSplitPath(fullpath string) []string {
	// TrimPrefix avoids empty [0] element in pathSegments
	trimmed := strings.TrimPrefix(fullpath, "/")
	if trimmed == "" {
		// path.Clean("") = ".", which is not useful
		return []string{}
	}

	segments := strings.Split(path.Clean(trimmed), "/")
	slen := len(segments)
	for i := 0; i < slen/2; i++ {
		segments[i], segments[slen-i-1] = segments[slen-i-1], segments[i]
	}
	return segments
}

// Helper function to look up a path and return its inode, inode type,
// and a (locked) read lock for that inode.
//
// When the returned err is nil, the caller is responsible for
// unlocking. If err is non-nil, this function will handle any
// unlocking. This lets the caller return immediately on error, as Go
// code likes to do.
//
// If the referenced entity is a symlink, then it will be followed.
// Subsequent symlinks will also be followed until a terminal
// non-symlink is reached, up to a limit of MaxSymlinks. A terminal
// non-symlink may be a directory, a file, or something that does not
// exist.
func (mS *mountStruct) resolvePathForRead(fullpath string, callerID dlm.CallerID) (inodeNumber inode.InodeNumber, inodeType inode.InodeType, inodeLock *dlm.RWLockStruct, err error) {
	return mS.resolvePath(fullpath, callerID, mS.ensureReadLock)
}

func (mS *mountStruct) resolvePathForWrite(fullpath string, callerID dlm.CallerID) (inodeNumber inode.InodeNumber, inodeType inode.InodeType, inodeLock *dlm.RWLockStruct, err error) {
	return mS.resolvePath(fullpath, callerID, mS.ensureWriteLock)
}

func (mS *mountStruct) resolvePath(fullpath string, callerID dlm.CallerID, getLock func(inode.InodeNumber, dlm.CallerID) (*dlm.RWLockStruct, error)) (inodeNumber inode.InodeNumber, inodeType inode.InodeType, inodeLock *dlm.RWLockStruct, err error) {
	// pathSegments is the reversed split path. For example, if
	// fullpath is "/etc/thing/default.conf", then pathSegments is
	// ["default.conf", "thing", "etc"].
	//
	// The reversal is just because Go gives us append() but no
	// prepend() for slices.
	pathSegments := revSplitPath(fullpath)

	// Our protection against symlink loops is a limit on the number
	// of symlinks that we will follow.
	followsRemaining := MaxSymlinks

	var cursorInodeNumber inode.InodeNumber
	var cursorInodeType inode.InodeType
	var cursorInodeLock *dlm.RWLockStruct
	dirInodeNumber := inode.RootDirInodeNumber
	dirInodeLock, err := getLock(dirInodeNumber, callerID)

	// Use defer for cleanup so that we don't have to think as hard
	// about every if-error-return block.
	defer func() {
		if dirInodeLock != nil {
			dirInodeLock.Unlock()
		}
	}()
	defer func() {
		if cursorInodeLock != nil {
			cursorInodeLock.Unlock()
		}
	}()

	for len(pathSegments) > 0 {
		segment := pathSegments[len(pathSegments)-1]
		pathSegments = pathSegments[:len(pathSegments)-1]

		if segment == "." {
			continue
		}

		// Look up the entry in the directory.
		//
		// If we find a relative symlink (does not start with "/"),
		// then we'll need to keep this lock around for our next pass
		// through the loop.
		cursorInodeNumber, err = mS.VolumeHandle.Lookup(dirInodeNumber, segment)
		if err != nil {
			return
		}

		cursorInodeLock, err = getLock(cursorInodeNumber, callerID)
		if err != nil {
			return
		}
		cursorInodeType, err = mS.VolumeHandle.GetType(cursorInodeNumber)
		if err != nil {
			return
		}

		if cursorInodeType == inode.SymlinkType {
			// Dereference the symlink and continue path traversal
			// from the appropriate location.
			if followsRemaining == 0 {
				err = blunder.NewError(blunder.TooManySymlinksError, "Too many symlinks while resolving %s", fullpath)
				return
			} else {
				followsRemaining -= 1
			}

			target, err1 := mS.GetSymlink(cursorInodeNumber)
			if cursorInodeLock != nil {
				cursorInodeLock.Unlock() // done with this symlink, error or not
				cursorInodeLock = nil
			}
			if err1 != nil {
				err = err1
				return
			}

			if strings.HasPrefix(target, "/") {
				// Absolute symlink; we don't keep track of the
				// current directory any more, but restart traversal
				// from the root directory.
				if dirInodeLock != nil {
					dirInodeLock.Unlock()
					dirInodeLock = nil
				}
				dirInodeNumber = inode.RootDirInodeNumber
				dirInodeLock, err = getLock(inode.RootDirInodeNumber, callerID)
				if err != nil {
					return
				}
			}
			newSegments := revSplitPath(target)
			pathSegments = append(pathSegments, newSegments...)
		} else if len(pathSegments) == 0 {
			// This was the final path segment (and not a symlink), so
			// return what we've found. File? Directory? Something
			// else entirely? Doesn't matter; it's the caller's
			// problem now.
			inodeNumber = cursorInodeNumber
			inodeType = cursorInodeType
			// We're returning a held lock. This is intentional.
			inodeLock = cursorInodeLock
			cursorInodeLock = nil // prevent deferred cleanup
			return
		} else if cursorInodeType == inode.FileType {
			// If we hit a file but there's still path segments
			// left, then the path is invalid , e.g.
			// "/stuff/so-far-so-good/kitten.png/you-cannot-have-this-part"
			err = blunder.NewError(blunder.NotDirError, "%s is a file, not a directory", segment)
			return
		} else {
			// Found a directory; continue traversal from therein
			if dirInodeLock != nil {
				dirInodeLock.Unlock()
			}
			dirInodeNumber = cursorInodeNumber
			dirInodeLock = cursorInodeLock
			cursorInodeLock = nil
		}
	}

	// If we ever had any pathSegments at all, we exited via one of
	// the return statements in the above loop. The only way to get
	// here should be to have pathSegments be empty, which means the
	// caller is resolving the path "/".
	inodeNumber = dirInodeNumber
	inodeType = inode.DirType
	inodeLock = dirInodeLock
	dirInodeLock = nil // prevent deferred cleanup
	return
}

// Utility function to unlink, but not destroy, a particular file or empty subdirectory.
//
// This function checks that the directory is empty.
//
// The caller of this function must hold appropriate locks.
//
// obstacleInodeNumber must refer to an existing file or directory
// that is (a) already part of the directory tree and (b) not the root
// directory.
func (mS *mountStruct) removeObstacleToObjectPut(callerID dlm.CallerID, dirInodeNumber inode.InodeNumber, obstacleName string, obstacleInodeNumber inode.InodeNumber) error {
	statResult, err := mS.getstatHelper(obstacleInodeNumber, callerID)
	if err != nil {
		return err
	}

	fileType := inode.InodeType(statResult[StatFType])
	if fileType == inode.FileType || fileType == inode.SymlinkType {
		// Files and symlinks can always, barring errors, be unlinked
		err = mS.VolumeHandle.Unlink(dirInodeNumber, obstacleName)
		if err != nil {
			return err
		}
	} else if fileType == inode.DirType {
		numEntries, err := mS.NumDirEntries(obstacleInodeNumber)
		if err != nil {
			return err
		}
		if numEntries >= 3 {
			// We're looking at a pre-existing, user-visible directory
			// that's linked into the directory structure, so we've
			// got at least two entries, namely "." and ".."
			//
			// If there's a third, then the directory is non-empty.
			return blunder.NewError(blunder.IsDirError, "%s is a non-empty directory", obstacleName)
		} else {
			// We don't want to call Rmdir() here since
			// that function (a) grabs locks, (b) checks
			// that it's a directory and is empty, then
			// (c) calls Unlink() and Destroy().
			//
			// We already have the locks and we've already
			// checked that it's empty, so let's just get
			// down to it.
			err = mS.VolumeHandle.Unlink(dirInodeNumber, obstacleName)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Utility function to unlink, but not destroy, a particular file or empty subdirectory.
//
// This function checks that the directory is empty.
//
// The caller of this function must hold appropriate locks.
//
// obstacleInodeNumber must refer to an existing file or directory
// that is (a) already part of the directory tree and (b) not the root
// directory.
func removeObstacleToObjectPut(mount *mountStruct, callerID dlm.CallerID, dirInodeNumber inode.InodeNumber, obstacleName string, obstacleInodeNumber inode.InodeNumber) error {
	statResult, err := mount.getstatHelper(obstacleInodeNumber, callerID)
	if err != nil {
		return err
	}

	fileType := inode.InodeType(statResult[StatFType])
	if fileType == inode.FileType || fileType == inode.SymlinkType {
		// Files and symlinks can always, barring errors, be unlinked
		err = mount.VolumeHandle.Unlink(dirInodeNumber, obstacleName)
		if err != nil {
			return err
		}
	} else if fileType == inode.DirType {
		numEntries, err := mount.NumDirEntries(obstacleInodeNumber)
		if err != nil {
			return err
		}
		if numEntries >= 3 {
			// We're looking at a pre-existing, user-visible directory
			// that's linked into the directory structure, so we've
			// got at least two entries, namely "." and ".."
			//
			// If there's a third, then the directory is non-empty.
			return blunder.NewError(blunder.IsDirError, "%s is a non-empty directory", obstacleName)
		} else {
			// We don't want to call Rmdir() here since
			// that function (a) grabs locks, (b) checks
			// that it's a directory and is empty, then
			// (c) calls Unlink() and Destroy().
			//
			// We already have the locks and we've already
			// checked that it's empty, so let's just get
			// down to it.
			err = mount.VolumeHandle.Unlink(dirInodeNumber, obstacleName)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Utility function to append entries to reply
func appendReadPlanEntries(readPlan []inode.ReadPlanStep, readRangeOut *[]inode.ReadPlanStep) (numEntries uint64) {
	for i := range readPlan {
		entry := inode.ReadPlanStep{ObjectPath: readPlan[i].ObjectPath, Offset: readPlan[i].Offset, Length: readPlan[i].Length}
		*readRangeOut = append(*readRangeOut, entry)
		numEntries++
	}
	return
}

type dirToDescend struct {
	name string
	ino  inode.InodeNumber
}

// readdir is a helper function to do the work of Readdir once we hold the lock.
func (mS *mountStruct) readdirHelper(inodeNumber inode.InodeNumber, prevBasenameReturned string, maxEntries uint64, maxBufSize uint64, callerID dlm.CallerID) (entries []inode.DirEntry, numEntries uint64, areMoreEntries bool, err error) {
	lockID, err := mS.makeLockID(inodeNumber)
	if err != nil {
		return
	}
	if !dlm.IsLockHeld(lockID, callerID, dlm.ANYLOCK) {
		err = fmt.Errorf("%s: inode %v lock must be held before calling.", utils.GetFnName(), inodeNumber)
		return nil, 0, false, blunder.AddError(err, blunder.NotFoundError)
	}

	entries, areMoreEntries, err = mS.ReadDir(inodeNumber, maxEntries, maxBufSize, prevBasenameReturned)
	if err != nil {
		return entries, numEntries, areMoreEntries, err
	}
	numEntries = uint64(len(entries))

	// Tracker: 129872175: Directory entry must have the type, we should not be getting from inode, due to potential lock order issues.
	for i := range entries {
		if inodeNumber == entries[i].InodeNumber {
			entries[i].Type, _ = mS.getTypeHelper(entries[i].InodeNumber, callerID) // in case of "."
		} else {
			entryInodeLock, err1 := mS.initInodeLock(entries[i].InodeNumber, callerID)
			if err = err1; err != nil {
				return
			}
			err = entryInodeLock.ReadLock()
			if err != nil {
				return
			}
			entries[i].Type, _ = mS.getTypeHelper(entries[i].InodeNumber, entryInodeLock.GetCallerID())
			entryInodeLock.Unlock()
		}
	}
	return entries, numEntries, areMoreEntries, err
}

// readdirOne is a helper function to do the work of ReaddirOne once we hold the lock.
func (mS *mountStruct) readdirOneHelper(inodeNumber inode.InodeNumber, prevDirLocation inode.InodeDirLocation, callerID dlm.CallerID) (entries []inode.DirEntry, err error) {
	lockID, err := mS.makeLockID(inodeNumber)
	if err != nil {
		return
	}
	if !dlm.IsLockHeld(lockID, callerID, dlm.ANYLOCK) {
		err = fmt.Errorf("%s: inode %v lock must be held before calling.", utils.GetFnName(), inodeNumber)
		err = blunder.AddError(err, blunder.NotFoundError)
		return
	}

	entries, _, err = mS.ReadDir(inodeNumber, 1, 0, prevDirLocation)
	if err != nil {
		// Note: by convention, we don't log errors in helper functions; the caller should
		//       be the one to log or not given its use case.
		return entries, err
	}

	// Tracker: 129872175: Directory entry must have the type, we should not be getting from inode, due to potential lock order issues.
	for i := range entries {
		if inodeNumber == entries[i].InodeNumber {
			entries[i].Type, _ = mS.getTypeHelper(entries[i].InodeNumber, callerID) // in case of "."
		} else {
			entryInodeLock, err1 := mS.initInodeLock(entries[i].InodeNumber, callerID)
			if err = err1; err != nil {
				return
			}
			err = entryInodeLock.ReadLock()
			if err != nil {
				return
			}
			entries[i].Type, _ = mS.getTypeHelper(entries[i].InodeNumber, callerID)
			entryInodeLock.Unlock()
		}
	}

	return entries, err
}
