package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
	"golang.org/x/crypto/bcrypt"
)

type registerRequest struct {
	Email     string `json:"email"`
	Password  string `json:"password"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

func sendJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Printf("Response encoding failed: %v", err)
	}
}

func registerHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		fail := func(status int, message string) {
			sendJSON(w, status, map[string]string{"error": message})
		}

		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			fail(http.StatusMethodNotAllowed, "Use POST for registration")
			return
		}

		// Limit incoming JSON to 16 KB.
		r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
		defer r.Body.Close()

		var input registerRequest
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()

		if err := decoder.Decode(&input); err != nil {
			fail(http.StatusBadRequest, "Invalid registration JSON")
			return
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			fail(http.StatusBadRequest, "Send exactly one JSON object")
			return
		}

		input.Email = strings.ToLower(strings.TrimSpace(input.Email))
		input.FirstName = strings.TrimSpace(input.FirstName)
		input.LastName = strings.TrimSpace(input.LastName)

		address, err := mail.ParseAddress(input.Email)
		if err != nil || address.Address != input.Email ||
			utf8.RuneCountInString(input.Email) > 150 {
			fail(http.StatusBadRequest, "Enter a valid email, maximum 150 characters")
			return
		}

		if utf8.RuneCountInString(input.FirstName) > 100 ||
			utf8.RuneCountInString(input.LastName) > 100 {
			fail(http.StatusBadRequest, "Names must not exceed 100 characters")
			return
		}

		// Do not trim or otherwise change the password.
		if utf8.RuneCountInString(input.Password) < 15 ||
			len([]byte(input.Password)) > 72 {
			fail(http.StatusBadRequest,
				"Password must contain at least 15 characters and at most 72 bytes")
			return
		}

		hash, err := bcrypt.GenerateFromPassword(
			[]byte(input.Password),
			bcrypt.DefaultCost,
		)
		if err != nil {
			log.Printf("Password hashing failed: %v", err)
			fail(http.StatusInternalServerError, "Could not create account")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		result, err := db.ExecContext(ctx, `
			INSERT INTO indoor_nav_common.users
				(email, password_hash, first_name, last_name)
			VALUES (?, ?, ?, ?)
		`, input.Email, string(hash), input.FirstName, input.LastName)

		if err != nil {
			var mysqlErr *mysql.MySQLError
			if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
				fail(http.StatusConflict, "An account with this email already exists")
				return
			}

			log.Printf("Registration insert failed: %v", err)
			fail(http.StatusInternalServerError, "Could not create account")
			return
		}

		userID, err := result.LastInsertId()
		if err != nil {
			log.Printf("Reading inserted ID failed: %v", err)
			fail(http.StatusInternalServerError, "Could not retrieve account ID")
			return
		}

		sendJSON(w, http.StatusCreated, map[string]any{
			"message": "Account created successfully",
			"user_id": userID,
			"email":   input.Email,
		})
	}
}
