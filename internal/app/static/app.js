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
    actionMenu.querySelectorAll("button:not(#select-all)").forEach((button) => {
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

function selectAll() {
  //select all checkboxes with data-photo-selection attribute
  document.querySelectorAll("[data-photo-selection]").forEach((input) => {
    input.checked = true;
  });
  updateActionMenu();
}

function selectNone() {
  //deselect all checkboxes with data-photo-selection attribute
  document.querySelectorAll("[data-photo-selection]").forEach((input) => {
    input.checked = false;
  });
  updateActionMenu();
}

//when the document has loaded then update the action menu
document.addEventListener("DOMContentLoaded", () => {
 updateActionMenu();
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

document.addEventListener("click", (event) => {
  if (event.target.matches("#photo-viewer-back")) {
    closePhotoViewer();
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
  if (event.key === "a" || event.key === "A") {
    selectAll();
  }
  if (event.key === "d" || event.key === "D") {
    unselectAll();
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

htmx.on('htmx:before:request', function (evt) {
  console.log(evt);
    let ctx = evt.detail.ctx;
   let sourceElementId = ctx.sourceElement.id;

    //if we're POSTing to /collections we need to include an array of the selected photo ids in the request body
  if (ctx.request.method === 'POST' && ctx.request.action === '/collections') {
    console.log('Adding selected photo ids to request body');
    const selectedPhotoIds = Array.from(document.querySelectorAll('.photo input[type="checkbox"]:checked')).map(input => input.value);
    //create a comma-separated string of the selected photo ids and add it to the request body
    ctx.request.body.set('ids', selectedPhotoIds.join(','));
  }
  //if the source has the data-append-photo-ids attribute, we need to add the selected photo ids to the querystring
  else if (ctx.sourceElement.hasAttribute('data-append-photo-ids')) {
    //add the selected photoids to the querystring ids
    console.log('Adding selected photo ids to querystring');
    const selectedPhotoIds = Array.from(document.querySelectorAll('.photo input[type="checkbox"]:checked')).map(input => input.value);
    //if the querystring already has an '?' we need to append with a '&', otherwise we append with a '?'
    if (ctx.request.action.includes('?')) {
      ctx.request.action = ctx.request.action + '&ids=' + selectedPhotoIds.join(',');
    }
    else {
      ctx.request.action = ctx.request.action + '?ids=' + selectedPhotoIds.join(',');
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
