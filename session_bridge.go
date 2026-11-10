package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"
)

// Reuses login.go's hashed, expiring session store.
// Technician routes separately verify that the user is active and has a role.
func resolveSessionUser(ctx context.Context, token string) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if token == "" {
		return 0, errors.New("missing session token")
	}
	key := sha256.Sum256([]byte(token))
	sessionStore.Lock()
	defer sessionStore.Unlock()
	session, exists := sessionStore.items[key]
	if !exists {
		return 0, errors.New("invalid session token")
	}
	if !time.Now().Before(session.ExpiresAt) {
		delete(sessionStore.items, key)
		return 0, errors.New("expired session token")
	}
	if session.UserID <= 0 {
		return 0, errors.New("invalid session user")
	}
	return session.UserID, nil
}

func revokeSessionToken(token string) {
	key := sha256.Sum256([]byte(token))
	sessionStore.Lock()
	delete(sessionStore.items, key)
	sessionStore.Unlock()
}
