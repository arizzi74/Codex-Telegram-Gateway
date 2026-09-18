package codexadapter

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"reflect"
	"testing"

	"github.com/iaia/telegramgw/internal/protocol"
)

func TestImageInputsReachTurnRPCs(t *testing.T) {
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	images := []protocol.Image{{MIMEType: "image/png", Data: data.Bytes()}}
	wantImage := map[string]string{"type": "image", "url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(data.Bytes())}
	for _, tc := range []struct {
		name, text string
		steer      bool
	}{
		{name: "image only"},
		{name: "caption", text: "What does this show?"},
		{name: "steer image only", steer: true},
		{name: "steer caption", text: "Use this reference", steer: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, fake := newFake(t)
			initialize(t, client, fake)
			done := make(chan error, 1)
			go func() {
				var err error
				if tc.steer {
					_, err = client.SteerWithImages(context.Background(), "thread", "turn", tc.text, images)
				} else {
					_, err = client.StartTurnWithImages(context.Background(), "thread", tc.text, images)
				}
				done <- err
			}()
			request := fake.next(t)
			wantMethod := "turn/start"
			if tc.steer {
				wantMethod = "turn/steer"
			}
			if got := method(t, request); got != wantMethod {
				t.Fatalf("method = %s, want %s", got, wantMethod)
			}
			var input []map[string]string
			if err := json.Unmarshal(params(t, request)["input"], &input); err != nil {
				t.Fatal(err)
			}
			wantInput := []map[string]string{}
			if tc.text != "" {
				wantInput = append(wantInput, map[string]string{"type": "text", "text": tc.text})
			}
			wantInput = append(wantInput, wantImage)
			if !reflect.DeepEqual(input, wantInput) {
				t.Fatalf("input = %#v, want %#v", input, wantInput)
			}
			if tc.steer {
				var expected string
				if err := json.Unmarshal(params(t, request)["expectedTurnId"], &expected); err != nil || expected != "turn" {
					t.Fatalf("steer lost turn fence: %q, %v", expected, err)
				}
				fake.respond(t, request, map[string]any{"turnId": "turn"})
			} else {
				fake.respond(t, request, map[string]any{"turn": map[string]any{"id": "turn"}})
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if !tc.steer {
				if _, err := client.SteerWithImages(context.Background(), "thread", "other", tc.text, images); !errors.Is(err, ErrStaleTurn) {
					t.Fatalf("stale image steer = %v", err)
				}
			}
		})
	}
}

func TestTurnInputPreservesTextAndRejectsInvalidImages(t *testing.T) {
	input, err := turnInput("ordinary text", nil)
	if err != nil || !reflect.DeepEqual(input, []map[string]string{{"type": "text", "text": "ordinary text"}}) {
		t.Fatalf("text input = %#v, %v", input, err)
	}
	for _, tc := range []struct {
		name   string
		images []protocol.Image
	}{
		{name: "empty"},
		{name: "missing bytes", images: []protocol.Image{{MIMEType: "image/png"}}},
		{name: "unsupported MIME", images: []protocol.Image{{MIMEType: "text/plain", Data: []byte("not an image")}}},
		{name: "oversized", images: []protocol.Image{{MIMEType: "image/png", Data: make([]byte, protocol.MaxImageBytes+1)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := turnInput("", tc.images); err == nil {
				t.Fatal("invalid input was accepted")
			}
		})
	}
}

func TestResumeThreadExcludesHistoryAndPreservesMetadata(t *testing.T) {
	client, fake := newFake(t)
	initialize(t, client, fake)
	done := make(chan struct {
		thread Thread
		err    error
	}, 1)
	go func() {
		thread, err := client.ResumeThread(context.Background(), "thread", ThreadOptions{})
		done <- struct {
			thread Thread
			err    error
		}{thread, err}
	}()
	request := fake.next(t)
	if method(t, request) != "thread/resume" {
		t.Fatal("resume used the wrong RPC")
	}
	parameters := params(t, request)
	if len(parameters) != 2 || string(parameters["threadId"]) != `"thread"` || string(parameters["excludeTurns"]) != "true" {
		t.Fatalf("resume parameters = %#v", parameters)
	}
	fake.respond(t, request, map[string]any{"thread": map[string]any{
		"id": "thread", "name": "Image discussion", "cwd": "/workspace/project", "status": map[string]any{"type": "active"},
	}})
	result := <-done
	if result.err != nil || result.thread.ID != "thread" || result.thread.Name != "Image discussion" || result.thread.CWD != "/workspace/project" || result.thread.Status != "active" {
		t.Fatalf("resume metadata = %#v, %v", result.thread, result.err)
	}
}
