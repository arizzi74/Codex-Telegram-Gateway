package admin

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/gateway"
	"github.com/iaia/telegramgw/internal/protocol"
	"github.com/iaia/telegramgw/internal/registry"
)

// Files use authenticated POSTs and transient worker streams. Neither paths
// nor file contents are put in the command/event database or URL query string.
func (s *Server) webuiFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		method(w)
		return
	}
	if _, ok := s.requireAuth(w, r, true); !ok {
		return
	}
	var in struct {
		SessionID string `json:"session_id"`
		Path      string `json:"path"`
		Download  bool   `json:"download"`
	}
	if !decode(w, r, &in) {
		return
	}
	id, err := uuid.Parse(in.SessionID)
	request := protocol.WebUIFileRequest{Path: in.Path}
	_, pathErr := protocol.NormalizeWebUIFilePath(in.Path)
	if err != nil || id == uuid.Nil || pathErr != nil {
		bad(w)
		return
	}
	if s.webui == nil {
		fileFailure(w, 503, "File downloads are unavailable.")
		return
	}
	d, err := s.store.WebUIInventorySnapshot(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	var target *registry.AdminSession
	for i := range d.Sessions {
		if d.Sessions[i].ID == id.String() {
			target = &d.Sessions[i]
			break
		}
	}
	if target == nil {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	stream, err := s.webui.OpenWebUIFile(ctx, *target, request)
	if err != nil {
		message := "Worker is disconnected. Try again when it is online."
		if errors.Is(err, gateway.ErrWebUIFilesUnsupported) || errors.Is(err, gateway.ErrWebUILimit) {
			message = err.Error()
		}
		fileFailure(w, 503, message)
		return
	}
	defer stream.Close()
	authCookie, _ := r.Cookie(adminCookie)
	check := func() error {
		checkCtx, done := context.WithTimeout(ctx, 5*time.Second)
		defer done()
		if _, err := s.store.ValidateAdminSession(checkCtx, authCookie.Value); err != nil {
			return err
		}
		if err := stream.Check(checkCtx); err != nil {
			return err
		}
		return s.store.CheckWebUIFileTarget(checkCtx, *target)
	}
	read := func() (protocol.WebUIFrame, error) {
		timer := time.NewTimer(15 * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return protocol.WebUIFrame{}, ctx.Err()
		case <-stream.Done():
			return protocol.WebUIFrame{}, errors.New("Worker connection lost.")
		case <-timer.C:
			return protocol.WebUIFrame{}, errors.New("Worker took too long to read the file. Please try again.")
		case frame := <-stream.Frames():
			stream.Consumed(frame)
			if err := check(); err != nil {
				return protocol.WebUIFrame{}, errors.New("The session or worker is no longer available. Reconnect and try again.")
			}
			if frame.Action == "error" {
				// Worker errors are text only; retain normal secret redaction.
				data, err := s.redactWebUI(json.RawMessage(strconv.Quote(frame.Error)))
				var message string
				if err == nil {
					_ = json.Unmarshal(data, &message)
				}
				if message == "" {
					message = "The file is unavailable in this session's project."
				}
				return protocol.WebUIFrame{}, errors.New(message)
			}
			return frame, nil
		}
	}
	frame, err := read()
	if err != nil {
		fileFailure(w, 409, err.Error())
		return
	}
	var info protocol.WebUIFileInfo
	if frame.Action != "ready" || json.Unmarshal(frame.Data, &info) != nil || !validFileInfo(info) {
		fileFailure(w, 502, "Worker returned invalid file information.")
		return
	}
	if !in.Download {
		writeJSON(w, http.StatusOK, info)
		return
	}
	// Ask for one chunk at a time. A slow browser cannot make the gateway or
	// worker buffer the entire artifact, or queue an unbounded series of frames.
	var offset int64
	started := false
	defer func() { _ = http.NewResponseController(w).SetWriteDeadline(time.Time{}) }()
	abort := func(message string) {
		if started {
			panic(http.ErrAbortHandler)
		}
		fileFailure(w, 409, message)
	}
	for {
		if err := check(); err != nil {
			abort("The session or worker is no longer available.")
			return
		}
		credit, _ := json.Marshal(protocol.WebUIFileRead{Offset: offset})
		if err := stream.Send(ctx, credit); err != nil {
			abort("Worker connection lost.")
			return
		}
		frame, err := read()
		if err != nil {
			abort(err.Error())
			return
		}
		var chunk protocol.WebUIFileChunk
		if frame.Action != "output" || json.Unmarshal(frame.Data, &chunk) != nil || chunk.Offset != offset || len(chunk.Data) > protocol.WebUIFileChunkBytes || int64(len(chunk.Data)) > info.Size-offset || (len(chunk.Data) == 0 && !chunk.EOF) || chunk.EOF != (offset+int64(len(chunk.Data)) == info.Size) {
			abort("File changed or the worker returned an incomplete download. Please try again.")
			return
		}
		if !started {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": info.Name}))
			w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
			w.Header().Set("X-Content-Type-Options", "nosniff")
			started = true
		}
		// Bound a blocked browser write as well as the worker-side credit wait.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Second))
		if _, err := w.Write(chunk.Data); err != nil {
			return
		}
		offset += int64(len(chunk.Data))
		if chunk.EOF {
			return
		}
	}
}

func validFileInfo(info protocol.WebUIFileInfo) bool {
	if info.Size < 0 || info.Size > protocol.MaxWebUIFileBytes || info.Name == "" || len(info.Name) > 255 || len(info.Path) > 4096 || !strings.HasPrefix(info.Path, "/") || strings.ContainsAny(info.Name, "/\\") || !utf8.ValidString(info.Name+info.Path) {
		return false
	}
	for _, value := range info.Name + info.Path {
		if unicode.IsControl(value) {
			return false
		}
	}
	return info.Name != "." && info.Name != ".."
}

func fileFailure(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"message": message})
}
