package nsqd

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"path"
	"sync"
	"sync/atomic"
	"time"

	"github.com/youzan/nsq/internal/util"
)

const (
	MAX_QUEUE_OFFSET_META_DATA_KEEP = 100
)

var (
	ErrInvalidOffset     = errors.New("invalid offset")
	ErrNeedFixQueueStart = errors.New("init queue start should be fixed")
	writeBufSize         = 1024 * 128
)

func getQueueFileOffsetMeta(dataFileName string) (int64, int64, int64, error) {
	fName := dataFileName + ".offsetmeta.dat"
	f, err := os.OpenFile(fName, os.O_RDONLY, 0644)
	if err != nil {
		return 0, 0, 0, err
	}
	defer f.Close()
	cnt := int64(0)
	startPos := int64(0)
	endPos := int64(0)
	_, err = fmt.Fscanf(f, "%d\n%d,%d\n",
		&cnt,
		&startPos, &endPos)
	if err != nil {
		nsqLog.LogErrorf("failed to read offset meta (%v): %v", fName, err)
		return 0, 0, 0, err
	}
	return cnt, startPos, endPos, nil
}

// diskQueueWriter implements the BackendQueue interface
// providing a filesystem backed FIFO queue
type diskQueueWriter struct {
	// 64bit atomic vars need to be first for proper alignment on 32bit platforms
	// run-time state (also persisted to disk)
	diskWriteEnd diskQueueEndInfo
	diskReadEnd  diskQueueEndInfo
	// the start of the queue , will be set to the cleaned offset
	diskQueueStart diskQueueEndInfo
	sync.RWMutex

	// instantiation time metadata
	name            string
	dataPath        string
	maxBytesPerFile int64 // currently this cannot change once created
	minMsgSize      int32
	maxMsgSize      int32
	exitFlag        int32
	needSync        bool

	writeFile    *os.File
	bufferWriter *bufio.Writer
}

type extraMeta struct {
	SegOffset     diskQueueOffset
	VirtualOffset BackendOffset
	TotalMsgCnt   int64
}

func NewDiskQueueWriter(name string, dataPath string, maxBytesPerFile int64,
	minMsgSize int32, maxMsgSize int32,
	syncEvery int64) (BackendQueueWriter, error) {
	return newDiskQueueWriter(name, dataPath, maxBytesPerFile,
		minMsgSize, maxMsgSize, syncEvery, false)
}

func NewDiskQueueWriterForRead(name string, dataPath string, maxBytesPerFile int64,
	minMsgSize int32, maxMsgSize int32,
	syncEvery int64) (BackendQueueWriter, error) {
	return newDiskQueueWriter(name, dataPath, maxBytesPerFile,
		minMsgSize, maxMsgSize, syncEvery, true)
}

// newDiskQueue instantiates a new instance of diskQueueWriter, retrieving metadata
// from the filesystem and starting the read ahead goroutine
func newDiskQueueWriter(name string, dataPath string, maxBytesPerFile int64,
	minMsgSize int32, maxMsgSize int32,
	syncEvery int64, readOnly bool) (BackendQueueWriter, error) {

	d := diskQueueWriter{
		name:            name,
		dataPath:        dataPath,
		maxBytesPerFile: maxBytesPerFile,
		minMsgSize:      minMsgSize,
		maxMsgSize:      maxMsgSize,
	}

	// no need to lock here, nothing else could possibly be touching this instance
	err := d.retrieveMetaData()
	if err != nil && !os.IsNotExist(err) {
		nsqLog.LogErrorf("diskqueue(%s) failed to retrieveMetaData - %s", d.name, err)
	}
	err = d.initQueueReadStart()
	if err != nil && !os.IsNotExist(err) {
		nsqLog.LogErrorf("diskqueue(%s) failed to init queue start- %s", d.name, err)
		return &d, err
	}

	if !readOnly {
		d.saveExtraMeta()
	}

	return &d, nil
}

func (d *diskQueueWriter) PutV2(data []byte) (BackendOffset, int32, diskQueueEndInfo, error) {
	d.Lock()

	if d.exitFlag == 1 {
		d.Unlock()
		return 0, 0, diskQueueEndInfo{}, errors.New("exiting")
	}
	offset, writeBytes, dend, werr := d.writeOne(data, false, 0)
	var e diskQueueEndInfo
	if dend != nil {
		e = *dend
	}
	d.needSync = true
	d.Unlock()
	return offset, writeBytes, e, werr
}

func (d *diskQueueWriter) PutRawV2(data []byte, msgCnt int32) (BackendOffset, int32, diskQueueEndInfo, error) {
	d.Lock()

	if d.exitFlag == 1 {
		d.Unlock()
		return 0, 0, diskQueueEndInfo{}, errors.New("exiting")
	}
	offset, writeBytes, dend, werr := d.writeOne(data, true, msgCnt)
	var e diskQueueEndInfo
	if dend != nil {
		e = *dend
	}
	d.needSync = true
	d.Unlock()
	return offset, writeBytes, e, werr
}

// Put writes a []byte to the queue
func (d *diskQueueWriter) Put(data []byte) (BackendOffset, int32, int64, error) {
	d.Lock()

	if d.exitFlag == 1 {
		d.Unlock()
		return 0, 0, 0, errors.New("exiting")
	}
	offset, writeBytes, dend, werr := d.writeOne(data, false, 0)
	var e diskQueueEndInfo
	if dend != nil {
		e = *dend
	}
	d.needSync = true
	d.Unlock()
	return offset, writeBytes, e.TotalMsgCnt(), werr
}

func (d *diskQueueWriter) RollbackWriteV2(offset BackendOffset, diffCnt uint64) (diskQueueEndInfo, error) {
	d.Lock()
	defer d.Unlock()

	if offset < d.diskQueueStart.virtualEnd ||
		d.diskWriteEnd.TotalMsgCnt()-int64(diffCnt) < d.diskQueueStart.TotalMsgCnt() {
		nsqLog.Logf("rollback write to %v:%v invalid, less than queue start %v", offset, diffCnt, d.diskQueueStart)
		return d.diskWriteEnd, ErrInvalidOffset
	}

	if d.needSync {
		d.sync()
	}

	if offset > d.diskWriteEnd.Offset() {
		return d.diskWriteEnd, ErrInvalidOffset
	}
	if offset < d.diskWriteEnd.Offset()-BackendOffset(d.diskWriteEnd.EndOffset.Pos) {
		nsqLog.Logf("rollback write position can not across file %v, %v, %v", offset, d.diskWriteEnd.Offset(), d.diskWriteEnd.EndOffset.Pos)
		return d.diskWriteEnd, ErrInvalidOffset
	}
	nsqLog.Logf("rollback from %v-%v, %v to %v, roll cnt: %v", d.diskWriteEnd.EndOffset.FileNum, d.diskWriteEnd.EndOffset.Pos, d.diskWriteEnd.Offset(), offset, diffCnt)
	d.diskWriteEnd.EndOffset.Pos -= int64(d.diskWriteEnd.Offset() - offset)
	d.diskWriteEnd.virtualEnd = offset
	atomic.AddInt64(&d.diskWriteEnd.totalMsgCnt, -1*int64(diffCnt))

	if d.diskReadEnd.EndOffset.Pos > d.diskWriteEnd.EndOffset.Pos ||
		d.diskReadEnd.Offset() > d.diskWriteEnd.Offset() {
		d.diskReadEnd = d.diskWriteEnd
	}
	nsqLog.Logf("after rollback : %v, %v, read end: %v", d.diskWriteEnd.EndOffset.Pos, d.diskWriteEnd.TotalMsgCnt(), d.diskReadEnd)
	d.truncateDiskQueueToWriteEnd()
	return d.diskWriteEnd, nil
}

func (d *diskQueueWriter) RollbackWrite(offset BackendOffset, diffCnt uint64) error {
	_, err := d.RollbackWriteV2(offset, diffCnt)
	return err
}

func (d *diskQueueWriter) ResetWriteEnd(offset BackendOffset, totalCnt int64) error {
	_, err := d.ResetWriteEndV2(offset, totalCnt)
	return err
}

func (d *diskQueueWriter) ResetWriteWithQueueStart(queueStart BackendQueueEnd) error {
	d.Lock()
	defer d.Unlock()
	nsqLog.Warningf("DISKQUEUE %v reset the queue start from %v:%v to new queue start: %v", d.name,
		d.diskQueueStart, d.diskWriteEnd, queueStart)
	d.cleanOldData()

	d.diskQueueStart = d.diskWriteEnd
	d.diskQueueStart.virtualEnd = queueStart.Offset()
	d.diskQueueStart.totalMsgCnt = queueStart.TotalMsgCnt()
	d.diskWriteEnd = d.diskQueueStart
	d.diskReadEnd = d.diskWriteEnd
	nsqLog.Warningf("DISKQUEUE %v new queue start : %v:%v", d.name,
		d.diskQueueStart, d.diskWriteEnd)
	d.saveExtraMeta()
	return nil
}

func (d *diskQueueWriter) prepareCleanByRetention(cleanEndInfo BackendQueueOffset,
	noRealClean bool, maxCleanOffset BackendOffset) (BackendQueueEnd, int64, int64, error) {
	if cleanEndInfo == nil {
		return nil, 0, 0, nil
	}
	d.Lock()
	defer d.Unlock()
	newStart := d.diskQueueStart
	var endInfo *diskQueueOffset
	switch e := cleanEndInfo.(type) {
	case *diskQueueOffsetInfo:
		endInfo = &e.EndOffset
	case *diskQueueEndInfo:
		endInfo = &e.EndOffset
	}
	cleanOffset := cleanEndInfo.Offset()
	if endInfo != nil {
		if endInfo.FileNum >= d.diskReadEnd.EndOffset.FileNum-1 {
			endInfo.FileNum = d.diskReadEnd.EndOffset.FileNum - 1
		}
		// it may happen while the disk is started with (filenum, offset) = (2, 0) because
		// of truncated from leader to replica
		// so we should ignore clean file less than queue start file num.
		if endInfo.FileNum <= d.diskQueueStart.EndOffset.FileNum {
			return nil, 0, 0, nil
		}
		if maxCleanOffset != BackendOffset(0) && cleanOffset > maxCleanOffset {
			nsqLog.LogWarningf("disk %v clean position %v exceed the max allowed clean end: %v", d.name, cleanOffset, maxCleanOffset)
			return nil, 0, 0, nil
		}
	} else {
		if cleanOffset >= d.diskReadEnd.Offset()-BackendOffset(d.diskReadEnd.EndOffset.Pos) {
			cleanOffset = d.diskReadEnd.Offset() - BackendOffset(d.diskReadEnd.EndOffset.Pos)
		}
		if maxCleanOffset != BackendOffset(0) && cleanOffset > maxCleanOffset {
			cleanOffset = maxCleanOffset
		}
	}
	cleanFileNum := int64(0)
	if endInfo != nil {
		cleanFileNum = endInfo.FileNum
		if cleanFileNum <= d.diskQueueStart.EndOffset.FileNum {
			return &newStart, 0, 0, nil
		}
		cnt, _, endPos, err := getQueueFileOffsetMeta(d.fileName(cleanFileNum - 1))
		if err != nil {
			nsqLog.Logf("disk %v failed to get queue offset meta: %v", d.fileName(cleanFileNum), err)
			return &newStart, 0, 0, err
		}
		if maxCleanOffset != BackendOffset(0) && BackendOffset(endPos) > maxCleanOffset {
			nsqLog.LogWarningf("disk %v clean position %v exceed the max allowed clean end: %v", d.name, endPos, maxCleanOffset)
			return &newStart, 0, 0, errors.New("clean exceed the max allowed")
		}
		newStart.EndOffset.FileNum = cleanFileNum
		newStart.EndOffset.Pos = 0
		newStart.virtualEnd = BackendOffset(endPos)
		newStart.totalMsgCnt = cnt
	} else if cleanOffset >= 0 {
		for {
			cnt, _, endPos, err := getQueueFileOffsetMeta(d.fileName(newStart.EndOffset.FileNum))
			if err != nil {
				nsqLog.LogWarningf("disk %v failed to get queue offset meta: %v", newStart, err)
				return &newStart, 0, 0, err
			}
			if BackendOffset(endPos) < cleanOffset {
				if cleanFileNum >= d.diskReadEnd.EndOffset.FileNum-1 {
					break
				}
				newStart.EndOffset.FileNum++
				newStart.EndOffset.Pos = 0
				newStart.virtualEnd = BackendOffset(endPos)
				newStart.totalMsgCnt = cnt
				cleanFileNum = newStart.EndOffset.FileNum
			} else {
				break
			}
		}
	}

	if noRealClean {
		return &newStart, 0, 0, nil
	}

	nsqLog.Infof("DISKQUEUE %v clean queue from %v, %v to new start : %v", d.name,
		d.diskQueueStart, d.diskWriteEnd, newStart)

	cleanStartFileNum := d.diskQueueStart.EndOffset.FileNum - MAX_QUEUE_OFFSET_META_DATA_KEEP - 1
	if cleanStartFileNum < 0 {
		cleanStartFileNum = 0
	}
	d.diskQueueStart = newStart
	d.saveExtraMeta()
	return &newStart, cleanStartFileNum, cleanFileNum, nil
}

func (d *diskQueueWriter) CleanOldDataByRetention(cleanEndInfo BackendQueueOffset,
	noRealClean bool, maxCleanOffset BackendOffset) (BackendQueueEnd, error) {
	newStart, cleanStartFileNum, cleanFileNum, err := d.prepareCleanByRetention(cleanEndInfo, noRealClean, maxCleanOffset)
	if err != nil {
		return nil, err
	}
	cleanMetaFileNum := cleanFileNum - MAX_QUEUE_OFFSET_META_DATA_KEEP
	for i := cleanStartFileNum; i < cleanFileNum; i++ {
		fn := d.fileName(i)
		innerErr := os.Remove(fn)
		if innerErr != nil {
			if !os.IsNotExist(innerErr) {
				nsqLog.LogErrorf("diskqueue(%s) failed to remove data file %v - %s", d.name, fn, innerErr)
				continue
			}
		} else {
			nsqLog.Logf("DISKQUEUE(%s): removed data file: %v", d.name, fn)
		}

		//remove queue meta file
		if i <= cleanMetaFileNum {
			fn = d.fileName(i) + ".offsetmeta.dat"
			innerErr = os.Remove(fn)
			if innerErr != nil {
				if !os.IsNotExist(innerErr) {
					nsqLog.LogErrorf("diskqueue(%s) failed to remove offset meta data file %v - %s", d.name, fn, innerErr)
				}
			} else {
				nsqLog.Debugf("DISKQUEUE(%s): removed offset meta data file: %v", d.name, fn)
			}
		}
	}

	return newStart, nil
}

func (d *diskQueueWriter) closeCurrentFile() {
	if d.bufferWriter != nil {
		d.bufferWriter.Flush()
	}
	if d.diskReadEnd.EndOffset.GreatThan(&d.diskWriteEnd.EndOffset) {
		nsqLog.LogWarningf("DISKQUEUE(%s): old read is greater: %v, %v", d.name,
			d.diskReadEnd, d.diskWriteEnd)
	}
	d.diskReadEnd = d.diskWriteEnd
	if d.writeFile != nil {
		d.writeFile.Close()
		d.writeFile = nil
	}
}

func (d *diskQueueWriter) truncateDiskQueueToWriteEnd() {
	if d.writeFile != nil {
		d.writeFile.Truncate(d.diskWriteEnd.EndOffset.Pos)
		d.writeFile.Close()
		d.writeFile = nil
	} else {
		curFileName := d.fileName(d.diskWriteEnd.EndOffset.FileNum)
		tmpFile, err := os.OpenFile(curFileName, os.O_RDWR|os.O_CREATE, 0644)
		if err != nil {
			nsqLog.LogErrorf("open write queue failed: %v", err)
		} else {
			tmpFile.Truncate(d.diskWriteEnd.EndOffset.Pos)
			tmpFile.Close()
		}
	}
	cleanNum := d.diskWriteEnd.EndOffset.FileNum + 1
	for {
		fileName := d.fileName(cleanNum)
		err := os.Rename(fileName, fileName+".rolldata")
		if err != nil {
			if os.IsNotExist(err) {
				return
			}
			nsqLog.LogErrorf("truncate and remove the write file %v failed: %v", fileName, err)
		}
		nsqLog.LogWarningf("truncate queue and remove the write file %v ", fileName)
		cleanNum++
	}
}

func (d *diskQueueWriter) ResetWriteEndV2(offset BackendOffset, totalCnt int64) (diskQueueEndInfo, error) {
	d.Lock()
	defer d.Unlock()
	if offset < d.diskQueueStart.virtualEnd || totalCnt < d.diskQueueStart.TotalMsgCnt() {
		nsqLog.Logf("reset write end to %v:%v invalid, less than queue start %v", offset, totalCnt, d.diskQueueStart)
		return d.diskWriteEnd, ErrInvalidOffset
	}
	if offset > d.diskWriteEnd.Offset() {
		return d.diskWriteEnd, ErrInvalidOffset
	}
	if d.needSync {
		d.sync()
	}
	nsqLog.Logf("reset write end from %v to %v, reset to totalCnt: %v", d.diskWriteEnd.Offset(), offset, totalCnt)
	if offset == 0 {
		d.closeCurrentFile()
		d.diskWriteEnd = d.diskQueueStart
		d.diskReadEnd = d.diskWriteEnd
		d.truncateDiskQueueToWriteEnd()
		return d.diskWriteEnd, nil
	}
	newEnd := d.diskWriteEnd.Offset()
	newWriteFileNum := d.diskWriteEnd.EndOffset.FileNum
	newWritePos := d.diskWriteEnd.EndOffset.Pos
	for offset < newEnd-BackendOffset(newWritePos) {
		nsqLog.Logf("reset write acrossing file %v, %v, %v, %v", offset, newEnd, newWritePos, newWriteFileNum)
		newEnd -= BackendOffset(newWritePos)
		newWriteFileNum--
		if newWriteFileNum < 0 {
			nsqLog.Logf("reset write acrossed the begin %v, %v, %v", offset, newEnd, newWriteFileNum)
			return d.diskWriteEnd, ErrInvalidOffset
		}
		f, err := os.Stat(d.fileName(newWriteFileNum))
		if err != nil {
			nsqLog.LogErrorf("stat data file error %v, %v", offset, newWriteFileNum)
			return d.diskWriteEnd, err
		}
		newWritePos = f.Size()
	}
	d.diskWriteEnd.EndOffset.FileNum = newWriteFileNum
	newWritePos -= int64(newEnd - offset)
	d.diskWriteEnd.EndOffset.Pos = newWritePos
	d.diskWriteEnd.virtualEnd = offset
	atomic.StoreInt64(&d.diskWriteEnd.totalMsgCnt, int64(totalCnt))
	d.diskReadEnd = d.diskWriteEnd
	d.closeCurrentFile()
	nsqLog.Logf("reset write end result : %v", d.diskWriteEnd)
	d.truncateDiskQueueToWriteEnd()

	return d.diskWriteEnd, nil
}

// Close cleans up the queue and persists metadata
func (d *diskQueueWriter) Close() error {
	return d.exit(false)
}

func (d *diskQueueWriter) Delete() error {
	return d.exit(true)
}

func (d *diskQueueWriter) RemoveTo(destPath string) error {
	d.Lock()
	defer d.Unlock()
	d.exitFlag = 1
	nsqLog.Logf("DISKQUEUE(%s): removing to %v", d.name, destPath)
	d.sync()
	d.closeCurrentFile()
	d.saveFileOffsetMeta()
	for i := int64(0); i <= d.diskWriteEnd.EndOffset.FileNum; i++ {
		fn := d.fileName(i)
		destFile := GetQueueFileName(destPath, d.name, i)
		innerErr := util.AtomicRename(fn, destFile)
		nsqLog.Logf("DISKQUEUE(%s): renamed data file %v to %v", d.name, fn, destFile)
		if innerErr != nil && !os.IsNotExist(innerErr) {
			nsqLog.LogErrorf("diskqueue(%s) failed to remove data file - %s", d.name, innerErr)
		}
		fName := d.fileName(i) + ".offsetmeta.dat"
		innerErr = util.AtomicRename(fName, destFile+".offsetmeta.dat")
		nsqLog.Logf("DISKQUEUE(%s): rename offset meta file %v ", d.name, fName)
		if innerErr != nil && !os.IsNotExist(innerErr) {
			nsqLog.LogErrorf("diskqueue(%s) failed to remove offset meta file %v - %s", d.name, fName, innerErr)
		}
	}
	d.diskWriteEnd.EndOffset.FileNum++
	d.diskWriteEnd.EndOffset.Pos = 0
	d.diskReadEnd = d.diskWriteEnd
	destFile := fmt.Sprintf(path.Join(destPath, "%s.diskqueue.meta.writer.dat"), d.name)
	nsqLog.Logf("DISKQUEUE(%s): rename meta file to %v", d.name, destFile)
	innerErr := util.AtomicRename(d.metaDataFileName(), destFile)
	if innerErr != nil && !os.IsNotExist(innerErr) {
		nsqLog.LogErrorf("diskqueue(%s) failed to remove metadata file - %s", d.name, innerErr)
		return innerErr
	}
	destFile = fmt.Sprintf(path.Join(destPath, "%s.diskqueue.meta.extra.dat"), d.name)
	util.AtomicRename(d.extraMetaFileName(), destFile)
	return nil
}

func (d *diskQueueWriter) exit(deleted bool) error {
	d.Lock()
	defer d.Unlock()

	d.exitFlag = 1

	if deleted {
		nsqLog.Logf("DISKQUEUE(%s): deleting", d.name)
	} else {
		nsqLog.Logf("DISKQUEUE(%s): closing", d.name)
	}

	d.sync()
	if deleted {
		return d.deleteAllFiles(deleted)
	}
	return nil
}

// Empty destructively clears out any pending data in the queue
// by fast forwarding read positions and removing intermediate files
func (d *diskQueueWriter) Empty() error {
	d.Lock()
	defer d.Unlock()

	if d.exitFlag == 1 {
		return errors.New("exiting")
	}

	nsqLog.Logf("DISKQUEUE(%s): emptying", d.name)
	return d.deleteAllFiles(false)
}

func (d *diskQueueWriter) deleteAllFiles(deleted bool) error {
	d.cleanOldData()

	if deleted {
		nsqLog.Logf("DISKQUEUE(%s): deleting meta file", d.name)
		innerErr := os.Remove(d.metaDataFileName())
		if innerErr != nil && !os.IsNotExist(innerErr) {
			nsqLog.LogErrorf("diskqueue(%s) failed to remove metadata file - %s", d.name, innerErr)
			return innerErr
		}
		cleanStartFileNum := d.diskQueueStart.EndOffset.FileNum - MAX_QUEUE_OFFSET_META_DATA_KEEP - 1
		if cleanStartFileNum < 0 {
			cleanStartFileNum = 0
		}
		os.Remove(d.extraMetaFileName())
		for i := cleanStartFileNum; i <= d.diskWriteEnd.EndOffset.FileNum; i++ {
			fName := d.fileName(i) + ".offsetmeta.dat"
			innerErr := os.Remove(fName)
			nsqLog.Logf("DISKQUEUE(%s): removed offset meta file: %v", d.name, fName)
			if innerErr != nil && !os.IsNotExist(innerErr) {
				nsqLog.LogErrorf("diskqueue(%s) failed to remove offset meta file %v - %s", d.name, fName, innerErr)
			}
		}
	}
	return nil
}

func (d *diskQueueWriter) cleanOldData() error {

	d.closeCurrentFile()

	d.saveFileOffsetMeta()

	cleanStartFileNum := d.diskQueueStart.EndOffset.FileNum - MAX_QUEUE_OFFSET_META_DATA_KEEP - 1
	if cleanStartFileNum < 0 {
		cleanStartFileNum = 0
	}
	for i := cleanStartFileNum; i <= d.diskWriteEnd.EndOffset.FileNum; i++ {
		fn := d.fileName(i)
		innerErr := os.Remove(fn)
		nsqLog.Logf("DISKQUEUE(%s): removed data file: %v", d.name, fn)
		if innerErr != nil && !os.IsNotExist(innerErr) {
			nsqLog.LogErrorf("diskqueue(%s) failed to remove data file - %s", d.name, innerErr)
		}
	}

	d.diskWriteEnd.EndOffset.FileNum++
	d.diskWriteEnd.EndOffset.Pos = 0
	d.diskReadEnd = d.diskWriteEnd
	d.diskQueueStart = d.diskWriteEnd
	d.saveExtraMeta()
	return nil
}

func (d *diskQueueWriter) saveFileOffsetMeta() {
	fName := d.fileName(d.diskWriteEnd.EndOffset.FileNum) + ".offsetmeta.dat"
	f, err := os.OpenFile(fName, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		nsqLog.LogErrorf("diskqueue(%s) failed to save data offset meta: %v", d.name, err)
		return
	}
	_, err = fmt.Fprintf(f, "%d\n%d,%d\n",
		atomic.LoadInt64(&d.diskWriteEnd.totalMsgCnt),
		d.diskWriteEnd.Offset()-BackendOffset(d.diskWriteEnd.EndOffset.Pos), d.diskWriteEnd.Offset())
	if err != nil {
		f.Close()
		nsqLog.LogErrorf("diskqueue(%s) failed to save data offset meta: %v", d.name, err)
		return
	}
	f.Sync()
	f.Close()
}

func (d *diskQueueWriter) GetQueueWriteEnd() BackendQueueEnd {
	d.RLock()
	e := &diskQueueEndInfo{}
	*e = d.diskWriteEnd
	d.RUnlock()
	return e
}

func (d *diskQueueWriter) GetQueueReadStart() BackendQueueEnd {
	d.RLock()
	e := &diskQueueEndInfo{}
	*e = d.diskQueueStart
	d.RUnlock()
	return e
}

func (d *diskQueueWriter) GetQueueReadEnd() BackendQueueEnd {
	d.RLock()
	e := d.internalGetQueueReadEnd()
	d.RUnlock()
	return e
}

func (d *diskQueueWriter) internalGetQueueReadEnd() *diskQueueEndInfo {
	e := &diskQueueEndInfo{}
	*e = d.diskReadEnd
	return e
}

// writeOne performs a low level filesystem write for a single []byte
// while advancing write positions and rolling files, if necessary
func (d *diskQueueWriter) writeOne(data []byte, isRaw bool, msgCnt int32) (BackendOffset, int32, *diskQueueEndInfo, error) {
	var err error

	if d.writeFile == nil {
		curFileName := d.fileName(d.diskWriteEnd.EndOffset.FileNum)
		d.writeFile, err = os.OpenFile(curFileName, os.O_RDWR|os.O_CREATE, 0644)
		if err != nil {
			return 0, 0, nil, err
		}

		nsqLog.Logf("DISKQUEUE(%s): writeOne() opened %s", d.name, curFileName)

		if d.diskWriteEnd.EndOffset.Pos > 0 {
			_, err = d.writeFile.Seek(d.diskWriteEnd.EndOffset.Pos, 0)
			if err != nil {
				d.writeFile.Close()
				d.writeFile = nil
				return 0, 0, nil, err
			}
		}
		if d.bufferWriter == nil {
			d.bufferWriter = bufio.NewWriterSize(d.writeFile, writeBufSize)
		} else {
			d.bufferWriter.Reset(d.writeFile)
		}
	}

	dataLen := int32(len(data))
	if !isRaw {
		if dataLen < d.minMsgSize || dataLen > d.maxMsgSize {
			return 0, 0, nil, fmt.Errorf("invalid message write size (%d) maxMsgSize=%d", dataLen, d.maxMsgSize)
		}

		err = binary.Write(d.bufferWriter, binary.BigEndian, dataLen)
		if err != nil {
			d.sync()
			if d.writeFile != nil {
				d.writeFile.Close()
				d.writeFile = nil
			}
			nsqLog.Logf("DISKQUEUE(%s): writeOne() faled %s", d.name, err)
			return 0, 0, nil, err
		}
	}
	_, err = d.bufferWriter.Write(data)
	if err != nil {
		d.sync()
		if d.writeFile != nil {
			d.writeFile.Close()
			d.writeFile = nil
		}
		nsqLog.Logf("DISKQUEUE(%s): writeOne() faled %s", d.name, err)
		return 0, 0, nil, err
	}

	writeOffset := d.diskWriteEnd.Offset()
	totalBytes := int64(dataLen)
	if !isRaw {
		totalBytes += 4
	}
	d.diskWriteEnd.EndOffset.Pos += totalBytes
	d.diskWriteEnd.virtualEnd += BackendOffset(totalBytes)
	if !isRaw {
		atomic.AddInt64(&d.diskWriteEnd.totalMsgCnt, 1)
	} else {
		atomic.AddInt64(&d.diskWriteEnd.totalMsgCnt, int64(msgCnt))
	}

	if d.diskWriteEnd.EndOffset.Pos >= d.maxBytesPerFile {
		// sync every time we start writing to a new file
		err = d.sync()
		if err != nil {
			nsqLog.LogErrorf("diskqueue(%s) failed to sync - %s", d.name, err)
		}

		if d.writeFile != nil {
			d.writeFile.Close()
			d.writeFile = nil
		}
		d.saveFileOffsetMeta()
		nsqLog.LogDebugf("DISKQUEUE(%s): new file write, last file: %v", d.name, d.diskWriteEnd)

		d.diskWriteEnd.EndOffset.FileNum++
		d.diskWriteEnd.EndOffset.Pos = 0
		d.diskReadEnd = d.diskWriteEnd
	}

	return writeOffset, int32(totalBytes), &d.diskWriteEnd, err
}

func (d *diskQueueWriter) Flush() error {
	d.Lock()
	defer d.Unlock()

	if d.exitFlag == 1 {
		return errors.New("exiting")
	}
	s := time.Now()
	if d.needSync {
		return d.sync()
	}
	cost := time.Now().Sub(s)
	if cost > time.Second {
		nsqLog.Logf("disk writer(%s): flush cost: %v", d.name, cost)
	}

	return nil
}

func (d *diskQueueWriter) FlushBuffer() bool {
	hasData := false
	d.Lock()
	if d.bufferWriter != nil && d.bufferWriter.Buffered() > 0 {
		hasData = true
		d.bufferWriter.Flush()
		if d.diskReadEnd.EndOffset.GreatThan(&d.diskWriteEnd.EndOffset) {
			nsqLog.LogWarningf("DISKQUEUE(%s): old read is greater: %v, %v", d.name,
				d.diskReadEnd, d.diskWriteEnd)
		}
		d.diskReadEnd = d.diskWriteEnd
	}
	d.Unlock()
	return hasData
}

// sync fsyncs the current writeFile and persists metadata
func (d *diskQueueWriter) sync() error {
	if d.bufferWriter != nil {
		d.bufferWriter.Flush()
	}
	if d.writeFile != nil {
		err := d.writeFile.Sync()
		if err != nil {
			d.writeFile.Close()
			d.writeFile = nil
			return err
		}
	}

	if d.diskReadEnd.EndOffset.GreatThan(&d.diskWriteEnd.EndOffset) {
		nsqLog.LogWarningf("DISKQUEUE(%s): old read is greater: %v, %v", d.name,
			d.diskReadEnd, d.diskWriteEnd)
	}

	d.diskReadEnd = d.diskWriteEnd

	err := d.persistMetaData()
	if err != nil {
		return err
	}

	d.needSync = false
	return nil
}

func (d *diskQueueWriter) initQueueReadStart() error {
	// first try read from meta file
	err := d.loadExtraMeta()
	if err != nil {
		nsqLog.Infof("failed to load extra meta from file: %v", err)
	} else {
		return nil
	}
	var readStart diskQueueEndInfo
	needFix := false
	for {
		curFile := d.fileName(readStart.EndOffset.FileNum)
		_, err := os.Stat(curFile)
		if err != nil {
			needFix = true
			if os.IsNotExist(err) {
				if d.diskWriteEnd.EndOffset.FileNum == int64(0) &&
					d.diskWriteEnd.EndOffset.Pos == int64(0) {
					// init with empty
					return nil
				}
				readStart.EndOffset.FileNum++
				readStart.EndOffset.Pos = 0
				if readStart.EndOffset.FileNum > d.diskWriteEnd.EndOffset.FileNum {
					nsqLog.Errorf("topic %v no data file found to end: %v, reset read start to end", d.name, d.diskWriteEnd)
					d.diskQueueStart = d.diskWriteEnd
					return ErrNeedFixQueueStart
				}
			} else {
				return err
			}
		} else if needFix {
			_, _, _, err := getQueueFileOffsetMeta(d.fileName(readStart.EndOffset.FileNum - 1))
			if err != nil {
				if os.IsNotExist(err) {
					readStart.EndOffset.FileNum++
					readStart.EndOffset.Pos = 0
					if readStart.EndOffset.FileNum > d.diskWriteEnd.EndOffset.FileNum {
						nsqLog.Errorf("topic %v no data meta file found to end: %v, reset read start to end", d.name, d.diskWriteEnd)
						d.diskQueueStart = d.diskWriteEnd
						return ErrNeedFixQueueStart
					}
				} else {
					return err
				}
			} else {
				break
			}
		} else {
			break
		}
	}
	if !needFix {
		if readStart.EndOffset.FileNum != 0 {
			nsqLog.Warningf("topic : %v not start as 0 but no fix : %v", d.name, readStart)
		}
		d.diskQueueStart = readStart
	} else {
		cnt, _, endPos, err := getQueueFileOffsetMeta(d.fileName(readStart.EndOffset.FileNum - 1))
		if err != nil {
			return err
		}
		readStart.virtualEnd = BackendOffset(endPos)
		readStart.totalMsgCnt = cnt
		d.diskQueueStart = readStart
		nsqLog.Logf("%v init the disk queue start: %v", d.name, d.diskQueueStart)
	}
	return nil
}

func (d *diskQueueWriter) loadExtraMeta() error {
	fileName := d.extraMetaFileName()
	data, err := ioutil.ReadFile(fileName)
	if err != nil {
		return err
	}
	var tmp extraMeta
	err = json.Unmarshal(data, &tmp)
	if err != nil {
		return err
	}
	d.diskQueueStart.EndOffset = tmp.SegOffset
	d.diskQueueStart.virtualEnd = tmp.VirtualOffset
	d.diskQueueStart.totalMsgCnt = tmp.TotalMsgCnt
	nsqLog.Infof("topic : %v load extra meta: %v", d.name, tmp)
	return nil
}

func (d *diskQueueWriter) saveExtraMeta() error {
	var err error

	fileName := d.extraMetaFileName()
	tmpFileName := fmt.Sprintf("%s.%d.tmp", fileName, rand.Int())

	var tmp extraMeta
	tmp.SegOffset = d.diskQueueStart.EndOffset
	tmp.VirtualOffset = d.diskQueueStart.Offset()
	tmp.TotalMsgCnt = d.diskQueueStart.TotalMsgCnt()
	data, _ := json.Marshal(tmp)
	err = ioutil.WriteFile(tmpFileName, data, 0644)
	if err != nil {
		return err
	}
	// atomically rename
	return util.AtomicRename(tmpFileName, fileName)
}

// retrieveMetaData initializes state from the filesystem
func (d *diskQueueWriter) retrieveMetaData() error {
	var f *os.File
	var err error

	fileName := d.metaDataFileName()
	f, err = os.OpenFile(fileName, os.O_RDONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	var totalCnt int64
	_, err = fmt.Fscanf(f, "%d\n%d,%d,%d\n",
		&totalCnt,
		&d.diskWriteEnd.EndOffset.FileNum, &d.diskWriteEnd.EndOffset.Pos, &d.diskWriteEnd.virtualEnd)
	if err != nil {
		return err
	}
	atomic.StoreInt64(&d.diskWriteEnd.totalMsgCnt, totalCnt)
	d.diskReadEnd = d.diskWriteEnd

	return nil
}

// persistMetaData atomically writes state to the filesystem
func (d *diskQueueWriter) persistMetaData() error {
	var f *os.File
	var err error

	fileName := d.metaDataFileName()
	tmpFileName := fmt.Sprintf("%s.%d.tmp", fileName, rand.Int())

	// write to tmp file
	f, err = os.OpenFile(tmpFileName, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(f, "%d\n%d,%d,%d\n",
		atomic.LoadInt64(&d.diskWriteEnd.totalMsgCnt),
		d.diskWriteEnd.EndOffset.FileNum, d.diskWriteEnd.EndOffset.Pos, d.diskWriteEnd.Offset())
	if err != nil {
		f.Close()
		return err
	}
	f.Sync()
	f.Close()

	// atomically rename
	return util.AtomicRename(tmpFileName, fileName)
}

func (d *diskQueueWriter) metaDataFileName() string {
	return fmt.Sprintf(path.Join(d.dataPath, "%s.diskqueue.meta.writer.dat"), d.name)
}

func (d *diskQueueWriter) fileName(fileNum int64) string {
	return GetQueueFileName(d.dataPath, d.name, fileNum)
}

func (d *diskQueueWriter) extraMetaFileName() string {
	return fmt.Sprintf(path.Join(d.dataPath, "%s.diskqueue.meta.extra.dat"), d.name)
}
