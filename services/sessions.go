package services

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	sessionExpiryDurationMs = 7 * 24 * 60 * 60 * 1000 // 7 days in milliseconds
	sessionsSweepInterval   = 1 * time.Hour
)

// SessionsService tracks active UI sessions in memory. Process restart wipes
// all state (acceptable for a single-user tool - restart = re-login).
type SessionsService struct {
	activeSessions map[string]int64
	mu             sync.RWMutex
}

func NewSessionsService() *SessionsService {
	return &SessionsService{
		activeSessions: make(map[string]int64),
	}
}

// RunSweeper prunes expired sessions until ctx is cancelled. Blocks.
func (ss *SessionsService) RunSweeper(ctx context.Context) {
	ticker := time.NewTicker(sessionsSweepInterval)
	defer ticker.Stop()

	for {
		select {
		case now := <-ticker.C:
			ss.sweep(now.UnixMilli())
		case <-ctx.Done():
			return
		}
	}
}

func (ss *SessionsService) sweep(nowMs int64) {
	ss.mu.Lock()
	defer ss.mu.Unlock()

	for sessionId, expiry := range ss.activeSessions {
		if expiry < nowMs {
			delete(ss.activeSessions, sessionId)
		}
	}
}

func (ss *SessionsService) CreateSession() (string, int64) {
	sessionId := uuid.New().String()
	expiresAt := time.Now().UnixMilli() + sessionExpiryDurationMs

	ss.mu.Lock()
	ss.activeSessions[sessionId] = expiresAt
	ss.mu.Unlock()

	return sessionId, expiresAt
}

func (ss *SessionsService) IsSessionValid(sessionId string) bool {
	ss.mu.RLock()
	defer ss.mu.RUnlock()

	if expiry, exists := ss.activeSessions[sessionId]; exists {
		if time.Now().UnixMilli() < expiry {
			return true
		}
	}
	return false
}

func (ss *SessionsService) InvalidateSession(sessionId string) {
	ss.mu.Lock()
	delete(ss.activeSessions, sessionId)
	ss.mu.Unlock()
}
