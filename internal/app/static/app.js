document.body.addEventListener('htmx:configRequest', function(evt) {
    //see https://htmx.org/events/#htmx:configRequest
    console.log(evt);
});

//this function updates the action-menu-panel - if there are no photos selected then
//disable all of the menu buttons except for that with the id select-all, otherwise enable them all
function updateActionMenu() {
  const selectedCount = document.querySelectorAll("[data-photo-selection]:checked").length;
  const actionMenu = document.querySelector(".action-menu-panel");
  if (!actionMenu) return;
  if (selectedCount === 0) {
    actionMenu.querySelectorAll("button:not(#select-all):not(#unselect-all):not(#clear-selection-all)").forEach((button) => {
      button.disabled = true;
    });
  } else {
    actionMenu.querySelectorAll("button").forEach((button) => {
      button.disabled = false;
    });
  }
}


function toggleOne(id) {
  const input = document.querySelector(`[data-photo-selection][value="${id}"]`);
  if (input) {
    input.checked = !input.checked;
  }
  updateActionMenu();
}

//persists selection changes to the reserved selected gallery
function persistSelection(ids, selected) {
  if (ids.length === 0) return;
  const body = new URLSearchParams({ ids: ids.join(",") });
  fetch("/selection", { method: selected ? "POST" : "DELETE", body }).catch((error) => {
    console.error("Could not save selection", error);
  });
}

function setPageSelection(selected) {
  const ids = [];
  document.querySelectorAll("[data-photo-selection]").forEach((input) => {
    if (input.checked !== selected) ids.push(input.value);
    input.checked = selected;
  });
  persistSelection(ids, selected);
  updateActionMenu();
}

function selectAll() {
  setPageSelection(true);
}

function selectNone() {
  setPageSelection(false);
}

function clearSelection() {
  document.querySelectorAll("[data-photo-selection]").forEach((input) => {
    input.checked = false;
  });
  fetch("/selection/clear", { method: "POST" }).catch((error) => {
    console.error("Could not clear selection", error);
  });
  updateActionMenu();
}

//when the document has loaded then update the action menu
document.addEventListener("DOMContentLoaded", () => {
  //adds an eventlistener to each checkbox with the data-photo-selection attribute to update the action menu when the checkbox is changed
  document.addEventListener("change", (event) => {
    if (!event.target.matches("[data-photo-selection]")) return;
    persistSelection([event.target.value], event.target.checked);
    updateActionMenu();
  });

 updateActionMenu();

});

document.addEventListener("click", (event) => {
  const month = event.target.closest(".month");
  if (!month || !month.hasAttribute("hx-get")) return;

  document.querySelectorAll(".month.active").forEach((item) => {
    item.classList.remove("active");
    item.removeAttribute("aria-current");
  });
  month.classList.add("active");
  month.setAttribute("aria-current", "page");
});

document.addEventListener("submit", (event) => {
  if (!event.target.matches("[data-search-form]")) return;
  event.preventDefault();
  const entries = [...new FormData(event.target).entries()]
    .filter(([, value]) => value !== "")
    .map(([key, value]) => `${encodeURIComponent(key)}=${encodeURIComponent(value)}`);
  const query = entries.join(",");
  event.target.action = query ? `/search/${query}` : "/search";
  window.location.assign(event.target.action);
});

document.addEventListener("click", (event) => {
  target = event.target;
  console.log(target);
  if (target.matches("#photo-viewer-back")) {
    closePhotoViewer();
  }
  else if (target.matches("[data-close-gallery-dialog]")) {
    document.querySelector("#add-to-gallery-dialog").close();
  }
});


document.addEventListener("htmx:responseError", (event) => {
  if (!event.detail.elt.matches("#add-to-gallery-form")) return;

  const error = document.querySelector("#add-to-gallery-error");
  error.textContent = event.detail.xhr.responseText || "Could not add photos to the gallery";
  error.hidden = false;
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
  if (checkbox) {
    checkbox.checked = viewerCheckbox.checked;
    persistSelection([checkbox.value], checkbox.checked);
  }
}

document.addEventListener("change", (event) => {
  if (event.target.matches("#photo-viewer-checkbox")) updatePreviewSelection();
});

// Adds or removes the current photo from a gallery without leaving the page.
async function toggleCurrentPhotoInGallery(item) {
  const photoId = document.querySelector("#image-gallery-dialog").dataset.photoId;
  const status = document.querySelector("#gallery-status");
  const remove = item.classList.contains("in-gallery");
  const params = new URLSearchParams({ ids: photoId, gallery_id: item.dataset.galleryShortcut });
  const response = remove
    ? await fetch(`/gallery/0?${params}`, { method: "DELETE" })
    : await fetch("/gallery", { method: "POST", body: params });
  if (!response.ok) {
    status.textContent = (await response.text()).trim();
    return;
  }
  item.classList.toggle("in-gallery", !remove);
  status.textContent = `${remove ? "Removed from" : "Added to"} ${item.textContent}`;
}

//zoom and pan the displayed image; zoom level 0 is 1:1, each level adds 50%
const MAX_ZOOM_LEVEL = 8;
const zoomState = { level: 0, x: 0, y: 0 };

function applyZoom() {
  const image = document.querySelector(".image-panel img");
  if (!image) return;
  const scale = 1 + zoomState.level * 0.5;
  const maxX = (scale - 1) * image.offsetWidth / 2;
  const maxY = (scale - 1) * image.offsetHeight / 2;
  zoomState.x = Math.max(-maxX, Math.min(maxX, zoomState.x));
  zoomState.y = Math.max(-maxY, Math.min(maxY, zoomState.y));
  image.style.transform = zoomState.level === 0 ? "" : `translate(${zoomState.x}px, ${zoomState.y}px) scale(${scale})`;
  image.closest(".image-panel").classList.toggle("zoomed", zoomState.level > 0);
}

function zoomBy(delta) {
  zoomState.level = Math.max(0, Math.min(MAX_ZOOM_LEVEL, zoomState.level + delta));
  applyZoom();
}

function panBy(dx, dy) {
  const image = document.querySelector(".image-panel img");
  if (!image || zoomState.level === 0) return;
  zoomState.x += dx * image.offsetWidth * 0.1;
  zoomState.y += dy * image.offsetHeight * 0.1;
  applyZoom();
}

function toggleSidebar() {
  document.body.classList.toggle("sidebar-hidden");
}

async function addCurrentPhotoToGallery(fields) {
  const photoId = document.querySelector("#image-gallery-dialog").dataset.photoId;
  const body = new URLSearchParams({ ids: photoId, ...fields });
  const status = document.querySelector("#gallery-status");
  const response = await fetch("/gallery", { method: "POST", body });
  const text = (await response.text()).trim();
  if (!response.ok) {
    status.textContent = text.startsWith("No selected photos") ? "Already in that gallery" : text;
    return false;
  }
  status.textContent = "Added to gallery";
  return true;
}

function setupImageGalleryDialog() {
  const dialog = document.querySelector("#image-gallery-dialog");
  if (!dialog) return;
  const select = dialog.querySelector("#image-gallery-select");
  const nameLabel = dialog.querySelector("#image-gallery-name-label");
  const nameInput = nameLabel.querySelector("input");
  const error = dialog.querySelector("#image-gallery-error");
  const syncName = () => {
    nameLabel.hidden = select.value !== "";
  };
  select.addEventListener("change", syncName);
  dialog.querySelector("#image-gallery-cancel").addEventListener("click", () => dialog.close());
  dialog.querySelector("form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const created = select.value === "";
    const ok = await addCurrentPhotoToGallery({ gallery_id: select.value, name: created ? nameInput.value : "" });
    if (!ok) {
      error.textContent = document.querySelector("#gallery-status").textContent;
      error.hidden = false;
      return;
    }
    dialog.close();
    if (created) window.location.reload();
  });
  dialog.addEventListener("close", () => {
    error.hidden = true;
    nameInput.value = "";
  });
  dialog.syncName = syncName;
}

document.addEventListener("DOMContentLoaded", setupImageGalleryDialog);

document.addEventListener("keydown", (event) => {
  if (!document.body.classList.contains("page-image")) return;
  if (event.ctrlKey || event.metaKey || event.altKey) return;
  if (event.target.closest("dialog[open], textarea, select, input:not([type=checkbox])")) return;
  if (event.key === "c" || event.key === "C") {
    const dialog = document.querySelector("#image-gallery-dialog");
    event.preventDefault();
    dialog.syncName();
    dialog.showModal();
    return;
  }
  if (/^[1-9]$/.test(event.key)) {
    const item = document.querySelectorAll("[data-gallery-shortcut]")[Number(event.key) - 1];
    if (item) {
      event.preventDefault();
      toggleCurrentPhotoInGallery(item);
    }
    return;
  }
  const keyActions = {
    s: toggleSidebar, "+": () => zoomBy(1), "=": () => zoomBy(1), "-": () => zoomBy(-1), "_": () => zoomBy(-1),
    q: () => panBy(1, 0), w: () => panBy(-1, 0), p: () => panBy(0, 1), l: () => panBy(0, -1)
  };
  const action = keyActions[event.key.toLowerCase()];
  if (action) {
    event.preventDefault();
    action();
    return;
  }
  if (event.key === "Escape") {
    event.preventDefault();
    const returnLink = document.querySelector(".image-info > a");
    if (document.referrer.startsWith(window.location.origin) && !new URL(document.referrer).pathname.includes("/photo/")) {
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
  if (event.key === "a" || event.key === "A") {
    selectAll();
  }
  if (event.key === "d" || event.key === "D") {
    selectNone();
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
    history.pushState({photoId: preview.dataset.photoId}, "", locationPhotoURL(preview.dataset.photoId));
  } else if (event.key === "ArrowRight" || event.key === "PageDown") {
    event.preventDefault();
    showPhotoPreviewData(triggers[Math.min(triggers.length - 1, currentIndex + 1)], clusterPhotos);
    history.pushState({photoId: preview.dataset.photoId}, "", locationPhotoURL(preview.dataset.photoId));
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
  if (!viewer.hidden) {
    closePhotoViewer();
  }
});

function closePhotoViewer() {
  const viewer = document.querySelector("#photo-viewer");
  viewer.hidden = true;
}

let indexProgressTimer;

//update the indexing progress every 500ms if indexing is active
function setIndexingControls(active) {
  console.log("setIndexingControls", active);
  const stop = document.querySelector("[data-stop-indexing]");
  console.log("stop", stop);
  if (!stop) return;
  document.querySelectorAll("[data-indexing] button").forEach((button) => {
    button.disabled = active;
  });
  stop.hidden = !active;
}

function updateIndexProgress() {
  const status = document.querySelector("#index-progress");
  const detectionStatus = document.querySelector("#detection-progress");
  if (!status) {
    console.warn("No index progress element found");
    return;
  }
  fetch("/index-progress")
    .then((response) => {
      if (!response.ok) throw new Error("Could not load indexing progress");
      return response.json();
    })
    .then((progress) => {
      console.log("Indexing progress:", progress);
      setIndexingControls(progress.active);
      if (progress.active) {
        status.hidden = false;
        status.textContent = progress.total === 0
          ? progress.message
          : `Indexing: ${progress.processed} / ${progress.total} (${progress.percentage}%)`;
      } else {
        status.hidden = !progress.message;
        status.textContent = progress.message;
      }
      const detection = progress.detection;
      if (detectionStatus && detection && detection.active) {
        detectionStatus.hidden = false;
        detectionStatus.textContent = detection.total === 0
          ? detection.message
          : `Object detection: ${detection.processed} / ${detection.total} (${detection.percentage}%)`;
      } else if (detectionStatus) {
        detectionStatus.hidden = true;
      }
      if (progress.active || (detection && detection.active)) {
        if (!indexProgressTimer) {
          indexProgressTimer = window.setInterval(updateIndexProgress, 500);
        }
      } else {
        window.clearInterval(indexProgressTimer);
        indexProgressTimer = undefined;
      }
    })
    .catch(() => {
      document.querySelector("#index-progress").textContent = "Could not load indexing progress";
    });
}

htmx.on('htmx:before:request', function (evt) {
  console.log(evt);
    let ctx = evt.detail.ctx;
   let sourceElementId = ctx.sourceElement.id;

  //if the source has the data-append-photo-ids attribute, we need to add the selected photo ids to the querystring
  //or body of the request depending on the method
  if (ctx.sourceElement.hasAttribute('data-append-photo-ids')) {
    console.log('Adding selected photo ids');
    const selectedPhotoIds = Array.from(document.querySelectorAll('.photo input[type="checkbox"]:checked')).map(input => input.value);

    if (ctx.request.method != 'GET') {
      ctx.request.body.set('ids', selectedPhotoIds.join(','));
    }
    else {
      //if the querystring already has an '?' we need to append with a '&', otherwise we append with a '?'
      if (ctx.request.action.includes('?')) {
        ctx.request.action = ctx.request.action + '&ids=' + selectedPhotoIds.join(',');
      }
      else {
        ctx.request.action = ctx.request.action + '?ids=' + selectedPhotoIds.join(',');
      }
    }
  }
  else if (ctx.sourceElement.hasAttribute("data-indexing")) {
    console.log('Indexing request detected');
    const status = document.querySelector("#index-progress");
    console.log('status', status);  
    status.hidden = false;
    status.textContent = "Indexing: preparing files...";
    setIndexingControls(true);
  }
  else if (ctx.sourceElement.hasAttribute("data-stop-indexing")) {
    setIndexingControls(false);
    ctx.sourceElement.disabled = true;
  }
  console.log(evt);
});

htmx.on('htmx:after:request', function (evt) {
  console.log(evt);
  if (evt.srcElement.hasAttribute("data-indexing")) {
    updateIndexProgress();
  }
});

/*
document.addEventListener("htmx:beforeRequest", (event) => {
  console.log('event', event);
  if (event.detail.elt.matches("[data-indexing]")) {
    const status = document.querySelector("#index-progress");
    status.hidden = false;
    status.textContent = "Indexing: preparing files...";
    setIndexingControls(true);
    updateIndexProgress();
  }
  else if (!event.detail.elt.matches("[data-stop-indexing]")) {
    event.detail.elt.querySelector("button").disabled = true;
  }
});
*/

function locationPhotoURL(photoID) {
  let path = window.location.pathname;
  path = path.replace(/\/map\/?$/, "");
  path = path.replace(/\/photo\/[^/]+\/?$/, "");
  return `${path.replace(/\/$/, "")}/photo/${photoID}`;
}

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
  const pathCriteria = mapURL.pathname.startsWith("/locations/")
    ? new URLSearchParams(mapURL.pathname.slice("/locations/".length).replaceAll(",", "&"))
    : mapURL.searchParams;
  const savedCenter = [Number(pathCriteria.get("mapLng")), Number(pathCriteria.get("mapLat"))];
  const savedZoom = Number(pathCriteria.get("mapZoom"));
  const hasSavedView = savedCenter.every(Number.isFinite) && Number.isFinite(savedZoom);
  const initialMinLat = Number(element.dataset.minLat);
  const initialMinLng = Number(element.dataset.minLng);
  const initialMaxLat = Number(element.dataset.maxLat);
  const initialMaxLng = Number(element.dataset.maxLng);
  const hasInitialBounds = [initialMinLat, initialMinLng, initialMaxLat, initialMaxLng].every(Number.isFinite);
  const initialCenter = hasInitialBounds
    ? [(initialMinLng + initialMaxLng) / 2, (initialMinLat + initialMaxLat) / 2]
    : [0, 20];
  const map = new maplibregl.Map({
    container: element,
    center: hasInitialBounds ? initialCenter : (hasSavedView ? savedCenter : [0, 20]),
    zoom: hasInitialBounds ? 12 : (hasSavedView ? savedZoom : 2),
    style: {
      version: 8,
      sources: { osm: { type: "raster", tiles: ["https://tile.openstreetmap.org/{z}/{x}/{y}.png"], tileSize: 256, attribution: "&copy; OpenStreetMap contributors" } },
      layers: [{ id: "osm", type: "raster", source: "osm" }]
    }
  });
  let mapReturnURL = element.dataset.returnUrl || "/locations";
  function saveMapView() {
    if (!window.location.pathname.startsWith("/locations")) {
      return;
    }
    const bounds = map.getBounds();
    const clamp = (value, minimum, maximum) => Math.max(minimum, Math.min(maximum, value));
    const minLng = clamp(bounds.getWest(), -180, 180);
    const maxLng = clamp(bounds.getEast(), -180, 180);
    const minLat = clamp(bounds.getSouth(), -90, 90);
    const maxLat = clamp(bounds.getNorth(), -90, 90);
    const parts = [];
    parts.push(`minLat=${minLat.toFixed(6)}`);
    parts.push(`minLng=${minLng.toFixed(6)}`);
    parts.push(`maxLat=${maxLat.toFixed(6)}`);
    parts.push(`maxLng=${maxLng.toFixed(6)}`);
    window.history.replaceState({}, "", `/locations/${parts.join(",")}`);
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
  const photoId = element.dataset.photoId || "";
  const photoIds = element.dataset.photoIds || "";
  const locationBounds = ["minLat", "minLng", "maxLat", "maxLng"]
    .map((key) => Number(element.dataset[key]))
    .every((value) => Number.isFinite(value));
  if (locationBounds) {
    map.fitBounds([[Number(element.dataset.minLng), Number(element.dataset.minLat)], [Number(element.dataset.maxLng), Number(element.dataset.maxLat)]], { padding: 20, maxZoom: 22 });
  }

  function renderPoints(points) {
    markers.forEach((marker) => marker.remove());
    markers = points.map((point) => {
      const markerElement = document.createElement("button");
      markerElement.type = "button";
      const atMaxZoom = map.getZoom() >= map.getMaxZoom() - 0.01;
      if (point.count === 1 || atMaxZoom) {
        markerElement.className = "map-photo-marker";
        markerElement.setAttribute("aria-label", point.count === 1 ? "1 photo" : `${point.count} photos`);
        const image = document.createElement("img");
        image.src = `/thumbnails/${point.photoId}`;
        image.alt = "";
        markerElement.append(image);
      } else {
        markerElement.className = "map-cluster";
        markerElement.textContent = String(point.count);
        markerElement.setAttribute("aria-label", `${point.count} photos`);
      }
      markerElement.addEventListener("click", () => {
        const bounds = [[point.minLongitude, point.minLatitude], [point.maxLongitude, point.maxLatitude]];
        if (point.count === 1 || map.getZoom() >= map.getMaxZoom() - 0.01) {
          window.location.assign(locationPhotoURL(point.photoId));
          return;
        }
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
            link.href = locationPhotoURL(photo.photoId);
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
        if (Number(photoId) > 0) params.set("photoId", photoId);
        if (photoIds) params.set("photoIds", photoIds);
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
    if (Number(photoId) > 0) params.set("photoId", photoId);
    if (photoIds) params.set("photoIds", photoIds);
    if (settlement) params.set("settlement", settlement);
    if (region) params.set("region", region);
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
    if (locationBounds) map.fitBounds([[Number(element.dataset.minLng), Number(element.dataset.minLat)], [Number(element.dataset.maxLng), Number(element.dataset.maxLat)]], { padding: 20, maxZoom: 22 });
    loadPoints();
  });
  map.on("moveend", loadPoints);
  window.setTimeout(() => map.resize(), 0);
}

updateIndexProgress();
if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", initialisePhotoMap);
} else {
  initialisePhotoMap();
}

window.addEventListener("load", initialisePhotoMap);

(() => {
  const toggle = document.getElementById("sidebar-toggle");
  const sidebar = document.querySelector("main > aside");
  if (!toggle || !sidebar) { if (toggle) toggle.hidden = true; return; }
  sidebar.id = "sidebar";
  const setOpen = (open) => {
    document.body.classList.toggle("sidebar-open", open);
    toggle.setAttribute("aria-expanded", String(open));
  };
  toggle.addEventListener("click", () => setOpen(!document.body.classList.contains("sidebar-open")));
  sidebar.addEventListener("click", (event) => { if (event.target.closest("a")) setOpen(false); });
})();

// Touch gestures on a displayed photo reuse the keyboard handlers: swipe = previous/next, tap = select.
(() => {
  let start = null;
  const photoFrom = (target) => target.closest?.(".image-panel img, #photo-viewer img");
  const press = (key) => document.dispatchEvent(new KeyboardEvent("keydown", { key, bubbles: true, cancelable: true }));
  document.addEventListener("touchstart", (event) => {
    start = event.touches.length === 1 && photoFrom(event.target)
      ? { x: event.touches[0].clientX, y: event.touches[0].clientY }
      : null;
  }, { passive: true });
  document.addEventListener("touchend", (event) => {
    if (!start || !photoFrom(event.target)) { start = null; return; }
    const dx = event.changedTouches[0].clientX - start.x;
    const dy = event.changedTouches[0].clientY - start.y;
    start = null;
    if (Math.abs(dx) >= 50 && Math.abs(dx) > Math.abs(dy) * 1.5) {
      press(dx < 0 ? "ArrowRight" : "ArrowLeft");
    } else if (Math.abs(dx) < 10 && Math.abs(dy) < 10) {
      press(" ");
    }
  }, { passive: true });
})();
