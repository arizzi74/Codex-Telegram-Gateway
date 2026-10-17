package worker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/workerdb"
	"golang.org/x/sys/unix"
)

// A completed temporary inode is durably identified before publication. This
// distinguishes our own retry from an unrelated file with the same name/bytes,
// including the crash window between publishing a file and recording a result.
type inboundFileRecord struct {
	Directory string `json:"directory"`
	Name      string `json:"name"`
	Temporary string `json:"temporary"`
	Device    uint64 `json:"device"`
	Inode     uint64 `json:"inode"`
	Digest    string `json:"digest"`
	Prompt    string `json:"prompt,omitempty"`
	Published bool   `json:"published,omitempty"`
}

func (f *inboundFileRecord) savedPath() string {
	if f == nil || !f.Published {
		return ""
	}
	return filepath.Join(f.Directory, "inbound", f.Name)
}

func (s *Store) markInboundFileSaved(commandID string) error {
	return s.db.Update(func(tx *workerdb.Tx) error {
		bucket := tx.Bucket(bucketCommands)
		record, err := decodeCommand(bucket.Get([]byte(commandID)))
		if err != nil {
			return err
		}
		if record.InboundFile == nil {
			return errors.New("inbound file is not recorded")
		}
		record.InboundFile.Published = true
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(commandID), encoded)
	})
}

func (s *Store) recordInboundFile(commandID string, file *inboundFileRecord) error {
	return s.db.Update(func(tx *workerdb.Tx) error {
		bucket := tx.Bucket(bucketCommands)
		value := bucket.Get([]byte(commandID))
		if value == nil {
			return errors.New("inbound file command is not recorded")
		}
		record, err := decodeCommand(value)
		if err != nil {
			return err
		}
		if record.InboundFile != nil {
			return errors.New("inbound file is already recorded")
		}
		record.InboundFile = file
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(commandID), encoded)
	})
}

func inboundFilename(name, commandID string) string {
	clean := strings.Map(func(ch rune) rune {
		if unicode.IsLetter(ch) || unicode.IsDigit(ch) || ch == '.' || ch == '-' || ch == '_' {
			return ch
		}
		return '_'
	}, name)
	clean = strings.Trim(clean, "._-")
	if clean == "" {
		clean = "file"
	}
	ext := filepath.Ext(clean)
	if len(ext) > 32 {
		ext = ""
	}
	stem := strings.TrimSuffix(clean, ext)
	if len(stem) > 150 {
		stem = stem[:150]
		for !utf8.ValidString(stem) {
			stem = stem[:len(stem)-1]
		}
	}
	return stem + "-" + commandID + ext
}

// fileWorkspace checks the frozen execution target even when a queued command
// starts after discovery has changed the session's working directory.
func (s *sessionActor) fileWorkspace(c protocol.Command) (string, error) {
	if len(c.Arguments.Files) == 0 {
		return "", nil
	}
	if c.Arguments.CWD == "" || c.Arguments.CWD != s.session.CWD {
		return "", errors.New("The session workspace changed before this file could be saved. Select the session again and resend the file.")
	}
	cwd, err := auth.CanonicalWorkspace(c.Arguments.CWD, s.agent.cfg.AllowedWorkspaceRoots)
	if err != nil || cwd != c.Arguments.CWD {
		return "", errors.New("The session workspace is not available or allowed.")
	}
	return cwd, nil
}

func (s *sessionActor) saveInboundFile(c protocol.Command) (protocol.Command, string, error) {
	cwd, err := s.fileWorkspace(c)
	if err != nil {
		return c, "", err
	}
	path, prompt, err := storeInboundFile(s.agent.store, c, cwd, s.agent.cfg.AllowedWorkspaceRoots)
	if err != nil {
		return c, "", err
	}
	if c.Operation != protocol.StoreFiles {
		c.Arguments.Text = prompt
	}
	return c, path, nil
}

func storeInboundFile(store *Store, c protocol.Command, cwd string, roots []string) (string, string, error) {
	if err := protocol.ValidateFiles(c.Arguments.Files); err != nil || len(c.Arguments.Files) != 1 {
		return "", "", errors.New("The file is invalid.")
	}
	if _, err := uuid.Parse(c.ID); err != nil {
		return "", "", errors.New("The file command identity is invalid.")
	}
	workspace, err := openWebUIFileDirectory(cwd, roots)
	if err != nil {
		return "", "", errors.New("The worker cannot open this session's workspace.")
	}
	defer workspace.Close()
	if err := unix.Mkdirat(int(workspace.Fd()), "inbound", 0o700); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			return "", "", errors.New("The worker cannot create the inbound directory.")
		}
	} else if err := workspace.Sync(); err != nil {
		return "", "", errors.New("The worker cannot persist the inbound directory.")
	}
	fd, err := unix.Openat(int(workspace.Fd()), "inbound", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_DIRECTORY, 0)
	if err != nil {
		return "", "", errors.New("The inbound path must be a regular directory without symlinks.")
	}
	directory := os.NewFile(uintptr(fd), "inbound")
	defer directory.Close()
	var directoryStat unix.Stat_t
	if err := unix.Fstat(fd, &directoryStat); err != nil || directoryStat.Uid != uint32(os.Geteuid()) {
		return "", "", errors.New("The worker cannot use this inbound directory.")
	}
	fence := func() error {
		current, err := openWebUIFileDirectory(cwd, roots)
		if err != nil {
			return errors.New("The session workspace changed while saving the file.")
		}
		defer current.Close()
		before, beforeErr := workspace.Stat()
		after, afterErr := current.Stat()
		var inbound unix.Stat_t
		if beforeErr != nil || afterErr != nil || !os.SameFile(before, after) ||
			unix.Fstatat(int(current.Fd()), "inbound", &inbound, unix.AT_SYMLINK_NOFOLLOW) != nil ||
			inbound.Mode&unix.S_IFMT != unix.S_IFDIR || inbound.Dev != directoryStat.Dev || inbound.Ino != directoryStat.Ino {
			return errors.New("The session workspace or inbound directory changed while saving the file.")
		}
		return nil
	}
	if err := fence(); err != nil {
		return "", "", err
	}
	input := c.Arguments.Files[0]
	digest := sha256.Sum256(input.Data)
	name := inboundFilename(input.Name, c.ID)
	path := filepath.Join(cwd, "inbound", name)
	record, found, err := store.LoadCommand(c.ID)
	if err != nil || !found {
		return "", "", errors.New("The worker cannot read the file command record.")
	}
	meta := record.InboundFile
	if meta == nil {
		var existing unix.Stat_t
		if err := unix.Fstatat(fd, name, &existing, unix.AT_SYMLINK_NOFOLLOW); !errors.Is(err, unix.ENOENT) {
			return "", "", errors.New("A file already exists at the upload destination. Resend the file to use another name.")
		}
		temporary := ".telegramgw-" + uuid.NewString() + ".tmp"
		tempFD, err := unix.Openat(fd, temporary, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if err != nil {
			return "", "", errors.New("The worker cannot create the uploaded file.")
		}
		temp := os.NewFile(uintptr(tempFD), temporary)
		owned := false
		defer func() {
			temp.Close()
			if !owned {
				_ = unix.Unlinkat(fd, temporary, 0)
			}
		}()
		var stat unix.Stat_t
		if _, err := temp.Write(input.Data); err != nil {
			return "", "", errors.New("The worker cannot write the uploaded file.")
		}
		if temp.Sync() != nil || unix.Fstat(tempFD, &stat) != nil || directory.Sync() != nil {
			return "", "", errors.New("The worker cannot persist the uploaded file.")
		}
		meta = &inboundFileRecord{Directory: cwd, Name: name, Temporary: temporary, Device: uint64(stat.Dev), Inode: stat.Ino, Digest: hex.EncodeToString(digest[:])}
		if c.Operation != protocol.StoreFiles {
			meta.Prompt = c.Arguments.Text + "\nFile: " + path
		}
		if err := store.recordInboundFile(c.ID, meta); err != nil {
			return "", "", errors.New("The worker cannot record the uploaded file.")
		}
		owned = true
	}
	if meta.Directory != cwd || meta.Name != name || meta.Digest != hex.EncodeToString(digest[:]) ||
		!strings.HasPrefix(meta.Temporary, ".telegramgw-") || filepath.Base(meta.Temporary) != meta.Temporary {
		return "", "", errors.New("The saved upload no longer matches this command.")
	}
	// Open both names without following symlinks. Only an inode recorded after
	// its full contents were synced may be published or reused.
	verify := func(name string) (*os.File, error) {
		fileFD, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
		if err != nil {
			return nil, err
		}
		file := os.NewFile(uintptr(fileFD), name)
		var stat unix.Stat_t
		if unix.Fstat(fileFD, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0o777 != 0o600 || stat.Uid != uint32(os.Geteuid()) || uint64(stat.Dev) != meta.Device || stat.Ino != meta.Inode || stat.Size != int64(len(input.Data)) {
			file.Close()
			return nil, errors.New("saved upload identity changed")
		}
		data, err := io.ReadAll(io.LimitReader(file, int64(len(input.Data))+1))
		if err != nil || !bytes.Equal(data, input.Data) {
			file.Close()
			return nil, errors.New("saved upload content changed")
		}
		return file, nil
	}
	final, err := verify(name)
	if errors.Is(err, unix.ENOENT) {
		temp, tempErr := verify(meta.Temporary)
		if tempErr != nil {
			return "", "", errors.New("The saved upload is unavailable or changed. Resend the file.")
		}
		temp.Close()
		if err := fence(); err != nil {
			return "", "", err
		}
		if err := unix.Linkat(fd, meta.Temporary, fd, name, 0); err != nil {
			return "", "", errors.New("The upload destination already exists or cannot be created. Resend the file.")
		}
		final, err = verify(name)
	}
	if err != nil {
		return "", "", errors.New("The saved upload is unavailable or changed. Resend the file.")
	}
	defer final.Close()
	if temp, err := verify(meta.Temporary); err == nil {
		temp.Close()
		if err := unix.Unlinkat(fd, meta.Temporary, 0); err != nil {
			return "", "", errors.New("The worker cannot finalize the uploaded file.")
		}
	} else if !errors.Is(err, unix.ENOENT) {
		return "", "", errors.New("The saved upload temporary file changed. Resend the file.")
	}
	info, err := final.Stat()
	if err != nil || !singleLinkedWebUIFile(info) || directory.Sync() != nil {
		return "", "", errors.New("The saved upload could not be secured or persisted.")
	}
	if err := fence(); err != nil {
		return "", "", err
	}
	if err := store.markInboundFileSaved(c.ID); err != nil {
		return "", "", errors.New("The worker cannot record the completed file save.")
	}
	return path, meta.Prompt, nil
}
