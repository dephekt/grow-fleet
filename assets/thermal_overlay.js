// Inject a live thermal-camera card into the ESPHome web_server (v3) dashboard.
//
// ESPHome's web UI only renders entity tiles (sensors, switches, numbers, ...),
// so the MLX90640 image served at /thermal.jpg never shows there on its own.
// This file is baked into the firmware via `web_server: js_include:` and loaded
// by the dashboard page (served at /0.js). It appends a card with an
// auto-refreshing <img> above the entity list.
//
// The cache-busting `?t=` query forces the browser to fetch a fresh frame each
// cycle; the component's handler matches on the path only (web_server_idf strips
// the query before the URL compare), so the request still serves the image.

(function () {
  var REFRESH_MS = 2000; // matches the default Thermal Update Interval

  function build() {
    if (document.getElementById('thermal-card')) return; // idempotent

    var card = document.createElement('div');
    card.id = 'thermal-card';
    card.style.cssText = 'text-align:center;margin:16px auto;max-width:480px;padding:0 12px';

    var title = document.createElement('h2');
    title.textContent = 'Thermal Camera';
    title.style.cssText = 'font:600 1.1rem system-ui,sans-serif;margin:0 0 8px';

    var img = document.createElement('img');
    img.alt = 'MLX90640 thermal image';
    img.style.cssText = 'width:100%;image-rendering:pixelated;border-radius:8px;background:#000';

    function refresh() {
      img.src = '/thermal.jpg?t=' + Date.now();
    }
    img.addEventListener('load', function () {
      setTimeout(refresh, REFRESH_MS);
    });
    img.addEventListener('error', function () {
      setTimeout(refresh, REFRESH_MS * 2);
    });
    refresh();

    card.appendChild(title);
    card.appendChild(img);
    document.body.insertBefore(card, document.body.firstChild);
  }

  // The v3 single-page app loads its bundle asynchronously, so retry a few times
  // to make sure the card survives the initial render.
  function arm() {
    build();
    [500, 1500, 3000].forEach(function (d) {
      setTimeout(build, d);
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', arm);
  } else {
    arm();
  }
})();
