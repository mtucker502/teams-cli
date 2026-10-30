package main

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	teams_api "github.com/fossteams/teams-api"
	"github.com/sirupsen/logrus"
)

const (
	tokenExpiryWindow        = 5 * time.Minute
	silentRefreshTimeout     = 2 * time.Minute
	interactiveRefreshWindow = 12 * time.Minute
	noticeClearDelay         = 10 * time.Second
)

// noteAuthFailure triggers a background token refresh when an API call was
// rejected for bad credentials.
func (s *AppState) noteAuthFailure(err error) {
	if isAuthFailure(err) {
		s.requestTokenRefresh("credentials rejected")
	}
}

// checkTokenExpiry refreshes ahead of time when a token is about to lapse.
func (s *AppState) checkTokenExpiry() {
	if tokensExpireWithin(s.tokenDir, time.Now(), tokenExpiryWindow) {
		s.requestTokenRefresh("token expiring")
	}
}

// requestTokenRefresh runs a silent refresh in the background, at most one at
// a time. Once a sign-in is known to need a person, silent retries stop until
// the R key starts an interactive one.
func (s *AppState) requestTokenRefresh(reason string) {
	if s.signInNeeded() {
		return
	}
	if !atomic.CompareAndSwapUint32(&s.tokenRefreshActive, 0, 1) {
		return
	}

	s.appLogger().WithField("reason", reason).Info("refreshing tokens in the background")
	s.setNotice("Refreshing Teams sign-in...")
	go func() {
		defer atomic.StoreUint32(&s.tokenRefreshActive, 0)
		ctx, cancel := context.WithTimeout(s.appContext(), silentRefreshTimeout)
		defer cancel()
		s.handleRefreshOutcome(s.refresher()(ctx, false))
	}()
}

// startInteractiveSignIn opens a visible browser for the user to sign in
// while the TUI keeps running.
func (s *AppState) startInteractiveSignIn() {
	if !atomic.CompareAndSwapUint32(&s.tokenRefreshActive, 0, 1) {
		s.setNotice("A sign-in is already in progress")
		return
	}

	s.appLogger().Info("starting interactive sign-in")
	s.setNotice("Sign in using the browser window that just opened...")
	go func() {
		defer atomic.StoreUint32(&s.tokenRefreshActive, 0)
		ctx, cancel := context.WithTimeout(s.appContext(), interactiveRefreshWindow)
		defer cancel()
		s.handleRefreshOutcome(s.refresher()(ctx, true))
	}()
}

func (s *AppState) handleRefreshOutcome(err error) {
	switch {
	case err == nil:
		reload := s.reloadFunc
		if reload == nil {
			reload = s.reloadSession
		}
		if reloadErr := reload(); reloadErr != nil {
			s.appLogger().WithError(reloadErr).Error("tokens refreshed but session reload failed")
			s.setNotice("Signed in, but reload failed (see log)")
			return
		}
		atomic.StoreUint32(&s.signInRequired, 0)
		s.appLogger().Info("tokens refreshed and session reloaded")
		s.setTransientNotice("Teams sign-in refreshed; signed in again")
		go s.refreshConversationTree()
		go s.refreshSelectedConversationMessages()
	case errors.Is(err, errSignInRequired):
		atomic.StoreUint32(&s.signInRequired, 1)
		s.appLogger().WithError(err).Warn("silent token refresh needs a person")
		s.setNotice("Teams session expired. Press R to sign in again.")
	default:
		s.appLogger().WithError(err).Warn("token refresh failed")
		s.setNotice("Token refresh failed (see log). Press R to sign in.")
	}
}

func (s *AppState) refresher() func(ctx context.Context, interactive bool) error {
	if s.refreshFunc != nil {
		return s.refreshFunc
	}
	return s.defaultRefresh
}

func (s *AppState) defaultRefresh(ctx context.Context, interactive bool) error {
	opts := s.refreshOpts
	opts.Interactive = interactive
	// After a silent attempt has already failed there is no point repeating
	// it, so an interactive request goes straight to a visible browser.
	opts.ForceHeaded = interactive
	opts.Progress = func(message string) { s.appLogger().Info(message) }
	_, err := refreshTokens(ctx, opts, chromeFlowLauncher(opts))
	return err
}

// reloadSession rebuilds the library client and the messaging session from
// the freshly written token files.
func (s *AppState) reloadSession() error {
	if err := applyTokenDirToEnv(s.tokenDir); err != nil {
		return err
	}

	client, err := teams_api.New()
	if err != nil {
		return err
	}
	s.setClient(client)

	rootToken, err := resolveToken(tokenTypeSkype, s.tokenDir)
	if err != nil {
		return err
	}
	session, err := newMessagingSession(s.appContext(), s.httpClient, rootToken.Value, authzEndpoint)
	if err != nil {
		return err
	}
	s.setMessaging(session)
	s.appLogger().WithFields(logrus.Fields{"messages_host": session.host.String()}).Info("messaging session reloaded")

	return nil
}

func (s *AppState) signInNeeded() bool {
	return atomic.LoadUint32(&s.signInRequired) == 1
}

func (s *AppState) tokenRefreshRunning() bool {
	return atomic.LoadUint32(&s.tokenRefreshActive) == 1
}

// setNotice stores a status line shown in the help bar and asks for a redraw.
func (s *AppState) setNotice(text string) {
	s.stateMu.Lock()
	s.notice = text
	s.stateMu.Unlock()

	// Queue the redraw without blocking: this runs from background goroutines
	// and from key handlers on the event loop, where a synchronous queue
	// operation would deadlock.
	if s.app != nil {
		go s.app.QueueUpdateDraw(s.updateHelpBar)
	}
}

func (s *AppState) setTransientNotice(text string) {
	s.setNotice(text)
	time.AfterFunc(noticeClearDelay, func() {
		if s.noticeText() == text {
			s.setNotice("")
		}
	})
}

func (s *AppState) noticeText() string {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.notice
}
