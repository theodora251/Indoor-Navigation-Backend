package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type loginSession struct {
	UserID    int64
	ExpiresAt time.Time
}

var sessionStore = struct {
	sync.Mutex
	items map[[32]byte]loginSession
}{
	items: make(map[[32]byte]loginSession),
}

func loginHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")

		fail := func(status int, message string) {
			sendJSON(w, status, map[string]string{"error": message})
		}

		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			fail(http.StatusMethodNotAllowed, "Use POST for login")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
		defer r.Body.Close()

		var input struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}

		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()

		if err := decoder.Decode(&input); err != nil {
			fail(http.StatusBadRequest, "Invalid login JSON")
			return
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			fail(http.StatusBadRequest, "Send exactly one JSON object")
			return
		}

		input.Email = strings.ToLower(strings.TrimSpace(input.Email))

		if input.Email == "" || input.Password == "" ||
			len([]byte(input.Password)) > 72 {
			fail(http.StatusUnauthorized, "Invalid email or password")
			return
		}

		var userID int64
		var passwordHash string
		var active bool

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		err := db.QueryRowContext(ctx, `
			SELECT user_id, password_hash, is_active
			FROM indoor_nav_common.users
			WHERE email = ?
		`, input.Email).Scan(&userID, &passwordHash, &active)

		if errors.Is(err, sql.ErrNoRows) {
			fail(http.StatusUnauthorized, "Invalid email or password")
			return
		}
		if err != nil {
			log.Printf("Login query failed: %v", err)
			fail(http.StatusInternalServerError, "Could not sign in")
			return
		}

		passwordErr := bcrypt.CompareHashAndPassword(
			[]byte(passwordHash),
			[]byte(input.Password),
		)
		if passwordErr != nil || !active {
			fail(http.StatusUnauthorized, "Invalid email or password")
			return
		}

		var randomBytes [32]byte
		if _, err := rand.Read(randomBytes[:]); err != nil {
			log.Printf("Session creation failed: %v", err)
			fail(http.StatusInternalServerError, "Could not sign in")
			return
		}

		token := hex.EncodeToString(randomBytes[:])
		tokenHash := sha256.Sum256([]byte(token))
		now := time.Now()
		expiresAt := now.Add(time.Hour)

		sessionStore.Lock()
		for key, session := range sessionStore.items {
			if !now.Before(session.ExpiresAt) {
				delete(sessionStore.items, key)
			}
		}
		sessionStore.items[tokenHash] = loginSession{
			UserID:    userID,
			ExpiresAt: expiresAt,
		}
		sessionStore.Unlock()

		sendJSON(w, http.StatusOK, map[string]any{
			"message":      "Login successful",
			"access_token": token,
			"token_type":   "Bearer",
			"expires_in":   3600,
			"user_id":      userID,
		})
	}
}

func meHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")

		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			sendJSON(w, http.StatusMethodNotAllowed,
				map[string]string{"error": "Use GET"})
			return
		}

		unauthorized := func() {
			sendJSON(w, http.StatusUnauthorized,
				map[string]string{"error": "Please sign in again"})
		}

		auth := strings.Fields(r.Header.Get("Authorization"))
		if len(auth) != 2 || !strings.EqualFold(auth[0], "Bearer") {
			unauthorized()
			return
		}

		tokenHash := sha256.Sum256([]byte(auth[1]))

		sessionStore.Lock()
		session, exists := sessionStore.items[tokenHash]
		if exists && !time.Now().Before(session.ExpiresAt) {
			delete(sessionStore.items, tokenHash)
			exists = false
		}
		sessionStore.Unlock()

		if !exists {
			unauthorized()
			return
		}

		var email string
		var active bool

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		err := db.QueryRowContext(ctx, `
			SELECT email, is_active
			FROM indoor_nav_common.users
			WHERE user_id = ?
		`, session.UserID).Scan(&email, &active)

		if errors.Is(err, sql.ErrNoRows) {
			unauthorized()
			return
		}
		if err != nil {
			log.Printf("Account query failed: %v", err)
			sendJSON(w, http.StatusInternalServerError,
				map[string]string{"error": "Could not load account"})
			return
		}
		if !active {
			unauthorized()
			return
		}

		sendJSON(w, http.StatusOK, map[string]any{
			"user_id": session.UserID,
			"email":   email,
		})
	}
}
