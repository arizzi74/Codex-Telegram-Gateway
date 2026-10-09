package worker

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	bolt "github.com/iaia/telegramgw/internal/workerdb"
)

const (
	// Advance when the parser gains coverage so durable EOF checkpoints do not
	// skip records that an older parser ignored. Existing generations are
	// retired in bounded batches while the new version reindexes incrementally.
	rolloutTimestampIndexVersion    = 3
	rolloutTimestampScanBytes       = 16 << 20
	rolloutTimestampTailBytes       = 4 << 20
	rolloutTimestampRecords         = 4096
	rolloutTimestampCandidatesLimit = 256
	rolloutTimestampAnchorBytes     = 4096
)

type rolloutTimestampQuery struct {
	TurnID, ItemID, Role, Phase, Digest               string
	ExpectedSequenceDigest                            string
	Occurrence, ExpectedOccurrences, ExpectedMessages int
	Synthetic                                         bool
}

type rolloutTimestampResult struct {
	Timestamp time.Time
	Found     bool
}

// This cache retains only a bounded number of small checkpoints. Message
// metadata is individually indexed in the existing private worker SQLite
// ledger. No text, image, raw JSON, or reasoning is persisted by this index.
type rolloutTimestampIndex struct {
	mu                sync.Mutex
	entries           map[string]*rolloutTimestampCheckpoint
	memory            map[string][]byte // bounded fallback for store-less worker tests
	bytesRead         int64
	discoveryAfter    time.Time
	paths             map[string]string
	discoveryNext     map[string]int
	pathSets          map[string]map[string]bool
	discoveryHomes    map[string]time.Time
	discoveryLastHome string
	discoveryYielded  time.Time
}

type rolloutTimestampCheckpoint struct {
	Version                    int
	Device, Inode              uint64
	Size, Offset               int64
	Modified                   time.Time
	Generation                 string
	StaleGenerations           []string
	HeaderDigest, AnchorDigest string
	AnchorStart, AnchorLength  int64
	TailSize                   int64
	TailModified               time.Time
	TailOffset                 int64
	TailWaiting                bool
	TailScan                   rolloutTimestampScan
	WaitingSize                int64
	RollbackBefore             int64
	Scan                       rolloutTimestampScan
	used                       time.Time
}

// The only state retained across an oversized record consists of identity
// metadata. Small incomplete records are reread from their starting offset on
// append, so checkpoints never contain a fragment of conversation content.
type rolloutTimestampScan struct {
	Turn                     string
	Start                    int64
	Known, DigestGap, Closed bool
	Messages                 int
	SequenceDigest           string
	Discard                  bool
	Pending                  *rolloutTimestampRecord
}

type rolloutTimestampRecord struct {
	Start        int64
	Role, ItemID string
	Timestamp    time.Time
}

type rolloutTimestampCoverage struct {
	Start, Through               int64
	Closed, Ambiguous, DigestGap bool
	Messages                     int
	SequenceDigest               string
}

// A saved snapshot can remain an exact prefix of a growing turn. Retain only
// its canonical sequence hash, message count, and inclusive final byte offset,
// so later records cannot change its occurrence counts or supply its dates.
type rolloutTimestampPrefixProof struct {
	Messages int
	Through  int64
}

type rolloutTimestampBatch struct {
	values         map[string][]byte
	coverage       map[string]rolloutTimestampCoverage
	rollbackBefore int64
}

type rolloutTimestampCountingReader struct {
	reader io.Reader
	count  *int64
}

func (r rolloutTimestampCountingReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	*r.count += int64(n)
	return n, err
}

func validRolloutTimestampCheckpoint(cp rolloutTimestampCheckpoint) bool {
	if _, err := uuid.Parse(cp.Generation); err != nil {
		return false
	}
	// Older versions are loadable only to retire their generations. update
	// never resumes their scans, including checkpoints at an unchanged EOF.
	if (cp.Version < 1 || cp.Version > rolloutTimestampIndexVersion) || cp.Offset < 0 || cp.Size < 0 || cp.Offset > cp.Size || cp.RollbackBefore < 0 || cp.RollbackBefore > cp.Size || len(cp.Generation) != 36 || cp.AnchorLength < 0 || cp.AnchorLength > rolloutTimestampAnchorBytes || cp.AnchorStart < 0 || cp.AnchorStart > cp.Size || cp.AnchorLength > cp.Size-cp.AnchorStart || cp.Scan.Start < 0 || cp.Scan.Start > cp.Size || cp.Scan.Messages < 0 || len(cp.Scan.Turn) > 256 || len(cp.StaleGenerations) > 1024 {
		return false
	}
	if cp.Scan.Pending != nil && (cp.Scan.Pending.Start < 0 || cp.Scan.Pending.Start > cp.Size || len(cp.Scan.Pending.ItemID) > 256) {
		return false
	}
	if cp.TailSize > cp.Size || cp.TailOffset < 0 || (cp.TailSize >= 0 && cp.TailOffset > cp.TailSize) || len(cp.Scan.SequenceDigest) > 64 || len(cp.TailScan.SequenceDigest) > 64 || len(cp.TailScan.Turn) > 256 {
		return false
	}
	return true
}

func rolloutMessageDigest(role, text string) string {
	h := sha256.New()
	h.Write([]byte(role))
	h.Write([]byte{0})
	h.Write([]byte(text))
	return hex.EncodeToString(h.Sum(nil))
}

func rolloutTimestampSequenceDigest(previous, role, phase, digest string) string {
	return rolloutTimestampHash(previous, role, phase, digest)
}

func rolloutTimestampHash(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		fmt.Fprintf(h, "%d:", len(part))
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func rolloutTimestampRelativePath(home, path string) (string, bool) {
	if !filepath.IsAbs(home) || !filepath.IsAbs(path) {
		return "", false
	}
	rel, err := filepath.Rel(filepath.Clean(home), filepath.Clean(path))
	if err != nil || filepath.IsAbs(rel) || filepath.Ext(rel) != ".jsonl" {
		return "", false
	}
	normalized := filepath.ToSlash(rel)
	return rel, strings.HasPrefix(normalized, "sessions/") || strings.HasPrefix(normalized, "archived_sessions/")
}

func rolloutTimestampScope(namespace, home, rel, thread string) string {
	return "rollout_time/v1/" + rolloutTimestampHash(namespace, filepath.Clean(home), filepath.ToSlash(rel), thread)
}

func rolloutTimestampPathKey(namespace, home, thread string) []byte {
	return []byte("rollout_time_path/v1/" + rolloutTimestampHash(namespace, filepath.Clean(home), thread))
}

func (s *Store) loadRolloutTimestampPath(namespace, home, thread string) string {
	if s == nil {
		return ""
	}
	var rel string
	if s.db.View(func(tx *bolt.Tx) error {
		rel = string(tx.Bucket(bucketMeta).Get(rolloutTimestampPathKey(namespace, home, thread)))
		return nil
	}) != nil {
		return ""
	}
	path := filepath.Join(home, rel)
	if valid, ok := rolloutTimestampRelativePath(home, path); !ok || valid != rel {
		return ""
	}
	return path
}

type rolloutTimestampFile struct {
	Path, ThreadID string
}

// Inventory discovery shares one bounded work chunk per thirty seconds. A
// rotating starting point lets large or active sessions make progress without
// starving the later entries in the same authoritative inventory.
func (c *rolloutStatsCache) discoverRolloutTimestampPass(home string, files []rolloutTimestampFile) {
	idx := &c.timestamps
	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.paths == nil {
		idx.paths = make(map[string]string)
	}
	if idx.discoveryNext == nil {
		idx.discoveryNext = make(map[string]int)
	}
	scope := rolloutTimestampHash(c.namespace, filepath.Clean(home))
	if idx.pathSets == nil {
		idx.pathSets = make(map[string]map[string]bool)
	}
	currentPaths := make(map[string]bool)
	var eligible []rolloutTimestampFile
	mappings := make(map[string]string)
	for _, file := range files {
		rel, ok := rolloutTimestampRelativePath(home, file.Path)
		if !ok || file.ThreadID == "" {
			continue
		}
		eligible = append(eligible, file)
		key := string(rolloutTimestampPathKey(c.namespace, home, file.ThreadID))
		currentPaths[key] = true
		if idx.paths[key] != rel {
			mappings[key] = rel
		}
	}
	for key := range idx.pathSets[scope] {
		if !currentPaths[key] {
			delete(idx.paths, key)
		}
	}
	idx.pathSets[scope] = currentPaths
	if c.store != nil && len(mappings) > 0 {
		if c.store.db.Update(func(tx *bolt.Tx) error {
			bucket := tx.Bucket(bucketMeta)
			for key, rel := range mappings {
				if string(bucket.Get([]byte(key))) != rel {
					if err := bucket.Put([]byte(key), []byte(rel)); err != nil {
						return err
					}
				}
			}
			return nil
		}) == nil {
			for key, rel := range mappings {
				idx.paths[key] = rel
			}
		}
	}
	now := time.Now()
	if idx.discoveryHomes == nil {
		idx.discoveryHomes = make(map[string]time.Time)
	}
	idx.discoveryHomes[scope] = now
	if len(eligible) == 0 || now.Before(idx.discoveryAfter) {
		return
	}
	if idx.discoveryLastHome == scope {
		for other, seen := range idx.discoveryHomes {
			if other != scope && now.Sub(seen) < time.Minute && (idx.discoveryYielded.IsZero() || now.Sub(idx.discoveryYielded) < 10*time.Second) {
				if idx.discoveryYielded.IsZero() {
					idx.discoveryYielded = now
				}
				return // Give the next recently observed home this global slot.
			}
		}
	}
	start := idx.discoveryNext[scope] % len(eligible)
	for attempt := 0; attempt < len(eligible); attempt++ {
		position := (start + attempt) % len(eligible)
		file := eligible[position]
		before := idx.bytesRead
		_, _ = idx.update(c.store, c.namespace, home, file.Path, file.ThreadID, false)
		idx.discoveryNext[scope] = (position + 1) % len(eligible)
		if idx.bytesRead != before {
			idx.discoveryAfter = time.Now().Add(30 * time.Second)
			idx.discoveryLastHome, idx.discoveryYielded = scope, time.Time{}
			return
		}
	}
}

func (c *rolloutStatsCache) rolloutTimestampCandidates(home, path, thread string, queries []rolloutTimestampQuery) []rolloutTimestampResult {
	results := make([]rolloutTimestampResult, len(queries))
	if len(queries) == 0 {
		return results
	}
	idx := &c.timestamps
	idx.mu.Lock()
	defer idx.mu.Unlock()
	cp, prefix := idx.update(c.store, c.namespace, home, path, thread, true)
	if cp == nil {
		return results
	}
	read := func(get func(string) []byte, candidates func(string, int, int64) []time.Time) {
		for i, q := range queries {
			if q.TurnID == "" || (q.Role != "user" && q.Role != "assistant" && q.Role != "tool" && q.Role != "activity" && q.Role != "compaction") {
				continue
			}
			var coverage rolloutTimestampCoverage
			if json.Unmarshal(get(prefix+"/turn/"+rolloutTimestampHash(q.TurnID)), &coverage) != nil || coverage.Start < cp.RollbackBefore || coverage.Ambiguous || (!coverage.Closed && coverage.Through != cp.Size) {
				continue
			}
			var times []time.Time
			if q.Synthetic {
				if q.Role == "tool" || q.Role == "activity" || q.Digest == "" || q.ExpectedSequenceDigest == "" || coverage.DigestGap || q.ExpectedOccurrences <= 0 || q.ExpectedOccurrences > rolloutTimestampCandidatesLimit || q.Occurrence < 0 || q.Occurrence >= q.ExpectedOccurrences {
					continue
				}
				through := int64(-1)
				if q.ExpectedSequenceDigest == coverage.SequenceDigest {
					if q.ExpectedMessages != coverage.Messages {
						continue
					}
				} else {
					var proof rolloutTimestampPrefixProof
					key := prefix + "/sequence/" + rolloutTimestampHash(q.TurnID, q.ExpectedSequenceDigest)
					if json.Unmarshal(get(key), &proof) != nil || proof.Messages != q.ExpectedMessages || proof.Messages < 0 || proof.Messages > coverage.Messages || proof.Through < coverage.Start || proof.Through >= coverage.Through {
						continue
					}
					through = proof.Through
				}
				key := prefix + "/digest/" + rolloutTimestampHash(q.TurnID, q.Role, q.Phase, q.Digest) + "/"
				times = candidates(key, q.ExpectedOccurrences+1, through)
				if len(times) == q.ExpectedOccurrences {
					results[i] = rolloutTimestampResult{times[q.Occurrence], true}
				}
				continue
			}
			if q.ItemID == "" {
				continue
			}
			kinds := []string{"exact-start", "exact-end", "exact"}
			if q.Role == "tool" {
				kinds = []string{"tool-begin", "tool-end", "exact-start", "exact-end", "exact"}
			}
			for _, kind := range kinds {
				times = candidates(prefix+"/"+kind+"/"+rolloutTimestampHash(q.TurnID, q.Role, q.ItemID)+"/", 2, -1)
				if len(times) != 0 {
					if len(times) == 1 {
						results[i] = rolloutTimestampResult{times[0], true}
					}
					break
				}
			}
		}
	}
	if c.store != nil {
		if c.store.db.View(func(tx *bolt.Tx) error {
			bucket := tx.Bucket(bucketMeta)
			read(func(key string) []byte { return bucket.Get([]byte(key)) }, func(key string, limit int, through int64) []time.Time {
				var result []time.Time
				var last []byte
				if through >= 0 {
					last = fmt.Appendf(nil, "%s%016x", key, through)
				}
				cursor := bucket.Cursor()
				for k, value := cursor.Seek([]byte(key)); k != nil && bytes.HasPrefix(k, []byte(key)) && len(result) < limit; k, value = cursor.Next() {
					if last != nil && bytes.Compare(k, last) > 0 {
						break
					}
					var stamp time.Time
					if json.Unmarshal(value, &stamp) != nil {
						return nil
					}
					result = append(result, stamp)
				}
				return result
			})
			return nil
		}) != nil {
			return make([]rolloutTimestampResult, len(queries))
		}
	} else {
		read(func(key string) []byte { return idx.memory[key] }, func(key string, limit int, through int64) []time.Time {
			// Store-less tests have a capped metadata map. Sort only this small
			// matching set; production uses the SQLite primary-key index.
			return memoryRolloutTimestampCandidates(idx.memory, key, limit, through)
		})
	}
	return results
}

func (idx *rolloutTimestampIndex) update(store *Store, namespace, home, path, thread string, tail bool) (*rolloutTimestampCheckpoint, string) {
	rel, ok := rolloutTimestampRelativePath(home, path)
	if !ok || thread == "" {
		return nil, ""
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		return nil, ""
	}
	defer root.Close()
	info, err := root.Stat(rel)
	if err != nil || !info.Mode().IsRegular() {
		return nil, ""
	}
	device, inode, ok := fileIdentity(info)
	if !ok {
		return nil, ""
	}
	scope := rolloutTimestampScope(namespace, home, rel, thread)
	if idx.entries == nil {
		idx.entries = make(map[string]*rolloutTimestampCheckpoint)
	}
	cp := idx.entries[scope]
	if cp == nil && store != nil {
		_ = store.db.View(func(tx *bolt.Tx) error {
			var saved rolloutTimestampCheckpoint
			if json.Unmarshal(tx.Bucket(bucketMeta).Get([]byte(scope+"/checkpoint")), &saved) == nil && validRolloutTimestampCheckpoint(saved) {
				cp = &saved
			}
			return nil
		})
	}
	valid := cp != nil && cp.Version == rolloutTimestampIndexVersion && cp.Device == device && cp.Inode == inode && info.Size() >= cp.Size && !info.ModTime().Before(cp.Modified) && (info.Size() != cp.Size || info.ModTime().Equal(cp.Modified))
	unchanged := valid && cp.Size == info.Size() && cp.Modified.Equal(info.ModTime())
	if unchanged && (cp.Offset == cp.Size || cp.WaitingSize == cp.Size) && (!tail || (cp.TailSize == info.Size() && cp.TailModified.Equal(info.ModTime()) && (cp.TailOffset == cp.TailSize || cp.TailWaiting)) || cp.Offset == cp.Size) {
		if len(cp.StaleGenerations) > 0 && store != nil {
			copy := *cp
			copy.StaleGenerations = append([]string(nil), cp.StaleGenerations...)
			cp = &copy
			if idx.commit(store, scope, scope+"/"+cp.Generation, cp, rolloutTimestampBatch{}) != nil {
				return nil, ""
			}
		}
		idx.remember(scope, cp)
		return cp, scope + "/" + cp.Generation
	}
	file, err := root.Open(rel)
	if err != nil {
		return nil, ""
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || info.Size() != opened.Size() || !info.ModTime().Equal(opened.ModTime()) {
		return nil, ""
	}
	meta, err := bufio.NewReader(io.LimitReader(rolloutTimestampCountingReader{file, &idx.bytesRead}, statsLineBytes)).ReadBytes('\n')
	var identity struct {
		Type    string `json:"type"`
		Payload struct {
			ID string `json:"id"`
		} `json:"payload"`
	}
	if err != nil || json.Unmarshal(meta, &identity) != nil || identity.Type != "session_meta" || identity.Payload.ID != thread {
		return nil, ""
	}
	header := rolloutTimestampHash(string(meta))
	if valid && cp.HeaderDigest != header {
		valid = false
	}
	if valid && !unchanged && cp.AnchorLength > 0 {
		anchor := make([]byte, cp.AnchorLength)
		n, readErr := file.ReadAt(anchor, cp.AnchorStart)
		idx.bytesRead += int64(n)
		if readErr != nil || rolloutTimestampHash(string(anchor)) != cp.AnchorDigest {
			valid = false
		}
	}
	if !valid {
		var stale []string
		if cp != nil {
			stale = append(append([]string(nil), cp.StaleGenerations...), cp.Generation)
		}
		cp = &rolloutTimestampCheckpoint{Version: rolloutTimestampIndexVersion, Device: device, Inode: inode, Generation: uuid.NewString(), HeaderDigest: header, WaitingSize: -1, TailSize: -1, StaleGenerations: stale}
	} else {
		copy := *cp
		copy.StaleGenerations = append([]string(nil), cp.StaleGenerations...)
		cp = &copy // Never expose progress that failed to commit.
	}
	prefix := scope + "/" + cp.Generation
	batch := rolloutTimestampBatch{values: make(map[string][]byte), coverage: make(map[string]rolloutTimestampCoverage)}
	if cp.Offset < info.Size() && !(unchanged && cp.WaitingSize == info.Size()) {
		if _, err = file.Seek(cp.Offset, io.SeekStart); err != nil {
			return nil, ""
		}
		_, next, waiting, readErr := cp.Scan.read(rolloutTimestampCountingReader{io.LimitReader(file, min(int64(rolloutTimestampScanBytes), info.Size()-cp.Offset)), &idx.bytesRead}, cp.Offset, info.Size(), prefix, &batch)
		if readErr != nil {
			return nil, ""
		}
		cp.Offset = next
		if waiting {
			cp.WaitingSize = info.Size()
		} else {
			cp.WaitingSize = -1
		}
	}
	if tail && cp.Offset < info.Size() && (cp.TailSize != info.Size() || !cp.TailModified.Equal(info.ModTime()) || (cp.TailOffset < info.Size() && !cp.TailWaiting)) {
		if cp.TailSize != info.Size() || !cp.TailModified.Equal(info.ModTime()) {
			cp.TailOffset = max(int64(0), info.Size()-rolloutTimestampTailBytes)
			cp.TailScan = rolloutTimestampScan{Discard: cp.TailOffset > 0}
			cp.TailSize, cp.TailModified, cp.TailWaiting = info.Size(), info.ModTime(), false
		}
		if _, err = file.Seek(cp.TailOffset, io.SeekStart); err != nil {
			return nil, ""
		}
		_, next, waiting, readErr := cp.TailScan.read(rolloutTimestampCountingReader{io.LimitReader(file, info.Size()-cp.TailOffset), &idx.bytesRead}, cp.TailOffset, info.Size(), prefix, &batch)
		if readErr != nil {
			return nil, ""
		}
		cp.TailOffset, cp.TailWaiting = next, waiting
	}
	// A tiny anchor catches truncate/regrow and in-place replacement followed by
	// append without replaying the historical prefix.
	cp.AnchorLength = min(int64(rolloutTimestampAnchorBytes), info.Size())
	cp.AnchorStart = info.Size() - cp.AnchorLength
	anchor := make([]byte, cp.AnchorLength)
	n, err := file.ReadAt(anchor, cp.AnchorStart)
	idx.bytesRead += int64(n)
	if err != nil {
		return nil, ""
	}
	cp.AnchorDigest = rolloutTimestampHash(string(anchor))
	after, err := file.Stat()
	current, statErr := root.Stat(rel)
	if err != nil || statErr != nil || !os.SameFile(info, current) || !os.SameFile(info, after) || after.Size() < info.Size() || (after.Size() == info.Size() && !after.ModTime().Equal(info.ModTime())) {
		return nil, ""
	}
	cp.Size, cp.Modified = info.Size(), info.ModTime()
	cp.RollbackBefore = max(cp.RollbackBefore, batch.rollbackBefore)
	if err := idx.commit(store, scope, prefix, cp, batch); err != nil {
		return nil, ""
	}
	idx.remember(scope, cp)
	if store != nil {
		_ = store.db.Update(func(tx *bolt.Tx) error {
			bucket := tx.Bucket(bucketMeta)
			key := rolloutTimestampPathKey(namespace, home, thread)
			if string(bucket.Get(key)) == rel {
				return nil
			}
			return bucket.Put(key, []byte(rel))
		})
	}
	return cp, prefix
}

func (idx *rolloutTimestampIndex) remember(scope string, cp *rolloutTimestampCheckpoint) {
	if idx.entries[scope] == nil && len(idx.entries) >= statsCacheSize {
		var oldest string
		for key, entry := range idx.entries {
			if oldest == "" || entry.used.Before(idx.entries[oldest].used) {
				oldest = key
			}
		}
		delete(idx.entries, oldest)
	}
	cp.used = time.Now()
	idx.entries[scope] = cp
}

func (idx *rolloutTimestampIndex) commit(store *Store, scope, prefix string, cp *rolloutTimestampCheckpoint, batch rolloutTimestampBatch) error {
	apply := func(get func(string) []byte, put func(string, []byte) error) error {
		for key, coverage := range batch.coverage {
			var previous rolloutTimestampCoverage
			if json.Unmarshal(get(key), &previous) == nil {
				coverage.Ambiguous = coverage.Ambiguous || previous.Ambiguous || coverage.Start != previous.Start
				coverage.DigestGap = coverage.DigestGap || previous.DigestGap
				if previous.Through > coverage.Through {
					coverage.Through, coverage.Closed, coverage.Messages = previous.Through, previous.Closed, previous.Messages
					coverage.SequenceDigest = previous.SequenceDigest
				}
			}
			value, err := json.Marshal(coverage)
			if err != nil {
				return err
			}
			if err = put(key, value); err != nil {
				return err
			}
		}
		for key, value := range batch.values {
			if err := put(key, value); err != nil {
				return err
			}
		}
		value, err := json.Marshal(cp)
		if err != nil {
			return err
		}
		return put(scope+"/checkpoint", value)
	}
	if store != nil {
		return store.db.Update(func(tx *bolt.Tx) error {
			b := tx.Bucket(bucketMeta)
			// Remove invalid generations in small indexed batches, including on
			// unchanged-file lookups. A rewrite never triggers a history-sized
			// deletion or allows old candidates into the new generation.
			budget := 256
			for len(cp.StaleGenerations) > 0 && budget > 0 {
				oldPrefix := []byte(scope + "/" + cp.StaleGenerations[0] + "/")
				cursor := b.Cursor()
				key, _ := cursor.Seek(oldPrefix)
				for key != nil && bytes.HasPrefix(key, oldPrefix) && budget > 0 {
					if err := cursor.Delete(); err != nil {
						return err
					}
					budget--
					key, _ = cursor.Next()
				}
				if key == nil || !bytes.HasPrefix(key, oldPrefix) {
					cp.StaleGenerations = cp.StaleGenerations[1:]
					budget--
				}
			}
			return apply(func(key string) []byte { return b.Get([]byte(key)) }, func(key string, value []byte) error { return b.Put([]byte(key), value) })
		})
	}
	if idx.memory == nil {
		idx.memory = make(map[string][]byte)
	}
	return apply(func(key string) []byte { return idx.memory[key] }, func(key string, value []byte) error {
		if len(idx.memory) >= 50000 && idx.memory[key] == nil {
			return fmt.Errorf("temporary timestamp metadata capacity reached")
		}
		idx.memory[key] = value
		return nil
	})
}

func (s *rolloutTimestampScan) coverage(prefix string, through int64, closed bool, batch *rolloutTimestampBatch) {
	if !s.Known || s.Turn == "" {
		return
	}
	key := prefix + "/turn/" + rolloutTimestampHash(s.Turn)
	next := rolloutTimestampCoverage{Start: s.Start, Through: through, Closed: closed, DigestGap: s.DigestGap, Messages: s.Messages, SequenceDigest: s.SequenceDigest}
	if previous, exists := batch.coverage[key]; exists {
		next.Ambiguous = previous.Ambiguous || previous.Start != next.Start
		next.DigestGap = next.DigestGap || previous.DigestGap
		if previous.Through > next.Through {
			next.Through, next.Closed, next.Messages = previous.Through, previous.Closed, previous.Messages
			next.SequenceDigest = previous.SequenceDigest
		}
	}
	batch.coverage[key] = next
}

func (s *rolloutTimestampScan) read(reader io.Reader, start, size int64, prefix string, batch *rolloutTimestampBatch) (int64, int64, bool, error) {
	r := bufio.NewReaderSize(reader, 64<<10)
	var consumed int64
	lineStart := start
	var partial []byte
	var records int
	for {
		fragment, err := r.ReadSlice('\n')
		consumed += int64(len(fragment))
		if !s.Discard {
			remaining := statsLineBytes - len(partial)
			partial = append(partial, fragment[:min(remaining, len(fragment))]...)
			if len(fragment) > remaining {
				s.Discard = true
				id, role, stamp := historyRecordIdentity(partial)
				if id != "" && len(id) <= 256 && stamp != nil {
					s.Pending = &rolloutTimestampRecord{lineStart, role, id, *stamp}
				}
				kind, event := rolloutTimestampPrefixKind(partial)
				if kind == "event_msg" && event == "thread_rolled_back" {
					batch.rollbackBefore = max(batch.rollbackBefore, lineStart+1)
					s.Turn, s.Known = "", false
				}
				if kind == "event_msg" && (event == "user_message" || event == "agent_message") {
					s.DigestGap = true
					s.Messages++
				} else if kind == "" || kind == "session_meta" || kind == "turn_context" || (kind == "event_msg" && (event == "" || event == "task_started" || event == "turn_started" || event == "task_complete" || event == "turn_aborted")) {
					s.coverage(prefix, lineStart, true, batch)
					s.Turn, s.Known = "", false
				}
			}
		}
		if len(fragment) > 0 && fragment[len(fragment)-1] == '\n' {
			if s.Discard {
				if s.Pending != nil && s.Turn != "" {
					s.save(prefix, "exact", s.Pending.Role, "", s.Pending.ItemID, s.Pending.Start, s.Pending.Timestamp, batch)
				}
			} else {
				s.record(partial, lineStart, prefix, batch)
			}
			s.Discard, s.Pending, partial = false, nil, nil
			lineStart = start + consumed
			records++
			if records >= rolloutTimestampRecords {
				s.coverage(prefix, lineStart, lineStart == size, batch)
				return consumed, lineStart, false, nil
			}
		}
		if err == io.EOF {
			if start+consumed == size {
				s.coverage(prefix, size, false, batch)
			}
			if len(partial) > 0 && !s.Discard {
				return consumed, lineStart, start+consumed == size, nil
			}
			return consumed, start + consumed, false, nil
		}
		if err != nil && err != bufio.ErrBufferFull {
			return consumed, start + consumed, false, err
		}
	}
}

func (s *rolloutTimestampScan) save(prefix, kind, role, phase, identity string, offset int64, stamp time.Time, batch *rolloutTimestampBatch) {
	if s.Turn == "" || !s.Known || stamp.Unix() <= 0 || stamp.Year() > 9999 {
		return
	}
	s.saveTurn(s.Turn, prefix, kind, role, phase, identity, offset, stamp, batch)
}

func (s *rolloutTimestampScan) savePrefixProof(prefix string, through int64, batch *rolloutTimestampBatch) {
	if !s.Known || s.Turn == "" || s.DigestGap || s.SequenceDigest == "" {
		return
	}
	key := prefix + "/sequence/" + rolloutTimestampHash(s.Turn, s.SequenceDigest)
	value, _ := json.Marshal(rolloutTimestampPrefixProof{Messages: s.Messages, Through: through})
	batch.values[key] = value
}

func (s *rolloutTimestampScan) saveTurn(turn, prefix, kind, role, phase, identity string, offset int64, stamp time.Time, batch *rolloutTimestampBatch) {
	if turn == "" || len(turn) > 256 || len(identity) > 256 || stamp.Unix() <= 0 || stamp.Year() > 9999 {
		return
	}
	parts := []string{turn, role, identity}
	if kind == "digest" {
		parts = []string{turn, role, phase, identity}
	}
	key := fmt.Sprintf("%s/%s/%s/%016x", prefix, kind, rolloutTimestampHash(parts...), offset)
	value, _ := json.Marshal(stamp.UTC())
	batch.values[key] = value
}

func (s *rolloutTimestampScan) record(line []byte, offset int64, prefix string, batch *rolloutTimestampBatch) {
	var record struct {
		Type, Timestamp string
		Payload         struct {
			Type        string `json:"type"`
			TurnID      string `json:"turn_id"`
			ID          string `json:"id"`
			CallID      string `json:"call_id"`
			EventID     string `json:"event_id"`
			Role, Phase string
			Message     *string
			Item        struct{ Type, ID string } `json:"item"`
		}
	}
	if json.Unmarshal(line, &record) != nil {
		s.DigestGap = true
		if _, boundary := historyRecordTurn(line); boundary {
			s.coverage(prefix, offset, true, batch)
			s.Turn, s.Known = "", false
		}
		return
	}
	p := record.Payload
	if record.Type == "event_msg" && p.Type == "thread_rolled_back" {
		// Mapping removed turns requires replaying the full reducer. Keep
		// earlier items' labelled fallback and resume verified indexing only
		// after a fresh, explicit turn boundary beyond the rollback.
		batch.rollbackBefore = max(batch.rollbackBefore, offset+int64(len(line)))
		s.Turn, s.Known = "", false
		return
	}
	if record.Type == "session_meta" {
		s.coverage(prefix, offset, true, batch)
		s.Turn, s.Known = "", false
		return
	}
	if record.Type == "turn_context" || (record.Type == "event_msg" && (p.Type == "task_started" || p.Type == "turn_started")) {
		if p.TurnID == "" || len(p.TurnID) > 256 {
			s.coverage(prefix, offset, true, batch)
			s.Turn, s.Known = "", false
			return
		}
		if s.Turn != p.TurnID || s.Closed {
			s.coverage(prefix, offset, true, batch)
			*s = rolloutTimestampScan{Turn: p.TurnID, Start: offset, Known: record.Type == "event_msg"}
		} else if record.Type == "event_msg" && !s.Known {
			s.Start, s.Known = offset, true
		}
		return
	}
	if record.Type == "event_msg" && (p.Type == "task_complete" || p.Type == "task_completed" || p.Type == "turn_completed" || p.Type == "turn_aborted") {
		if p.TurnID == "" {
			s.DigestGap = true
			s.coverage(prefix, offset, true, batch)
			s.Turn, s.Known = "", false
			return
		}
		if p.TurnID != "" && p.TurnID != s.Turn {
			return
		}
		s.coverage(prefix, offset+int64(len(line)), true, batch)
		s.Closed = true
		s.Turn, s.Known = "", false
		return
	}
	stamp := statsTime(record.Timestamp)
	if record.Type == "event_msg" {
		if (p.Type == "item_started" || p.Type == "item_completed") && p.Item.ID != "" && stamp != nil {
			role := rolloutTimestampLifecycleRole(p.Item.Type)
			if role != "" {
				kind := "exact-start"
				if p.Type == "item_completed" {
					kind = "exact-end"
				}
				turn := p.TurnID
				if turn == "" && s.Known {
					turn = s.Turn
				}
				s.saveTurn(turn, prefix, kind, role, "", p.Item.ID, offset, *stamp, batch)
			}
			return
		}
		if p.Type == "sub_agent_activity" {
			// The visible activity item uses event_id, not the child thread,
			// agent path, or a call identifier. An event_id can equal the
			// underlying function call ID, so keep its activity domain distinct.
			if p.EventID != "" && stamp != nil {
				turn := p.TurnID
				if turn == "" && s.Known {
					turn = s.Turn
				}
				s.saveTurn(turn, prefix, "exact", "activity", "", p.EventID, offset, *stamp, batch)
			}
			return
		}
		if p.Type == "context_compacted" {
			s.SequenceDigest = rolloutTimestampSequenceDigest(s.SequenceDigest, "compaction", "", rolloutMessageDigest("compaction", ""))
			s.savePrefixProof(prefix, offset+int64(len(line))-1, batch)
			if stamp != nil {
				s.save(prefix, "digest", "compaction", "", rolloutMessageDigest("compaction", ""), offset, *stamp, batch)
			}
			return
		}
		role := ""
		if p.Type == "user_message" {
			if p.Message == nil {
				s.DigestGap = true
				return
			}
			role = "user"
		}
		if p.Type == "agent_message" && p.Message == nil {
			s.DigestGap = true
			return
		}
		if p.Type == "agent_message" && p.Message != nil && *p.Message != "" {
			role = "assistant"
		}
		if role != "" {
			s.Messages++
			text := *p.Message
			if role == "user" && strings.TrimSpace(text) == "" {
				text = ""
			}
			digest := rolloutMessageDigest(role, text)
			s.SequenceDigest = rolloutTimestampSequenceDigest(s.SequenceDigest, role, p.Phase, digest)
			s.savePrefixProof(prefix, offset+int64(len(line))-1, batch)
			if stamp != nil {
				s.save(prefix, "digest", role, p.Phase, digest, offset, *stamp, batch)
			}
			return
		}
		if p.CallID != "" && stamp != nil {
			kind := ""
			switch p.Type {
			case "exec_command_begin", "mcp_tool_call_begin", "dynamic_tool_call_begin", "web_search_begin", "patch_apply_begin", "image_generation_begin":
				kind = "tool-begin"
			case "exec_command_end", "mcp_tool_call_end", "dynamic_tool_call_end", "web_search_end", "patch_apply_end", "image_generation_end":
				kind = "tool-end"
			}
			if kind != "" {
				turn := p.TurnID
				if turn == "" && s.Known {
					turn = s.Turn
				}
				s.saveTurn(turn, prefix, kind, "tool", "", p.CallID, offset, *stamp, batch)
			}
		}
		return
	}
	if record.Type == "response_item" && stamp != nil {
		if p.Type == "message" && (p.Role == "user" || p.Role == "assistant") && p.ID != "" {
			s.save(prefix, "exact", p.Role, "", p.ID, offset, *stamp, batch)
		}
		if p.Type == "function_call" || p.Type == "custom_tool_call" {
			id := p.CallID
			if id == "" {
				id = p.ID
			}
			if id != "" {
				s.save(prefix, "exact", "tool", "", id, offset, *stamp, batch)
			}
		}
	}
}

// Rollouts carry the core TurnItem enum's PascalCase names; native thread
// items use camelCase. Accept only the explicitly supported spellings, so an
// unknown item or arbitrary case variation cannot acquire an unrelated date.
func rolloutTimestampLifecycleRole(kind string) string {
	switch kind {
	case "UserMessage", "userMessage":
		return "user"
	case "AgentMessage", "agentMessage":
		return "assistant"
	case "SubAgentActivity", "subAgentActivity":
		return "activity"
	case "CommandExecution", "commandExecution", "McpToolCall", "mcpToolCall", "DynamicToolCall", "dynamicToolCall", "FileChange", "fileChange", "WebSearch", "webSearch", "ImageGeneration", "imageGeneration", "CollabAgentToolCall", "collabAgentToolCall":
		return "tool"
	default:
		return ""
	}
}

func memoryRolloutTimestampCandidates(memory map[string][]byte, prefix string, limit int, through int64) []time.Time {
	var keys []string
	last := ""
	if through >= 0 {
		last = fmt.Sprintf("%s%016x", prefix, through)
	}
	for key := range memory {
		if strings.HasPrefix(key, prefix) && (last == "" || key <= last) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) > limit {
		keys = keys[:limit]
	}
	result := make([]time.Time, 0, len(keys))
	for _, key := range keys {
		var stamp time.Time
		if json.Unmarshal(memory[key], &stamp) != nil {
			return nil
		}
		result = append(result, stamp)
	}
	return result
}

// Read only the bounded header of a large record. Canonical event message
// bodies may be skipped, but large response mirrors and tool output have no
// effect on the canonical message cardinality.
func rolloutTimestampPrefixKind(prefix []byte) (kind, event string) {
	dec := json.NewDecoder(bytes.NewReader(prefix))
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		return "", ""
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return kind, ""
		}
		switch key {
		case "type":
			if dec.Decode(&kind) != nil {
				return "", ""
			}
		case "payload":
			if token, err := dec.Token(); err != nil || token != json.Delim('{') {
				return kind, ""
			}
			for dec.More() {
				field, err := dec.Token()
				if err != nil {
					return kind, ""
				}
				if field == "type" {
					_ = dec.Decode(&event)
					return kind, event
				}
				var ignored json.RawMessage
				if dec.Decode(&ignored) != nil {
					return kind, ""
				}
			}
			return kind, ""
		default:
			var ignored json.RawMessage
			if dec.Decode(&ignored) != nil {
				return kind, ""
			}
		}
	}
	return kind, ""
}
