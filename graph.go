package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"math"
	"net/http"
	"strconv"
	"time"
)

type graphNode struct {
	ID      int64   `json:"id"`
	FloorID int64   `json:"floor_id"`
	Code    string  `json:"code"`
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	Type    string  `json:"type"`
}

type graphEdge struct {
	ID             int64   `json:"id"`
	FromNodeID     int64   `json:"from_node_id"`
	ToNodeID       int64   `json:"to_node_id"`
	DistanceMeters float64 `json:"distance_meters"`
	Type           string  `json:"type"`
	IsAccessible   bool    `json:"is_accessible"`
}

func graphHandler(db *sql.DB) http.HandlerFunc {
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

		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		fail := func(err error) {
			log.Printf("Graph request failed: %v", err)
			sendJSON(w, http.StatusInternalServerError,
				map[string]string{"error": "Could not load navigation graph"})
		}

		// Read nodes and edges from one consistent database snapshot.
		tx, err := db.BeginTx(ctx, &sql.TxOptions{
			Isolation: sql.LevelRepeatableRead,
			ReadOnly:  true,
		})
		if err != nil {
			fail(err)
			return
		}
		defer tx.Rollback()

		var locationName string
		err = tx.QueryRowContext(ctx, `
			SELECT location_name
			FROM indoor_nav_common.locations
			WHERE location_id = ? AND is_active = 1
		`, locationID).Scan(&locationName)

		if errors.Is(err, sql.ErrNoRows) {
			sendJSON(w, http.StatusNotFound,
				map[string]string{"error": "Active location not found"})
			return
		}
		if err != nil {
			fail(err)
			return
		}

		nodes := make([]graphNode, 0)
		nodeIDs := make(map[int64]bool)

		nodeRows, err := tx.QueryContext(ctx, `
			SELECT node_id, floor_id, node_code,
			       x_coordinate, y_coordinate, node_type
			FROM indoor_nav_locations.navigation_nodes
			WHERE location_id = ? AND is_active = 1
			ORDER BY node_id
		`, locationID)
		if err != nil {
			fail(err)
			return
		}

		for nodeRows.Next() {
			var node graphNode
			if err := nodeRows.Scan(
				&node.ID, &node.FloorID, &node.Code,
				&node.X, &node.Y, &node.Type,
			); err != nil {
				nodeRows.Close()
				fail(err)
				return
			}

			if math.IsNaN(node.X) || math.IsInf(node.X, 0) ||
				math.IsNaN(node.Y) || math.IsInf(node.Y, 0) {
				nodeRows.Close()
				fail(errors.New("invalid node coordinates"))
				return
			}

			nodes = append(nodes, node)
			nodeIDs[node.ID] = true
		}

		err = nodeRows.Err()
		nodeRows.Close()
		if err != nil {
			fail(err)
			return
		}

		edges := make([]graphEdge, 0)

		edgeRows, err := tx.QueryContext(ctx, `
			SELECT edge_id, from_node_id, to_node_id,
			       distance_meters, edge_type, is_accessible
			FROM indoor_nav_locations.navigation_edges
			WHERE location_id = ? AND is_active = 1
			ORDER BY edge_id
		`, locationID)
		if err != nil {
			fail(err)
			return
		}

		for edgeRows.Next() {
			var edge graphEdge
			if err := edgeRows.Scan(
				&edge.ID, &edge.FromNodeID, &edge.ToNodeID,
				&edge.DistanceMeters, &edge.Type,
				&edge.IsAccessible,
			); err != nil {
				edgeRows.Close()
				fail(err)
				return
			}

			// Exclude paths whose endpoints are not active in this location.
			if !nodeIDs[edge.FromNodeID] || !nodeIDs[edge.ToNodeID] {
				continue
			}

			if edge.DistanceMeters < 0 ||
				math.IsNaN(edge.DistanceMeters) ||
				math.IsInf(edge.DistanceMeters, 0) {
				edgeRows.Close()
				fail(errors.New("invalid edge distance"))
				return
			}

			edges = append(edges, edge)
		}

		err = edgeRows.Err()
		edgeRows.Close()
		if err != nil {
			fail(err)
			return
		}

		if err := tx.Commit(); err != nil {
			fail(err)
			return
		}

		sendJSON(w, http.StatusOK, map[string]any{
			"location_id":   locationID,
			"location_name": locationName,
			"directed":      true,
			"nodes":         nodes,
			"edges":         edges,
		})
	}
}
