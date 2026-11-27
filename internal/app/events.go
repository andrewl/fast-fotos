package app

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	eventMaxGap      = 48 * time.Hour
	eventMaxDuration = 14 * 24 * time.Hour
	eventMaxDistance = 50.0 // kilometres from the first reliable location
	eventMinPhotos   = 3
)

type Event struct {
	ID      int64
	Name    string
	Start   time.Time
	End     time.Time
	Count   int
	CoverID int64
	Hidden  bool
}

func (e Event) DateRange() string {
	if e.Start.Format("2006-01-02") == e.End.Format("2006-01-02") {
		return e.Start.Format("2 Jan 2006")
	}
	return e.Start.Format("2 Jan 2006") + " – " + e.End.Format("2 Jan 2006")
}

type eventPhoto struct {
	id                          int64
	taken                       time.Time
	latitude, longitude         sql.NullFloat64
	source                      string
	settlement, region, country string
}

func (p eventPhoto) reliableLocation() bool {
	return p.source != "derived" && p.latitude.Valid && p.longitude.Valid &&
		!(p.latitude.Float64 == 0 && p.longitude.Float64 == 0) &&
		math.Abs(p.latitude.Float64) <= 90 && math.Abs(p.longitude.Float64) <= 180
}

func eventDistance(a, b eventPhoto) float64 {
	const radius = 6371.0
	lat1, lat2 := a.latitude.Float64*math.Pi/180, b.latitude.Float64*math.Pi/180
	dlat := lat2 - lat1
	dlon := (b.longitude.Float64 - a.longitude.Float64) * math.Pi / 180
	h := math.Pow(math.Sin(dlat/2), 2) + math.Cos(lat1)*math.Cos(lat2)*math.Pow(math.Sin(dlon/2), 2)
	return 2 * radius * math.Asin(math.Sqrt(min(1, max(0, h))))
}

type eventGroup struct {
	photos []eventPhoto
	start  time.Time
	end    time.Time
	name   string
}

// groupEvents uses only measured GPS coordinates as anchors, avoiding inferred locations.
func groupEvents(photos []eventPhoto) []eventGroup {
	var groups []eventGroup
	var last eventPhoto
	for _, p := range photos {
		if !p.reliableLocation() {
			continue
		}
		if len(groups) == 0 || p.taken.Sub(last.taken) > eventMaxGap ||
			p.taken.Sub(groups[len(groups)-1].start) > eventMaxDuration ||
			eventDistance(groups[len(groups)-1].photos[0], p) > eventMaxDistance {
			groups = append(groups, eventGroup{start: p.taken})
		}
		g := &groups[len(groups)-1]
		g.photos = append(g.photos, p)
		g.end = p.taken
		last = p
	}
	for _, p := range photos {
		if p.reliableLocation() {
			continue
		}
		i := sort.Search(len(groups), func(i int) bool { return !groups[i].end.Before(p.taken) })
		if i == len(groups) || p.taken.Before(groups[i].start) ||
			(i+1 < len(groups) && !groups[i+1].start.After(p.taken)) {
			continue
		}
		groups[i].photos = append(groups[i].photos, p)
	}
	result := make([]eventGroup, 0, len(groups))
	for _, g := range groups {
		if len(g.photos) < eventMinPhotos {
			continue
		}
		sort.Slice(g.photos, func(i, j int) bool {
			if g.photos[i].taken.Equal(g.photos[j].taken) {
				return g.photos[i].id < g.photos[j].id
			}
			return g.photos[i].taken.Before(g.photos[j].taken)
		})
		g.name = eventLocationName(g.photos)
		result = append(result, g)
	}
	return result
}

// Use a place only when it describes a majority of the reliable GPS photos.
func eventLocationName(photos []eventPhoto) string {
	for _, field := range []func(eventPhoto) string{
		func(p eventPhoto) string { return p.settlement },
		func(p eventPhoto) string { return p.region },
		func(p eventPhoto) string { return p.country },
	} {
		counts := make(map[string]int)
		total := 0
		for _, p := range photos {
			if !p.reliableLocation() {
				continue
			}
			total++
			if name := strings.TrimSpace(field(p)); name != "" {
				counts[name]++
			}
		}
		for name, count := range counts {
			if count*2 > total {
				return name
			}
		}
	}
	return "Unknown location"
}

// RebuildEvents replaces memberships atomically, retaining IDs and edits by maximal photo overlap.
func (d *DB) RebuildEvents(ctx context.Context) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id, taken_at, latitude, longitude, COALESCE(location_source, ''),
		COALESCE(settlement, ''), COALESCE(region, ''), COALESCE(country, '')
		FROM photos ORDER BY taken_at, id`)
	if err != nil {
		return err
	}
	var photos []eventPhoto
	for rows.Next() {
		var p eventPhoto
		var taken string
		if err := rows.Scan(&p.id, &taken, &p.latitude, &p.longitude, &p.source, &p.settlement, &p.region, &p.country); err != nil {
			rows.Close()
			return err
		}
		p.taken, err = parseTime(taken)
		if err == nil && !p.taken.IsZero() && p.taken.Year() > 1 {
			photos = append(photos, p)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	groups := groupEvents(photos)
	rows, err = tx.QueryContext(ctx, `SELECT event_id, photo_id FROM event_photos`)
	if err != nil {
		return err
	}
	old := make(map[int64]int64)
	for rows.Next() {
		var eventID, photoID int64
		if err := rows.Scan(&eventID, &photoID); err != nil {
			rows.Close()
			return err
		}
		old[photoID] = eventID
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	type match struct {
		group int
		id    int64
		count int
	}
	var matches []match
	for i, g := range groups {
		counts := make(map[int64]int)
		for _, p := range g.photos {
			if id := old[p.id]; id != 0 {
				counts[id]++
			}
		}
		for id, count := range counts {
			matches = append(matches, match{i, id, count})
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].count != matches[j].count {
			return matches[i].count > matches[j].count
		}
		if matches[i].id != matches[j].id {
			return matches[i].id < matches[j].id
		}
		return matches[i].group < matches[j].group
	})
	ids := make([]int64, len(groups))
	used := make(map[int64]bool)
	for _, m := range matches {
		if ids[m.group] == 0 && !used[m.id] {
			ids[m.group], used[m.id] = m.id, true
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM event_photos`); err != nil {
		return err
	}
	insertPhoto, err := tx.PrepareContext(ctx, `INSERT INTO event_photos (event_id, photo_id) VALUES (?, ?)`)
	if err != nil {
		return err
	}
	defer insertPhoto.Close()
	for i, g := range groups {
		if ids[i] == 0 {
			if err := tx.QueryRowContext(ctx, `INSERT INTO events (auto_name, start_at, end_at) VALUES (?, ?, ?) RETURNING id`,
				g.name, formatTime(g.start), formatTime(g.end)).Scan(&ids[i]); err != nil {
				return err
			}
		} else if _, err := tx.ExecContext(ctx, `UPDATE events SET auto_name = ?, start_at = ?, end_at = ? WHERE id = ?`,
			g.name, formatTime(g.start), formatTime(g.end), ids[i]); err != nil {
			return err
		}
		for _, p := range g.photos {
			if _, err := insertPhoto.ExecContext(ctx, ids[i], p.id); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE NOT EXISTS (SELECT 1 FROM event_photos WHERE event_id = events.id)`); err != nil {
		return err
	}
	return tx.Commit()
}

func scanEvent(row rowScanner) (Event, error) {
	var e Event
	var start, end string
	if err := row.Scan(&e.ID, &e.Name, &start, &end, &e.Count, &e.CoverID, &e.Hidden); err != nil {
		return Event{}, err
	}
	var err error
	if e.Start, err = parseTime(start); err != nil {
		return Event{}, err
	}
	e.End, err = parseTime(end)
	return e, err
}

const eventColumns = `e.id, CASE WHEN e.custom_name <> '' THEN e.custom_name ELSE e.auto_name END,
	e.start_at, e.end_at, COUNT(ep.photo_id),
	(SELECT ep2.photo_id FROM event_photos ep2 JOIN photos p ON p.id = ep2.photo_id
	 WHERE ep2.event_id = e.id ORDER BY p.taken_at, p.id LIMIT 1), e.hidden`

func (d *DB) Events(ctx context.Context, hidden bool) ([]Event, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT `+eventColumns+` FROM events e
		JOIN event_photos ep ON ep.event_id = e.id WHERE e.hidden = ? GROUP BY e.id ORDER BY e.start_at DESC, e.id DESC`, hidden)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (d *DB) EventByID(ctx context.Context, id int64) (Event, error) {
	return scanEvent(d.sql.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM events e
		JOIN event_photos ep ON ep.event_id = e.id WHERE e.id = ? GROUP BY e.id`, id))
}

func (d *DB) PhotosForEvent(ctx context.Context, id int64) ([]Photo, error) {
	return d.queryPhotos(ctx, `SELECT `+photoColumns+` FROM photos
		JOIN event_photos ep ON ep.photo_id = photos.id WHERE ep.event_id = ? ORDER BY taken_at, id`, id)
}

func (s *Server) eventsPage(w http.ResponseWriter, r *http.Request) {
	path, photoID, hasPhoto, mapView, ok := splitCollectionPhotoMapPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if path == "/events" {
		if hasPhoto || mapView {
			http.NotFound(w, r)
			return
		}
		hidden := r.URL.Query().Get("hidden") == "1"
		events, err := s.db.Events(r.Context(), hidden)
		if err != nil {
			http.Error(w, "Could not load events", http.StatusInternalServerError)
			return
		}
		s.render(w, "events", PageData{Page: "events", Events: events, ShowHiddenEvents: hidden})
		return
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(path, "/events/"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	event, err := s.db.EventByID(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	photos, err := s.db.PhotosForEvent(r.Context(), id)
	if err != nil {
		http.Error(w, "Could not load event photos", http.StatusInternalServerError)
		return
	}
	returnURL := fmt.Sprintf("/events/%d", id)
	if hasPhoto && !photoInCollection(photos, photoID) {
		http.NotFound(w, r)
		return
	}
	if mapView {
		if hasPhoto {
			for _, p := range photos {
				if p.ID == photoID {
					photos = []Photo{p}
					break
				}
			}
		}
		s.renderCollectionMap(w, r, returnURL, photos)
		return
	}
	if hasPhoto {
		s.renderCollectionPhoto(w, r, photoID, returnURL, event.Name, photos)
		return
	}
	s.render(w, "events", PageData{Page: "events", Event: &event, Photos: photos, ReturnURL: returnURL, CollectionName: event.Name})
}

func (s *Server) updateEventName(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid event name", http.StatusBadRequest)
		return
	}
	names, ok := r.PostForm["name"]
	if !ok || len(names) != 1 || !utf8.ValidString(names[0]) || utf8.RuneCountInString(names[0]) > 200 {
		http.Error(w, "Invalid event name", http.StatusBadRequest)
		return
	}
	s.updateEvent(w, r, id, `UPDATE events SET custom_name = ? WHERE id = ?`, strings.TrimSpace(names[0]), fmt.Sprintf("/events/%d", id))
}

func (s *Server) setEventHidden(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	hidden := strings.HasSuffix(r.URL.Path, "/hide")
	s.updateEvent(w, r, id, `UPDATE events SET hidden = ? WHERE id = ?`, hidden, "/events")
}

func (s *Server) updateEvent(w http.ResponseWriter, r *http.Request, id int64, query string, value any, redirect string) {
	result, err := s.db.sql.ExecContext(r.Context(), query, value, id)
	if err != nil {
		slog.Error("update event", "id", id, "error", err)
		http.Error(w, "Could not update event", http.StatusInternalServerError)
		return
	}
	affected, err := result.RowsAffected()
	if err != nil {
		http.Error(w, "Could not update event", http.StatusInternalServerError)
		return
	}
	if affected == 0 {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}
