package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/go-sql-driver/mysql"
)

func main() {
	if os.Getenv("DB_USER") == "" {
		log.Fatal("Set DB_USER and DB_PASSWORD before running.")
	}

	cfg := mysql.NewConfig()
	cfg.User = os.Getenv("DB_USER")
	cfg.Passwd = os.Getenv("DB_PASSWORD")
	cfg.Net = "tcp"
	cfg.Addr = os.Getenv("DB_ADDR")
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:3307"
	}
	cfg.DBName = "indoor_nav_common"
	cfg.ParseTime = true
	cfg.Timeout = 5 * time.Second
	cfg.ReadTimeout = 5 * time.Second
	cfg.WriteTimeout = 5 * time.Second

	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(3 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = db.PingContext(ctx)
	cancel()
	if err != nil {
		log.Fatalf("MySQL connection failed: %v", err)
	}

	log.Println("Connected to MySQL")

	mux := http.NewServeMux()

	registerTechnicianRoutes(mux, db, resolveSessionUser)
	registerSessionLogout(mux, resolveSessionUser, revokeSessionToken)

	mux.HandleFunc("/auth/register", registerHandler(db))
	mux.HandleFunc("/auth/login", loginHandler(db))
	mux.HandleFunc("/auth/me", meHandler(db))
	mux.HandleFunc("/destinations", destinationsHandler(db))
	mux.HandleFunc("/navigation/graph", graphHandler(db))
	mux.HandleFunc("/beacons", beaconsHandler(db))

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		var users, beacons int64

		err := db.QueryRowContext(
			ctx,
			"SELECT COUNT(*) FROM indoor_nav_common.users",
		).Scan(&users)
		if err != nil {
			log.Printf("Users query failed: %v", err)
			http.Error(w, "Could not read users table; check terminal",
				http.StatusServiceUnavailable)
			return
		}

		err = db.QueryRowContext(
			ctx,
			"SELECT COUNT(*) FROM indoor_nav_locations.beacons",
		).Scan(&beacons)
		if err != nil {

			log.Printf("Beacons query failed: %v", err)
			http.Error(w, "Could not read beacons table; check terminal",
				http.StatusServiceUnavailable)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"status":             "ok",
			"common_database":    "connected",
			"locations_database": "connected",
			"users_count":        users,
			"beacons_count":      beacons,
		})
	})

	server := &http.Server{
		Addr:              "0.0.0.0:8080",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      15 * time.Second,
	}

	log.Println("Open http://127.0.0.1:8080/health")
	log.Fatal(server.ListenAndServe())
}
