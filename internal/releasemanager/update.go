package releasemanager

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"modernc.org/sqlite"
)

// sqliteBackup uses SQLite's online backup API, including committed WAL pages.
// Copying the database file alone can silently omit accepted writes.
func sqliteBackup(ctx context.Context, database, backup string) error {
	f, err := os.OpenFile(backup, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	u := url.URL{Scheme: "file", Path: database, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	err = conn.Raw(func(driverConn any) (retErr error) {
		source, ok := driverConn.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("SQLite driver does not support online backups")
		}
		copy, err := source.NewBackup((&url.URL{Scheme: "file", Path: backup}).String())
		if err != nil {
			return err
		}
		defer func() { retErr = errors.Join(retErr, copy.Finish()) }()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			more, err := copy.Step(256)
			if err != nil || !more {
				return err
			}
		}
	})
	if err != nil {
		return err
	}
	check, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: backup, RawQuery: "mode=ro"}).String())
	if err != nil {
		return err
	}
	defer check.Close()
	rows, err := check.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return err
		}
		if result != "ok" {
			return errors.New("SQLite backup integrity check failed")
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count != 1 {
		return errors.New("SQLite backup integrity check failed")
	}
	f, err = os.Open(backup)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

type updateDatabase struct {
	root    *os.Root
	path    string
	rel     string
	existed bool
	mode    os.FileMode
	owner   *Ownership
}

func openGatewayDatabase(l *Layout) (*updateDatabase, error) {
	cfg, err := ReadJSON(l.Config)
	if err != nil {
		return nil, err
	}
	path, _ := cfg["database_path"].(string)
	if _, postgres := cfg["database_url_env"]; postgres || path == "" {
		return nil, errors.New("this updater only supports a SQLite gateway")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(filepath.Dir(l.Config), path)
	}
	path = filepath.Clean(path)
	dataRoot := l.DataRoot
	if dataRoot == "" {
		dataRoot = GatewayDataRoot
	}
	rootInfo, err := os.Lstat(dataRoot)
	if err != nil {
		return nil, err
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("gateway data directory must be a real directory, not a symbolic link")
	}
	rel, err := filepath.Rel(dataRoot, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return nil, errors.New("gateway database must be a regular SQLite file inside its data directory")
	}
	root, err := os.OpenRoot(dataRoot)
	if err != nil {
		return nil, err
	}
	database := &updateDatabase{root: root, path: path, rel: rel, mode: 0o600}
	info, err := root.Lstat(rel)
	if err == nil {
		if !info.Mode().IsRegular() {
			root.Close()
			return nil, errors.New("gateway database must be a regular SQLite file inside its data directory")
		}
		database.existed = true
		database.mode = info.Mode().Perm()
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			database.owner = &Ownership{UID: int(stat.Uid), GID: int(stat.Gid)}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		root.Close()
		return nil, err
	}
	// Resolve parents through Root as well, so an existing parent symlink cannot
	// point outside the data directory even when the database does not exist yet.
	parent, err := root.Open(filepath.Dir(rel))
	if err != nil {
		root.Close()
		return nil, err
	}
	parent.Close()
	if err := database.validateSourcePaths(); err != nil {
		root.Close()
		return nil, err
	}
	return database, nil
}

func (database *updateDatabase) validateSourcePaths() error {
	openedRoot, err := database.root.Stat(".")
	if err != nil {
		return err
	}
	currentRoot, err := os.Lstat(database.root.Name())
	if err != nil || !currentRoot.IsDir() || !os.SameFile(openedRoot, currentRoot) {
		return errors.New("gateway data directory changed during the update")
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		info, err := database.root.Lstat(database.rel + suffix)
		if errors.Is(err, os.ErrNotExist) {
			if suffix == "" && database.existed {
				return errors.New("gateway database disappeared during the update")
			}
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("gateway database and sidecars must be regular files, not symbolic links")
		}
	}
	return nil
}

func randomSuffix() (string, error) {
	var buf [12]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

// restoreSQLite keeps every mutation beneath the directory opened before the
// update. A service-owned directory swapped for a symlink cannot redirect root
// writes elsewhere. Ownership changes apply to the open temporary file only.
func restoreSQLite(database *updateDatabase, backup string) error {
	var source *os.File
	if database.existed {
		var err error
		source, err = os.Open(backup)
		if err != nil {
			return err
		}
		defer source.Close()
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if err := database.root.Remove(database.rel + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if !database.existed {
		err := database.root.Remove(database.rel)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	suffix, err := randomSuffix()
	if err != nil {
		return err
	}
	temporary := filepath.Join(filepath.Dir(database.rel), ".gateway-restore-"+suffix)
	f, err := database.root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer database.root.Remove(temporary)
	defer f.Close()
	if _, err := io.Copy(f, source); err != nil {
		return err
	}
	if err := f.Chmod(database.mode); err != nil {
		return err
	}
	if os.Geteuid() == 0 && database.owner != nil {
		if err := f.Chown(database.owner.UID, database.owner.GID); err != nil {
			return err
		}
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := database.root.Rename(temporary, database.rel); err != nil {
		return err
	}
	dir, err := database.root.Open(filepath.Dir(database.rel))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func serviceDetails(data []byte) map[string]string {
	values := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			values[key] = value
		}
	}
	return values
}

func (m *Manager) gatewayServiceActive(ctx context.Context, l *Layout) bool {
	data, err := m.command(ctx, "systemctl", "show", "codex-gateway.service", "-p", "MainPID", "-p", "ActiveState")
	if err != nil {
		return false
	}
	values := serviceDetails(data)
	pid, err := strconv.Atoi(values["MainPID"])
	if err != nil || pid <= 0 || values["ActiveState"] != "active" {
		return false
	}
	executable, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil {
		return false
	}
	installed, err := os.Stat(l.Binary)
	return err == nil && os.SameFile(executable, installed)
}

func (m *Manager) readyContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := m.ReadyTimeout
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	return context.WithTimeout(ctx, timeout)
}

func (m *Manager) readyPause(ctx context.Context) error {
	interval := m.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (m *Manager) updateNow() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) GatewayReady(ctx context.Context, l *Layout) error {
	cfg, err := ReadJSON(l.Config)
	if err != nil {
		return err
	}
	listen, _ := cfg["listen"].(string)
	if listen == "" {
		listen = "127.0.0.1:8080"
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return errors.New("invalid gateway listen address")
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	address := "http://" + net.JoinHostPort(host, port) + "/tgw/readyz"
	ctx, cancel := m.readyContext(ctx)
	defer cancel()
	client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for ctx.Err() == nil {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK && m.gatewayServiceActive(ctx, l) {
				return nil
			}
		}
		if m.readyPause(ctx) != nil {
			break
		}
	}
	return errors.New("gateway did not become ready; new database and binaries were retained; inspect service logs")
}

func (m *Manager) workerRunning(ctx context.Context, l *Layout) (bool, error) {
	if l.System == "linux" {
		data, err := m.command(ctx, "systemctl", "--user", "show", "codex-worker.service", "-p", "MainPID", "-p", "ActiveState")
		if err != nil {
			return false, err
		}
		values := serviceDetails(data)
		pid, err := strconv.Atoi(values["MainPID"])
		if err == nil && values["ActiveState"] == "active" && pid > 0 {
			return true, nil
		}
		if err != nil || (values["ActiveState"] != "inactive" && values["ActiveState"] != "failed") || pid != 0 {
			return false, &BusyError{Reason: "worker service is changing state; retry the update later"}
		}
	} else {
		result, err := m.Run(ctx, "launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())+"/com.iaia.codex-worker")
		if err != nil {
			return false, err
		}
		if result.ExitCode == 0 {
			return true, nil
		}
	}
	result, err := m.Run(ctx, l.Binary, "--config", l.Config, "status")
	if err != nil {
		return false, err
	}
	if result.ExitCode == 0 {
		var status struct {
			PID int `json:"pid"`
		}
		if json.Unmarshal(result.Output, &status) != nil || status.PID <= 0 {
			return false, &BusyError{Reason: "cannot confirm that the old worker process has stopped"}
		}
		err := syscall.Kill(status.PID, 0)
		if errors.Is(err, syscall.ESRCH) {
			return false, nil
		}
		if err != nil {
			return false, &BusyError{Reason: "cannot confirm that the old worker process has stopped"}
		}
		return false, &BusyError{Reason: "a worker process is still running outside the managed service"}
	}
	cfg, err := ReadJSON(l.Config)
	if err != nil {
		return false, err
	}
	state, _ := cfg["state_file"].(string)
	if state == "" {
		return false, &BusyError{Reason: "cannot confirm the stopped worker status"}
	}
	if !filepath.IsAbs(state) {
		state = filepath.Join(filepath.Dir(l.Config), state)
	}
	if _, err := os.Lstat(state + ".status.json"); !errors.Is(err, os.ErrNotExist) {
		return false, &BusyError{Reason: "cannot confirm the stopped worker status"}
	}
	return false, nil
}

type workerLease struct {
	WorkerID  string    `json:"worker_id"`
	PID       int       `json:"pid"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (m *Manager) workerAbort(ctx context.Context, l *Layout, token string) error {
	_, err := m.command(ctx, l.Binary, "--config", l.Config, "update", "abort", "--token", token)
	return err
}

const workerPrepareUnknownReason = "worker update deferred: running/waiting work or no safe update support; see installation documentation"

// A failed worker command may contain private configuration or native runtime
// output. Accept only the prepare command's bounded error object, and translate
// known fixed worker diagnostics instead of forwarding any subprocess output.
func workerPrepareFailure(output []byte) string {
	if len(output) > 1024 {
		return workerPrepareUnknownReason
	}
	var response map[string]json.RawMessage
	if json.Unmarshal(output, &response) != nil || len(response) != 1 {
		return workerPrepareUnknownReason
	}
	var reason string
	if json.Unmarshal(response["error"], &reason) != nil {
		return workerPrepareUnknownReason
	}
	switch reason {
	case "worker update: command admission is busy":
		return "worker update deferred: command admission is busy"
	case "worker update: another update is already prepared":
		return "worker update deferred: another update is already prepared"
	case "worker update: runtime startup or discovery is in progress":
		return "worker update deferred: runtime startup or discovery is in progress"
	case "worker update: runtime is not settled":
		return "worker update deferred: runtime is still starting or degraded"
	case "worker update: runtime attachment admission is not managed":
		return "worker update deferred: runtime attachment admission is not managed"
	case "worker update: attachment proxy is closed":
		return "worker update deferred: runtime attachment proxy is closed"
	case "worker update: a local CLI is attached":
		return "worker update deferred: a local CLI is attached"
	case "worker update: this runtime was used by a native CLI; finish native work and stop the worker service before updating":
		return "worker update deferred: the installed worker cannot verify a runtime previously used by a native CLI"
	case "worker update: runtime has pending or unconfirmed RPCs; finish work and stop the worker service before updating":
		return "worker update deferred: runtime has pending or unconfirmed requests"
	case "worker update: native CLI requests are still in flight":
		return "worker update deferred: native CLI requests are still in flight"
	case "worker update: native CLI approvals or input are pending":
		return "worker update deferred: native CLI approvals or input are pending"
	case "worker update: native CLI queued work is pending or cannot be verified":
		return "worker update deferred: native CLI queued work is pending or cannot be verified"
	case "worker update: native CLI activity could not be verified":
		return "worker update deferred: native CLI activity could not be verified"
	case "worker update: native CLI activity changed during idle verification; retry when settled":
		return "worker update deferred: native CLI activity changed during idle verification; retrying at the next check"
	case "worker update: native CLI connection is still initializing":
		return "worker update deferred: native CLI connection is still initializing"
	case "worker update: a native CLI thread is active or its idle state cannot be verified":
		return "worker update deferred: a native CLI thread is active or its idle state cannot be verified"
	case "worker update: runtime RPC state changed while checking idle":
		return "worker update deferred: runtime request state changed while checking idle"
	case "worker update: session is busy":
		return "worker update deferred: a session is busy"
	case "worker update: session has active or queued work", "worker update: commands are queued or executing":
		return "worker update deferred: commands are queued or executing"
	case "worker update: a session has an active turn or pending response":
		return "worker update deferred: a session has an active turn or pending response"
	case "worker update: durable events are awaiting gateway acknowledgement":
		return "worker update deferred: events are awaiting gateway acknowledgement"
	case "worker update: too many loaded threads to verify", "worker update: repeated loaded-thread cursor":
		return "worker update deferred: loaded threads could not be verified"
	case "worker update: too many native queues to verify":
		return "worker update deferred: too many native queues to verify"
	case "worker update coordination unavailable; the running worker must support update prepare":
		return "worker update deferred: the running worker does not provide update coordination"
	default:
		if strings.HasPrefix(reason, "worker update: native CLI activity could not be verified: ") {
			// The worker logs its own classified protocol reason. Never forward
			// arbitrary subprocess output or protocol method names here.
			return "worker update deferred: native CLI activity could not be verified; see worker service log for the protocol reason"
		}
		if strings.HasPrefix(reason, "worker update: native CLI activity needs fresh verification: ") {
			return "worker update deferred: native CLI activity needs fresh verification"
		}
		return workerPrepareUnknownReason
	}
}

func (m *Manager) workerPrepare(ctx context.Context, l *Layout) (_ *workerLease, retErr error) {
	result, err := m.Run(ctx, l.Binary, "--config", l.Config, "update", "prepare")
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, &BusyError{Reason: workerPrepareFailure(result.Output)}
	}
	// Decode the token separately: an invalid timestamp/identity still needs to
	// release an otherwise valid reservation instead of pausing work until expiry.
	var token struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(result.Output, &token)
	defer func() {
		if retErr != nil && token.Token != "" {
			abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			defer cancel()
			if err := m.workerAbort(abortCtx, l, token.Token); err != nil {
				retErr = errors.Join(retErr, errors.New("worker reservation could not be released; it will expire automatically"))
			}
		}
	}()
	invalid := errors.New("worker did not acknowledge preparation for the managed service process")
	var lease workerLease
	if json.Unmarshal(result.Output, &lease) != nil || lease.PID <= 0 || lease.Token == "" || lease.ExpiresAt.Sub(m.updateNow()) < 45*time.Second {
		return nil, invalid
	}
	cfg, err := ReadJSON(l.Config)
	if err != nil {
		return nil, err
	}
	workerID, _ := cfg["worker_id"].(string)
	if workerID == "" || lease.WorkerID != workerID {
		return nil, invalid
	}
	if l.System == "linux" {
		data, err := m.command(ctx, "systemctl", "--user", "show", "codex-worker.service", "-p", "MainPID", "--value")
		if err != nil {
			return nil, invalid
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || pid != lease.PID {
			return nil, invalid
		}
	} else {
		data, err := m.command(ctx, "launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())+"/com.iaia.codex-worker")
		if err != nil {
			return nil, invalid
		}
		matches := 0
		for _, line := range strings.Split(string(data), "\n") {
			key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
			if !ok || strings.TrimSpace(key) != "pid" {
				continue
			}
			pid, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || pid != lease.PID {
				return nil, invalid
			}
			matches++
		}
		if matches != 1 {
			return nil, invalid
		}
	}
	if lease.ExpiresAt.Sub(m.updateNow()) < 45*time.Second {
		return nil, invalid
	}
	return &lease, nil
}

func (m *Manager) WorkerReady(ctx context.Context, l *Layout, after time.Time, expectedVersion string) error {
	cfg, err := ReadJSON(l.Config)
	if err != nil {
		return err
	}
	expectedCount := 0
	if runtimes, ok := cfg["runtimes"].([]any); ok {
		for _, value := range runtimes {
			if runtime, ok := value.(map[string]any); ok && runtime["autostart"] == true {
				expectedCount++
			}
		}
	}
	ctx, cancel := m.readyContext(ctx)
	defer cancel()
	for ctx.Err() == nil {
		data, err := m.command(ctx, l.Binary, "--config", l.Config, "status")
		var value struct {
			UpdatedAt time.Time `json:"updated_at"`
			Connected bool      `json:"gateway_connected"`
			Version   string    `json:"version"`
			Runtimes  []struct {
				State string `json:"state"`
			} `json:"runtimes"`
		}
		if err == nil && json.Unmarshal(data, &value) == nil && !value.UpdatedAt.Before(after) && value.Connected && len(value.Runtimes) == expectedCount {
			ready := true
			if expectedVersion != "" {
				comparison, err := CompareVersions(value.Version, expectedVersion)
				ready = err == nil && comparison == 0
			}
			for _, runtime := range value.Runtimes {
				ready = ready && runtime.State == "running"
			}
			if ready {
				return nil
			}
		}
		if m.readyPause(ctx) != nil {
			break
		}
	}
	return errors.New("worker did not become ready; inspect its service logs")
}

type updateSnapshot struct {
	path, backup, link string
	existed            bool
	mode               os.FileMode
	owner              *Ownership
}

func snapshotUpdateFile(path, backup string) (*updateSnapshot, error) {
	snapshot := &updateSnapshot{path: path, backup: backup}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return snapshot, nil
	}
	if err != nil {
		return nil, err
	}
	snapshot.existed, snapshot.mode = true, info.Mode().Perm()
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		snapshot.owner = &Ownership{UID: int(stat.Uid), GID: int(stat.Gid)}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		snapshot.link, err = os.Readlink(path)
		return snapshot, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("updater destination is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if err := AtomicWrite(backup, data, 0o600, nil); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (snapshot *updateSnapshot) restore() error {
	if !snapshot.existed {
		err := os.Remove(snapshot.path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if snapshot.link != "" {
		suffix, err := randomSuffix()
		if err != nil {
			return err
		}
		temporary := filepath.Join(filepath.Dir(snapshot.path), ".restore-link-"+suffix)
		if err := os.Symlink(snapshot.link, temporary); err != nil {
			return err
		}
		defer os.Remove(temporary)
		if err := os.Rename(temporary, snapshot.path); err != nil {
			return err
		}
		return SyncDir(filepath.Dir(snapshot.path))
	}
	data, err := os.ReadFile(snapshot.backup)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(snapshot.path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(snapshot.path); err != nil {
			return err
		}
	}
	return AtomicWrite(snapshot.path, data, snapshot.mode, snapshot.owner)
}

// ApplyUpdate rolls back only before attempting to start the new service. Once
// started, its database and binaries must survive even a failed readiness check.
func (m *Manager) ApplyUpdate(ctx context.Context, l *Layout, packages map[string]string, release *Release, fresh bool) (retErr error) {
	started := false
	defer func() {
		if retErr != nil {
			retErr = &ApplyError{Err: retErr, ServiceStarted: started}
		}
	}()
	if err := os.MkdirAll(l.Bin, 0o755); err != nil {
		return err
	}
	if l.Component == "gateway" {
		if err := os.Chmod(l.Bin, 0o755); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(l.Backups, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(l.Backups, 0o700); err != nil {
		return err
	}
	backup, err := os.MkdirTemp(l.Backups, m.updateNow().UTC().Format("20060102T150405Z")+"-")
	if err != nil {
		return err
	}
	sources := []struct{ destination, source string }{
		{l.Binary, filepath.Join(packages[l.Component], filepath.Base(l.Binary))},
		{l.Manager, filepath.Join(packages[l.Component], "codex-telegramgw")},
	}
	if l.Component == "worker" {
		sources = append(sources, struct{ destination, source string }{filepath.Join(l.Bin, "codex-local"), filepath.Join(packages["local"], "codex-local")})
	}
	paths := []string{l.State, l.Command}
	for _, source := range sources {
		paths = append(paths, source.destination)
	}
	snapshots := make(map[string]*updateSnapshot)
	for index, path := range paths {
		if _, exists := snapshots[path]; exists {
			continue
		}
		snapshot, err := snapshotUpdateFile(path, filepath.Join(backup, fmt.Sprintf("%02d-%s", index, filepath.Base(path))))
		if err != nil {
			return err
		}
		snapshots[path] = snapshot
	}
	var database *updateDatabase
	if l.Component == "gateway" {
		database, err = openGatewayDatabase(l)
		if err != nil {
			return err
		}
		defer database.root.Close()
	}
	changed := make(map[string]bool)
	var prepared *workerLease
	stopped, databaseMutated := false, false
	err = func() error {
		running := !fresh
		if !fresh && l.Component == "worker" {
			var err error
			running, err = m.workerRunning(ctx, l)
			if err != nil {
				return err
			}
			if running {
				prepared, err = m.workerPrepare(ctx, l)
				if err != nil {
					return err
				}
			}
		}
		if prepared != nil && prepared.ExpiresAt.Sub(m.updateNow()) < 45*time.Second {
			return &BusyError{Reason: "worker update reservation expired; retry later"}
		}
		if running {
			stopped = true
			if err := m.Service(ctx, l, "stop"); err != nil {
				return err
			}
		}
		if database != nil && database.existed {
			if err := database.validateSourcePaths(); err != nil {
				return err
			}
			if err := backupSQLiteFromRoot(ctx, database, filepath.Join(backup, "gateway.db")); err != nil {
				return err
			}
		}
		for _, source := range sources {
			changed[source.destination] = true
			if err := AtomicCopy(source.source, source.destination, 0o755); err != nil {
				return err
			}
		}
		changed[l.Command] = true
		if err := m.InstallManager(l, filepath.Join(packages[l.Component], "codex-telegramgw")); err != nil {
			return err
		}
		if database != nil {
			databaseMutated = true
			if _, err := m.command(ctx, "runuser", "-u", "codexgateway", "--", l.Binary, "--config", l.Config, "migrate"); err != nil {
				return err
			}
		}
		changed[l.State] = true
		if err := WriteJSON(l.State, map[string]any{"schema": 1, "component": l.Component, "repo": release.Repo, "version": release.Tag, "pending": true}); err != nil {
			return err
		}
		// Even a failed start command can have launched a service that wrote data.
		started = true
		after := m.updateNow()
		if err := m.Service(ctx, l, "start"); err != nil {
			return err
		}
		if l.Component == "gateway" {
			if err := m.GatewayReady(ctx, l); err != nil {
				return err
			}
		} else {
			if err := m.WorkerReady(ctx, l, after, release.Tag); err != nil {
				return err
			}
		}
		return SaveSettings(l, release.Repo, release.Tag)
	}()
	if err != nil {
		if started {
			if m.Out != nil {
				fmt.Fprintf(m.Out, "New service startup was attempted. No automatic rollback was performed; backup: %s\n", backup)
			}
			return err
		}
		var rollbackErr error
		for _, path := range paths {
			if changed[path] {
				rollbackErr = errors.Join(rollbackErr, snapshots[path].restore())
				delete(changed, path)
			}
		}
		if databaseMutated {
			rollbackErr = errors.Join(rollbackErr, restoreSQLite(database, filepath.Join(backup, "gateway.db")))
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
		defer cancel()
		if rollbackErr == nil && stopped {
			rollbackErr = m.Service(cleanupCtx, l, "start")
		} else if !stopped && prepared != nil {
			rollbackErr = errors.Join(rollbackErr, m.workerAbort(cleanupCtx, l, prepared.Token))
		}
		if rollbackErr != nil {
			return errors.Join(err, fmt.Errorf("rollback could not complete; inspect private backup %s: %w", backup, rollbackErr))
		}
		return err
	}
	if m.Out != nil {
		fmt.Fprintf(m.Out, "Installed %s %s. Backup: %s\n", l.Component, release.Tag, backup)
	}
	return nil
}
