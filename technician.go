package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// resolve must verify the existing bearer session, including its expiry.
// Only explicitly assigned TECHNICIAN / ADMINISTRATOR roles get panel access.
// These demo roles manage all locations. Add client/location scopes for multi-client deployment.
func registerTechnicianRoutes(mux *http.ServeMux, db *sql.DB, resolve func(context.Context, string) (int64, error)) {
	authorized := func(next func(http.ResponseWriter, *http.Request, int64)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			parts := strings.Fields(r.Header.Get("Authorization"))
			if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
				techJSON(w, 401, map[string]any{"error": "Sign in required"})
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			userID, err := resolve(ctx, parts[1])
			if err != nil || userID <= 0 {
				techJSON(w, 401, map[string]any{"error": "Session expired or invalid"})
				return
			}
			var allowed bool
			err = db.QueryRowContext(ctx, `SELECT EXISTS (
    SELECT 1 FROM indoor_nav_common.users u
    JOIN indoor_nav_common.user_roles ur ON ur.user_id=u.user_id
    JOIN indoor_nav_common.roles role ON role.role_id=ur.role_id
    WHERE u.user_id=? AND u.is_active=1 AND UPPER(role.role_name) IN ('TECHNICIAN','ADMINISTRATOR'))`, userID).Scan(&allowed)
			if err != nil {
				techFail(w, err)
				return
			}
			if !allowed {
				techJSON(w, 403, map[string]any{"error": "Technician role required"})
				return
			}
			next(w, r.WithContext(ctx), userID)
		}
	}
	mux.HandleFunc("/technician/locations", authorized(func(w http.ResponseWriter, r *http.Request, _ int64) {
		if !techMethod(w, r, http.MethodGet) {
			return
		}
		rows, err := db.QueryContext(r.Context(), `SELECT location_id, location_name, beacon_uuid FROM indoor_nav_common.locations WHERE is_active=1 ORDER BY location_name,location_id`)
		if err != nil {
			techFail(w, err)
			return
		}
		defer rows.Close()
		locations := make([]map[string]any, 0)
		for rows.Next() {
			var id int64
			var name, uuid string
			if err = rows.Scan(&id, &name, &uuid); err != nil {
				techFail(w, err)
				return
			}
			locations = append(locations, map[string]any{"id": id, "name": name, "uuid": uuid})
		}
		if err = rows.Err(); err != nil {
			techFail(w, err)
			return
		}
		techJSON(w, 200, map[string]any{"locations": locations})
	}))
	mux.HandleFunc("/technician/access", authorized(func(w http.ResponseWriter, r *http.Request, id int64) {
		if !techMethod(w, r, http.MethodGet) {
			return
		}
		techJSON(w, 200, map[string]any{"authorized": true, "user_id": id})
	}))
	mux.HandleFunc("/technician/users", authorized(func(w http.ResponseWriter, r *http.Request, _ int64) {
		if !techMethod(w, r, http.MethodGet) {
			return
		}
		rows, err := db.QueryContext(r.Context(), `SELECT user_id,email,is_active FROM indoor_nav_common.users ORDER BY user_id LIMIT 1000`)
		if err != nil {
			techFail(w, err)
			return
		}
		defer rows.Close()
		users := make([]map[string]any, 0)
		for rows.Next() {
			var id int64
			var email string
			var active bool
			if err = rows.Scan(&id, &email, &active); err != nil {
				techFail(w, err)
				return
			}
			users = append(users, map[string]any{"id": id, "email": email, "is_active": active})
		}
		if err = rows.Err(); err != nil {
			techFail(w, err)
			return
		}
		techJSON(w, 200, map[string]any{"users": users})
	}))
	mux.HandleFunc("/technician/config", authorized(func(w http.ResponseWriter, r *http.Request, _ int64) {
		if !techMethod(w, r, http.MethodGet) {
			return
		}
		locationID, err := strconv.ParseInt(r.URL.Query().Get("location_id"), 10, 64)
		if err != nil || locationID <= 0 {
			techJSON(w, 400, map[string]any{"error": "Valid location_id required"})
			return
		}
		tx, err := db.BeginTx(r.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
		if err != nil {
			techFail(w, err)
			return
		}
		defer tx.Rollback()
		var name, uuid string
		err = tx.QueryRowContext(r.Context(), `SELECT location_name,beacon_uuid FROM indoor_nav_common.locations WHERE location_id=? AND is_active=1`, locationID).Scan(&name, &uuid)
		if errors.Is(err, sql.ErrNoRows) {
			techJSON(w, 404, map[string]any{"error": "Active location not found"})
			return
		}
		if err != nil {
			techFail(w, err)
			return
		}
		floors := make([]map[string]any, 0)
		rows, err := tx.QueryContext(r.Context(), `SELECT floor_id,COALESCE(floor_name,''),floor_number FROM indoor_nav_locations.floors WHERE location_id=? ORDER BY building_id,floor_number`, locationID)
		if err != nil {
			techFail(w, err)
			return
		}
		majors := map[int]int{}
		for rows.Next() {
			var id int64
			var floorName string
			var major int
			if err = rows.Scan(&id, &floorName, &major); err != nil {
				rows.Close()
				techFail(w, err)
				return
			}
			if major < 0 || major > 65535 {
				rows.Close()
				techJSON(w, 409, map[string]any{"error": "Floor number cannot be used as BLE Major. Add a dedicated floor-to-major mapping."})
				return
			}
			majors[major]++
			if majors[major] > 1 {
				rows.Close()
				techJSON(w, 409, map[string]any{"error": "Repeated floor numbers across buildings need a dedicated BLE Major mapping."})
				return
			}
			if floorName == "" {
				floorName = fmt.Sprintf("Floor %d", major)
			}
			floors = append(floors, map[string]any{"id": id, "name": floorName, "major": major})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			techFail(w, err)
			return
		}
		nodes := make([]map[string]any, 0)
		rows, err = tx.QueryContext(r.Context(), `SELECT n.node_id,n.floor_id,n.node_code,n.x_coordinate,n.y_coordinate FROM indoor_nav_locations.navigation_nodes n JOIN indoor_nav_locations.floors f ON f.floor_id=n.floor_id AND f.location_id=n.location_id WHERE n.location_id=? AND n.is_active=1 ORDER BY n.node_id`, locationID)
		if err != nil {
			techFail(w, err)
			return
		}
		for rows.Next() {
			var id, floor int64
			var code string
			var x, y float64
			if err = rows.Scan(&id, &floor, &code, &x, &y); err != nil {
				rows.Close()
				techFail(w, err)
				return
			}
			nodes = append(nodes, map[string]any{"id": id, "floor_id": floor, "code": code, "x": x, "y": y})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			techFail(w, err)
			return
		}
		beacons := make([]map[string]any, 0)
		rows, err = tx.QueryContext(r.Context(), `SELECT beacon_id,device_serial,floor_id,navigation_node_id,beacon_minor,COALESCE(label,''),x_coordinate,y_coordinate,tx_power_calibration,status FROM indoor_nav_locations.beacons WHERE location_id=? ORDER BY floor_id,beacon_minor`, locationID)
		if err != nil {
			techFail(w, err)
			return
		}
		for rows.Next() {
			var id, floor int64
			var node sql.NullInt64
			var serial, label, status string
			var minor, power int
			var x, y float64
			if err = rows.Scan(&id, &serial, &floor, &node, &minor, &label, &x, &y, &power, &status); err != nil {
				rows.Close()
				techFail(w, err)
				return
			}
			var linked any
			if node.Valid {
				linked = node.Int64
			}
			beacons = append(beacons, map[string]any{"id": id, "device_serial": serial, "floor_id": floor, "navigation_node_id": linked, "minor": minor, "label": label, "x": x, "y": y, "tx_power_calibration": power, "status": status})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			techFail(w, err)
			return
		}
		if err = tx.Commit(); err != nil {
			techFail(w, err)
			return
		}
		techJSON(w, 200, map[string]any{"location": map[string]any{"id": locationID, "name": name, "uuid": uuid}, "floors": floors, "nodes": nodes, "beacons": beacons})
	}))
	mux.HandleFunc("/technician/beacons", authorized(func(w http.ResponseWriter, r *http.Request, userID int64) {
		if !techMethod(w, r, http.MethodPost) {
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 8192)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var input techBeaconInput
		if err := decoder.Decode(&input); err != nil {
			techJSON(w, 400, map[string]any{"error": "Invalid beacon JSON"})
			return
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			techJSON(w, 400, map[string]any{"error": "Send exactly one JSON object"})
			return
		}
		input.Serial = strings.TrimSpace(input.Serial)
		input.Label = strings.TrimSpace(input.Label)
		if err := input.validate(); err != nil {
			techJSON(w, 400, map[string]any{"error": err.Error()})
			return
		}
		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			techFail(w, err)
			return
		}
		defer tx.Rollback()
		var major int
		err = tx.QueryRowContext(r.Context(), `SELECT f.floor_number FROM indoor_nav_locations.floors f JOIN indoor_nav_common.locations l ON l.location_id=f.location_id WHERE f.floor_id=? AND f.location_id=? AND l.is_active=1 FOR UPDATE`, input.FloorID, input.LocationID).Scan(&major)
		if errors.Is(err, sql.ErrNoRows) {
			techJSON(w, 400, map[string]any{"error": "Floor does not belong to the active location"})
			return
		}
		if err != nil {
			techFail(w, err)
			return
		}
		if major < 0 || major > 65535 {
			techJSON(w, 409, map[string]any{"error": "Floor needs a dedicated BLE Major mapping"})
			return
		}
		var sameMajor int
		err = tx.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM indoor_nav_locations.floors WHERE location_id=? AND floor_number=?`, input.LocationID, major).Scan(&sameMajor)
		if err != nil {
			techFail(w, err)
			return
		}
		if sameMajor != 1 {
			techJSON(w, 409, map[string]any{"error": "Repeated floor numbers need a dedicated BLE Major mapping"})
			return
		}
		if input.NodeID != nil {
			var nodeID int64
			err = tx.QueryRowContext(r.Context(), `SELECT node_id FROM indoor_nav_locations.navigation_nodes WHERE node_id=? AND location_id=? AND floor_id=? AND is_active=1 FOR UPDATE`, *input.NodeID, input.LocationID, input.FloorID).Scan(&nodeID)
			if errors.Is(err, sql.ErrNoRows) {
				techJSON(w, 400, map[string]any{"error": "Linked node must be active on the chosen floor"})
				return
			}
			if err != nil {
				techFail(w, err)
				return
			}
		}
		var existing int64
		err = tx.QueryRowContext(r.Context(), `SELECT beacon_id FROM indoor_nav_locations.beacons WHERE (device_serial=? OR (location_id=? AND floor_id=? AND beacon_minor=?)) AND beacon_id<>? LIMIT 1`, input.Serial, input.LocationID, input.FloorID, input.Minor, input.IDOrZero()).Scan(&existing)
		if err == nil {
			techJSON(w, 409, map[string]any{"error": "Device serial or floor/minor already belongs to another beacon"})
			return
		}
		if !errors.Is(err, sql.ErrNoRows) {
			techFail(w, err)
			return
		}
		var result sql.Result
		var id int64
		if input.ID == nil {
			result, err = tx.ExecContext(r.Context(), `INSERT INTO indoor_nav_locations.beacons (location_id,floor_id,navigation_node_id,device_serial,beacon_minor,label,x_coordinate,y_coordinate,tx_power_calibration,status) VALUES (?,?,?,?,?,?,?,?,?,?)`, input.LocationID, input.FloorID, input.NodeID, input.Serial, input.Minor, input.Label, input.X, input.Y, input.Power, input.Status)
			if err == nil {
				id, err = result.LastInsertId()
			}
		} else {
			id = *input.ID
			var oldLocation int64
			err = tx.QueryRowContext(r.Context(), `SELECT location_id FROM indoor_nav_locations.beacons WHERE beacon_id=? FOR UPDATE`, id).Scan(&oldLocation)
			if errors.Is(err, sql.ErrNoRows) {
				techJSON(w, 404, map[string]any{"error": "Beacon not found"})
				return
			}
			if err != nil {
				techFail(w, err)
				return
			}
			if oldLocation != input.LocationID {
				techJSON(w, 409, map[string]any{"error": "Moving beacons between locations requires a separate reassignment operation"})
				return
			}
			// Preserve the block on the same floor; clear it when moving floors.
			_, err = tx.ExecContext(r.Context(), `UPDATE indoor_nav_locations.beacons SET block_id=CASE WHEN floor_id=? THEN block_id ELSE NULL END,floor_id=?,navigation_node_id=?,device_serial=?,beacon_minor=?,label=?,x_coordinate=?,y_coordinate=?,tx_power_calibration=?,status=? WHERE beacon_id=?`, input.FloorID, input.FloorID, input.NodeID, input.Serial, input.Minor, input.Label, input.X, input.Y, input.Power, input.Status, id)
		}
		if err != nil {
			techFail(w, err)
			return
		}
		if err = tx.Commit(); err != nil {
			techFail(w, err)
			return
		}
		log.Printf("Technician user %d saved beacon %d at location %d", userID, id, input.LocationID)
		techJSON(w, 200, map[string]any{"id": id, "saved": true})
	}))
}

type techBeaconInput struct {
	ID         *int64  `json:"id"`
	LocationID int64   `json:"location_id"`
	FloorID    int64   `json:"floor_id"`
	NodeID     *int64  `json:"navigation_node_id"`
	Serial     string  `json:"device_serial"`
	Minor      int     `json:"minor"`
	Label      string  `json:"label"`
	X          float64 `json:"x"`
	Y          float64 `json:"y"`
	Power      int     `json:"tx_power_calibration"`
	Status     string  `json:"status"`
}

func (b techBeaconInput) IDOrZero() int64 {
	if b.ID == nil {
		return 0
	}
	return *b.ID
}
func (b techBeaconInput) validate() error {
	if b.LocationID <= 0 || b.FloorID <= 0 || (b.ID != nil && *b.ID <= 0) || (b.NodeID != nil && *b.NodeID <= 0) {
		return errors.New("Valid location, floor and beacon IDs required")
	}
	if b.Serial == "" || utf8.RuneCountInString(b.Serial) > 32 {
		return errors.New("Device serial must contain 1–32 characters")
	}
	if b.Label == "" || utf8.RuneCountInString(b.Label) > 32 || strings.ContainsAny(b.Label, "\r\n") {
		return errors.New("Label must contain 1–32 characters on one line")
	}
	if b.Minor < 0 || b.Minor > 65535 || b.Power < -127 || b.Power > 127 {
		return errors.New("Minor or calibration out of range")
	}
	if math.IsNaN(b.X) || math.IsInf(b.X, 0) || math.IsNaN(b.Y) || math.IsInf(b.Y, 0) {
		return errors.New("Coordinates must be finite")
	}
	if b.Status != "ACTIVE" && b.Status != "INACTIVE" && b.Status != "MAINTENANCE" && b.Status != "REPLACED" {
		return errors.New("Status must be ACTIVE, INACTIVE, MAINTENANCE or REPLACED")
	}
	return nil
}
func techJSON(w http.ResponseWriter, status int, value any) {
	bytes, err := json.Marshal(value)
	if err != nil {
		log.Printf("Technician JSON failed: %v", err)
		http.Error(w, "Could not encode response", 500)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(bytes)
}
func techFail(w http.ResponseWriter, err error) {
	log.Printf("Technician request failed: %v", err)
	techJSON(w, 500, map[string]any{"error": "Database request failed; check backend terminal"})
}
func techMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	techJSON(w, 405, map[string]any{"error": "Use " + method})
	return false
}

// revoke must remove the token from the existing server session store.
func registerSessionLogout(mux *http.ServeMux, resolve func(context.Context, string) (int64, error), revoke func(string)) {
	mux.HandleFunc("/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		if !techMethod(w, r, http.MethodPost) {
			return
		}
		parts := strings.Fields(r.Header.Get("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			techJSON(w, 401, map[string]any{"error": "Sign in required"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if _, err := resolve(ctx, parts[1]); err != nil {
			techJSON(w, 401, map[string]any{"error": "Invalid session"})
			return
		}
		revoke(parts[1])
		techJSON(w, 200, map[string]any{"signed_out": true})
	})
}
