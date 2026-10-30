package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRequestTokenRefreshReloadsSessionOnSuccess(t *testing.T) {
	state := newUITestState()
	interactive := make(chan bool, 1)
	reloaded := make(chan struct{}, 1)
	state.refreshFunc = func(ctx context.Context, allowInteractive bool) error {
		interactive <- allowInteractive
		return nil
	}
	state.reloadFunc = func() error {
		reloaded <- struct{}{}
		return nil
	}

	state.requestTokenRefresh("test")

	select {
	case got := <-interactive:
		if got {
			t.Fatal("expected a silent refresh first")
		}
	case <-time.After(time.Second):
		t.Fatal("expected refresh to run")
	}
	select {
	case <-reloaded:
	case <-time.After(time.Second):
		t.Fatal("expected session reload after refresh")
	}
	waitFor(t, "signed-in notice", func() bool { return strings.Contains(state.noticeText(), "signed in") })
}

func TestRequestTokenRefreshMarksSignInRequired(t *testing.T) {
	state := newUITestState()
	state.refreshFunc = func(ctx context.Context, allowInteractive bool) error { return errSignInRequired }
	state.reloadFunc = func() error { t.Error("reload must not run after a failed refresh"); return nil }

	state.requestTokenRefresh("test")

	waitFor(t, "sign-in required state", state.signInNeeded)
	if !strings.Contains(state.noticeText(), "R") {
		t.Fatalf("expected notice to mention the R key, got %q", state.noticeText())
	}
}

func TestRequestTokenRefreshRunsOneAtATime(t *testing.T) {
	state := newUITestState()
	release := make(chan struct{})
	calls := make(chan struct{}, 4)
	state.refreshFunc = func(ctx context.Context, allowInteractive bool) error {
		calls <- struct{}{}
		<-release
		return errors.New("boom")
	}

	state.requestTokenRefresh("first")
	state.requestTokenRefresh("second")
	close(release)

	waitFor(t, "refresh to finish", func() bool { return !state.tokenRefreshRunning() })
	if len(calls) != 1 {
		t.Fatalf("expected a single refresh run, got %d", len(calls))
	}
	if !strings.Contains(state.noticeText(), "failed") {
		t.Fatalf("expected failure notice, got %q", state.noticeText())
	}
}

func TestGlobalKeyRStartsInteractiveSignIn(t *testing.T) {
	state := newUITestState()
	interactive := make(chan bool, 1)
	state.refreshFunc = func(ctx context.Context, allowInteractive bool) error {
		interactive <- allowInteractive
		return nil
	}
	state.reloadFunc = func() error { return nil }
	state.focusComponent(TrChat)

	if got := state.globalKeyHandler(tcell.NewEventKey(tcell.KeyRune, 'R', tcell.ModNone)); got != nil {
		t.Fatal("expected R to be consumed")
	}
	select {
	case got := <-interactive:
		if !got {
			t.Fatal("expected R to allow an interactive sign-in")
		}
	case <-time.After(time.Second):
		t.Fatal("expected sign-in to start")
	}
}

func TestNoteAuthFailureIgnoresOtherErrors(t *testing.T) {
	state := newUITestState()
	state.refreshFunc = func(ctx context.Context, allowInteractive bool) error {
		t.Error("refresh must not run for a non-auth error")
		return nil
	}

	state.noteAuthFailure(errors.New("dial tcp: timeout"))
	time.Sleep(50 * time.Millisecond)
}
