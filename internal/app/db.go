package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// timeLayout is a fixed-width UTC layout so timestamps stored as TEXT sort chronologically.
const timeLayout = "2006-01-02 15:04:05.000000"

// SelectedCollectionName is the reserved collection that holds the user's current photo selection.
const SelectedCollectionName = "selected"

// SelectedCollectionID is the fixed ID of the reserved selected collection.
const SelectedCollectionID int64 = 0

// ErrProtectedCollection is returned when attempting to delete the selected collection.
var ErrProtectedCollection = errors.New("the selected collection cannot be deleted")

// SavedSearch represents criteria persisted for later reuse.
type SavedSearch struct {
	ID    int64
	URL   string
	Title string
}

// DB wraps the SQLite database. All SQL used by the application lives in this file.
type DB struct {
	sql *loggedDB
}

// PhotoRecord holds the values written to the photos table when a file is indexed.
type PhotoRecord struct {
	Path            string
	RawPath         string
	TakenAt         time.Time
	Latitude        *float64
	Longitude       *float64
	Thumbnail       []byte
	Location        string
	LocationSource  string
	Settlement      string
	Region          string
	Country         string
	CameraModel     string
	FocalLength     *float64
	FlashFired      *bool
	Objects         []string
	DominantColors  []string
	MediaType       string
	DurationSeconds *float64
	IndexedAt       time.Time
	DayOfYear       int
}

// OpenDB opens (creating if needed) the SQLite database at path and applies the schema.
func OpenDB(ctx context.Context, path string) (*DB, error) {
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	conn, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db := &DB{sql: &loggedDB{loggedQuerier: loggedQuerier{querier: conn}, db: conn}}
	if err := conn.PingContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	if err := db.migrate(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	return db, nil
}

// Close closes the underlying database handle.
func (d *DB) Close() error {
	return d.sql.Close()
}

// migrate creates application tables and indexes.
func (d *DB) migrate(ctx context.Context) error {
	schema := []string{`CREATE TABLE IF NOT EXISTS photos (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			path TEXT UNIQUE NOT NULL,
			raw_path TEXT,
			taken_at TEXT NOT NULL,
			latitude REAL,
			longitude REAL,
			thumbnail BLOB,
			location TEXT,
			location_source TEXT,
			settlement TEXT,
			region TEXT,
			country TEXT,
			camera_model TEXT,
			focal_length REAL,
			flash_fired INTEGER,
			objects TEXT NOT NULL DEFAULT '[]',
			dominant_colors TEXT NOT NULL DEFAULT '[]',
			media_type TEXT NOT NULL DEFAULT 'photo' CHECK (media_type IN ('photo', 'video')),
			duration_seconds REAL,
			day_of_year INTEGER,
			indexed_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f000', 'now'))
		)`, `CREATE TABLE IF NOT EXISTS index_state (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			last_indexed_at TEXT NOT NULL
		)`, `CREATE TABLE IF NOT EXISTS collections (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT UNIQUE NOT NULL CHECK (length(trim(name)) > 0),
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f000', 'now'))
		)`, `CREATE TABLE IF NOT EXISTS collection_photos (
			collection_id INTEGER NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
			photo_id INTEGER NOT NULL REFERENCES photos(id) ON DELETE CASCADE,
			PRIMARY KEY (collection_id, photo_id)
		)`, `CREATE TABLE IF NOT EXISTS saved_searches (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			url TEXT NOT NULL UNIQUE CHECK (length(trim(url)) > 0),
			title TEXT NOT NULL CHECK (length(trim(title)) > 0),
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%d %H:%M:%f000', 'now')),
			UNIQUE(url)
		)`, `CREATE INDEX IF NOT EXISTS photos_taken_at_idx ON photos (taken_at)`,
		`CREATE INDEX IF NOT EXISTS photos_location_idx ON photos (latitude, longitude)
			WHERE latitude IS NOT NULL AND longitude IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS photos_media_type_idx ON photos (media_type)`,
		`CREATE INDEX IF NOT EXISTS photos_day_of_year ON photos (day_of_year)`}
	for _, statement := range schema {
		if _, err := d.sql.ExecContext(ctx, statement); err != nil {
			slog.Error("migrate database", "error", err)
			return fmt.Errorf("migrate database: %w", err)
		}
	}
	if err := d.migrateSelectedCollection(ctx); err != nil {
		return fmt.Errorf("migrate selected collection: %w", err)
	}
	if err := d.addColumnIfMissing(ctx, "photos", "golden_hour", "INTEGER"); err != nil {
		return err
	}
	if err := d.addColumnIfMissing(ctx, "photos", "objects_detected_at", "TEXT"); err != nil {
		return err
	}
	if err := d.addColumnIfMissing(ctx, "photos", "x", "REAL"); err != nil {
		return err
	}
	if err := d.addColumnIfMissing(ctx, "photos", "y", "REAL"); err != nil {
		return err
	}
	if err := d.populateMercatorCoordinates(ctx); err != nil {
		return err
	}
	if _, err := d.sql.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS photos_golden_hour_idx ON photos (golden_hour)`); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	return nil
}

// migrateSelectedCollection reserves ID zero for the selected collection, preserving memberships.
// Input: a migration context; output: an error if the transactional migration fails.
func (d *DB) migrateSelectedCollection(ctx context.Context) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `PRAGMA defer_foreign_keys = ON`); err != nil {
		return err
	}

	var existingName string
	err = tx.QueryRowContext(ctx, `SELECT name FROM collections WHERE id = ?`, SelectedCollectionID).Scan(&existingName)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && existingName != SelectedCollectionName {
		var replacementID int64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(id) + 1 FROM collections`).Scan(&replacementID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE collections SET id = ? WHERE id = ?`, replacementID, SelectedCollectionID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE collection_photos SET collection_id = ? WHERE collection_id = ?`, replacementID, SelectedCollectionID); err != nil {
			return err
		}
	}

	var selectedID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM collections WHERE name = ?`, SelectedCollectionName).Scan(&selectedID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.ExecContext(ctx, `INSERT INTO collections (id, name) VALUES (?, ?)`, SelectedCollectionID, SelectedCollectionName)
	case err != nil:
		return err
	case selectedID != SelectedCollectionID:
		if _, err = tx.ExecContext(ctx, `UPDATE collections SET id = ? WHERE id = ?`, SelectedCollectionID, selectedID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE collection_photos SET collection_id = ? WHERE collection_id = ?`, SelectedCollectionID, selectedID)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// EPSG900913Project converts WGS84 latitude/longitude into metres in EPSG:900913.
func EPSG900913Project(latitude, longitude float64) (x, y float64) {
	const earthRadius = 6378137.0
	const maxMercatorLatitude = 85.05112878
	if latitude > maxMercatorLatitude {
		latitude = maxMercatorLatitude
	} else if latitude < -maxMercatorLatitude {
		latitude = -maxMercatorLatitude
	}
	radians := math.Pi / 180.0
	x = earthRadius * longitude * radians
	y = earthRadius * math.Log(math.Tan(math.Pi/4.0+(latitude*radians)/2.0))
	return x, y
}

// populateMercatorCoordinates backfills x/y for existing geotagged photo records.
func (d *DB) populateMercatorCoordinates(ctx context.Context) error {
	rows, err := d.sql.QueryContext(ctx, `SELECT id, latitude, longitude FROM photos
		WHERE latitude IS NOT NULL AND longitude IS NOT NULL
			AND (x IS NULL OR y IS NULL)`)
	if err != nil {
		return fmt.Errorf("migrate mercator coordinates: %w", err)
	}
	type coordinate struct {
		id        int64
		latitude  float64
		longitude float64
	}
	var coordinates []coordinate
	for rows.Next() {
		var item coordinate
		if err := rows.Scan(&item.id, &item.latitude, &item.longitude); err != nil {
			rows.Close()
			return fmt.Errorf("migrate mercator coordinates: %w", err)
		}
		coordinates = append(coordinates, item)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("migrate mercator coordinates: %w", err)
	}
	for _, item := range coordinates {
		x, y := EPSG900913Project(item.latitude, item.longitude)
		if _, err := d.sql.ExecContext(ctx,
			`UPDATE photos SET x = ?, y = ? WHERE id = ?`, x, y, item.id); err != nil {
			return fmt.Errorf("migrate mercator coordinates: %w", err)
		}
	}
	_, err = d.sql.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS photos_xy_idx ON photos (x, y) WHERE x IS NOT NULL AND y IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("migrate mercator coordinates: %w", err)
	}
	return nil
}

// addColumnIfMissing adds a column to an existing table when it is not already present.
func (d *DB) addColumnIfMissing(ctx context.Context, table, column, definition string) error {
	var count int
	err := d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&count)
	if err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	if count > 0 {
		return nil
	}
	if _, err := d.sql.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, definition)); err != nil {
		slog.Error("migrate database", "error", err)
		return fmt.Errorf("migrate database: %w", err)
	}
	return nil
}

// UpdateGoldenHour computes golden_hour (1/0) for located photos where it is not yet set.
// It must run after locations are extracted and derived. It returns the number of photos updated.
func (d *DB) UpdateGoldenHour(ctx context.Context) (int, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT id, taken_at, latitude, longitude FROM photos
		WHERE golden_hour IS NULL AND latitude IS NOT NULL AND longitude IS NOT NULL
		AND NOT (latitude = 0 AND longitude = 0)`)
	if err != nil {
		return 0, fmt.Errorf("select photos for golden hour: %w", err)
	}
	type update struct {
		id    int64
		value int
	}
	var updates []update
	for rows.Next() {
		var id int64
		var takenAt string
		var lat, lon float64
		if err := rows.Scan(&id, &takenAt, &lat, &lon); err != nil {
			rows.Close()
			return 0, err
		}
		t, err := parseTime(takenAt)
		if err != nil {
			slog.Warn("golden hour: invalid taken_at", "id", id, "error", err)
			continue
		}
		value := 0
		if isGoldenHour(t, lat, lon) {
			value = 1
		}
		updates = append(updates, update{id, value})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `UPDATE photos SET golden_hour = ? WHERE id = ?`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	for _, u := range updates {
		if _, err := stmt.ExecContext(ctx, u.value, u.id); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(updates), nil
}

// formatTime converts a timestamp to the fixed-width UTC format used by SQLite.
// Input: a timestamp; output: its UTC database representation.
func formatTime(t time.Time) string {
	return t.UTC().Format(timeLayout)
}

// parseTime parses a database timestamp as UTC.
// Input: the stored timestamp string; output: the parsed time or a parse error.
func parseTime(value string) (time.Time, error) {
	return time.ParseInLocation(timeLayout, value, time.UTC)
}

// encodeStrings serializes string values as JSON, encoding nil as an empty array.
// Input: a string slice; output: its JSON representation.
func encodeStrings(values []string) string {
	if values == nil {
		values = []string{}
	}
	encoded, _ := json.Marshal(values)
	return string(encoded)
}

// decodeStrings deserializes a JSON string array, treating an empty value as no strings.
// Input: the stored JSON string; output: decoded values or a JSON parse error.
func decodeStrings(value string) ([]string, error) {
	var values []string
	if value == "" {
		return values, nil
	}
	if err := json.Unmarshal([]byte(value), &values); err != nil {
		return nil, err
	}
	return values, nil
}

// idPlaceholders returns "?,?,?" and the matching args for an IN clause.
func idPlaceholders(ids []int64) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
}

type rowScanner interface {
	Scan(...any) error
}

// scanPhoto scans the columns shared by all photo list queries; extra receives any trailing columns.
func scanPhoto(row rowScanner, extra ...any) (Photo, error) {
	var photo Photo
	var takenAt, objects, colors string
	dest := append([]any{&photo.ID, &photo.Path, &photo.RawPath, &takenAt, &photo.Latitude, &photo.Longitude,
		&photo.Location, &photo.CameraModel, &photo.FocalLength, &photo.FlashFired, &objects, &colors}, extra...)
	if err := row.Scan(dest...); err != nil {
		return Photo{}, err
	}
	var err error
	if photo.TakenAt, err = parseTime(takenAt); err != nil {
		return Photo{}, err
	}
	if photo.Objects, err = decodeStrings(objects); err != nil {
		return Photo{}, err
	}
	if photo.DominantColors, err = decodeStrings(colors); err != nil {
		return Photo{}, err
	}
	return photo, nil
}

const photoColumns = `id, path, COALESCE(raw_path, ''), taken_at, latitude, longitude, COALESCE(location, ''),
	camera_model, focal_length, flash_fired, objects, dominant_colors`

// queryPhotos executes a parameterized photo query and scans every result row.
// Inputs: a context, SQL query, and query arguments; outputs: matching photos or an error.
func (d *DB) queryPhotos(ctx context.Context, query string, args ...any) ([]Photo, error) {
	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var photos []Photo
	for rows.Next() {
		photo, err := scanPhoto(rows)
		if err != nil {
			slog.Error("Error scanning photo row: ", "err", err)
			return nil, err
		}
		photos = append(photos, photo)
	}
	return photos, rows.Err()
}

// queryStrings executes a parameterized query whose rows each contain one string.
// Inputs: a context, SQL query, and query arguments; outputs: returned strings or an error.
func (d *DB) queryStrings(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := d.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

// LastIndexedAt returns the time of the last completed index run, or nil if none.
func (d *DB) LastIndexedAt(ctx context.Context) (*time.Time, error) {
	var value string
	err := d.sql.QueryRowContext(ctx, "SELECT last_indexed_at FROM index_state WHERE id = 1").Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get last indexed time: %w", err)
	}
	parsed, err := parseTime(value)
	if err != nil {
		return nil, fmt.Errorf("get last indexed time: %w", err)
	}
	return &parsed, nil
}

// RecordIndexedAt stores the timestamp of the latest successful indexing run.
func (d *DB) RecordIndexedAt(ctx context.Context, indexedAt time.Time) error {
	_, err := d.sql.ExecContext(ctx, `INSERT INTO index_state (id, last_indexed_at) VALUES (1, ?)
		ON CONFLICT (id) DO UPDATE SET last_indexed_at = excluded.last_indexed_at`, formatTime(indexedAt))
	if err != nil {
		slog.Error("record indexed time", "error", err)
		return fmt.Errorf("record indexed time: %w", err)
	}
	return nil
}

// ClearIndexState forgets the last index time so the next run indexes everything.
func (d *DB) ClearIndexState(ctx context.Context) error {
	_, err := d.sql.ExecContext(ctx, "DELETE FROM index_state")
	return err
}

// DeleteStalePhotos removes photos not re-indexed since the given time.
func (d *DB) DeleteStalePhotos(ctx context.Context, before time.Time) error {
	_, err := d.sql.ExecContext(ctx, "DELETE FROM photos WHERE indexed_at < ?", formatTime(before))
	return err
}

// DeleteMissingPhotos removes photos whose path is not in existing, returning the number removed.
func (d *DB) DeleteMissingPhotos(ctx context.Context, existing []string) (int64, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS existing_paths (path TEXT PRIMARY KEY)`); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM existing_paths`); err != nil {
		return 0, err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO existing_paths (path) VALUES (?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	for _, path := range existing {
		if _, err := stmt.ExecContext(ctx, path); err != nil {
			return 0, err
		}
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM photos WHERE path NOT IN (SELECT path FROM existing_paths)`)
	if err != nil {
		return 0, err
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM existing_paths`); err != nil {
		return 0, err
	}
	return removed, tx.Commit()
}

// UpsertPhoto inserts or updates a photo record keyed by path. A nil thumbnail keeps any existing one.
func (d *DB) UpsertPhoto(ctx context.Context, p PhotoRecord) error {
	var thumbnail any
	if len(p.Thumbnail) > 0 {
		thumbnail = p.Thumbnail
	}
	var x, y any
	if p.Latitude != nil && p.Longitude != nil {
		mercatorX, mercatorY := EPSG900913Project(*p.Latitude, *p.Longitude)
		x, y = mercatorX, mercatorY
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO photos (path, raw_path, taken_at, latitude, longitude, x, y, thumbnail, location, location_source, settlement, region, country, camera_model, focal_length, flash_fired, dominant_colors, media_type, duration_seconds, indexed_at, day_of_year)
		VALUES (?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (path) DO UPDATE
		SET taken_at = excluded.taken_at, latitude = excluded.latitude, longitude = excluded.longitude, x = excluded.x, y = excluded.y,
		raw_path = excluded.raw_path, thumbnail = COALESCE(excluded.thumbnail, photos.thumbnail), location = excluded.location, location_source = excluded.location_source,
			settlement = excluded.settlement, region = excluded.region, country = excluded.country,
			camera_model = excluded.camera_model, focal_length = excluded.focal_length,
			flash_fired = excluded.flash_fired, dominant_colors = excluded.dominant_colors,
			media_type = excluded.media_type, duration_seconds = excluded.duration_seconds, indexed_at = excluded.indexed_at, day_of_year = excluded.day_of_year, golden_hour = NULL, objects_detected_at = NULL`,
		p.Path, p.RawPath, formatTime(p.TakenAt), p.Latitude, p.Longitude, x, y, thumbnail, p.Location, p.LocationSource,
		p.Settlement, p.Region, p.Country, p.CameraModel, p.FocalLength, p.FlashFired,
		encodeStrings(p.DominantColors), p.MediaType, p.DurationSeconds,
		formatTime(p.IndexedAt), p.DayOfYear)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteGallery deletes a user gallery.
func (d *DB) DeleteGallery(ctx context.Context, id int64) error {
	return d.DeleteCollection(ctx, id)
}

// PendingDetection is a photo awaiting object detection.
type PendingDetection struct {
	ID        int64
	Path      string
	Thumbnail []byte
}

// PendingDetectionCount returns the number of photos awaiting object detection.
func (d *DB) PendingDetectionCount(ctx context.Context) (int, error) {
	var count int
	err := d.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM photos WHERE objects_detected_at IS NULL`).Scan(&count)
	return count, err
}

// NextPendingDetections returns up to limit photos with id > afterID whose object detection has not run.
func (d *DB) NextPendingDetections(ctx context.Context, afterID int64, limit int) ([]PendingDetection, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT id, path, thumbnail FROM photos
		WHERE objects_detected_at IS NULL AND id > ? ORDER BY id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pending []PendingDetection
	for rows.Next() {
		var p PendingDetection
		if err := rows.Scan(&p.ID, &p.Path, &p.Thumbnail); err != nil {
			return nil, err
		}
		pending = append(pending, p)
	}
	return pending, rows.Err()
}

// SetDetectedObjects stores detection results and marks detection as done for a photo.
func (d *DB) SetDetectedObjects(ctx context.Context, id int64, labels []string) error {
	_, err := d.sql.ExecContext(ctx, `UPDATE photos SET objects = ?, objects_detected_at = ? WHERE id = ?`,
		encodeStrings(labels), formatTime(time.Now().UTC()), id)
	return err
}

// DeriveNearbyLocations populates location data for un-geotagged photos taken within 1 hour of a geotagged photo.
func (d *DB) DeriveNearbyLocations(ctx context.Context) error {
	_, err := d.sql.ExecContext(ctx, `
		WITH ranked_sources AS (
			SELECT target.id AS target_id,
				source.latitude, source.longitude, source.location,
				source.settlement, source.region, source.country,
				ROW_NUMBER() OVER (
					PARTITION BY target.id
					ORDER BY ABS(julianday(source.taken_at) - julianday(target.taken_at)), source.id
				) AS rn
			FROM photos AS target
			JOIN photos AS source
				ON source.id <> target.id
				AND source.location IS NOT NULL AND source.location <> ''
				AND source.location_source = 'extracted'
				AND source.latitude IS NOT NULL AND source.longitude IS NOT NULL
				AND source.latitude <> 0 AND source.longitude <> 0
				AND source.taken_at BETWEEN strftime('%Y-%m-%d %H:%M:%f000', target.taken_at, '-1 hour')
					AND strftime('%Y-%m-%d %H:%M:%f000', target.taken_at, '+1 hour')
			WHERE (target.location IS NULL OR target.location = ''
					OR target.latitude IS NULL OR target.longitude IS NULL
					OR target.latitude = 0 OR target.longitude = 0)
				AND target.location_source IS NOT 'extracted'
		)
		UPDATE photos AS target
		SET latitude = source.latitude, longitude = source.longitude,
			location = source.location, location_source = 'derived',
			settlement = source.settlement, region = source.region, country = source.country
		FROM ranked_sources AS source
		WHERE source.rn = 1 AND source.target_id = target.id`)
	if err != nil {
		return err
	}
	return d.populateMercatorCoordinates(ctx)
}

// CameraModels returns distinct camera models.
func (d *DB) CameraModels(ctx context.Context) ([]string, error) {
	return d.queryStrings(ctx, `SELECT DISTINCT camera_model FROM photos WHERE camera_model IS NOT NULL AND camera_model <> '' ORDER BY camera_model`)
}

// FocalLengths returns distinct focal lengths formatted as text.
func (d *DB) FocalLengths(ctx context.Context) ([]string, error) {
	return d.queryStrings(ctx, `SELECT DISTINCT printf('%g', focal_length) FROM photos WHERE focal_length IS NOT NULL ORDER BY focal_length`)
}

// Labels returns distinct detected object labels.
func (d *DB) Labels(ctx context.Context) ([]string, error) {
	return d.queryStrings(ctx, `SELECT DISTINCT j.value FROM photos, json_each(photos.objects) AS j ORDER BY 1`)
}

// Colors returns distinct dominant colors.
func (d *DB) Colors(ctx context.Context) ([]string, error) {
	return d.queryStrings(ctx, `SELECT DISTINCT j.value FROM photos, json_each(photos.dominant_colors) AS j ORDER BY 1`)
}

// SearchLocations loads distinct settlement and region filter choices for the search view.
func (d *DB) SearchLocations(ctx context.Context) ([]SearchLocation, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT DISTINCT 'settlement:' || settlement, settlement,
		concat_ws(', ', settlement, region, country), settlement, region
		FROM photos WHERE settlement IS NOT NULL AND settlement <> ''
		UNION
		SELECT DISTINCT 'region:' || region, region, concat_ws(', ', region, country), '', region
		FROM photos WHERE region IS NOT NULL AND region <> ''
	ORDER BY 3`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var locations []SearchLocation
	for rows.Next() {
		var location SearchLocation
		var rawValue string
		if err := rows.Scan(&location.Value, &rawValue, &location.Label, &location.Settlement, &location.Region); err != nil {
			return nil, err
		}
		locations = append(locations, location)
	}
	return locations, rows.Err()
}

// SearchPhotos queries photos using the filters in SearchData.
func (d *DB) SearchPhotos(ctx context.Context, search SearchData) ([]Photo, error) {
	return d.queryPhotos(ctx, `SELECT `+photoColumns+`
		FROM photos
		WHERE (?1 = '' OR taken_at >= ?1)
		AND (?2 = '' OR taken_at < date(?2, '+1 day'))
		AND (?3 = '' OR EXISTS (SELECT 1 FROM json_each(photos.objects) WHERE value = ?3))
		AND (?4 = '' OR camera_model = ?4)
		AND (?5 = '' OR focal_length = CAST(?5 AS REAL))
		AND (?6 = '' OR (?6 = 'yes' AND flash_fired = 1) OR (?6 = 'no' AND flash_fired = 0))
		AND (?7 = '' OR settlement = ?7)
		AND (?8 = '' OR region = ?8)
		AND (?9 = '' OR EXISTS (SELECT 1 FROM json_each(photos.dominant_colors) WHERE value = ?9))
		ORDER BY taken_at ASC, id ASC`,
		search.DateFrom, search.DateTo, search.Label, search.CameraModel,
		search.FocalLength, search.Flash, search.Settlement, search.Region, search.Color)
}

// LocationGroups returns all settlement and region names with photo counts.
func (d *DB) LocationGroups(ctx context.Context) ([]LocationGroup, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT 'settlement', settlement, COUNT(*) FROM photos
		WHERE settlement IS NOT NULL AND settlement <> '' GROUP BY settlement
		UNION ALL
		SELECT 'region', region, COUNT(*) FROM photos
		WHERE region IS NOT NULL AND region <> '' GROUP BY region
	ORDER BY 2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var groups []LocationGroup
	for rows.Next() {
		var group LocationGroup
		if err := rows.Scan(&group.Kind, &group.Name, &group.Count); err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

// LocationBounds returns the bounding box of geotagged photos matching a settlement or region.
func (d *DB) LocationBounds(ctx context.Context, settlement, region string) (*MapBounds, error) {
	var minLat, minLng, maxLat, maxLng *float64
	err := d.sql.QueryRowContext(ctx, `SELECT MIN(latitude), MIN(longitude), MAX(latitude), MAX(longitude)
		FROM photos WHERE (?1 = '' OR settlement = ?1) AND (?2 = '' OR region = ?2)
		AND latitude IS NOT NULL AND longitude IS NOT NULL`, settlement, region).
		Scan(&minLat, &minLng, &maxLat, &maxLng)
	if err != nil {
		return nil, err
	}
	if minLat == nil || minLng == nil || maxLat == nil || maxLng == nil {
		return nil, sql.ErrNoRows
	}
	return &MapBounds{MinLatitude: *minLat, MinLongitude: *minLng, MaxLatitude: *maxLat, MaxLongitude: *maxLng}, nil
}

// PlaceBounds resolves a named settlement or region and returns its kind and geotagged photo bounds.
func (d *DB) PlaceBounds(ctx context.Context, place string) (kind string, bounds *MapBounds, err error) {
	place = strings.TrimSpace(place)
	if place == "" {
		return "", nil, sql.ErrNoRows
	}
	var minLat, minLng, maxLat, maxLng *float64
	err = d.sql.QueryRowContext(ctx, `SELECT
		CASE WHEN EXISTS (SELECT 1 FROM photos WHERE settlement = ?) THEN 'settlement'
		     WHEN EXISTS (SELECT 1 FROM photos WHERE region = ?) THEN 'region'
		     ELSE '' END,
		MIN(latitude), MIN(longitude), MAX(latitude), MAX(longitude)
		FROM photos
		WHERE (settlement = ? OR region = ?)
		  AND latitude IS NOT NULL AND longitude IS NOT NULL`,
		place, place, place, place).Scan(&kind, &minLat, &minLng, &maxLat, &maxLng)
	if err != nil {
		return "", nil, err
	}
	if kind == "" || minLat == nil || minLng == nil || maxLat == nil || maxLng == nil {
		return "", nil, sql.ErrNoRows
	}
	return kind, &MapBounds{MinLatitude: *minLat, MinLongitude: *minLng, MaxLatitude: *maxLat, MaxLongitude: *maxLng}, nil
}

// MapPoints aggregates geotagged photos into grid cells of cellSize degrees.
func (d *DB) MapPoints(ctx context.Context, minLat, maxLat, minLng, maxLng, cellSize float64, settlement, region string) ([]mapPoint, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT AVG(latitude), AVG(longitude), COUNT(*), MIN(id),
		MIN(latitude), MIN(longitude), MAX(latitude), MAX(longitude)
		FROM photos
		WHERE latitude BETWEEN ?1 AND ?2 AND longitude BETWEEN ?3 AND ?4
		AND (?6 = '' OR settlement = ?6) AND (?7 = '' OR region = ?7)
		GROUP BY CAST(floor((latitude + 90) / ?5) AS INTEGER), CAST(floor((longitude + 180) / ?5) AS INTEGER)
		ORDER BY MIN(id) LIMIT 10000`, minLat, maxLat, minLng, maxLng, cellSize, settlement, region)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	points := make([]mapPoint, 0)
	for rows.Next() {
		var point mapPoint
		if err := rows.Scan(&point.Latitude, &point.Longitude, &point.Count, &point.PhotoID,
			&point.MinLatitude, &point.MinLongitude, &point.MaxLatitude, &point.MaxLongitude); err != nil {
			return nil, err
		}
		points = append(points, point)
	}
	return points, rows.Err()
}

// MapPointForPhoto returns one map point for a geotagged photo.
func (d *DB) MapPointForPhoto(ctx context.Context, photoID int64) ([]mapPoint, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT latitude, longitude, 1, id,
		latitude, longitude, latitude, longitude
		FROM photos WHERE id = ? AND latitude IS NOT NULL AND longitude IS NOT NULL
		AND NOT (latitude = 0 AND longitude = 0)`, photoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	points := make([]mapPoint, 0, 1)
	for rows.Next() {
		var point mapPoint
		if err := rows.Scan(&point.Latitude, &point.Longitude, &point.Count, &point.PhotoID,
			&point.MinLatitude, &point.MinLongitude, &point.MaxLatitude, &point.MaxLongitude); err != nil {
			return nil, err
		}
		points = append(points, point)
	}
	return points, rows.Err()
}

// MapPointsForPhotos returns one point per geotagged photo in an explicit collection ID set.
func (d *DB) MapPointsForPhotos(ctx context.Context, photoIDs []int64) ([]mapPoint, error) {
	if len(photoIDs) == 0 {
		return []mapPoint{}, nil
	}
	placeholders, args := idPlaceholders(photoIDs)
	rows, err := d.sql.QueryContext(ctx, `SELECT latitude, longitude, 1, id,
		latitude, longitude, latitude, longitude
		FROM photos WHERE id IN (`+placeholders+`)
		AND latitude IS NOT NULL AND longitude IS NOT NULL
		AND NOT (latitude = 0 AND longitude = 0)
		ORDER BY taken_at ASC, id ASC LIMIT 10000`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	points := make([]mapPoint, 0, len(photoIDs))
	for rows.Next() {
		var point mapPoint
		if err := rows.Scan(&point.Latitude, &point.Longitude, &point.Count, &point.PhotoID,
			&point.MinLatitude, &point.MinLongitude, &point.MaxLatitude, &point.MaxLongitude); err != nil {
			return nil, err
		}
		points = append(points, point)
	}
	return points, rows.Err()
}

// ClusterPointsForPhotos returns lightweight cluster records for an explicit collection ID set.
func (d *DB) ClusterPointsForPhotos(ctx context.Context, photoIDs []int64) ([]mapPoint, error) {
	if len(photoIDs) == 0 {
		return []mapPoint{}, nil
	}
	placeholders, args := idPlaceholders(photoIDs)
	rows, err := d.sql.QueryContext(ctx, `SELECT id, latitude, longitude, path,
		strftime('%Y-%m-%dT%H:%M:%SZ', taken_at), COALESCE(location, '')
		FROM photos WHERE id IN (`+placeholders+`)
		AND latitude IS NOT NULL AND longitude IS NOT NULL
		AND NOT (latitude = 0 AND longitude = 0)
		ORDER BY taken_at ASC, id ASC LIMIT 10000`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	photos := make([]mapPoint, 0, len(photoIDs))
	for rows.Next() {
		var point mapPoint
		if err := rows.Scan(&point.PhotoID, &point.Latitude, &point.Longitude, &point.Path,
			&point.TakenAt, &point.Location); err != nil {
			return nil, err
		}
		photos = append(photos, point)
	}
	return photos, rows.Err()
}

// ClusterPointForPhoto returns lightweight cluster metadata for one geotagged photo.
func (d *DB) ClusterPointForPhoto(ctx context.Context, photoID int64) ([]mapPoint, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT id, latitude, longitude, path,
		strftime('%Y-%m-%dT%H:%M:%SZ', taken_at), COALESCE(location, '')
		FROM photos WHERE id = ? AND latitude IS NOT NULL AND longitude IS NOT NULL
		AND NOT (latitude = 0 AND longitude = 0)`, photoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	points := make([]mapPoint, 0, 1)
	for rows.Next() {
		var point mapPoint
		if err := rows.Scan(&point.PhotoID, &point.Latitude, &point.Longitude, &point.Path,
			&point.TakenAt, &point.Location); err != nil {
			return nil, err
		}
		points = append(points, point)
	}
	return points, rows.Err()
}

// ClusterPoints lists lightweight records for photos inside a bounding box.
func (d *DB) ClusterPoints(ctx context.Context, minLat, maxLat, minLng, maxLng float64) ([]mapPoint, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT id, latitude, longitude, path,
		strftime('%Y-%m-%dT%H:%M:%SZ', taken_at), COALESCE(location, '')
		FROM photos
		WHERE latitude BETWEEN ?1 AND ?2 AND longitude BETWEEN ?3 AND ?4
		ORDER BY taken_at ASC, id ASC LIMIT 10000`, minLat, maxLat, minLng, maxLng)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	photos := make([]mapPoint, 0)
	for rows.Next() {
		var photo mapPoint
		if err := rows.Scan(&photo.PhotoID, &photo.Latitude, &photo.Longitude, &photo.Path, &photo.TakenAt, &photo.Location); err != nil {
			return nil, err
		}
		photos = append(photos, photo)
	}
	return photos, rows.Err()
}

// PhotosInBounds returns full photo records inside a bounding box.
func (d *DB) PhotosInBounds(ctx context.Context, minLat, maxLat, minLng, maxLng float64) ([]Photo, error) {
	return d.queryPhotos(ctx, `SELECT `+photoColumns+`
		FROM photos
		WHERE latitude BETWEEN ?1 AND ?2 AND longitude BETWEEN ?3 AND ?4
		ORDER BY taken_at ASC, id ASC LIMIT 10000`, minLat, maxLat, minLng, maxLng)
}

// Months returns all capture months (YYYY-MM) with photo counts, newest first.
func (d *DB) Months(ctx context.Context) ([]Month, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT strftime('%Y-%m', taken_at), COUNT(*)
		FROM photos GROUP BY 1 ORDER BY 1 DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var months []Month
	for rows.Next() {
		var month Month
		if err := rows.Scan(&month.Key, &month.Count); err != nil {
			return nil, err
		}
		months = append(months, month)
	}
	return months, rows.Err()
}

// PhotosForMonth returns photos captured in the month key (YYYY-MM).
func (d *DB) PhotosForMonth(ctx context.Context, month string) ([]Photo, error) {
	start, err := time.Parse("2006-01", month)
	if err != nil {
		return nil, err
	}
	return d.queryPhotos(ctx, `SELECT `+photoColumns+` FROM photos
		WHERE taken_at >= ? AND taken_at < ? ORDER BY taken_at ASC`,
		formatTime(start), formatTime(start.AddDate(0, 1, 0)))
}

// PhotoIDsForDayRange returns IDs of photos whose day_of_year is in (startDay, endDay].
func (d *DB) PhotoIDsForDayRange(ctx context.Context, startDay, endDay int) ([]int64, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT id FROM photos WHERE day_of_year > ? AND day_of_year <= ? ORDER BY taken_at ASC`, startDay, endDay)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			slog.Error("Error scanning photo id for this week: ", "err", err)
			continue
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// PhotosByIDs loads photos (including media type and day of year) by primary key.
func (d *DB) PhotosByIDs(ctx context.Context, ids []int64) ([]Photo, error) {
	if len(ids) == 0 {
		slog.Warn("PhotosByIDs called with empty ids slice")
		return nil, nil
	}
	placeholders, args := idPlaceholders(ids)
	rows, err := d.sql.QueryContext(ctx, `SELECT `+photoColumns+`, media_type, COALESCE(day_of_year, 0), COALESCE(golden_hour, 0)
		FROM photos WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		slog.Error("Error querying photos by IDs: ", "err", err)
		return nil, err
	}
	defer rows.Close()
	var photos []Photo
	for rows.Next() {
		var mediaType string
		var dayOfYear, goldenHour int
		photo, err := scanPhoto(rows, &mediaType, &dayOfYear, &goldenHour)
		if err != nil {
			slog.Error("Error scanning photo row: ", "err", err)
			return nil, err
		}
		photo.MediaType, photo.DayOfYear, photo.GoldenHour = mediaType, dayOfYear, goldenHour != 0
		photos = append(photos, photo)
	}
	return photos, rows.Err()
}

// PhotoByID loads a single photo, returning sql.ErrNoRows if it does not exist.
func (d *DB) PhotoByID(ctx context.Context, id int64) (Photo, error) {
	photos, err := d.PhotosByIDs(ctx, []int64{id})
	if err != nil {
		return Photo{}, err
	}
	if len(photos) != 1 {
		return Photo{}, sql.ErrNoRows
	}
	return photos[0], nil
}

// PhotoPath returns the relative path of a photo.
func (d *DB) PhotoPath(ctx context.Context, id string) (string, error) {
	var path string
	err := d.sql.QueryRowContext(ctx, "SELECT path FROM photos WHERE id = ?", id).Scan(&path)
	return path, err
}

// PhotoRawPath returns the relative raw file path of a photo, or "" if it has none.
func (d *DB) PhotoRawPath(ctx context.Context, id string) (string, error) {
	var rawPath sql.NullString
	err := d.sql.QueryRowContext(ctx, "SELECT raw_path FROM photos WHERE id = ?", id).Scan(&rawPath)
	return rawPath.String, err
}

// PhotoThumbnail returns the stored thumbnail (possibly empty) and relative path of a photo.
func (d *DB) PhotoThumbnail(ctx context.Context, id string) ([]byte, string, error) {
	var thumbnail []byte
	var path string
	err := d.sql.QueryRowContext(ctx, "SELECT thumbnail, path FROM photos WHERE id = ?", id).Scan(&thumbnail, &path)
	return thumbnail, path, err
}

// PhotoFilePath is a photo's relative path and optional raw file path.
type PhotoFilePath struct {
	Path    string
	RawPath string
}

// PhotoFilePaths returns file paths for the given photo IDs.
func (d *DB) PhotoFilePaths(ctx context.Context, ids []int64) ([]PhotoFilePath, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders, args := idPlaceholders(ids)
	rows, err := d.sql.QueryContext(ctx, `SELECT path, COALESCE(raw_path, '') FROM photos WHERE id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var files []PhotoFilePath
	for rows.Next() {
		var file PhotoFilePath
		if err := rows.Scan(&file.Path, &file.RawPath); err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, rows.Err()
}

// Collections returns all collections with photo counts.
func (d *DB) Collections(ctx context.Context) ([]Collection, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT g.id, g.name, COUNT(gp.photo_id)
		FROM collections g LEFT JOIN collection_photos gp ON gp.collection_id = g.id
		GROUP BY g.id ORDER BY g.name ASC`)
	if err != nil {
		slog.Error("Error querying collections: ", "err", err)
		return nil, err
	}
	defer rows.Close()
	var collections []Collection
	for rows.Next() {
		var collection Collection
		if err := rows.Scan(&collection.ID, &collection.Name, &collection.Count); err != nil {
			return nil, err
		}
		collections = append(collections, collection)
	}
	return collections, rows.Err()
}

// Galleries is the gallery terminology for Collections.
func (d *DB) Galleries(ctx context.Context) ([]Collection, error) {
	return d.Collections(ctx)
}

// CollectionIDsForPhoto returns the set of collection IDs that contain a photo.
func (d *DB) CollectionIDsForPhoto(ctx context.Context, photoID int64) (map[int64]bool, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT collection_id FROM collection_photos WHERE photo_id = ?`, photoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

// CollectionByID returns one collection with its photo count.
func (d *DB) CollectionByID(ctx context.Context, id int64) (Collection, error) {
	var collection Collection
	err := d.sql.QueryRowContext(ctx, `SELECT g.id, g.name, COUNT(gp.photo_id)
		FROM collections g LEFT JOIN collection_photos gp ON gp.collection_id = g.id
		WHERE g.id = ? GROUP BY g.id`, id).Scan(&collection.ID, &collection.Name, &collection.Count)
	return collection, err
}

// GalleryByID loads a gallery with compatibility for the previous collection API.
func (d *DB) GalleryByID(ctx context.Context, id int64) (Collection, error) {
	return d.CollectionByID(ctx, id)
}

// CollectionName returns the name of a collection.
func (d *DB) CollectionName(ctx context.Context, id int64) (string, error) {
	var name string
	err := d.sql.QueryRowContext(ctx, `SELECT name FROM collections WHERE id = ?`, id).Scan(&name)
	return name, err
}

// CollectionExists reports whether a collection with the ID exists.
func (d *DB) CollectionExists(ctx context.Context, id int64) (bool, error) {
	var found bool
	err := d.sql.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM collections WHERE id = ?)", id).Scan(&found)
	return found, err
}

// CreateCollection inserts a new collection and returns its ID.
func (d *DB) CreateCollection(ctx context.Context, name string) (int64, error) {
	var id int64
	err := d.sql.QueryRowContext(ctx, "INSERT INTO collections (name) VALUES (?) RETURNING id", name).Scan(&id)
	return id, err
}

// CreateGallery creates a user gallery.
func (d *DB) CreateGallery(ctx context.Context, name string) (int64, error) {
	return d.CreateCollection(ctx, name)
}

// SaveSearch persists a search URL and derived title.
func (d *DB) SaveSearch(ctx context.Context, searchURL, title string) (int64, error) {
	searchURL = strings.TrimSpace(searchURL)
	title = strings.TrimSpace(title)
	if searchURL == "" || title == "" {
		return 0, errors.New("search URL and title are required")
	}
	var id int64
	err := d.sql.QueryRowContext(ctx,
		`INSERT INTO saved_searches (url, title) VALUES (?, ?) ON CONFLICT(url) DO UPDATE SET title=excluded.title RETURNING id`,
		searchURL, title).Scan(&id)
	return id, err
}

// SavedSearches returns saved search criteria newest first.
func (d *DB) SavedSearches(ctx context.Context) ([]SavedSearch, error) {
	rows, err := d.sql.QueryContext(ctx,
		`SELECT id, url, title FROM saved_searches ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	searches := make([]SavedSearch, 0)
	for rows.Next() {
		var search SavedSearch
		if err := rows.Scan(&search.ID, &search.URL, &search.Title); err != nil {
			return nil, err
		}
		searches = append(searches, search)
	}
	return searches, rows.Err()
}

// DeleteSavedSearch removes a saved search by ID.
func (d *DB) DeleteSavedSearch(ctx context.Context, id int64) error {
	if id < 1 {
		return errors.New("invalid saved search ID")
	}
	_, err := d.sql.ExecContext(ctx, `DELETE FROM saved_searches WHERE id = ?`, id)
	return err
}

// PhotosForCollection returns photos in a collection.
func (d *DB) PhotosForCollection(ctx context.Context, collectionID int64) ([]Photo, error) {
	photos, err := d.queryPhotos(ctx, `SELECT p.id, p.path, COALESCE(p.raw_path, ''), p.taken_at, p.latitude, p.longitude, COALESCE(p.location, ''), p.camera_model, p.focal_length, p.flash_fired, p.objects, p.dominant_colors FROM photos p
		JOIN collection_photos gp ON gp.photo_id = p.id WHERE gp.collection_id = ? ORDER BY p.taken_at ASC`, collectionID)
	if err != nil {
		slog.Error("Error querying photos for collection: ", "err", err)
	}
	return photos, err
}

// PhotosForGallery loads photos from a user gallery.
func (d *DB) PhotosForGallery(ctx context.Context, galleryID int64) ([]Photo, error) {
	return d.PhotosForCollection(ctx, galleryID)
}

// SimilarPhotos returns up to limit photos ranked by overlap with a target photo.
// Detected tags/objects are weighted 10 times more heavily than dominant colours.
func (d *DB) SimilarPhotos(ctx context.Context, targetID int64, limit int) ([]Photo, error) {
	if targetID < 1 {
		return nil, sql.ErrNoRows
	}
	if limit < 1 {
		return nil, nil
	}
	if limit > 100 {
		limit = 100
	}
	return d.queryPhotos(ctx, `
		WITH target AS (
			SELECT id, objects, dominant_colors FROM photos WHERE id = ?
		),
		ranked AS (
			SELECT p.id,
				COALESCE((
					SELECT COUNT(*)
					FROM json_each(p.objects) candidate_tag
					JOIN json_each(target.objects) target_tag
						ON candidate_tag.value = target_tag.value
				), 0) AS tag_matches,
				COALESCE((
					SELECT COUNT(*)
					FROM json_each(p.dominant_colors) candidate_color
					JOIN json_each(target.dominant_colors) target_color
						ON candidate_color.value = target_color.value
				), 0) AS color_matches
			FROM photos p CROSS JOIN target
			WHERE p.id != target.id
		)
		SELECT p.id, p.path, COALESCE(p.raw_path, ''), p.taken_at, p.latitude, p.longitude,
			COALESCE(p.location, ''), p.camera_model, p.focal_length, p.flash_fired,
			p.objects, p.dominant_colors
		FROM ranked JOIN photos p ON p.id = ranked.id
		ORDER BY (ranked.tag_matches * 10 + ranked.color_matches) DESC,
			p.taken_at ASC, p.id ASC
		LIMIT ?`,
		targetID, limit)
}

// NearPhotos returns up to 100 geotagged photos within 50 km of the target,
// ordered by Euclidean EPSG:900913 distance.
func (d *DB) NearPhotos(ctx context.Context, targetID int64, limit int) ([]Photo, error) {
	if targetID < 1 {
		return nil, sql.ErrNoRows
	}
	if limit < 1 {
		return nil, nil
	}
	if limit > 100 {
		limit = 100
	}
	return d.queryPhotos(ctx, `
		WITH target AS (
			SELECT x, y FROM photos WHERE id = ? AND x IS NOT NULL AND y IS NOT NULL
		),
		ranked AS (
			SELECT p.id,
				SQRT((p.x - target.x) * (p.x - target.x) +
					(p.y - target.y) * (p.y - target.y)) AS distance_metres
			FROM photos p CROSS JOIN target
			WHERE p.id != ? AND p.x IS NOT NULL AND p.y IS NOT NULL
				AND SQRT((p.x - target.x) * (p.x - target.x) +
					(p.y - target.y) * (p.y - target.y)) <= 50000
		)
		SELECT p.id, p.path, COALESCE(p.raw_path, ''), p.taken_at, p.latitude, p.longitude,
			COALESCE(p.location, ''), p.camera_model, p.focal_length, p.flash_fired,
			p.objects, p.dominant_colors
		FROM ranked JOIN photos p ON p.id = ranked.id
		ORDER BY ranked.distance_metres ASC, p.taken_at ASC, p.id ASC
		LIMIT ?`,
		targetID, targetID, limit)
}

// RandomPhotoID returns the ID of one randomly selected photo.
func (d *DB) RandomPhotoID(ctx context.Context) (int64, error) {
	var id int64
	err := d.sql.QueryRowContext(ctx,
		`SELECT id FROM photos ORDER BY RANDOM() LIMIT 1`).Scan(&id)
	return id, err
}

// RandomCollectionPhotos returns the target plus up to two distinct random neighbors.
func (d *DB) RandomCollectionPhotos(ctx context.Context, targetID int64) ([]Photo, error) {
	if targetID < 1 {
		return nil, sql.ErrNoRows
	}
	target, err := d.PhotoByID(ctx, targetID)
	if err != nil {
		return nil, err
	}

	rows, err := d.sql.QueryContext(ctx,
		`SELECT id FROM photos WHERE id != ? ORDER BY RANDOM() LIMIT 2`, targetID)
	if err != nil {
		return nil, err
	}
	var neighborIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		neighborIDs = append(neighborIDs, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	neighbors, err := d.PhotosByIDs(ctx, neighborIDs)
	if err != nil {
		return nil, err
	}
	if len(neighbors) == 0 {
		return []Photo{target}, nil
	}

	collection := make([]Photo, 0, 3)
	if len(neighbors) > 0 {
		collection = append(collection, neighbors[0])
	}
	collection = append(collection, target)
	if len(neighbors) > 1 {
		collection = append(collection, neighbors[1])
	}
	return collection, nil
}

// AddPhotosToCollection adds existing photos to a collection and returns how many were newly added.
func (d *DB) AddPhotosToCollection(ctx context.Context, collectionID int64, photoIDs []int64) (int64, error) {
	if len(photoIDs) == 0 {
		return 0, nil
	}
	placeholders, args := idPlaceholders(photoIDs)
	result, err := d.sql.ExecContext(ctx, `INSERT OR IGNORE INTO collection_photos (collection_id, photo_id)
		SELECT ?, id FROM photos WHERE id IN (`+placeholders+`)`, append([]any{collectionID}, args...)...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// AddPhotosToGallery adds photos to a user gallery.
func (d *DB) AddPhotosToGallery(ctx context.Context, galleryID int64, photoIDs []int64) (int64, error) {
	return d.AddPhotosToCollection(ctx, galleryID, photoIDs)
}

// RemovePhotosFromCollection removes photos from a collection.
func (d *DB) RemovePhotosFromCollection(ctx context.Context, collectionID int64, photoIDs []int64) error {
	if len(photoIDs) == 0 {
		return nil
	}
	placeholders, args := idPlaceholders(photoIDs)
	_, err := d.sql.ExecContext(ctx, `DELETE FROM collection_photos WHERE collection_id = ?
		AND photo_id IN (`+placeholders+`)`, append([]any{collectionID}, args...)...)
	return err
}

// RemovePhotosFromGallery removes photos from a user gallery.
func (d *DB) RemovePhotosFromGallery(ctx context.Context, galleryID int64, photoIDs []int64) error {
	return d.RemovePhotosFromCollection(ctx, galleryID, photoIDs)
}

// DeleteCollection deletes a collection and its photo memberships.
func (d *DB) DeleteCollection(ctx context.Context, id int64) error {
	if name, err := d.CollectionName(ctx, id); err == nil && name == SelectedCollectionName {
		return ErrProtectedCollection
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM collection_photos WHERE collection_id = ? AND collection_id <> (SELECT id FROM collections WHERE name = ?)`, id, SelectedCollectionName); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM collections WHERE id = ? AND name <> ?`, id, SelectedCollectionName); err != nil {
		return err
	}
	return tx.Commit()
}

// selectedCollectionID returns the ID of the reserved selected collection.
func (d *DB) selectedCollectionID(ctx context.Context) (int64, error) {
	var id int64
	err := d.sql.QueryRowContext(ctx, `SELECT id FROM collections WHERE name = ?`, SelectedCollectionName).Scan(&id)
	return id, err
}

// SelectedPhotoIDs returns the set of selected photo IDs.
func (d *DB) SelectedPhotoIDs(ctx context.Context) (map[int64]bool, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT cp.photo_id FROM collection_photos cp
		JOIN collections c ON c.id = cp.collection_id WHERE c.name = ?`, SelectedCollectionName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}

// SelectPhotos adds photos to the selected collection.
func (d *DB) SelectPhotos(ctx context.Context, photoIDs []int64) error {
	id, err := d.selectedCollectionID(ctx)
	if err != nil {
		return err
	}
	_, err = d.AddPhotosToCollection(ctx, id, photoIDs)
	return err
}

// UnselectPhotos removes photos from the selected collection.
func (d *DB) UnselectPhotos(ctx context.Context, photoIDs []int64) error {
	id, err := d.selectedCollectionID(ctx)
	if err != nil {
		return err
	}
	return d.RemovePhotosFromCollection(ctx, id, photoIDs)
}

// ClearSelection removes every photo from the selected collection.
func (d *DB) ClearSelection(ctx context.Context) error {
	id, err := d.selectedCollectionID(ctx)
	if err != nil {
		return err
	}
	_, err = d.sql.ExecContext(ctx, `DELETE FROM collection_photos WHERE collection_id = ?`, id)
	return err
}
