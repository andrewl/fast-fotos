A web-based photo viewing and indexing application.

Written in go with minimal dependencies and htmx front-end with postgresql for data storage.

Dockerized

See [Architecture and Roadmap](docs/ARCHITECTURE.md) for the data model, indexing
behavior, UI routes, testing, and planned enhancements. Run `npm install` followed by
`npx playwright install chromium` once to enable the browser test.

## Run

Point `PHOTO_ROOT` at an existing photo library, then start the stack:

```sh
PHOTO_ROOT=/absolute/path/to/photos docker compose up --build
```

Open http://localhost:8080 and choose **Index photos** or **Reindex all**. The source directory is mounted
read-only. Files retain their directory structure; Fast Fotos stores relative paths,
extracted capture time/GPS coordinates, and 400px JPEG thumbnails in PostgreSQL.
Indexing continues in the background if you refresh or navigate away; its progress and a stop control remain
available in the header.

Indexing uses a bounded worker pool. Set `INDEX_WORKER` to control concurrency and
`RESIZE_FILTER` to choose `bilinear` (default), `approx-bilinear`, or `catmull-rom`
thumbnail resizing.

Compose installs the default offline reverse-geocoding provider automatically on
startup when the data is missing:

The container downloads the official GeoNames files (`cities500.zip`,
`admin1CodesASCII.txt`, `admin2Codes.txt`, and `countryInfo.txt`) into a persistent Docker volume,
which Compose mounts automatically. Startup is idempotent; existing files are
retained. The
`GEONAMES_AUTO_INSTALL` setting can be set to `false` to disable downloading. To use
another directory, set the Compose `GEONAMES_DIR` variable to that host directory
(the directory must be writable by the container user).
The files are loaded at startup and
locations are stored with indexed photos. GeoNames data requires attribution under
CC BY 4.0; see `docs/ARCHITECTURE.md`.

Locations normally display as `settlement, region, country`. When the
nearest settlement is more than 5 km away, Fast Fotos displays the smallest available
administrative area and country instead. This avoids assigning a remote rural or island
photo to a distant town.

The standalone `./scripts/install-geonames.sh` command remains available for
installations outside Docker.

## Searching photos

The **Search** view can filter photos by capture date range, detected object label,
camera model, focal-length range, whether the flash fired, settlement, and region.
Results use the same selection controls as the timeline and galleries, so they can
be downloaded or added to a gallery. Existing photos need **Reindex all** after
upgrading to populate the camera, focal-length, and flash fields.
Selections are stored server-side per browser session, identified by an HTTP-only
cookie. They remain available while navigating between views and are shown at
`/selected`; use **Clear selection** to remove them.

Each image has a dedicated `/image/<id>` page. It shows metadata in the left panel
and the full image in the main panel. Image links retain their source collection, so
Escape returns to it and Page Up/Page Down navigate to the previous or next photo.
When an associated raw file is present beside the image, the image page shows a
**Download raw** link.

The Compose image also provisions ONNX Runtime 1.29.0 and the offline object-detection runtime and
YOLOv11 Small model into the persistent `fast-fotos-models` volume. Set
`OBJECT_AUTO_INSTALL=false` to disable this download; `OBJECT_MODEL_DIR` controls
the model directory. During indexing, the detector runs against each generated
thumbnail and stores distinct high-confidence COCO labels such as `Person` and
`Bicycle` with the photo.

After enabling GeoNames for an existing database, run **Reindex all** once so
previously indexed photos are processed and receive their locations.

The timeline groups photos by capture month, the map page shows clustered geotagged
photos on OpenStreetMap, and
custom galleries keep pointers to selected images without moving them from the source
library. Selected images can be downloaded as a ZIP with or without associated raw
files; both options include adjacent `.xmp` and `.json` sidecars.

During indexing, Fast Fotos uses an embedded EXIF JPEG thumbnail when one is present
and valid. Images without an embedded thumbnail continue to use a generated,
400-pixel JPEG thumbnail.

Keep your photos in their existing directory structure.

Scales to millions of photos.

Displays your photos by
- month/year
- location (on a vmap)

View as thumbnails, lists, full screen

Features
- view photo metadata
- select one or more images
- download selection with or without associated raw files, including same-basename sidecar files
