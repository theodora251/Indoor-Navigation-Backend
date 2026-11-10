package main

import (
	"context"
	"database/sql"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type destination struct {
	ID               int64  `json:"id"`
	Name             string `json:"name"`
	Type             string `json:"type"`
	FloorID          int64  `json:"floor_id"`
	FloorName        string `json:"floor_name"`
	NavigationNodeID int64  `json:"navigation_node_id"`
}

func destinationsHandler(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			sendJSON(w, http.StatusMethodNotAllowed,
				map[string]string{"error": "Use GET"})
			return
		}

		locationID, err := strconv.ParseInt(
			r.URL.Query().Get("location_id"), 10, 64,
		)
		if err != nil || locationID <= 0 {
			sendJSON(w, http.StatusBadRequest,
				map[string]string{"error": "A valid location_id is required"})
			return
		}

		query := strings.TrimSpace(r.URL.Query().Get("q"))

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		rows, err := db.QueryContext(ctx, `
			SELECT
				f.facility_id,
				f.facility_name,
				f.facility_type,
				f.floor_id,
				COALESCE(fl.floor_name, ''),
				f.navigation_node_id
			FROM indoor_nav_locations.facilities AS f
			JOIN indoor_nav_locations.floors AS fl
				ON fl.floor_id = f.floor_id
				AND fl.location_id = f.location_id
			JOIN indoor_nav_locations.navigation_nodes AS n
				ON n.node_id = f.navigation_node_id
				AND n.location_id = f.location_id
				AND n.floor_id = f.floor_id
			JOIN indoor_nav_common.locations AS l
				ON l.location_id = f.location_id
			WHERE f.location_id = ?
				AND f.is_active = 1
				AND n.is_active = 1
				AND l.is_active = 1
				AND (? = '' OR LOCATE(?, f.facility_name) > 0)
			ORDER BY f.facility_name, f.facility_id
			LIMIT 100
		`, locationID, query, query)

		if err != nil {
			log.Printf("Destinations query failed: %v", err)
			sendJSON(w, http.StatusInternalServerError,
				map[string]string{"error": "Could not load destinations"})
			return
		}
		defer rows.Close()

		destinations := make([]destination, 0)

		for rows.Next() {
			var item destination
			if err := rows.Scan(
				&item.ID,
				&item.Name,
				&item.Type,
				&item.FloorID,
				&item.FloorName,
				&item.NavigationNodeID,
			); err != nil {
				log.Printf("Destination scan failed: %v", err)
				sendJSON(w, http.StatusInternalServerError,
					map[string]string{"error": "Could not load destinations"})
				return
			}
			destinations = append(destinations, item)
		}

		if err := rows.Err(); err != nil {
			log.Printf("Destination rows failed: %v", err)
			sendJSON(w, http.StatusInternalServerError,
				map[string]string{"error": "Could not load destinations"})
			return
		}

		sendJSON(w, http.StatusOK, map[string]any{
			"location_id":  locationID,
			"destinations": destinations,
		})
	}
}
