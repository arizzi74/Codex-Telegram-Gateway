package main

import (
	"reflect"
	"testing"
)

func TestAttachCommandUsesRemoteResume(t *testing.T) {
	if got, want := attachCommand("/tmp/private.sock", "thr_1"), []string{"--remote", "unix:///tmp/private.sock", "resume", "thr_1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("attach args = %#v, want %#v", got, want)
	}
	if got, want := attachCommand("/tmp/private.sock", "--last"), []string{"--remote", "unix:///tmp/private.sock", "resume", "--last"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("last args = %#v, want %#v", got, want)
	}
}
