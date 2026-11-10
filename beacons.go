package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"math"
	"net/http"
	"strconv"
	"time"
)

type beaconConfig struct {
	ID               int64   `json:"id"`
	UUID             string  `json:"uuid"`
	Major            int     `json:"major"`
	Minor            int     `json:"minor"`
	FloorID          int64   `json:"floor_id"`
	NavigationNodeID *int64  `json:"navigation_node_id"`
	Label            string  `json:"label"`
	X                float64 `json:"x"`
	Y                float64 `json:"y"`
	TxPower          int     `json:"tx_power_calibration"`
}

func beaconsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		locationID, err := strconv.ParseInt(
			r.URL.Query().Get("location_id"), 10, 64,
		)
		if err != nil || locationID <= 0 {
			http.Error(w, "location_id must be a positive integer",
				http.StatusBadRequest)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		// For this demo, the advertised Major equals floor_number.
		// Your Departures floor has floor_number = 1 and Major = 1.
		rows, err := db.QueryContext(ctx, `
			SELECT b.beacon_id, l.beacon_uuid, f.floor_number,
			       b.beacon_minor, b.floor_id, b.navigation_node_id,
			       COALESCE(b.label, ''), b.x_coordinate,
			       b.y_coordinate, b.tx_power_calibration
			FROM indoor_nav_locations.beacons AS b
			JOIN indoor_nav_common.locations AS l
			  ON l.location_id = b.location_id
			JOIN indoor_nav_locations.floors AS f
			  ON f.floor_id = b.floor_id
			 AND f.location_id = b.location_id
			WHERE b.location_id = ?
			  AND b.status = 'ACTIVE'
			  AND l.is_active = 1
			ORDER BY b.beacon_id
		`, locationID)
		if err != nil {
			log.Printf("Beacon query failed: %v", err)
			http.Error(w, "Could not load beacons",
				http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		beacons := make([]beaconConfig, 0)

		for rows.Next() {
			var b beaconConfig
			var nodeID sql.NullInt64

			err := rows.Scan(
				&b.ID, &b.UUID, &b.Major, &b.Minor, &b.FloorID,
				&nodeID, &b.Label, &b.X, &b.Y, &b.TxPower,
			)
			if err != nil {
				log.Printf("Beacon scan failed: %v", err)
				http.Error(w, "Could not read beacon configuration",
					http.StatusInternalServerError)
				return
			}

			if b.Major < 0 || b.Major > 65535 ||
				b.Minor < 0 || b.Minor > 65535 ||
				b.TxPower < -128 || b.TxPower > 127 ||
				math.IsNaN(b.X) || math.IsInf(b.X, 0) ||
				math.IsNaN(b.Y) || math.IsInf(b.Y, 0) {
				log.Printf("Invalid configuration for beacon %d", b.ID)
				http.Error(w, "Invalid beacon configuration",
					http.StatusInternalServerError)
				return
			}

			if nodeID.Valid {
				id := nodeID.Int64
				b.NavigationNodeID = &id
			}

			beacons = append(beacons, b)
		}

		if err := rows.Err(); err != nil {
			log.Printf("Reading beacons failed: %v", err)
			http.Error(w, "Could not finish loading beacons",
				http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"location_id": locationID,
			"beacons":     beacons,
		}); err != nil {
			log.Printf("Writing beacon response failed: %v", err)
		}
	}
}
