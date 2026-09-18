package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/iaia/telegramgw/internal/codexadapter"
	"github.com/iaia/telegramgw/internal/protocol"
)

func workerTestImage(t *testing.T) protocol.Image {
	t.Helper()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	return protocol.Image{MIMEType: "image/png", Data: data.Bytes()}
}

func TestAgentForwardsImageOnlyAndCaptionInputs(t *testing.T) {
	for _, tc := range []struct {
		name, text, active string
		op                 protocol.Operation
		method             string
	}{
		{name: "image only", op: protocol.StartTurn, method: "turn/start"},
		{name: "caption", text: "Read this diagram", op: protocol.StartTurn, method: "turn/start"},
		{name: "steer image", active: "turn-live", op: protocol.Steer, method: "turn/steer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, runtime, server, cleanup := testAgent(t)
			defer cleanup()
			session := installSession(a, runtime, "thread-image", tc.active)
			command := agentCommand(runtime, session, tc.op)
			image := workerTestImage(t)
			command.Arguments.Text, command.Arguments.Images = tc.text, []protocol.Image{image}
			if ack, err := a.HandleCommand(context.Background(), command); err != nil || ack.Status != "accepted" {
				t.Fatalf("image command ack = %#v, %v", ack, err)
			}
			waitFor(t, func() bool {
				record, found, err := a.store.LoadCommand(command.ID)
				return err == nil && found && record.State == CommandCompleted
			})
			for _, call := range server.Calls() {
				if call.Method != tc.method {
					continue
				}
				var params struct {
					Input []map[string]string `json:"input"`
				}
				if err := json.Unmarshal(call.Params, &params); err != nil {
					t.Fatal(err)
				}
				want := []map[string]string{}
				if tc.text != "" {
					want = append(want, map[string]string{"type": "text", "text": tc.text})
				}
				want = append(want, map[string]string{"type": "image", "url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(image.Data)})
				if !reflect.DeepEqual(params.Input, want) {
					t.Fatalf("Codex input = %#v, want %#v", params.Input, want)
				}
				return
			}
			t.Fatal("image command never reached Codex")
		})
	}
}

func TestHistoryFiltersGatewayImagesWithoutHidingCLIImages(t *testing.T) {
	a, runtime, _, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "thread-image-history", "")
	image := workerTestImage(t)
	for _, tc := range []struct {
		op   protocol.Operation
		text string
	}{
		{op: protocol.StartTurn},
		{op: protocol.Steer, text: "Caption"},
	} {
		command := agentCommand(runtime, session, tc.op)
		command.Arguments.Text, command.Arguments.Images = tc.text, []protocol.Image{image}
		command.ExpectedTurnID = "turn-image"
		if _, err := a.store.Receive(command); err != nil {
			t.Fatal(err)
		}
		if err := a.store.SetCommandState(command.ID, CommandCompleted, &protocol.Result{TurnID: "turn-image"}); err != nil {
			t.Fatal(err)
		}
	}
	prompts := []codexadapter.UserPrompt{
		{TurnID: "turn-image", ItemID: "gateway-image", Text: "[Image]"},
		{TurnID: "turn-image", ItemID: "gateway-caption", Text: "Caption[Image]"},
		{TurnID: "turn-image", ItemID: "cli-repeat", Text: "[Image]"},
		{TurnID: "other-turn", ItemID: "cli-caption", Text: "Caption[Image]"},
	}
	got, err := a.store.externalHistoryPrompts(runtime.ID, session.ThreadID, prompts)
	if err != nil || !reflect.DeepEqual(got, prompts[2:]) {
		t.Fatalf("external image history = %#v, %v", got, err)
	}
}

func TestLargeImageCrossesWorkerWebSocketAndCodexRPC(t *testing.T) {
	a, runtime, codex, cleanup := testAgent(t)
	defer cleanup()
	session := installSession(a, runtime, "thread-large-image", "")
	command := agentCommand(runtime, session, protocol.StartTurn)
	image := workerTestImage(t)
	// PNG decoders ignore bytes after IEND. Padding preserves a valid image
	// while crossing the former 2 MiB WSS and 64 KiB fixture scanner limits.
	image.Data = append(image.Data, make([]byte, 3<<20)...)
	command.Arguments.Text, command.Arguments.Images = "Inspect this image", []protocol.Image{image}
	store, cfg := testConnectionStore(t, runtime.WorkerID)
	defer store.Close()
	acknowledged := make(chan struct{}, 1)
	server := newWorkerTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if _, err = readWorkerEnvelope(r.Context(), conn); err != nil {
			t.Error(err)
			return
		}
		if err = serverEnvelope(r.Context(), conn, "hello_ack", protocol.HelloAck{ConnectionID: uuid.NewString(), HeartbeatIntervalSeconds: 60}); err != nil {
			t.Error(err)
			return
		}
		if err = serverEnvelope(r.Context(), conn, "command", command); err != nil {
			t.Error(err)
			return
		}
		envelope, err := readWorkerEnvelope(r.Context(), conn)
		if err != nil {
			t.Error(err)
			return
		}
		ack, err := protocol.Payload[protocol.CommandAck](envelope)
		if err != nil || envelope.Type != "command_ack" || ack.CommandID != command.ID || ack.Status != "accepted" {
			t.Errorf("large image acknowledgment = %#v, %v", ack, err)
			return
		}
		acknowledged <- struct{}{}
	}))
	defer server.Close()
	c := testConnection(t, cfg, store, server.URL, server.Client(), a.HandleCommand)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_ = c.connect(ctx)
	select {
	case <-acknowledged:
	default:
		t.Fatal("large image did not cross the worker WebSocket")
	}
	waitControl(t, func() bool {
		record, found, err := a.store.LoadCommand(command.ID)
		return err == nil && found && record.State == CommandCompleted
	})
	for _, call := range codex.Calls() {
		if call.Method != "turn/start" {
			continue
		}
		var params struct {
			Input []map[string]string `json:"input"`
		}
		if err := json.Unmarshal(call.Params, &params); err != nil {
			t.Fatal(err)
		}
		if len(params.Input) != 2 || params.Input[0]["text"] != command.Arguments.Text || params.Input[1]["type"] != "image" {
			t.Fatal("large image RPC lost text or image input")
		}
		encoded, ok := strings.CutPrefix(params.Input[1]["url"], "data:image/png;base64,")
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if !ok || err != nil || !bytes.Equal(decoded, image.Data) {
			t.Fatalf("large image RPC bytes changed: received %d bytes, error %v", len(decoded), err)
		}
		return
	}
	t.Fatal("large image never reached the app-server RPC")
}
