package nsqd

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/absolute8511/bolt"
	"github.com/youzan/nsq/internal/levellogger"
)

var (
	syncedOffsetKey       = []byte("synced_offset")
	bucketDelayedMsg      = []byte("delayed_message")
	bucketDelayedMsgIndex = []byte("delayed_message_index")
	bucketMeta            = []byte("meta")
	CompactThreshold      = 1024 * 1024 * 16
	errBucketKeyNotFound  = errors.New("bucket key not found")
	txMaxBatch            = 10000
)

const (
	MinDelayedType      = 1
	ChannelDelayed      = 1
	PubDelayed          = 2
	TransactionDelayed  = 3
	MaxDelayedType      = 4
	TxMaxSize           = 65536
	CompactCntThreshold = 20000
)

type RecentKeyList [][]byte

func writeDelayedMessageToBackendWithCheck(buf *bytes.Buffer, msg *Message,
	checkSize int64, bq *diskQueueWriter,
	isExt bool) (BackendOffset, int32, diskQueueEndInfo, error) {
	buf.Reset()
	wsize, err := msg.WriteDelayedTo(buf, isExt)
	if err != nil {
		return 0, 0, diskQueueEndInfo{}, err
	}
	if checkSize > 0 && checkSize != wsize+4 {
		return 0, 0, diskQueueEndInfo{}, fmt.Errorf("write message size mismatch: %v vs %v", checkSize, wsize+4)
	}
	return bq.PutV2(buf.Bytes())
}

func IsValidDelayedMessage(m *Message) bool {
	if m.DelayedType == ChannelDelayed {
		return m.DelayedOrigID > 0 && len(m.DelayedChannel) > 0 && m.DelayedTs > 0
	} else if m.DelayedType == PubDelayed {
		return m.DelayedTs > 0
	} else if m.DelayedType == TransactionDelayed {
		return true
	}
	return false
}

func getDelayedMsgDBIndexValue(ts int64, id MessageID) []byte {
	d := make([]byte, 1+8+8)
	pos := 0
	d[0] = byte(1)
	pos++
	binary.BigEndian.PutUint64(d[pos:pos+8], uint64(ts))
	pos += 8
	binary.BigEndian.PutUint64(d[pos:pos+8], uint64(id))
	return d
}

func getDelayedMsgDBKey(dt int, ch string, ts int64, id MessageID) []byte {
	msgKey := make([]byte, len(ch)+2+1+2+8+8)
	binary.BigEndian.PutUint16(msgKey[:2], uint16(dt))
	pos := 2
	msgKey[pos] = '-'
	pos++
	binary.BigEndian.PutUint16(msgKey[pos:pos+2], uint16(len(ch)))
	pos += 2
	copy(msgKey[pos:pos+len(ch)], []byte(ch))
	pos += len(ch)
	binary.BigEndian.PutUint64(msgKey[pos:pos+8], uint64(ts))
	pos += 8
	binary.BigEndian.PutUint64(msgKey[pos:pos+8], uint64(id))
	return msgKey
}

func decodeDelayedMsgDBKey(b []byte) (uint16, int64, MessageID, string, error) {
	if len(b) < 2+1+2+8+8 {
		return 0, 0, 0, "", errors.New("invalid buffer length")
	}
	dt := binary.BigEndian.Uint16(b[:2])
	pos := 2
	pos++
	chLen := int(binary.BigEndian.Uint16(b[pos : pos+2]))
	pos += 2
	if len(b) < pos+chLen {
		return 0, 0, 0, "", errors.New("invalid buffer length")
	}
	ch := b[pos : pos+chLen]
	pos += chLen
	ts := int64(binary.BigEndian.Uint64(b[pos : pos+8]))
	pos += 8
	id := int64(binary.BigEndian.Uint64(b[pos : pos+8]))
	return dt, ts, MessageID(id), string(ch), nil
}

func getDelayedMsgDBIndexKey(dt int, ch string, id MessageID) []byte {
	msgKey := make([]byte, len(ch)+2+1+2+8)
	binary.BigEndian.PutUint16(msgKey[:2], uint16(dt))
	pos := 2
	msgKey[pos] = '-'
	pos++
	binary.BigEndian.PutUint16(msgKey[pos:pos+2], uint16(len(ch)))
	pos += 2
	copy(msgKey[pos:pos+len(ch)], []byte(ch))
	pos += len(ch)
	binary.BigEndian.PutUint64(msgKey[pos:pos+8], uint64(id))
	return msgKey
}

func decodeDelayedMsgDBIndexKey(b []byte) (uint16, MessageID, string, error) {
	if len(b) < 2+1+2+8 {
		return 0, 0, "", errors.New("invalid buffer length")
	}
	dt := binary.BigEndian.Uint16(b[:2])
	pos := 2
	pos++
	chLen := int(binary.BigEndian.Uint16(b[pos : pos+2]))
	pos += 2
	if len(b) < pos+chLen {
		return 0, 0, "", errors.New("invalid buffer length")
	}
	ch := b[pos : pos+chLen]
	pos += chLen
	id := int64(binary.BigEndian.Uint64(b[pos : pos+8]))
	return dt, MessageID(id), string(ch), nil
}

func getDelayedMsgDBPrefixKey(dt int, ch string) []byte {
	msgKey := make([]byte, len(ch)+2+1+2)
	binary.BigEndian.PutUint16(msgKey[:2], uint16(dt))
	pos := 2
	msgKey[pos] = '-'
	pos++
	binary.BigEndian.PutUint16(msgKey[pos:pos+2], uint16(len(ch)))
	pos += 2
	copy(msgKey[pos:pos+len(ch)], []byte(ch))
	return msgKey
}

func deleteMsgIndex(msgData []byte, tx *bolt.Tx, isExt bool) error {
	m, err := DecodeDelayedMessage(msgData, isExt)
	if err != nil {
		nsqLog.LogErrorf("failed to decode delayed message: %v, %v", msgData, err)
		return err
	}
	msgIndexKey := getDelayedMsgDBIndexKey(int(m.DelayedType), m.DelayedChannel, m.DelayedOrigID)
	b := tx.Bucket(bucketDelayedMsgIndex)
	err = b.Delete(msgIndexKey)
	if err != nil {
		nsqLog.Infof("failed to delete delayed index : %v", msgIndexKey)
		return err
	}
	return nil
}

func deleteBucketKey(dt int, ch string, ts int64, id MessageID, tx *bolt.Tx, isExt bool) error {
	b := tx.Bucket(bucketDelayedMsg)
	msgKey := getDelayedMsgDBKey(dt, ch, ts, id)
	oldV := b.Get(msgKey)
	err := b.Delete(msgKey)
	if err != nil {
		nsqLog.Infof("failed to delete delayed message: %v", msgKey)
		return err
	}
	if oldV != nil {
		err = deleteMsgIndex(oldV, tx, isExt)
		if err != nil {
			return err
		}

		b = tx.Bucket(bucketMeta)
		cntKey := append([]byte("counter_"), getDelayedMsgDBPrefixKey(dt, ch)...)
		cnt := uint64(0)
		cntBytes := b.Get(cntKey)
		if cntBytes != nil && len(cntBytes) == 8 {
			cnt = binary.BigEndian.Uint64(cntBytes)
		}
		if cnt > 0 {
			cnt--
			cntBytes = make([]byte, 8)
			binary.BigEndian.PutUint64(cntBytes[:8], cnt)
			err = b.Put(cntKey, cntBytes)
			if err != nil {
				nsqLog.Infof("failed to update the meta count: %v, %v", cntKey, err)
				return err
			}
		}
	} else {
		nsqLog.Infof("failed to get the deleting delayed message: %v", msgKey)
		return errBucketKeyNotFound
	}
	return nil
}

type DelayQueue struct {
	tname     string
	fullName  string
	partition int
	backend   *diskQueueWriter
	dataPath  string
	exitFlag  int32

	msgIDCursor  MsgIDGenerator
	defaultIDSeq uint64

	needFlush   int32
	putBuffer   bytes.Buffer
	kvStore     *bolt.DB
	EnableTrace int32
	SyncEvery   int64
	lastSyncCnt int64
	needFixData int32
	isExt       int32
	dbLock      sync.Mutex
	// prevent write while compact db
	compactMutex           sync.Mutex
	oldestChannelDelayedTs map[string]int64
	oldestMutex            sync.Mutex
}

func NewDelayQueueForRead(topicName string, part int, dataPath string, opt *Options,
	idGen MsgIDGenerator, isExt bool) (*DelayQueue, error) {
	ro := &bolt.Options{
		Timeout:  time.Second,
		ReadOnly: true,
	}
	return newDelayQueue(topicName, part, dataPath, opt, idGen, isExt, ro)
}

func NewDelayQueue(topicName string, part int, dataPath string, opt *Options,
	idGen MsgIDGenerator, isExt bool) (*DelayQueue, error) {

	return newDelayQueue(topicName, part, dataPath, opt, idGen, isExt, nil)
}
func newDelayQueue(topicName string, part int, dataPath string, opt *Options,
	idGen MsgIDGenerator, isExt bool, ro *bolt.Options) (*DelayQueue, error) {
	dataPath = path.Join(dataPath, "delayed_queue")
	os.MkdirAll(dataPath, 0755)
	q := &DelayQueue{
		tname:                  topicName,
		partition:              part,
		putBuffer:              bytes.Buffer{},
		dataPath:               dataPath,
		msgIDCursor:            idGen,
		oldestChannelDelayedTs: make(map[string]int64),
	}
	if isExt {
		q.isExt = 1
	}
	q.fullName = GetTopicFullName(q.tname, q.partition)
	backendName := getDelayQueueBackendName(q.tname, q.partition)
	// max delay message size need add the delay ts and channel name
	queue, err := NewDiskQueueWriter(backendName,
		q.dataPath,
		opt.MaxBytesPerFile,
		int32(minValidMsgLength),
		int32(opt.MaxMsgSize)+minValidMsgLength+8+255, 0)

	if err != nil {
		nsqLog.LogErrorf("topic(%v) failed to init delayed disk queue: %v , %v ", q.fullName, err, backendName)
		return nil, err
	}
	q.backend = queue.(*diskQueueWriter)
	if ro == nil {
		ro = &bolt.Options{
			Timeout:  time.Second,
			ReadOnly: false,
		}
	}
	q.kvStore, err = bolt.Open(path.Join(q.dataPath, getDelayQueueDBName(q.tname, q.partition)), 0644, ro)
	if err != nil {
		nsqLog.LogErrorf("topic(%v) failed to init delayed db: %v , %v ", q.fullName, err, backendName)
		return nil, err
	}
	q.kvStore.NoSync = true
	err = q.kvStore.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketDelayedMsg)
		if err != nil {
			return err
		}
		_, err = tx.CreateBucketIfNotExists(bucketDelayedMsgIndex)
		if err != nil {
			return err
		}
		_, err = tx.CreateBucketIfNotExists(bucketMeta)
		return err
	})
	if err != nil {
		nsqLog.LogErrorf("topic(%v) failed to init delayed db: %v , %v ", q.fullName, err, backendName)
		if ro == nil || !ro.ReadOnly {
			return nil, err
		}
	}

	return q, nil
}

func (q *DelayQueue) CheckConsistence() error {
	// Perform consistency check.
	return q.getStore().View(func(tx *bolt.Tx) error {
		var count int
		ch := tx.Check()
		done := false
		for !done {
			select {
			case err, ok := <-ch:
				if !ok {
					done = true
					break
				}
				nsqLog.LogErrorf("topic(%v) failed to check delayed db: %v ", q.fullName, err)
				if err != nil && strings.Contains(err.Error(), "unreachable unfreed") {
					continue
				}
				count++
			}
		}

		if count > 0 {
			nsqLog.LogErrorf("topic(%v) failed to check delayed db, %d errors found ", q.fullName, count)
			return errors.New("boltdb file corrupt")
		}
		return nil
	})
}

func (q *DelayQueue) reOpenStore() error {
	var err error
	ro := &bolt.Options{
		Timeout:  time.Second,
		ReadOnly: false,
	}
	q.kvStore, err = bolt.Open(path.Join(q.dataPath, getDelayQueueDBName(q.tname, q.partition)), 0644, ro)
	if err != nil {
		nsqLog.LogErrorf("topic(%v) failed to open delayed db: %v ", q.fullName, err)
		return err
	}

	q.oldestMutex.Lock()
	q.oldestChannelDelayedTs = make(map[string]int64)
	q.oldestMutex.Unlock()

	q.kvStore.NoSync = true
	err = q.kvStore.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketDelayedMsg)
		if err != nil {
			return err
		}
		_, err = tx.CreateBucketIfNotExists(bucketDelayedMsgIndex)
		if err != nil {
			return err
		}

		_, err = tx.CreateBucketIfNotExists(bucketMeta)
		return err
	})
	if err != nil {
		nsqLog.LogErrorf("topic(%v) failed to init delayed db: %v", q.fullName, err)
		return err
	}

	return nil
}

func (q *DelayQueue) getStore() *bolt.DB {
	q.dbLock.Lock()
	d := q.kvStore
	q.dbLock.Unlock()
	return d
}

func (q *DelayQueue) GetFullName() string {
	return q.fullName
}

func (q *DelayQueue) GetTopicName() string {
	return q.tname
}

func (q *DelayQueue) GetTopicPart() int {
	return q.partition
}

func (q *DelayQueue) SetTrace(enable bool) {
	if enable {
		atomic.StoreInt32(&q.EnableTrace, 1)
	} else {
		atomic.StoreInt32(&q.EnableTrace, 0)
	}
}

func (q *DelayQueue) setExt() {
	atomic.StoreInt32(&q.isExt, 1)
}

func (q *DelayQueue) IsExt() bool {
	return atomic.LoadInt32(&q.isExt) == 1
}

func (q *DelayQueue) nextMsgID() MessageID {
	id := uint64(0)
	if q.msgIDCursor != nil {
		id = q.msgIDCursor.NextID()
	} else {
		id = atomic.AddUint64(&q.defaultIDSeq, 1)
	}
	return MessageID(id)
}

func (q *DelayQueue) RollbackNoLock(vend BackendOffset, diffCnt uint64) error {
	old := q.backend.GetQueueWriteEnd()
	nsqLog.Logf("reset the backend from %v to : %v, %v", old, vend, diffCnt)
	_, err := q.backend.RollbackWriteV2(vend, diffCnt)
	atomic.StoreInt32(&q.needFlush, 1)
	return err
}

func (q *DelayQueue) ResetBackendEndNoLock(vend BackendOffset, totalCnt int64) error {
	old := q.backend.GetQueueWriteEnd()
	if old.Offset() == vend && old.TotalMsgCnt() == totalCnt {
		return nil
	}
	nsqLog.Logf("topic %v reset the backend from %v to : %v, %v", q.GetFullName(), old, vend, totalCnt)
	_, err := q.backend.ResetWriteEndV2(vend, totalCnt)
	if err != nil {
		nsqLog.LogErrorf("topic %v reset backend to %v error: %v", q.fullName, vend, err)
	}
	atomic.StoreInt32(&q.needFlush, 1)
	return err
}

func (q *DelayQueue) ResetBackendWithQueueStartNoLock(queueStartOffset int64, queueStartCnt int64) error {
	if queueStartOffset < 0 || queueStartCnt < 0 {
		return errors.New("queue start should not less than 0")
	}
	queueStart := q.backend.GetQueueWriteEnd().(*diskQueueEndInfo)
	queueStart.virtualEnd = BackendOffset(queueStartOffset)
	queueStart.totalMsgCnt = queueStartCnt
	nsqLog.Warningf("reset the topic %v backend with queue start: %v", q.GetFullName(), queueStart)
	err := q.backend.ResetWriteWithQueueStart(queueStart)
	if err != nil {
		return err
	}
	atomic.StoreInt32(&q.needFlush, 1)
	return nil
}

func (q *DelayQueue) GetDiskQueueSnapshot() *DiskQueueSnapshot {
	e := q.backend.GetQueueReadEnd()
	start := q.backend.GetQueueReadStart()
	d := NewDiskQueueSnapshot(getDelayQueueBackendName(q.tname, q.partition), q.dataPath, e)
	d.SetQueueStart(start)
	return d
}

func (q *DelayQueue) IsDataNeedFix() bool {
	return atomic.LoadInt32(&q.needFixData) == 1
}

func (q *DelayQueue) SetDataFixState(needFix bool) {
	if needFix {
		atomic.StoreInt32(&q.needFixData, 1)
	} else {
		atomic.StoreInt32(&q.needFixData, 0)
	}
}

func (q *DelayQueue) TotalMessageCnt() uint64 {
	return uint64(q.backend.GetQueueWriteEnd().TotalMsgCnt())
}

func (q *DelayQueue) TotalDataSize() int64 {
	e := q.backend.GetQueueWriteEnd()
	if e == nil {
		return 0
	}
	return int64(e.Offset())
}

func (q *DelayQueue) GetDBSize() (int64, error) {
	totalSize := int64(0)
	err := q.getStore().View(func(tx *bolt.Tx) error {
		totalSize = tx.Size()
		return nil
	})
	return totalSize, err
}

func (q *DelayQueue) BackupKVStoreTo(w io.Writer) (int64, error) {
	totalSize := int64(0)
	err := q.getStore().View(func(tx *bolt.Tx) error {
		buf := make([]byte, 8)
		binary.BigEndian.PutUint64(buf, uint64(tx.Size()))
		_, err := w.Write(buf)
		if err != nil {
			return err
		}
		totalSize = tx.Size() + 8
		_, err = tx.WriteTo(w)
		return err
	})
	return totalSize, err
}

func (q *DelayQueue) RestoreKVStoreFrom(body io.Reader) error {
	buf := make([]byte, 8)
	n, err := body.Read(buf)
	if err != nil {
		return err
	}
	if n != len(buf) {
		return errors.New("unexpected length for body length")
	}
	bodyLen := int64(binary.BigEndian.Uint64(buf))
	tmpPath := path.Join(q.dataPath, getDelayQueueDBName(q.tname, q.partition)+"-tmp.restore")
	err = os.Remove(tmpPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
	}
	f, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	_, err = io.CopyN(f, body, bodyLen)
	if err != nil {
		f.Close()
		return err
	}
	err = f.Sync()
	if err != nil {
		f.Close()
		return err
	}
	err = f.Close()
	if err != nil {
		return err
	}

	q.compactMutex.Lock()
	defer q.compactMutex.Unlock()
	kvPath := path.Join(q.dataPath, getDelayQueueDBName(q.tname, q.partition))
	q.dbLock.Lock()
	defer q.dbLock.Unlock()
	q.kvStore.Close()
	err = os.Rename(tmpPath, kvPath)
	if err != nil {
		return err
	}
	err = q.reOpenStore()
	if err != nil {
		nsqLog.LogErrorf("topic(%v) failed to restore delayed db: %v , %v ", q.fullName, err, kvPath)
		return err
	}
	return nil
}

func (q *DelayQueue) PutDelayMessage(m *Message) (MessageID, BackendOffset, int32, BackendQueueEnd, error) {
	if atomic.LoadInt32(&q.exitFlag) == 1 {
		return 0, 0, 0, nil, errors.New("exiting")
	}
	if m.ID > 0 {
		nsqLog.Logf("should not pass id in message ")
		return 0, 0, 0, nil, ErrInvalidMessageID
	}
	if !IsValidDelayedMessage(m) {
		return 0, 0, 0, nil, errors.New("invalid delayed message")
	}

	id, offset, writeBytes, dend, err := q.put(m, nil, true, 0)
	return id, offset, writeBytes, &dend, err
}

func (q *DelayQueue) PutRawDataOnReplica(rawData []byte, offset BackendOffset, checkSize int64, msgNum int32) (BackendQueueEnd, error) {
	if atomic.LoadInt32(&q.exitFlag) == 1 {
		return nil, ErrExiting
	}
	wend := q.backend.GetQueueWriteEnd()
	if wend.Offset() != offset {
		nsqLog.LogErrorf("topic %v: write offset mismatch: %v, %v", q.GetFullName(), offset, wend)
		return nil, ErrWriteOffsetMismatch
	}
	if msgNum != 1 {
		return nil, errors.New("delayed raw message number must be 1.")
	}
	var m Message
	_, _, _, dend, err := q.put(&m, rawData, false, checkSize)
	if err != nil {
		q.ResetBackendEndNoLock(wend.Offset(), wend.TotalMsgCnt())
		return nil, err
	}
	return &dend, nil
}

func (q *DelayQueue) PutMessageOnReplica(m *Message, offset BackendOffset, checkSize int64) (BackendQueueEnd, error) {
	if atomic.LoadInt32(&q.exitFlag) == 1 {
		return nil, ErrExiting
	}
	wend := q.backend.GetQueueWriteEnd()
	if wend.Offset() != offset {
		nsqLog.LogErrorf("topic %v: write offset mismatch: %v, %v", q.GetFullName(), offset, wend)
		return nil, ErrWriteOffsetMismatch
	}
	if !IsValidDelayedMessage(m) {
		return nil, errors.New("invalid delayed message")
	}
	_, _, _, dend, err := q.put(m, nil, false, checkSize)
	if err != nil {
		q.ResetBackendEndNoLock(wend.Offset(), wend.TotalMsgCnt())
		return nil, err
	}
	return &dend, nil
}

func (q *DelayQueue) put(m *Message, rawData []byte, trace bool, checkSize int64) (MessageID, BackendOffset, int32, diskQueueEndInfo, error) {
	var err error
	var dend diskQueueEndInfo
	// it may happened while the topic is upgraded to extend topic, so the message from leader will be raw.
	if rawData != nil {
		if len(rawData) < 4 {
			return 0, 0, 0, dend, fmt.Errorf("invalid raw message data: %v", rawData)
		}
		m, err = DecodeDelayedMessage(rawData[4:], q.IsExt())
		if err != nil {
			return 0, 0, 0, dend, err
		}
	}
	if m.ID <= 0 {
		m.ID = q.nextMsgID()
	}

	var offset BackendOffset
	var writeBytes int32
	if rawData != nil {
		q.putBuffer.Reset()
		_, err := m.WriteDelayedTo(&q.putBuffer, q.IsExt())
		if err != nil {
			return 0, 0, 0, dend, err
		}
		offset, writeBytes, dend, err = q.backend.PutRawV2(rawData, 1)
		if checkSize > 0 && checkSize != int64(writeBytes) {
			return 0, 0, 0, dend, err
		}
	} else {
		offset, writeBytes, dend, err = writeDelayedMessageToBackendWithCheck(&q.putBuffer,
			m, checkSize, q.backend, q.IsExt())
	}
	atomic.StoreInt32(&q.needFlush, 1)
	if err != nil {
		nsqLog.LogErrorf(
			"TOPIC(%s) : failed to write delayed message to backend - %s",
			q.GetFullName(), err)
		return m.ID, offset, writeBytes, dend, err
	}
	msgKey := getDelayedMsgDBKey(int(m.DelayedType), m.DelayedChannel, m.DelayedTs, m.ID)

	wstart := time.Now()
	q.compactMutex.Lock()
	err = q.getStore().Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketDelayedMsg)
		oldV := b.Get(msgKey)
		exists := oldV != nil
		if exists && bytes.Equal(oldV, q.putBuffer.Bytes()) {
		} else {
			err := b.Put(msgKey, q.putBuffer.Bytes())
			if err != nil {
				return err
			}
			if oldV != nil {
				err = deleteMsgIndex(oldV, tx, q.IsExt())
				if err != nil {
					nsqLog.Infof("failed to delete old delayed index : %v, %v", oldV, err)
					return err
				}
			}
			b = tx.Bucket(bucketDelayedMsgIndex)
			newIndexKey := getDelayedMsgDBIndexKey(int(m.DelayedType), m.DelayedChannel, m.DelayedOrigID)
			d := getDelayedMsgDBIndexValue(m.DelayedTs, m.DelayedOrigID)
			err = b.Put(newIndexKey, d)
			if err != nil {
				return err
			}
		}
		b = tx.Bucket(bucketMeta)
		if !exists {
			cntKey := append([]byte("counter_"), getDelayedMsgDBPrefixKey(int(m.DelayedType), m.DelayedChannel)...)
			cnt := uint64(0)
			cntBytes := b.Get(cntKey)
			if cntBytes != nil && len(cntBytes) == 8 {
				cnt = binary.BigEndian.Uint64(cntBytes)
			}
			cnt++
			cntBytes = make([]byte, 8)

			binary.BigEndian.PutUint64(cntBytes[:8], cnt)
			err = b.Put(cntKey, cntBytes)
			if err != nil {
				return err
			}
		}
		return b.Put(syncedOffsetKey, []byte(strconv.Itoa(int(dend.Offset()))))
	})
	q.compactMutex.Unlock()
	if err != nil {
		nsqLog.LogErrorf(
			"TOPIC(%s) : failed to write delayed message %v to kv store- %s",
			q.GetFullName(), m, err)
		return m.ID, offset, writeBytes, dend, err
	}
	if m.DelayedType == ChannelDelayed {
		q.oldestMutex.Lock()
		oldest, ok := q.oldestChannelDelayedTs[m.DelayedChannel]
		if !ok || oldest == 0 || m.DelayedTs < oldest {
			q.oldestChannelDelayedTs[m.DelayedChannel] = m.DelayedTs
		}
		q.oldestMutex.Unlock()
	}
	if nsqLog.Level() >= levellogger.LOG_DEBUG {
		cost := time.Since(wstart)
		if cost > time.Millisecond*2 {
			nsqLog.Logf("write local delayed queue db cost :%v", cost)
		}
	}
	if trace {
		if m.TraceID != 0 || atomic.LoadInt32(&q.EnableTrace) == 1 || nsqLog.Level() >= levellogger.LOG_DETAIL {
			nsqMsgTracer.TracePub(q.GetTopicName(), q.GetTopicPart(), "DELAY_QUEUE_PUB", m.TraceID, m, offset, dend.TotalMsgCnt())
		}
	}
	syncEvery := atomic.LoadInt64(&q.SyncEvery)
	if syncEvery == 1 ||
		dend.TotalMsgCnt()-atomic.LoadInt64(&q.lastSyncCnt) >= syncEvery {
		q.flush()
	}

	return m.ID, offset, writeBytes, dend, nil
}

func (q *DelayQueue) Delete() error {
	return q.exit(true)
}

func (q *DelayQueue) Close() error {
	return q.exit(false)
}

func (q *DelayQueue) exit(deleted bool) error {
	if !atomic.CompareAndSwapInt32(&q.exitFlag, 0, 1) {
		return errors.New("exiting")
	}

	if deleted {
		q.getStore().Close()
		os.RemoveAll(path.Join(q.dataPath, getDelayQueueDBName(q.tname, q.partition)))
		return q.backend.Delete()
	}

	// write anything leftover to disk
	q.flush()
	q.getStore().Close()
	return q.backend.Close()
}

func (q *DelayQueue) ForceFlush() {
	q.flush()
}

func (q *DelayQueue) flush() error {
	ok := atomic.CompareAndSwapInt32(&q.needFlush, 1, 0)
	if !ok {
		return nil
	}
	s := time.Now()
	atomic.StoreInt64(&q.lastSyncCnt, q.backend.GetQueueWriteEnd().TotalMsgCnt())
	err := q.backend.Flush()
	if err != nil {
		nsqLog.LogErrorf("failed flush: %v", err)
		return err
	}
	q.getStore().Sync()

	cost := time.Now().Sub(s)
	if cost > time.Second {
		nsqLog.Logf("topic(%s): flush cost: %v", q.GetFullName(), cost)
	}

	if nsqLog.Level() >= levellogger.LOG_DEBUG {
		if cost > time.Millisecond*5 {
			nsqLog.Logf("flush local delayed queue db cost :%v", cost)
		}
	}

	return err
}

func (q *DelayQueue) emptyDelayedUntil(dt int, peekTs int64, id MessageID, ch string) error {
	db := q.getStore()
	prefix := getDelayedMsgDBPrefixKey(dt, ch)
	q.compactMutex.Lock()
	defer q.compactMutex.Unlock()
	// to avoid too much in batch, we should empty at most 10000 at each tx
	for {
		batched := 0
		err := db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket(bucketDelayedMsg)
			c := b.Cursor()
			for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
				if batched > txMaxBatch {
					break
				}
				_, delayedTs, delayedID, delayedCh, err := decodeDelayedMsgDBKey(k)
				if err != nil {
					nsqLog.Infof("decode key failed : %v, %v", k, err)
					continue
				}
				if delayedTs > peekTs {
					break
				}
				if delayedTs == peekTs && delayedID >= id {
					break
				}
				if delayedCh != ch {
					continue
				}

				err = deleteBucketKey(dt, ch, delayedTs, delayedID, tx, q.IsExt())
				if err != nil {
					if err != errBucketKeyNotFound {
						nsqLog.Warningf("failed to delete : %v, %v", k, err)
						return err
					}
				}
				batched++
			}
			return nil
		})
		if err != nil {
			return err
		}
		if batched == 0 {
			break
		}
	}
	if dt == ChannelDelayed && ch != "" {
		q.oldestMutex.Lock()
		q.oldestChannelDelayedTs[ch] = peekTs
		if nsqLog.Level() >= levellogger.LOG_DETAIL {
			nsqLog.LogDebugf("channel %v update oldest to %v at time %v",
				ch, peekTs, time.Now().UnixNano())
		}

		q.oldestMutex.Unlock()
	}
	return nil
}

func (q *DelayQueue) emptyAllDelayedType(dt int, ch string) error {
	db := q.getStore()
	prefix := getDelayedMsgDBPrefixKey(dt, ch)
	q.compactMutex.Lock()
	defer q.compactMutex.Unlock()
	for {
		batched := 0
		err := db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket(bucketDelayedMsg)
			c := b.Cursor()
			for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
				if batched > txMaxBatch {
					break
				}
				delayedType, delayedTs, delayedID, delayedCh, err := decodeDelayedMsgDBKey(k)
				if err != nil {
					nsqLog.Infof("decode key failed : %v, %v", k, err)
					continue
				}
				if delayedType != uint16(dt) {
					continue
				}
				if ch != "" && delayedCh != ch {
					continue
				}
				err = deleteBucketKey(int(delayedType), delayedCh, delayedTs, delayedID, tx, q.IsExt())
				if err != nil {
					if err != errBucketKeyNotFound {
						nsqLog.Warningf("failed to delete : %v, %v", k, err)
						return err
					}
				}
				batched++
			}
			return nil
		})
		if err != nil {
			return err
		}
		nsqLog.Infof("channel %v empty batched : %v",
			ch, batched)
		if batched == 0 {
			break
		}
	}
	if dt == ChannelDelayed && ch != "" {
		// no message anymore, oldest as next hour
		q.oldestMutex.Lock()
		q.oldestChannelDelayedTs[ch] = time.Now().Add(time.Hour).UnixNano()
		q.oldestMutex.Unlock()
	}

	return nil
}

func (q *DelayQueue) EmptyDelayedType(dt int) error {
	return q.emptyAllDelayedType(dt, "")
}

func (q *DelayQueue) EmptyDelayedChannel(ch string) error {
	if ch == "" {
		// to avoid empty all channels by accident
		// we do not allow empty channel with empty channel name
		return errors.New("empty delayed channel name should be given")
	}
	return q.emptyAllDelayedType(ChannelDelayed, ch)
}

func (q *DelayQueue) PeekRecentTimeoutWithFilter(results []Message, peekTs int64, filterType int,
	filterChannel string) (int, error) {

	oldest := int64(0)
	if filterType == ChannelDelayed && filterChannel != "" {
		q.oldestMutex.Lock()
		ok := false
		oldest, ok = q.oldestChannelDelayedTs[filterChannel]
		q.oldestMutex.Unlock()
		if ok && oldest > peekTs {
			if nsqLog.Level() > levellogger.LOG_DETAIL {
				nsqLog.LogDebugf("channel %v peek until %v ignored since oldest is %v",
					filterChannel, peekTs, oldest)
			}
			return 0, nil
		}
	}

	db := q.getStore()
	idx := 0
	var prefix []byte
	if filterType > 0 {
		prefix = getDelayedMsgDBPrefixKey(filterType, filterChannel)
		if nsqLog.Level() > levellogger.LOG_DETAIL {
			nsqLog.LogDebugf("peek prefix %v: channel %v", prefix, filterChannel)
		}
	}
	oldest = int64(0)
	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketDelayedMsg)
		c := b.Cursor()
		for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			_, delayedTs, _, delayedCh, err := decodeDelayedMsgDBKey(k)
			if err != nil {
				nsqLog.Infof("decode key failed : %v, %v", k, err)
				continue
			}
			if oldest == 0 && filterType == ChannelDelayed && filterChannel != "" {
				oldest = delayedTs
			}
			if nsqLog.Level() > levellogger.LOG_DETAIL {
				nsqLog.LogDebugf("peek delayed message %v: %v at %v", k, delayedTs, time.Now().UnixNano())
			}

			if delayedTs > peekTs || idx >= len(results) {
				break
			}

			if filterChannel != "" && delayedCh != filterChannel {
				continue
			}

			if v == nil {
				// k is not nil, v is nil, sub bucket?
				nsqLog.LogErrorf("topic %v iterater nil value: %v",
					q.fullName, k)
				continue
			}
			buf := make([]byte, len(v))
			copy(buf, v)
			m, err := DecodeDelayedMessage(buf, q.IsExt())
			if err != nil {
				nsqLog.LogErrorf("topic %v failed to decode delayed message: %v, %v, %v",
					q.fullName, v, k, err)
				continue
			}
			if nsqLog.Level() > levellogger.LOG_DETAIL {
				nsqLog.LogDebugf("peek delayed message %v: %v, %v", k, delayedTs, m)
			}

			if filterType >= 0 && filterType != int(m.DelayedType) {
				continue
			}
			results[idx] = *m
			idx++
		}
		return nil
	})
	if err == nil && oldest > 0 {
		// no message anymore, oldest as next hour
		q.oldestMutex.Lock()
		q.oldestChannelDelayedTs[filterChannel] = oldest
		if nsqLog.Level() >= levellogger.LOG_DETAIL {
			nsqLog.LogDebugf("channel %v update oldest to %v at time %v",
				filterChannel, oldest, time.Now().UnixNano())
		}
		q.oldestMutex.Unlock()
	}
	return idx, err
}

func (q *DelayQueue) PeekRecentChannelTimeout(now int64, results []Message, ch string) (int, error) {
	return q.PeekRecentTimeoutWithFilter(results, now, ChannelDelayed, ch)
}

func (q *DelayQueue) PeekRecentDelayedPub(now int64, results []Message) (int, error) {
	return q.PeekRecentTimeoutWithFilter(results, now, PubDelayed, "")
}

func (q *DelayQueue) PeekAll(results []Message) (int, error) {
	return q.PeekRecentTimeoutWithFilter(results, time.Now().Add(time.Hour*24*365).UnixNano(), -1, "")
}

func (q *DelayQueue) GetSyncedOffset() (BackendOffset, error) {
	var synced BackendOffset
	err := q.getStore().View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		v := b.Get(syncedOffsetKey)
		offset, err := strconv.Atoi(string(v))
		if err != nil {
			return err
		}
		synced = BackendOffset(offset)
		return nil
	})
	if err != nil {
		nsqLog.LogErrorf("topic %v failed to get synced offset: %v", q.fullName, err)
	}
	return synced, err
}

func (q *DelayQueue) GetCurrentDelayedCnt(dt int, channel string) (uint64, error) {
	cnt := uint64(0)
	err := q.getStore().View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketMeta)
		cntKey := []byte("counter_" + string(getDelayedMsgDBPrefixKey(dt, channel)))
		cntBytes := b.Get(cntKey)
		if cntBytes != nil {
			cnt = binary.BigEndian.Uint64(cntBytes)
		}
		return nil
	})

	return cnt, err
}

func (q *DelayQueue) ConfirmedMessage(msg *Message) error {
	// confirmed message is finished by channel, this message has swap the
	// delayed id and original id to make sure the map key of inflight is original id
	q.compactMutex.Lock()
	err := q.getStore().Update(func(tx *bolt.Tx) error {
		return deleteBucketKey(int(msg.DelayedType), msg.DelayedChannel,
			msg.DelayedTs, msg.DelayedOrigID, tx, q.IsExt())
	})
	q.compactMutex.Unlock()
	if err != nil {
		nsqLog.LogErrorf(
			"%s : failed to delete delayed message %v-%v, %v",
			q.GetFullName(), msg.DelayedOrigID, msg, err)
	}
	return err
}

func (q *DelayQueue) IsChannelMessageDelayed(msgID MessageID, ch string) bool {
	found := false
	msgKey := getDelayedMsgDBIndexKey(ChannelDelayed, ch, msgID)
	q.getStore().View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketDelayedMsgIndex)
		v := b.Get(msgKey)
		if v != nil {
			found = true
		}
		return nil
	})
	return found
}

func (q *DelayQueue) GetOldestConsumedState(chList []string, includeOthers bool) (RecentKeyList, map[int]uint64, map[string]uint64) {
	db := q.getStore()
	prefixList := make([][]byte, 0, len(chList)+2)
	cntList := make(map[int]uint64)
	channelCntList := make(map[string]uint64)
	var err error
	if includeOthers {
		for filterType := MinDelayedType; filterType < MaxDelayedType; filterType++ {
			if filterType == ChannelDelayed {
				continue
			}
			prefixList = append(prefixList, getDelayedMsgDBPrefixKey(filterType, ""))
			cntList[filterType], err = q.GetCurrentDelayedCnt(filterType, "")
			if err != nil {
				return nil, nil, nil
			}
		}
	}
	chIndex := len(prefixList)
	for _, ch := range chList {
		prefixList = append(prefixList, getDelayedMsgDBPrefixKey(ChannelDelayed, ch))
		channelCntList[ch], err = q.GetCurrentDelayedCnt(ChannelDelayed, ch)

		if err != nil {
			return nil, nil, nil
		}
	}
	keyList := make(RecentKeyList, 0, len(prefixList))
	for i, prefix := range prefixList {
		var origCh string
		if i >= chIndex {
			origCh = chList[i-chIndex]
		}

		if nsqLog.Level() > levellogger.LOG_DETAIL {
			nsqLog.LogDebugf("peek prefix %v: channel %v", prefix, origCh)
		}

		err := db.View(func(tx *bolt.Tx) error {
			b := tx.Bucket(bucketDelayedMsg)
			c := b.Cursor()
			for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
				_, delayedTs, _, delayedCh, err := decodeDelayedMsgDBKey(k)
				if err != nil {
					nsqLog.Infof("decode key failed : %v, %v", k, err)
					continue
				}

				if nsqLog.Level() > levellogger.LOG_DETAIL {
					nsqLog.LogDebugf("peek delayed message %v: %v, %v", k, delayedTs, origCh)
				}

				// prefix seek may across to other channel with the same prefix
				if delayedCh != origCh {
					continue
				}
				ck := make([]byte, len(k))
				copy(ck, k)
				keyList = append(keyList, ck)
				break
			}
			return nil
		})
		if err != nil {
			return nil, nil, nil
		}
	}
	return keyList, cntList, channelCntList
}

func (q *DelayQueue) UpdateConsumedState(keyList RecentKeyList, cntList map[int]uint64, channelCntList map[string]uint64) error {
	for _, k := range keyList {
		dt, dts, id, delayedCh, err := decodeDelayedMsgDBKey(k)
		if err != nil {
			nsqLog.Infof("decode key failed : %v, %v", k, err)
			continue
		}
		q.emptyDelayedUntil(int(dt), dts, id, delayedCh)
	}
	for dt, cnt := range cntList {
		if cnt == 0 && dt != ChannelDelayed {
			q.EmptyDelayedType(dt)
		}
	}
	for ch, cnt := range channelCntList {
		if cnt == 0 {
			q.EmptyDelayedChannel(ch)
		}
	}
	return nil
}

func (q *DelayQueue) TryCleanOldData(retentionSize int64, noRealClean bool, maxCleanOffset BackendOffset) (BackendQueueEnd, error) {
	// clean the data that has been consumed and keep the retention policy
	var oldestPos BackendQueueEnd
	oldestPos = q.backend.GetQueueReadEnd()
	if oldestPos == nil {
		nsqLog.Logf("no end position found")
		return nil, nil
	}
	cleanStart := q.backend.GetQueueReadStart()
	if cleanStart.Offset()+BackendOffset(retentionSize) >= oldestPos.Offset() {
		return nil, nil
	}
	nsqLog.Logf("clean topic %v data current start: %v, oldest end %v, max clean end: %v",
		q.GetFullName(), cleanStart, oldestPos, maxCleanOffset)

	if oldestPos.Offset() < maxCleanOffset || maxCleanOffset == BackendOffset(0) {
		maxCleanOffset = oldestPos.Offset()
	}
	snapReader := NewDiskQueueSnapshot(getDelayQueueBackendName(q.tname, q.partition), q.dataPath, oldestPos)
	snapReader.SetQueueStart(cleanStart)
	err := snapReader.SeekTo(cleanStart.Offset())
	if err != nil {
		nsqLog.Errorf("topic: %v failed to seek to %v: %v", q.GetFullName(), cleanStart, err)
		return nil, err
	}
	readInfo := snapReader.GetCurrentReadQueueOffset()
	data := snapReader.ReadOne()
	if data.Err != nil {
		return nil, data.Err
	}
	var cleanEndInfo BackendQueueOffset
	retentionDay := int32(DEFAULT_RETENTION_DAYS)
	cleanTime := time.Now().Add(-1 * time.Hour * 24 * time.Duration(retentionDay))
	for {
		if retentionSize > 0 {
			// clean data ignore the retention day
			// only keep the retention size (start from the last consumed)
			if data.Offset > maxCleanOffset-BackendOffset(retentionSize) {
				break
			}
			cleanEndInfo = readInfo
		} else {
			msg, decodeErr := DecodeDelayedMessage(data.Data, q.IsExt())
			if decodeErr != nil {
				nsqLog.LogErrorf("topic %v failed to decode message - %s - %v", q.fullName, decodeErr, data)
			} else {
				if msg.Timestamp >= cleanTime.UnixNano() {
					break
				}
				if data.Offset >= maxCleanOffset {
					break
				}
				cleanEndInfo = readInfo
			}
		}
		err = snapReader.SkipToNext()
		if err != nil {
			nsqLog.Logf("failed to skip - %s ", err)
			break
		}
		readInfo = snapReader.GetCurrentReadQueueOffset()
		data = snapReader.ReadOne()
		if data.Err != nil {
			nsqLog.LogErrorf("topic %v failed to read - %s ", q.fullName, data.Err)
			break
		}
	}

	nsqLog.Infof("clean topic %v delayed queue from %v under retention %v, %v",
		q.GetFullName(), cleanEndInfo, cleanTime, retentionSize)
	if cleanEndInfo == nil || cleanEndInfo.Offset()+BackendOffset(retentionSize) >= maxCleanOffset {
		if cleanEndInfo != nil {
			nsqLog.Warningf("clean topic %v data at position: %v could not exceed current oldest confirmed %v and max clean end: %v",
				q.GetFullName(), cleanEndInfo, oldestPos, maxCleanOffset)
		}
		return nil, nil
	}
	if !noRealClean {
		err := q.compactStore(false)
		if err != nil {
			nsqLog.Errorf("topic %v failed to compact the bolt db: %v", q.fullName, err)
			return nil, err
		}
	}
	return q.backend.CleanOldDataByRetention(cleanEndInfo, noRealClean, maxCleanOffset)
}

func (q *DelayQueue) compactStore(force bool) error {
	src := q.getStore()
	origPath := src.Path()
	if !force {
		fi, err := os.Stat(origPath)
		if err != nil {
			return err
		}
		if fi.Size() < int64(CompactThreshold) {
			return nil
		}
		cnt := uint64(0)
		err = src.View(func(tx *bolt.Tx) error {
			b := tx.Bucket(bucketMeta)
			prefix := []byte("counter_")
			c := b.Cursor()
			for k, v := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
				if v != nil && len(v) == 8 {
					cnt += binary.BigEndian.Uint64(v)
				}
			}
			return nil
		})

		if err != nil {
			return err
		}
		if cnt > CompactCntThreshold {
			return nil
		}
	}
	tmpPath := fmt.Sprintf("%s-tmp.compact.%d", src.Path(), time.Now().UnixNano())
	// Open destination database.
	ro := &bolt.Options{
		Timeout:  time.Second,
		ReadOnly: false,
	}
	dst, err := bolt.Open(tmpPath, 0644, ro)
	if err != nil {
		return err
	}
	dst.NoSync = true
	nsqLog.Infof("db %v begin compact", origPath)
	defer nsqLog.Infof("db %v end compact", origPath)
	q.compactMutex.Lock()
	defer q.compactMutex.Unlock()
	err = compactBolt(dst, src, time.Second*2)
	if err != nil {
		nsqLog.Infof("db %v compact failed: %v", origPath, err)
		return err
	}

	q.dbLock.Lock()
	defer q.dbLock.Unlock()
	q.kvStore.Close()
	err = os.Rename(tmpPath, origPath)
	openErr := q.reOpenStore()
	if openErr != nil {
		nsqLog.Errorf("db %v failed to reopen while compacted : %v", origPath, openErr)
	}
	if err != nil {
		nsqLog.Infof("db %v failed to rename compacted db: %v", origPath, err)
		return err
	}
	if openErr != nil {
		return openErr
	}
	return nil
}

func compactBolt(dst, src *bolt.DB, maxCompactTime time.Duration) error {
	startT := time.Now()
	defer dst.Close()
	// commit regularly, or we'll run out of memory for large datasets if using one transaction.
	var size int64
	tx, err := dst.Begin(true)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := walkBolt(src, func(keys [][]byte, k, v []byte, seq uint64) error {
		// On each key/value, check if we have exceeded tx size.
		sz := int64(len(k) + len(v))
		if size+sz > TxMaxSize && TxMaxSize != 0 {
			// Commit previous transaction.
			if err := tx.Commit(); err != nil {
				return err
			}

			if time.Since(startT) >= maxCompactTime {
				return errors.New("compact timeout")
			}
			// Start new transaction.
			tx, err = dst.Begin(true)
			if err != nil {
				return err
			}
			size = 0
		}
		size += sz

		// Create bucket on the root transaction if this is the first level.
		nk := len(keys)
		if nk == 0 {
			bkt, err := tx.CreateBucketIfNotExists(k)
			if err != nil {
				return err
			}
			if err := bkt.SetSequence(seq); err != nil {
				return err
			}
			return nil
		}

		// Create buckets on subsequent levels, if necessary.
		b := tx.Bucket(keys[0])
		if nk > 1 {
			for _, k := range keys[1:] {
				b = b.Bucket(k)
			}
		}

		// If there is no value then this is a bucket call.
		if v == nil {
			bkt, err := b.CreateBucketIfNotExists(k)
			if err != nil {
				return err
			}
			if err := bkt.SetSequence(seq); err != nil {
				return err
			}
			return nil
		}

		// Otherwise treat it as a key/value pair.
		return b.Put(k, v)
	}); err != nil {
		return err
	}

	err = tx.Commit()
	if err == nil {
		dst.Sync()
	}
	return err
}

// walkFunc is the type of the function called for keys (buckets and "normal"
// values) discovered by Walk. keys is the list of keys to descend to the bucket
// owning the discovered key/value pair k/v.
type walkFunc func(keys [][]byte, k, v []byte, seq uint64) error

// walk walks recursively the bolt database db, calling walkFn for each key it finds.
func walkBolt(db *bolt.DB, walkFn walkFunc) error {
	return db.View(func(tx *bolt.Tx) error {
		return tx.ForEach(func(name []byte, b *bolt.Bucket) error {
			return walkBucket(b, nil, name, nil, b.Sequence(), walkFn)
		})
	})
}

func walkBucket(b *bolt.Bucket, keypath [][]byte, k, v []byte, seq uint64, fn walkFunc) error {
	// Execute callback.
	if err := fn(keypath, k, v, seq); err != nil {
		return err
	}

	// If this is not a bucket then stop.
	if v != nil {
		return nil
	}

	// Iterate over each child key/value.
	keypath = append(keypath, k)
	return b.ForEach(func(k, v []byte) error {
		if v == nil {
			bkt := b.Bucket(k)
			return walkBucket(bkt, keypath, k, nil, bkt.Sequence(), fn)
		}
		return walkBucket(b, keypath, k, v, b.Sequence(), fn)
	})
}
