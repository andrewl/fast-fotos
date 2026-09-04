let selectedIDs = new Set();

function selectedPhotoIDs() {
  return [...selectedIDs];
}

function updateSelectionControls() {
  const ids = selectedPhotoIDs();
  const downloadWithRaw = document.querySelector("#download-with-raw");
  const downloadWithoutRaw = document.querySelector("#download-without-raw");
  const addToGallery = document.querySelector("#add-to-gallery-action");
  const clearSelection = document.querySelector("#clear-selection");
  const selectionCount = document.querySelector("#selection-count");
  if (!downloadWithRaw || !downloadWithoutRaw || !addToGallery || !clearSelection) return;
  downloadWithRaw.disabled = ids.length === 0;
  downloadWithoutRaw.disabled = ids.length === 0;
  addToGallery.disabled = ids.length === 0;
  clearSelection.disabled = ids.length === 0;
  selectionCount.hidden = ids.length === 0;
  selectionCount.textContent = ids.length;
  const download = (includeRaw) => {
    const form = document.createElement("form");
    form.method = "post";
    form.action = "/download";
    const input = document.createElement("input");
    input.name = "ids";
    input.value = ids.join(",");
    const rawInput = document.createElement("input");
    rawInput.name = "raw";
    rawInput.value = String(includeRaw);
    form.append(input, rawInput);
    document.body.append(form);
    form.submit();
    form.remove();
  };
  downloadWithRaw.onclick = () => download(true);
  downloadWithoutRaw.onclick = () => download(false);
  addToGallery.onclick = () => {
    const dialog = document.querySelector("#add-to-gallery-dialog");
    dialog.querySelector('input[name="ids"]').value = ids.join(",");
    dialog.querySelector("#add-to-gallery-error").hidden = true;
    dialog.showModal();
  };
  clearSelection.onclick = () => {
    fetch("/clear-selection", {method: "POST"}).then((response) => {
      if (!response.ok) throw new Error("Could not clear selection");
      selectedIDs.clear();
      syncSelectionCheckboxes();
      updateSelectionControls();
      updatePreviewSelection();
    });
  };
}

document.addEventListener("change", (event) => {
  if (!event.target.matches("[data-photo-selection]")) return;
  const id = event.target.value;
  fetch("/selection", {
    method: "POST",
    headers: {"Content-Type": "application/x-www-form-urlencoded"},
    body: new URLSearchParams({id, selected: String(event.target.checked)})
  }).then((response) => {
    if (!response.ok) throw new Error("Could not update selection");
    if (event.target.checked) selectedIDs.add(id);
    else selectedIDs.delete(id);
    updateSelectionControls();
    updatePreviewSelection();
  });
});

function syncSelectionCheckboxes() {
  document.querySelectorAll("[data-photo-selection]").forEach((input) => {
    input.checked = selectedIDs.has(input.value);
  });
}

fetch("/selection")
  .then((response) => {
    if (!response.ok) throw new Error("Could not load selection");
    return response.json();
  })
  .then((ids) => {
    selectedIDs = new Set(ids.map(String));
    syncSelectionCheckboxes();
    updateSelectionControls();
  });

document.addEventListener("click", (event) => {
  const month = event.target.closest(".month");
  if (!month || !month.hasAttribute("hx-get")) return;

  document.querySelectorAll(".month.active").forEach((item) => {
    item.classList.remove("active");
    item.removeAttribute("aria-current");
  });

  document.addEventListener("submit", (event) => {
    if (!event.target.matches("[data-search-form]")) return;
    const query = new URLSearchParams(new FormData(event.target)).toString();
    event.target.action = query ? `/search/${encodeURIComponent(query)}` : "/search";
  });
  month.classList.add("active");
  month.setAttribute("aria-current", "page");
});

document.addEventListener("click", (event) => {
  if (!event.target.matches("[data-close-gallery-dialog]")) return;

  document.querySelector("#add-to-gallery-dialog").close();
});

document.addEventListener("htmx:responseError", (event) => {
  if (!event.detail.elt.matches("#add-to-gallery-form")) return;

  const error = document.querySelector("#add-to-gallery-error");
  error.textContent = event.detail.xhr.responseText || "Could not add photos to the gallery";
  error.hidden = false;
});

document.addEventListener("click", (event) => {
  if (event.target.matches("#photo-viewer-back")) {
    closePhotoViewer();
  }
});

function showPhotoPreview(trigger) {
  showPhotoPreviewData({
    photoId: trigger.dataset.photoId,
    photoPath: trigger.dataset.photoPath,
    photoTakenAt: trigger.dataset.photoTakenAt,
    photoLocation: trigger.dataset.photoLocation
  }, null);
}

function showPhotoPreviewData(data, clusterPhotos) {
  const preview = document.querySelector("#photo-viewer");
  const image = preview.querySelector("img");
  const takenAt = preview.querySelector(".photo-preview-taken-at");
  const location = preview.querySelector(".photo-preview-location");
  preview.dataset.photoId = String(data.photoId);
  preview.dataset.clusterPhotos = clusterPhotos ? JSON.stringify(clusterPhotos) : "";
  takenAt.textContent = data.photoTakenAt;
  location.textContent = data.photoLocation || "";
  location.hidden = !location.textContent;
  image.src = `/photos/${data.photoId}`;
  image.alt = data.photoPath;
  const sourceCheckbox = document.querySelector(`.photo-preview-trigger[data-photo-id="${preview.dataset.photoId}"]`)?.closest(".photo")?.querySelector("input");
  const checkbox = document.querySelector("#photo-viewer-checkbox");
  checkbox.checked = sourceCheckbox?.checked || false;
  updatePreviewSelection();
  preview.hidden = false;
}

function updatePreviewSelection() {
  const preview = document.querySelector("#photo-viewer");
  if (preview.hidden && !preview.dataset.photoId) return;
  const trigger = document.querySelector(`.photo-preview-trigger[data-photo-id="${preview.dataset.photoId}"]`);
  const checkbox = trigger?.closest(".photo")?.querySelector('input[type="checkbox"]');
  const viewerCheckbox = document.querySelector("#photo-viewer-checkbox");
  if (checkbox) checkbox.checked = viewerCheckbox.checked;
}

document.addEventListener("keydown", (event) => {
  if (!document.body.classList.contains("page-image")) return;
  if (event.key === "Escape") {
    event.preventDefault();
    const returnLink = document.querySelector(".image-info > a");
    if (document.referrer.startsWith(window.location.origin) && !new URL(document.referrer).pathname.startsWith("/image/")) {
      window.history.back();
    } else {
      window.location.assign(returnLink.href);
    }
    return;
  }
  if (event.key === " " || event.key === "Spacebar") {
    event.preventDefault();
    const checkbox = document.querySelector("[data-photo-selection]");
    if (checkbox) {
      checkbox.checked = !checkbox.checked;
      checkbox.dispatchEvent(new Event("change", {bubbles: true}));
    }
    return;
  }
  const selector = event.key === "PageUp" || event.key === "ArrowLeft"
    ? "[data-image-previous]"
    : event.key === "PageDown" || event.key === "ArrowRight"
      ? "[data-image-next]"
      : "";
  const link = selector && document.querySelector(selector);
  if (link) {
    event.preventDefault();
    window.location.replace(link.href);
  }
});

document.addEventListener("keydown", (event) => {
  const preview = document.querySelector("#photo-viewer");
  if (preview.hidden) return;
  if (event.key === "Escape") {
    event.preventDefault();
    closePhotoViewer();
    return;
  }

  const clusterPhotos = preview.dataset.clusterPhotos ? JSON.parse(preview.dataset.clusterPhotos) : null;
  const triggers = clusterPhotos || [...document.querySelectorAll(".photo-preview-trigger")].map((trigger) => ({
    photoId: trigger.dataset.photoId,
    photoPath: trigger.dataset.photoPath,
    photoTakenAt: trigger.dataset.photoTakenAt,
    photoLocation: trigger.dataset.photoLocation
  }));
  const currentIndex = triggers.findIndex((photo) => String(photo.photoId) === preview.dataset.photoId);
  if (currentIndex < 0) return;

  if (event.key === "ArrowLeft" || event.key === "PageUp") {
    event.preventDefault();
    showPhotoPreviewData(triggers[Math.max(0, currentIndex - 1)], clusterPhotos);
    history.pushState({photoId: preview.dataset.photoId}, "", `/image/${preview.dataset.photoId}`);
  } else if (event.key === "ArrowRight" || event.key === "PageDown") {
    event.preventDefault();
    showPhotoPreviewData(triggers[Math.min(triggers.length - 1, currentIndex + 1)], clusterPhotos);
    history.pushState({photoId: preview.dataset.photoId}, "", `/image/${preview.dataset.photoId}`);
  } else if (event.key === " ") {
    event.preventDefault();
    const checkbox = document.querySelector("#photo-viewer-checkbox");
    if (!checkbox) return;
    checkbox.checked = !checkbox.checked;
    checkbox.dispatchEvent(new Event("change", { bubbles: true }));
  }
});

window.addEventListener("popstate", () => {
  const viewer = document.querySelector("#photo-viewer");
  if (!viewer) return;
  if (location.pathname.startsWith("/image/")) {
    const id = location.pathname.split("/").pop();
    const trigger = document.querySelector(`.photo-preview-trigger[data-photo-id="${id}"]`);
    if (trigger) showPhotoPreview(trigger);
  } else if (!viewer.hidden) {
    closePhotoViewer();
  }
});

function closePhotoViewer() {
  const viewer = document.querySelector("#photo-viewer");
  viewer.hidden = true;
}

document.addEventListener("change", (event) => {
  if (!event.target.matches("#photo-viewer-checkbox")) return;
  const viewer = document.querySelector("#photo-viewer");
  const checkbox = document.querySelector(`.photo-preview-trigger[data-photo-id="${viewer.dataset.photoId}"]`)?.closest(".photo")?.querySelector("input");
  if (checkbox) checkbox.checked = event.target.checked;
  fetch("/selection", {
    method: "POST",
    headers: {"Content-Type": "application/x-www-form-urlencoded"},
    body: new URLSearchParams({id: viewer.dataset.photoId, selected: String(event.target.checked)})
  }).then((response) => {
    if (!response.ok) throw new Error("Could not update selection");
    if (event.target.checked) selectedIDs.add(viewer.dataset.photoId);
    else selectedIDs.delete(viewer.dataset.photoId);
    updateSelectionControls();
  });
});

let indexProgressTimer;

function setIndexingControls(active) {
  const stop = document.querySelector("[data-stop-indexing]");
  if (!stop) return;
  document.querySelectorAll("[data-indexing] button").forEach((button) => {
    button.disabled = active;
  });
  stop.hidden = !active;
}

function updateIndexProgress() {
  const status = document.querySelector("#index-progress");
  if (!status) return;
  fetch("/index-progress")
    .then((response) => {
      if (!response.ok) throw new Error("Could not load indexing progress");
      return response.json();
    })
    .then((progress) => {
      setIndexingControls(progress.active);
      if (progress.active) {
        status.hidden = false;
        status.textContent = progress.total === 0
          ? progress.message
          : `Indexing: ${progress.processed} / ${progress.total} (${progress.percentage}%)`;
        if (!indexProgressTimer) {
          indexProgressTimer = window.setInterval(updateIndexProgress, 500);
        }
      } else {
        window.clearInterval(indexProgressTimer);
        indexProgressTimer = undefined;
        status.hidden = !progress.message;
        status.textContent = progress.message;
      }
    })
    .catch(() => {
      document.querySelector("#index-progress").textContent = "Could not load indexing progress";
    });
}

document.addEventListener("htmx:beforeRequest", (event) => {
  if (!event.detail.elt.matches("[data-indexing]")) return;

  const status = document.querySelector("#index-progress");
  status.hidden = false;
  status.textContent = "Indexing: preparing files...";
  setIndexingControls(true);
  updateIndexProgress();
  updateSelectionControls();
});

function initialisePhotoMap() {
  const element = document.querySelector("#photo-map");
  if (!element) return;
  if (element.dataset.initialised === "true") return;
  if (typeof maplibregl === "undefined") {
    document.querySelector("#map-status").textContent = "Map library could not be loaded";
    window.setTimeout(initialisePhotoMap, 250);
    return;
  }
  element.dataset.initialised = "true";
  const mapURL = new URL(window.location.href);
  const savedCenter = [Number(mapURL.searchParams.get("mapLng")), Number(mapURL.searchParams.get("mapLat"))];
  const savedZoom = Number(mapURL.searchParams.get("mapZoom"));
  const hasSavedView = savedCenter.every(Number.isFinite) && Number.isFinite(savedZoom);
  const map = new maplibregl.Map({
    container: element,
    center: hasSavedView ? savedCenter : [0, 20],
    zoom: hasSavedView ? savedZoom : 2,
    style: {
      version: 8,
      sources: { osm: { type: "raster", tiles: ["https://tile.openstreetmap.org/{z}/{x}/{y}.png"], tileSize: 256, attribution: "&copy; OpenStreetMap contributors" } },
      layers: [{ id: "osm", type: "raster", source: "osm" }]
    }
  });
  let mapReturnURL = element.dataset.returnUrl || "/locations";
  function saveMapView() {
    const center = map.getCenter();
    const url = new URL(window.location.href);
    url.searchParams.set("mapLat", center.lat.toFixed(6));
    url.searchParams.set("mapLng", center.lng.toFixed(6));
    url.searchParams.set("mapZoom", map.getZoom().toFixed(2));
    window.history.replaceState({}, "", url.pathname + "?" + url.searchParams.toString());
  }
  map.on("moveend", saveMapView);
  map.addControl(new maplibregl.NavigationControl(), "top-right");
  const cache = new Map();
  const clusterPhotoZoom = Number(element.dataset.clusterPhotoZoom || 12);
  const clusterPhotosPanel = document.querySelector("#cluster-photos");
  const clusterPhotosGrid = clusterPhotosPanel?.querySelector(".grid");
  let markers = [];
  const settlement = element.dataset.settlement || "";
  const region = element.dataset.region || "";
  const locationBounds = ["minLat", "minLng", "maxLat", "maxLng"]
    .map((key) => Number(element.dataset[key]))
    .every((value) => Number.isFinite(value));
  if ((settlement || region) && locationBounds) map.fitBounds([[Number(element.dataset.minLng), Number(element.dataset.minLat)], [Number(element.dataset.maxLng), Number(element.dataset.maxLat)]], { padding: 20 });

  function renderPoints(points) {
    markers.forEach((marker) => marker.remove());
    markers = points.map((point) => {
      const markerElement = document.createElement("button");
      markerElement.type = "button";
      markerElement.className = "map-cluster";
      markerElement.textContent = String(point.count);
      markerElement.setAttribute("aria-label", `${point.count} photos`);
      markerElement.addEventListener("click", () => {
        const bounds = [[point.minLongitude, point.minLatitude], [point.maxLongitude, point.maxLatitude]];
        if (map.getZoom() < clusterPhotoZoom) {
          map.fitBounds(bounds, { padding: 40, maxZoom: clusterPhotoZoom });
          return;
        }

        function showClusterPhotos(photos, clusterURL) {
          if (!clusterPhotosPanel || !clusterPhotosGrid) return;
          clusterPhotosGrid.replaceChildren();
          photos.forEach((photo) => {
            const card = document.createElement("div");
            card.className = "photo";
            const checkbox = document.createElement("input");
            checkbox.type = "checkbox";
            checkbox.value = String(photo.photoId);
            checkbox.setAttribute("data-photo-selection", "");
            checkbox.setAttribute("aria-label", `Select ${photo.path}`);
            const link = document.createElement("a");
            link.className = "photo-preview-trigger";
            link.href = `/image/${photo.photoId}?from=${encodeURIComponent(clusterURL)}`;
            link.dataset.photoId = String(photo.photoId);
            link.dataset.photoPath = photo.path;
            link.dataset.photoTakenAt = photo.takenAt;
            link.dataset.photoLocation = photo.location || "";
            link.setAttribute("aria-label", `View ${photo.path} larger`);
            const image = document.createElement("img");
            image.src = `/thumbnails/${photo.photoId}`;
            image.alt = "";
            image.loading = "lazy";
            link.append(image);
            const caption = document.createElement("span");
            caption.textContent = photo.takenAt;
            card.append(checkbox, link, caption);
            clusterPhotosGrid.append(card);
          });
          element.hidden = true;
          clusterPhotosPanel.hidden = false;
          window.history.pushState({clusterURL}, "", clusterURL);
          syncSelectionCheckboxes();
          updateSelectionControls();
        }

        document.querySelector("#cluster-photos-back")?.addEventListener("click", () => {
          clusterPhotosPanel.hidden = true;
          element.hidden = false;
          window.history.pushState({}, "", mapReturnURL);
          map.resize();
          loadPoints();
        });
        const params = new URLSearchParams({
          minLat: point.minLatitude,
          maxLat: point.maxLatitude,
          minLng: point.minLongitude,
          maxLng: point.maxLongitude
        });
        fetch(`/api/map-cluster?${params}`)
          .then((response) => { if (!response.ok) throw new Error("Could not load cluster photos"); return response.json(); })
          .then((photos) => {
            if (!photos.length) return;
            mapReturnURL = window.location.pathname + window.location.search;
            params.set("map", window.location.pathname + window.location.search);
            const clusterURL = `/cluster?${params.toString()}`;
            showClusterPhotos(photos, clusterURL);
          });
      });
      return new maplibregl.Marker({ element: markerElement })
        .setLngLat([point.longitude, point.latitude])
        .addTo(map);
    });
  }

  function loadPoints() {
    const bounds = map.getBounds();
    const params = new URLSearchParams({
      minLat: Math.max(-90, bounds.getSouth()), maxLat: Math.min(90, bounds.getNorth()),
      minLng: Math.max(-180, bounds.getWest()), maxLng: Math.min(180, bounds.getEast()),
      zoom: Math.floor(map.getZoom())
    });
    const key = params.toString();
    const status = document.querySelector("#map-status");
    if (cache.has(key)) { renderPoints(cache.get(key)); return; }
    status.textContent = "Loading locations...";
    fetch(`/api/map-points?${key}`).then((response) => {
      if (!response.ok) throw new Error("Could not load map points");
      return response.json();
    }).then((points) => {
      cache.set(key, points);
      renderPoints(points);
    }).catch(() => { status.textContent = "Could not load map locations"; });
  }

  map.on("load", () => {
    if ((settlement || region) && locationBounds) map.fitBounds([[Number(element.dataset.minLng), Number(element.dataset.minLat)], [Number(element.dataset.maxLng), Number(element.dataset.maxLat)]], { padding: 20 });
    loadPoints();
  });
  map.on("moveend", loadPoints);
  window.setTimeout(() => map.resize(), 0);
}

document.addEventListener("htmx:afterRequest", (event) => {
  if (!event.detail.elt.matches("[data-indexing]")) return;

  updateIndexProgress();
});

document.addEventListener("htmx:beforeRequest", (event) => {
  if (!event.detail.elt.matches("[data-stop-indexing]")) return;

  event.detail.elt.querySelector("button").disabled = true;
});

updateIndexProgress();
if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", initialisePhotoMap);
} else {
  initialisePhotoMap();
}
if (location.pathname.startsWith("/image/")) {
  const trigger = document.querySelector(".photo-preview-trigger");
  if (trigger) showPhotoPreview(trigger);
}
window.addEventListener("load", initialisePhotoMap);
