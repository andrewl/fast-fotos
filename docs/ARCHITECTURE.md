# Architecture and Roadmap

Fast Fotos is a single-process Go web application for browsing a read-only photo
library. SQLite stores the index, thumbnails, and custom-gallery memberships;
the original files remain under `PHOTO_ROOT`.

## Running the application

The `fast-fotos` executable reads:

| Variable | Required | Default | Purpose |
|---|---:|---|---|
| `DATABASE_PATH` | No | `fast-fotos.db` | SQLite database file path |
| `PHOTO_ROOT` | Yes | - | Absolute path to the source photo library |
| `LISTEN_ADDR` | No | `:8090` | HTTP listener address |
| `INDEX_WORKER` | No | Up to 4, limited by `GOMAXPROCS` | Maximum number of images processed concurrently |
| `THUMBNAIL_MAX_DIMENSION` | No | `640` | Maximum width or height of generated photo and video thumbnails |
| `RESIZE_FILTER` | No | `bilinear` | Thumbnail filter: `bilinear`, `approx-bilinear`, or `catmull-rom` |
| `ONNX_INTRA_OP_THREADS` | No | `0` (ONNX Runtime default) | Threads used within a single ONNX graph node |
| `ONNX_INTER_OP_THREADS` | No | `0` (ONNX Runtime default) | Threads used across ONNX graph nodes |
| `GEONAMES_DIR` | No | disabled | Directory containing GeoNames text files or ZIP files for offline reverse geocoding |

`compose.yaml` mounts `PHOTO_ROOT` at `/photos` as read-only and persists PostgreSQL
in the `fast-fotos-data` Docker volume. The container entrypoint runs the idempotent
GeoNames installer when `GEONAMES_AUTO_INSTALL=true` (the Compose default) and the
configured directory is missing provider files. The default host directory is
the persistent `fast-fotos-geonames` Docker volume, mounted read/write at
`/geonames` so the startup installer can populate it. Set
`GEONAMES_AUTO_INSTALL=false` to require a pre-provisioned directory. The
`GEONAMES_DIR` Compose variable can replace the named volume with a host directory;
that directory must be writable by the container user. The standalone
`scripts/install-geonames.sh` command can also populate provider data outside Docker.
Compose also provisions ONNX Runtime 1.29.0 and the YOLOv11 Small object-detection
model into the persistent `fast-fotos-models` volume. Set `OBJECT_AUTO_INSTALL=false`
to disable this download; `OBJECT_MODEL_DIR` controls the model directory. Detection
runs separately from indexing, in the background and only while indexing is idle; starting
indexing interrupts it and it resumes afterwards. It runs against stored thumbnails of
photos with no `objects_detected_at` and stores distinct
high-confidence COCO labels with each photo in `photos.objects`.

The application is organized under `internal/app`:

| Area | Files | Responsibility |
|---|---|---|
| Startup | `cmd/fast-fotos/main.go` | Loads configuration and starts the HTTP server |
| HTTP, views | `internal/app/server.go` | Routes and template rendering |
| Database | `internal/app/db.go` | SQLite schema migration and all SQL queries |
| Indexing | `internal/app/indexer.go` | File discovery, EXIF extraction, thumbnails, and incremental/full indexes |
| UI | `internal/app/templates/`, `internal/app/static/` | Go HTML templates, CSS, and browser behavior |

Templates and static assets are embedded in the executable with `embed.FS`.
Templates are intentionally server-rendered; HTMX is used only for timeline-month
partial replacements and background action requests.

## Data model

| Table | Purpose |
|---|---|
| `photos` | One row per source-relative photo path; stores capture time, coordinates, JPEG thumbnail, and the last successful processing timestamp |
| `index_state` | Single-row checkpoint storing the start time of the most recent completed incremental index |
| `galleries` | Named custom galleries |
| `gallery_photos` | Many-to-many pointers from galleries to `photos` |

`gallery_photos` uses foreign keys with `ON DELETE CASCADE`. Galleries never own,
copy, or move source image files. Removing a stale `photos` row automatically removes
only its membership pointers; it does not remove the gallery itself.

## Indexing lifecycle

The supported photo extensions are AVIF, GIF, HEIC, JPEG/JPG, PNG, TIFF, and WebP.
The supported video extensions are 3GP, AVI, FLV, M4V, MKV, MOV, MP4, MPEG/MPG, MTS,
WebM, and WMV. Video files require `ffmpeg`/`ffprobe` to be available on `PATH`.
For each candidate the indexer:

1. Reads the filesystem modification time as the capture-time fallback.
2. Reads EXIF capture time and GPS coordinates when available (photos), or
   `ffprobe` container/stream tags for creation time, GPS, and duration (videos).
3. Produces a 400px-max JPEG thumbnail when Go can decode the image, or via
   `ffmpeg`'s `thumbnail` filter for videos. Unsupported decoder formats remain
   indexed and retain an existing thumbnail.
4. Upserts the record by source-relative `path`, recording a `media_type` of
   `photo` or `video` and, for videos, its `duration_seconds`.

### Incremental index

**Index photos** scans only files whose modification time is strictly later than
`index_state.last_indexed_at`. The checkpoint is written only after a successful
complete run. Its value is the run start time, avoiding a gap for files modified while
the run is in progress. A failed or stopped run leaves the prior checkpoint intact,
so its work is retried later.

### Full reindex

**Reindex all** clears the incremental checkpoint and scans every supported image.
It upserts existing paths, thereby preserving their `photos.id` values and all custom
gallery memberships. Once that scan succeeds, it deletes only `photos` rows not
updated by the run. Deleted source paths therefore disappear from galleries; remaining
source paths and their custom-gallery pointers survive intact.

### Background execution

Indexing runs in an in-process goroutine with its own `context.Background()` parent,
not the originating HTTP request context. It continues across page refreshes and
navigation. Only one index operation may run at a time. Progress and final status are
kept in memory, exposed through `GET /index-progress`, and polled by the header UI.

Each indexing run uses a bounded worker pool controlled by `INDEX_WORKER`. A worker
handles one file at a time, including metadata extraction, full-resolution decode,
thumbnail resizing, JPEG encoding, and the SQLite upsert. The default is at most
four workers, which improves throughput without creating an unbounded number of
simultaneous decoded images. `RESIZE_FILTER=bilinear` is the balanced default;
`catmull-rom` favors quality at higher CPU cost, while `approx-bilinear` favors speed.

`POST /stop-indexing` cancels the active context. Cancellation is checked between
files and is not considered a successful checkpointing run.

Because job state is in memory, restarting the application stops any active index.
The next **Index photos** run safely resumes from the last completed checkpoint.

## HTTP API

The application serves server-rendered HTML for pages and partial updates, JSON for
map data and indexing progress, and media files directly from the indexed library.
The selection and gallery endpoints are intended for same-origin browser requests.
Selections are stored in the reserved selected gallery (ID `0`) in SQLite and are shared by
clients using the same database. It is omitted from gallery lists and available at
`/selected`; legacy `/galleries/0` and `/gallery/0` links redirect there.

### Pages and media

| Method and path | Inputs | Response |
|---|---|---|
| `GET /` | None | Timeline HTML showing the newest month |
| `GET /maintenance` | None | Indexing controls and progress page |
| `GET /timeline/{YYYY}/{MM}` or `GET /timeline/{YYYY-MM}` | Month in path; optional `/photo/{id}` or `/map` suffix | Timeline HTML, photo detail, or collection map |
| `GET /months/{YYYY-MM}` | Month in path | Timeline HTML partial for HTMX |
| `GET /search[/{criteria}]` | Optional filters `from`, `to`, `label`, `color`, `camera`, `focal`, `flash`, and `location`; criteria may be query parameters or comma-separated path pairs | Search HTML; path filters use the same names |
| `GET /this-week` | Optional `/photo/{id}` or `/map` suffix | This-week gallery HTML, photo detail, or map |
| `GET /locations[/{criteria}]` | Optional `place`, `settlement`, `region`, bounds (`minLat`, `minLng`, `maxLat`, `maxLng`), map center/zoom, or photo ID filters | Interactive map page; invalid bounds return `400` |
| `GET /cluster` | Bounds `minLat`, `maxLat`, `minLng`, `maxLng`; optional `map` return URL or `photoId` | HTML photo grid for the cluster |
| `GET /galleries` | None | Gallery list HTML |
| `GET /selected` | Optional `/photo/{id}` or `/map` suffix | Selected photo grid, photo detail, or map |
| `GET /galleries/{id}` or `/gallery/{id}` | Gallery ID; optional `/photo/{id}` or `/map` suffix | Gallery HTML, photo detail, or map |
| `GET /similar/{photoId}`, `/near-to/{photoId}`, `/random` | Target photo ID where applicable; optional photo/map suffix | A generated photo collection as HTML |
| `GET /photos/{id}` | Indexed photo ID | Original image or video |
| `GET /thumbnails/{id}` | Indexed photo ID | Stored JPEG thumbnail, or original file if no thumbnail exists |
| `GET /raw/{id}` | Indexed photo ID | Associated raw file as an attachment |
| `GET /static/{path}` | Embedded asset path | Static application asset |

### Mutations and downloads

| Method and path | Inputs | Response |
|---|---|---|
| `POST /gallery` | Form fields `ids` (comma-separated photo IDs) and either `gallery_id` or `collection_id`, or `name` to create a gallery | `204` with `HX-Redirect` to the gallery; invalid or empty selections return `400` |
| `DELETE /gallery/` | Form/query fields `ids` and `gallery_id` (or `collection_id`) | Removes the IDs from that gallery; `204` with `HX-Redirect` |
| `DELETE /galleries/{id}` | Gallery ID in path | Deletes the gallery; `204` with `HX-Redirect`; the protected selected gallery returns `403` |
| `POST /selection` or `DELETE /selection` | Form field `ids`, a comma-separated list of positive photo IDs | Adds or removes IDs from the reserved selected gallery; `204` |
| `POST /selection/clear` | None | Clears the reserved selected gallery; `204` |
| `POST /galleries/0/remove` | Form field `ids` | Removes those IDs from the reserved selected gallery; `204` |
| `POST /saved-searches` | Form field `url` containing a local `/search` URL | Saves the search; `204` with `HX-Redirect` |
| `DELETE /saved-searches/{id}` | Saved-search ID in path | Deletes the saved search; `204` with `HX-Redirect` |
| `POST /download/{context}` | Query field `ids`; optional `raw=true` | ZIP containing selected originals and adjacent `.xmp`/`.json` sidecars; raw files are included when requested |
| `POST /index` | None | Starts incremental indexing; `204`, or `409` if an index is already running |
| `POST /reindex` | None | Starts a full reindex; `204`, or `409` if an index is already running |
| `POST /stop-indexing` | None | Cancels active indexing; `204`, or `409` if no index is active |

### JSON endpoints

`GET /api/map-points` returns a JSON array of map points. Normal viewport requests
require `minLat`, `maxLat`, `minLng`, `maxLng`, and `zoom` (0–22); `settlement` and
`region` optionally filter the result. `photoId` or comma-separated `photoIds` selects
specific photos instead of using viewport bounds. Each point includes `latitude`,
`longitude`, `count`, `photoId`, and its `minLatitude`, `minLongitude`, `maxLatitude`,
and `maxLongitude`; path, capture time, and location may also be present. Invalid
coordinates or zoom values return `400`.

`GET /api/map-cluster` accepts the same coordinate bounds or the `photoId`/
`photoIds` selector and returns the geotagged photo records within that selection.
The records include coordinates, ID, path, capture time, and location. Both map
endpoints set `Cache-Control: private, max-age=30` for normal bounds queries.

`GET /index-progress` returns JSON with `active`, `processed`, `total`, `percentage`,
and `message` fields for indexing, plus a `detection` object with the corresponding
background object-detection status fields.

### DLNA endpoints

The server advertises itself on the local network using SSDP. DLNA clients use
`GET /dlna/description.xml` and `GET /dlna/contentdirectory.xml` for device/service
metadata, `POST /dlna/control` for SOAP Browse requests, and
`GET /dlna/resource/{id}` or `GET /dlna/thumbnail/{id}` for media resources.

Photo preview supports left/right and Page Up/Page Down navigation. In selectable
photo grids, it displays whether the current photo is selected and lets the user
synchronize that selection with the grid.

Selections are persisted in SQLite as memberships in the reserved selected gallery.
The `/selection` endpoints synchronize visible checkboxes, and the selected gallery
renders the complete persisted selection across views.

Photo rows also store `raw_path` when a recognised raw-camera file shares the
image basename in the same directory. Raw files are not indexed as photos; they
are associated with their JPEG or other supported image and can be downloaded
from the image detail page.

Location data is marked as `extracted` when it comes from the image's own EXIF
coordinates and reverse geocoder. After indexing, photos without valid location
data (including coordinates at `0,0`) may inherit the nearest extracted location
from another photo taken within one hour; those rows are marked `derived`.

## Testing

Run the existing automated checks with:

```sh
go test ./...
node --check internal/app/static/app.js
npm run test:e2e
```

Current tests cover template parsing, date formatting, indexing progress state,
thumbnail creation, supported-path filtering, and ZIP downloads. The Playwright
end-to-end test uses the four images in `test-images/`, runs a full re-index through
the maintenance page, and verifies that the home page renders four thumbnails.

## Planned work

The session todo list records these items. This document captures their intended
scope and the decisions that should be made before implementation.

| Item | Intended direction | Key decisions |
|---|---|---|
| Unify timeline and custom-gallery views | Reuse a common page/layout template; timeline uses a month sidebar and custom galleries use the gallery name | Whether the gallery page should also expose a gallery sidebar |
| Preview selection control | Replace the current selection action control with a native checkbox | Checkbox label and placement while retaining keyboard Space behavior |
| Additional EXIF metadata | Store fields such as camera make/model and focal length | Schema columns versus JSON metadata; which values to display and index |
| Command-line indexing | Reuse `indexPhotos` and `reindexPhotos` without starting the web server | CLI flags/subcommands, progress output, and mutual exclusion with a running server |
| Interactive map | Replace the geotagged-photo list with a map and photo markers | Offline versus hosted tiles, JavaScript map library, attribution, clustering, and map-data persistence |
| Offline reverse geocoding (stretch) | Resolve coordinates to nearby town and administrative area | Dataset license/size, bundling versus mounted data, update process, lookup index, and accuracy expectations |
| Offline object identification (stretch) | Classify likely image subjects locally | Model license/size, CPU and memory budget, inference runtime, label storage, and whether results are user-visible, searchable, or both |

When extending the schema, keep migrations additive in `Server.migrate`. When adding
new photo-derived data, ensure both incremental indexing and full reindexing update it
and decide whether a reindex must refresh existing records.

## Reverse-geocoding provider boundary

Reverse geocoding is isolated in `internal/geocode`. Callers depend on the
`geocode.Provider` interface:

```go
type Provider interface {
    ReverseGeocode(context.Context, float64, float64) (Location, error)
}
```

`Location` contains settlement, region, and country text and provides a
consistent comma-separated display form. `ProviderFunc` makes small providers easy to
adapt, while `CachedProvider` adds concurrency-safe memoization without coupling callers
to a particular data source.

`PlaceIndex` is a provider-neutral nearest-place implementation. A GeoNames adapter
can load `cities500` and its admin1/admin2 administrative and country lookup files into `[]Place`; a
different provider can implement `Provider` directly or feed the same place index.
The application should depend on the interface rather than on GeoNames types, file
formats, or lookup details.

The current application wiring uses `GEONAMES_DIR` to construct a
`GeoNamesProvider` at startup. Compose sets this to `/geonames`, matching its
dataset mount. When enabled, indexed photos persist the formatted location in
`photos.location` plus the component values in `photos.settlement`, `photos.region`,
and `photos.country`; photos without GPS coordinates or without a configured provider
retain empty values. Locations use settlement, region, and country when the nearest
settlement is within 5 km; otherwise the settlement is replaced by the smallest
available region. The map menu uses the separate settlement and region values as
filters. The provider currently performs a linear
nearest-place scan over the loaded `cities500` records, which is appropriate for the
small bundled dataset and can later be replaced internally with a spatial index
without changing the `Provider` interface.

## Map

The `/locations` page uses Leaflet with OpenStreetMap tiles. It requests only the
currently visible map bounds through `/api/map-points`; the endpoint groups photos
into zoom-dependent latitude/longitude cells and returns one marker per cell with
its count. A single-photo marker opens the source image, while clusters remain
aggregated until the user zooms in.

This avoids sending the full geotagged library to the browser and scales better than
client-side clustering. The existing `(latitude, longitude)` partial index supports
the bounding-box filter. A future PostGIS migration could replace the grid query
with `ST_ClusterDBSCAN` or tile-oriented materialized aggregates if query volume or
dataset size requires it. The browser keeps a short-lived per-viewport cache and the
endpoint sends a private 30-second cache hint.
