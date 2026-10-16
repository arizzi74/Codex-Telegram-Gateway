package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTelegramSetMyProfilePhotoUploadsStaticJPEG(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/botprofile-token/setMyProfilePhoto" || r.ParseMultipartForm(1<<20) != nil {
			t.Error("invalid profile photo request")
			w.WriteHeader(400)
			return
		}
		defer r.MultipartForm.RemoveAll()
		var photo struct{ Type, Photo string }
		if json.Unmarshal([]byte(r.FormValue("photo")), &photo) != nil || photo.Type != "static" || photo.Photo != "attach://profile_photo" || len(r.MultipartForm.Value) != 1 || len(r.MultipartForm.File) != 1 {
			t.Error("invalid static photo metadata")
		}
		file, header, err := r.FormFile("profile_photo")
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		defer file.Close()
		data, _ := io.ReadAll(file)
		image, err := jpeg.DecodeConfig(bytes.NewReader(data))
		if err != nil || image.Width != 512 || image.Height != 512 || !bytes.Equal(data, defaultTelegramBotProfilePhoto) || header.Filename != "codex-gateway.jpg" || header.Header.Get("Content-Type") != "image/jpeg" {
			t.Error("uploaded file does not match embedded JPEG")
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
	}))
	defer server.Close()
	client := NewTelegramClient("profile-token")
	client.endpoint, client.http = server.URL, server.Client()
	if err := client.SetMyProfilePhoto(t.Context(), defaultTelegramBotProfilePhoto); err != nil || requests != 1 {
		t.Fatalf("profile photo upload: %v requests=%d", err, requests)
	}
	for _, photo := range [][]byte{nil, []byte("not JPEG"), bytes.Repeat([]byte{0}, (10<<20)+1)} {
		if err := client.SetMyProfilePhoto(t.Context(), photo); err == nil {
			t.Fatal("invalid profile photo accepted")
		}
	}
	if requests != 1 {
		t.Fatal("invalid profile photo reached Telegram")
	}
}

func TestTelegramHasUserProfilePhotoRejectsAmbiguousMetadata(t *testing.T) {
	for _, test := range []struct {
		name, result string
		exists, bad  bool
	}{
		{name: "no photos", result: `{"total_count":0,"photos":[]}`},
		{name: "existing photos", result: `{"total_count":1,"photos":[[{"file_id":"custom"}]]}`, exists: true},
		{name: "positive count", result: `{"total_count":1,"photos":[]}`, exists: true},
		{name: "photo despite zero count", result: `{"total_count":0,"photos":[[{"file_id":"custom"}]]}`, exists: true},
		{name: "empty object", result: `{}`, bad: true},
		{name: "null result", result: `null`, bad: true},
		{name: "missing count", result: `{"photos":[]}`, bad: true},
		{name: "null count", result: `{"total_count":null,"photos":[]}`, bad: true},
		{name: "negative count", result: `{"total_count":-1,"photos":[]}`, bad: true},
		{name: "missing photos", result: `{"total_count":0}`, bad: true},
		{name: "null photos", result: `{"total_count":0,"photos":null}`, bad: true},
		{name: "exceeds requested limit", result: `{"total_count":2,"photos":[[],[]]}`, bad: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var input struct {
					UserID int64 `json:"user_id"`
					Limit  int   `json:"limit"`
				}
				if r.URL.Path != "/botprofile-token/getUserProfilePhotos" || json.NewDecoder(r.Body).Decode(&input) != nil || input.UserID != 222 || input.Limit != 1 {
					t.Error("photo lookup did not use verified bot identity and limit 1")
				}
				_, _ = io.WriteString(w, `{"ok":true,"result":`+test.result+`}`)
			}))
			defer server.Close()
			client := NewTelegramClient("profile-token")
			client.endpoint, client.http = server.URL, server.Client()
			exists, err := client.HasUserProfilePhoto(t.Context(), 222)
			if exists != test.exists || (err != nil) != test.bad {
				t.Fatalf("profile metadata = %v, %v; want existing=%v error=%v", exists, err, test.exists, test.bad)
			}
		})
	}
}

func TestTelegramProfilePhotoRequestsRejectErrorsAndRedirects(t *testing.T) {
	for _, test := range []struct {
		name, response string
		status         int
	}{
		{name: "false result", status: 200, response: `{"ok":true,"result":false}`},
		{name: "missing result", status: 200, response: `{"ok":true}`},
		{name: "null result", status: 200, response: `{"ok":true,"result":null}`},
		{name: "malformed body", status: 200, response: `{"ok":`},
		{name: "unexpected HTTP status", status: 500, response: `{"ok":true,"result":true}`},
		{name: "oversized body", status: 200, response: `{"ok":true,"result":true}` + strings.Repeat(" ", 2<<20)},
		{name: "Telegram error", status: 429, response: `{"ok":false,"error_code":429,"description":"profile-token cannot upload","parameters":{"retry_after":8}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.response)
			}))
			defer server.Close()
			client := NewTelegramClient("profile-token")
			client.endpoint, client.http = server.URL, server.Client()
			err := client.SetMyProfilePhoto(t.Context(), defaultTelegramBotProfilePhoto)
			if err == nil || strings.Contains(err.Error(), "profile-token") {
				t.Fatalf("unsafe or missing profile error: %v", err)
			}
			if test.status == 429 {
				var apiError *TelegramError
				if !errors.As(err, &apiError) || apiError.Code != 429 || apiError.RetryAfter != 8*time.Second {
					t.Fatalf("lost Telegram retry metadata: %v", err)
				}
			}
		})
	}
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
	}))
	defer destination.Close()
	for _, status := range []int{302, 307, 308} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, destination.URL, status)
		}))
		client := NewTelegramClient("profile-token")
		client.endpoint, client.http = server.URL, server.Client()
		if err := client.SetMyProfilePhoto(t.Context(), defaultTelegramBotProfilePhoto); err == nil {
			t.Fatal("redirected profile upload succeeded")
		}
		if _, err := client.HasUserProfilePhoto(t.Context(), 222); err == nil {
			t.Fatal("redirected profile lookup succeeded")
		}
		server.Close()
	}
	if redirected.Load() != 0 {
		t.Fatal("profile requests followed a redirect with bot credentials")
	}
}

func TestTelegramManagedBotDefaultProfilePhotoCreationAndCustomPhotos(t *testing.T) {
	for _, custom := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "custom preserved"}[custom], func(t *testing.T) {
			m, _, fake := newTelegramBotManagerTest(t)
			token := fake.add(222, "CreatedBot", false)
			if custom {
				fake.profilePhotos[token] = 1
			}
			if _, err := m.CreateManagedTelegramBot(t.Context(), "", "CreatedBot", "Created"); err != nil {
				t.Fatal(err)
			}
			update := managedUpdate(101, 7, 222, "CreatedBot")
			if code := postManaged(t, m, m.cfg.Secrets.BotName, update, "original-secret"); code != 200 {
				t.Fatal(code)
			}
			wantUploads := 1
			if custom {
				wantUploads = 0
			}
			if fake.count("getUserProfilePhotos") != 1 || fake.count("setMyProfilePhoto") != wantUploads {
				t.Fatalf("creation photo reads=%d uploads=%d", fake.count("getUserProfilePhotos"), fake.count("setMyProfilePhoto"))
			}
			// Replayed receipts, token updates, and later custom photo removal
			// cannot reapply defaults after creation completed.
			fake.profilePhotos[token] = 0
			for _, id := range []int64{101, 102} {
				if code := postManaged(t, m, m.cfg.Secrets.BotName, managedUpdate(id, 7, 222, "CreatedBot"), "original-secret"); code != 200 {
					t.Fatal(code)
				}
			}
			if fake.count("getUserProfilePhotos") != 1 || fake.count("setMyProfilePhoto") != wantUploads {
				t.Fatal("completed creation reset a later profile photo")
			}
		})
	}
}

func TestTelegramManagedBotDefaultProfilePhotoRetriesAfterRestart(t *testing.T) {
	for _, failure := range []string{"read", "upload", "false result", "applied response lost"} {
		t.Run(failure, func(t *testing.T) {
			m, store, fake := newTelegramBotManagerTest(t)
			token := fake.add(222, "CreatedBot", false)
			fake.failPhotoRead = failure == "read"
			fake.photoApplyThenFail = failure == "applied response lost"
			fake.photoResultFalse = failure == "false result"
			if failure == "upload" {
				fake.photoErrorCode = 500
			}
			if _, err := m.CreateManagedTelegramBot(t.Context(), "", "CreatedBot", "Created"); err != nil {
				t.Fatal(err)
			}
			update := managedUpdate(111, 7, 222, "CreatedBot")
			if code := postManaged(t, m, m.cfg.Secrets.BotName, update, "original-secret"); code != 503 {
				t.Fatalf("photo failure response = %d", code)
			}
			if _, _, enabled := m.ResolveTelegramBot("telegram:222"); !enabled {
				t.Fatal("photo failure disabled the connected bot")
			}
			intents, err := store.TelegramManagedBotIntents(t.Context())
			seen, seenErr := store.TelegramManagedBotUpdateSeen(t.Context(), m.cfg.Secrets.BotName, update.ID)
			if err != nil || len(intents) != 1 || intents[0].Status != "activating" || seenErr != nil || seen {
				t.Fatalf("photo failure lost retry state: %+v err=%v seen=%v/%v", intents, err, seen, seenErr)
			}
			fake.failPhotoRead, fake.photoApplyThenFail, fake.photoResultFalse, fake.photoErrorCode = false, false, false, 0
			restarted, err := NewTelegramBotManager(t.Context(), store, m.cfg, "original-secret", m.log)
			if err != nil {
				t.Fatal(err)
			}
			fake.attach(restarted)
			if code := postManaged(t, restarted, m.cfg.Secrets.BotName, update, "original-secret"); code != 200 {
				t.Fatalf("profile retry response = %d", code)
			}
			intents, err = store.TelegramManagedBotIntents(t.Context())
			if err != nil || len(intents) != 1 || intents[0].Status != "connected" || fake.photoUploads[token] != 1 {
				t.Fatalf("profile retry did not complete exactly one upload: %+v %v uploads=%d", intents, err, fake.photoUploads[token])
			}
		})
	}
}

func TestTelegramManagedBotDefaultProfilePhotoHonorsDisableAndOwnership(t *testing.T) {
	for _, action := range []string{"disable", "owner changed", "remove"} {
		t.Run(action, func(t *testing.T) {
			m, _, fake := newTelegramBotManagerTest(t)
			fake.add(222, "CreatedBot", false)
			fake.failPhotoRead = true
			if _, err := m.CreateManagedTelegramBot(t.Context(), "", "CreatedBot", "Created"); err != nil {
				t.Fatal(err)
			}
			update := managedUpdate(121, 7, 222, "CreatedBot")
			if code := postManaged(t, m, m.cfg.Secrets.BotName, update, "original-secret"); code != 503 {
				t.Fatal(code)
			}
			fake.failPhotoRead = false
			switch action {
			case "disable":
				if err := m.SetTelegramBotEnabled(t.Context(), "telegram:222", false); err != nil {
					t.Fatal(err)
				}
			case "owner changed":
				if code := postManaged(t, m, m.cfg.Secrets.BotName, managedUpdate(122, 99, 222, "CreatedBot"), "original-secret"); code != 200 {
					t.Fatal(code)
				}
			case "remove":
				if err := m.RemoveTelegramBot(t.Context(), "telegram:222"); err != nil {
					t.Fatal(err)
				}
			}
			if code := postManaged(t, m, m.cfg.Secrets.BotName, update, "original-secret"); code != 200 {
				t.Fatal(code)
			}
			if fake.count("getUserProfilePhotos") != 1 || fake.count("setMyProfilePhoto") != 0 {
				t.Fatal("revoked creation changed a profile photo")
			}
			if _, _, enabled := m.ResolveTelegramBot("telegram:222"); enabled {
				t.Fatal("revoked creation was enabled by photo retry")
			}
		})
	}
}

func TestTelegramDefaultProfilePhotoNeverChangesExistingOrUnrequestedBots(t *testing.T) {
	m, _, fake := newTelegramBotManagerTest(t)
	token := fake.add(222, "ExistingBot", false)
	bot, err := m.ConnectTelegramBot(t.Context(), token, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.ReplaceTelegramBotToken(t.Context(), bot.ID, token, false); err != nil {
		t.Fatal(err)
	}
	fake.add(333, "UnknownBot", false)
	for i, update := range []TelegramUpdate{
		managedUpdate(131, 7, 222, "ExistingBot"),
		managedUpdate(132, 7, 333, "UnknownBot"),
		managedUpdate(133, 99, 333, "UnknownBot"),
		managedUpdate(134, 7, 111, "PrimaryBot"),
	} {
		if code := postManaged(t, m, m.cfg.Secrets.BotName, update, "original-secret"); code != 200 {
			t.Fatalf("unrequested update %d = %d", i, code)
		}
	}
	if fake.count("getUserProfilePhotos") != 0 || fake.count("setMyProfilePhoto") != 0 {
		t.Fatal("existing or unrequested bot changed a profile photo")
	}
}
