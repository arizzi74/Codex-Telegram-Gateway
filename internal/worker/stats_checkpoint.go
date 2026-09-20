package worker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"syscall"
	"time"

	"github.com/iaia/telegramgw/internal/protocol"
	bolt "github.com/iaia/telegramgw/internal/workerdb"
)

// Checkpoints are derived data in the same embedded SQLite backend as the
// worker ledger. Evicting the small in-memory cache no longer loses progress.
type statsCheckpoint struct {
	Version            int
	Device, Inode      uint64
	Size, Offset       int64
	Modified, Observed time.Time
	Scan, Tail         statsScanCheckpoint
}

type statsScanCheckpoint struct {
	Stats                                                      protocol.SessionStats
	Partial                                                    []byte
	Discard, Gap                                               bool
	PromptEvents, PromptResponses, ReplyEvents, ReplyResponses int64
	ActiveTurn                                                 string
	ActiveSince                                                *time.Time
}

func checkpointScan(scan rolloutStatsScan) statsScanCheckpoint {
	return statsScanCheckpoint{scan.stats, scan.partial, scan.discard, scan.gap,
		scan.promptEvents, scan.promptResponses, scan.replyEvents, scan.replyResponses,
		scan.activeTurn, scan.activeSince}
}

func (p statsScanCheckpoint) restore() rolloutStatsScan {
	return rolloutStatsScan{stats: p.Stats, partial: p.Partial, discard: p.Discard, gap: p.Gap,
		promptEvents: p.PromptEvents, promptResponses: p.PromptResponses,
		replyEvents: p.ReplyEvents, replyResponses: p.ReplyResponses,
		activeTurn: p.ActiveTurn, activeSince: p.ActiveSince}
}

func statsCheckpointKey(key string) []byte {
	hash := sha256.Sum256([]byte(key))
	return []byte("rollout_stats/v1/" + hex.EncodeToString(hash[:]))
}

func fileIdentity(info os.FileInfo) (uint64, uint64, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(stat.Dev), uint64(stat.Ino), true
}

func sameStatsFile(a, b os.FileInfo) bool {
	ad, ai, aok := fileIdentity(a)
	bd, bi, bok := fileIdentity(b)
	return aok && bok && ad == bd && ai == bi
}

type checkpointFileInfo struct {
	os.FileInfo
	size     int64
	modified time.Time
}

func (s checkpointFileInfo) Size() int64        { return s.size }
func (s checkpointFileInfo) ModTime() time.Time { return s.modified }

func (s *Store) loadStatsCheckpoint(key string, info os.FileInfo) *rolloutStatsEntry {
	var checkpoint statsCheckpoint
	if err := s.db.View(func(tx *bolt.Tx) error {
		value := tx.Bucket(bucketMeta).Get(statsCheckpointKey(key))
		if len(value) == 0 {
			return nil
		}
		return json.Unmarshal(value, &checkpoint)
	}); err != nil {
		return nil
	}
	device, inode, ok := fileIdentity(info)
	if !ok || checkpoint.Version != 1 || device != checkpoint.Device || inode != checkpoint.Inode ||
		checkpoint.Size < 0 || checkpoint.Offset < 0 || checkpoint.Offset > checkpoint.Size ||
		info.Size() < checkpoint.Size || info.ModTime().Before(checkpoint.Modified) ||
		(info.Size() == checkpoint.Size && !info.ModTime().Equal(checkpoint.Modified)) ||
		len(checkpoint.Scan.Partial) > statsLineBytes || len(checkpoint.Tail.Partial) > statsLineBytes {
		return nil
	}
	return &rolloutStatsEntry{fileInfo: checkpointFileInfo{info, checkpoint.Size, checkpoint.Modified},
		offset: checkpoint.Offset, observed: checkpoint.Observed,
		scan: checkpoint.Scan.restore(), tail: checkpoint.Tail.restore()}
}

func (s *Store) saveStatsCheckpoint(key string, entry *rolloutStatsEntry) error {
	device, inode, ok := fileIdentity(entry.fileInfo)
	if !ok {
		return nil
	}
	value, err := json.Marshal(statsCheckpoint{Version: 1, Device: device, Inode: inode,
		Size: entry.fileInfo.Size(), Modified: entry.fileInfo.ModTime(), Offset: entry.offset,
		Observed: entry.observed, Scan: checkpointScan(entry.scan), Tail: checkpointScan(entry.tail)})
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error { return tx.Bucket(bucketMeta).Put(statsCheckpointKey(key), value) })
}
