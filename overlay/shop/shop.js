(function () {
  // The stall art and layout.json come from tools/shop_stall.py: layout.json
  // says where each box's backing and price tag landed in the render, so the
  // items and prices drawn here sit in them exactly.
  const W = 1920;
  const H = 1080;
  const INK = '#1c1414';
  const stage = document.querySelector('[data-stage]');

  function fit() {
    stage.style.transform = `scale(${Math.min(innerWidth / W, innerHeight / H)})`;
  }
  addEventListener('resize', fit);
  fit();

  // ---- Hand-lettered prices ----

  // The same brush strokes as the stall's painted numbers, so a price looks
  // written on its tag. Each digit is a width and strokes in a box 1 tall, y
  // up; 's' strokes are smoothed through their points, 'l' ones stay straight.
  const DIGITS = {
    0: [0.6, [['s', [[0.3, 1], [0.06, 0.85], [0, 0.5], [0.06, 0.15], [0.3, 0], [0.54, 0.15], [0.6, 0.5], [0.54, 0.85], [0.3, 1], [0.08, 0.88]]]]],
    1: [0.5, [['l', [[0.06, 0.78], [0.28, 1], [0.28, 0]]], ['l', [[0.06, 0], [0.5, 0]]]]],
    2: [0.66, [['s', [[0.04, 0.74], [0.16, 0.93], [0.38, 1], [0.58, 0.9], [0.62, 0.7], [0.44, 0.45], [0.02, 0]]], ['l', [[0.02, 0], [0.66, 0]]]]],
    3: [0.62, [['s', [[0.04, 0.86], [0.3, 1], [0.56, 0.9], [0.56, 0.68], [0.3, 0.54]]], ['s', [[0.3, 0.54], [0.6, 0.4], [0.6, 0.14], [0.34, 0], [0.04, 0.1]]]]],
    4: [0.64, [['l', [[0.46, 0], [0.46, 1], [0, 0.3], [0.64, 0.3]]]]],
    5: [0.62, [['l', [[0.58, 1], [0.1, 1], [0.06, 0.56]]], ['s', [[0.06, 0.56], [0.3, 0.62], [0.56, 0.52], [0.62, 0.28], [0.48, 0.05], [0.24, 0], [0.02, 0.1]]]]],
    6: [0.6, [['s', [[0.56, 0.92], [0.36, 1], [0.12, 0.86], [0.02, 0.5], [0.06, 0.16], [0.3, 0], [0.54, 0.12], [0.6, 0.34], [0.48, 0.56], [0.26, 0.6], [0.06, 0.44]]]]],
    7: [0.62, [['l', [[0.02, 1], [0.62, 1], [0.22, 0]]]]],
    8: [0.64, [['s', [[0.32, 0.54], [0.08, 0.66], [0.08, 0.9], [0.32, 1], [0.56, 0.9], [0.56, 0.66], [0.32, 0.54], [0.04, 0.4], [0.04, 0.12], [0.32, 0], [0.6, 0.12], [0.6, 0.4], [0.32, 0.54]]]]],
    9: [0.6, [['s', [[0.58, 0.56], [0.36, 0.42], [0.1, 0.5], [0.02, 0.74], [0.14, 0.96], [0.36, 1], [0.56, 0.88], [0.6, 0.6], [0.54, 0.24], [0.32, 0], [0.08, 0.06]]]]],
  };
  const SPACING = 0.16;

  function catmull(pts, per = 8) {
    if (pts.length < 3) return pts;
    const p = [pts[0], ...pts, pts[pts.length - 1]];
    const out = [];
    for (let i = 1; i < p.length - 2; i++) {
      for (let k = 0; k < per; k++) {
        const t = k / per;
        const t2 = t * t;
        const t3 = t2 * t;
        out.push([0, 1].map((d) => 0.5 * (2 * p[i][d] + (-p[i - 1][d] + p[i + 1][d]) * t
          + (2 * p[i - 1][d] - 5 * p[i][d] + 4 * p[i + 1][d] - p[i + 2][d]) * t2
          + (-p[i - 1][d] + 3 * p[i][d] - 3 * p[i + 1][d] + p[i + 2][d]) * t3)));
      }
    }
    out.push(pts[pts.length - 1]);
    return out;
  }

  // drawPrice writes the price across the middle of its tag, as big as fits
  // between the tacks in its top corners.
  function drawPrice(canvas, price) {
    const ctx = canvas.getContext('2d');
    ctx.clearRect(0, 0, canvas.width, canvas.height);
    const glyphs = [...String(price)].map((ch) => DIGITS[ch]).filter(Boolean);
    if (!glyphs.length) return;
    const units = glyphs.reduce((sum, [width]) => sum + width + SPACING, -SPACING);
    const size = Math.min(canvas.height * 0.52, (canvas.width * 0.72) / units);
    const baseline = (canvas.height + size) / 2 + canvas.height * 0.04;
    let x = (canvas.width - units * size) / 2;
    ctx.strokeStyle = INK;
    ctx.lineWidth = size * 0.19;
    ctx.lineCap = 'round';
    ctx.lineJoin = 'round';
    for (const [width, strokes] of glyphs) {
      for (const [kind, pts] of strokes) {
        ctx.beginPath();
        (kind === 's' ? catmull(pts) : pts).forEach(([px, py], i) => {
          ctx[i ? 'lineTo' : 'moveTo'](x + px * size, baseline - py * size);
        });
        ctx.stroke();
      }
      x += (width + SPACING) * size;
    }
  }

  // ---- Items ----

  // Each box shows the item on the base (untinted) Dabling, so chat can see
  // how it'll look. A cosmetic is a layer on the same canvas as Dabling.png,
  // so the two stack the way the overlay draws them.
  const BASE = '/assets/Dabling.png';
  const images = new Map(); // url -> Promise of {image, bounds}

  // load fetches an image and finds where its visible pixels are, as fractions
  // of its size, since a layer can be a same-shape 2x export of the canvas.
  function load(url) {
    if (!images.has(url)) {
      images.set(url, new Promise((resolve, reject) => {
        const image = new Image();
        image.onload = () => {
          const { naturalWidth: w, naturalHeight: h } = image;
          const canvas = document.createElement('canvas');
          canvas.width = w;
          canvas.height = h;
          const ctx = canvas.getContext('2d');
          ctx.drawImage(image, 0, 0);
          const { data } = ctx.getImageData(0, 0, w, h);
          let x0 = w;
          let y0 = h;
          let x1 = -1;
          let y1 = -1;
          for (let y = 0; y < h; y++) {
            for (let x = 0; x < w; x++) {
              if (data[(y * w + x) * 4 + 3] > 8) {
                x0 = Math.min(x0, x);
                x1 = Math.max(x1, x);
                y0 = Math.min(y0, y);
                y1 = Math.max(y1, y);
              }
            }
          }
          const bounds = x1 < 0 ? null : { x0: x0 / w, y0: y0 / h, x1: (x1 + 1) / w, y1: (y1 + 1) / h };
          resolve({ image, bounds });
        };
        image.onerror = () => {
          images.delete(url);
          reject(new Error(`couldn't load ${url}`));
        };
        image.src = url;
      }));
    }
    return images.get(url);
  }

  async function drawItem(canvas, url) {
    const turn = (canvas.turn = {}); // a newer restock wins if the images load late
    const ctx = canvas.getContext('2d');
    if (!url) {
      ctx.clearRect(0, 0, canvas.width, canvas.height);
      return;
    }
    const layers = await Promise.all([load(BASE), load(url)]);
    if (canvas.turn !== turn) return;

    // Fit the part of the canvas the Dabling and the item cover (a hat can
    // stick up past the head) into the box.
    const all = layers.map((layer) => layer.bounds).filter(Boolean);
    if (!all.length) all.push({ x0: 0, y0: 0, x1: 1, y1: 1 });
    const x0 = Math.min(...all.map((b) => b.x0));
    const y0 = Math.min(...all.map((b) => b.y0));
    const x1 = Math.max(...all.map((b) => b.x1));
    const y1 = Math.max(...all.map((b) => b.y1));
    const base = layers[0].image;
    const pad = 0.06;
    const scale = Math.min(
      (canvas.width * (1 - 2 * pad)) / ((x1 - x0) * base.naturalWidth),
      (canvas.height * (1 - 2 * pad)) / ((y1 - y0) * base.naturalHeight),
    );
    const w = (x1 - x0) * base.naturalWidth * scale;
    const h = (y1 - y0) * base.naturalHeight * scale;
    ctx.clearRect(0, 0, canvas.width, canvas.height);
    ctx.imageSmoothingQuality = 'high';
    for (const { image } of layers) {
      const sw = image.naturalWidth;
      const sh = image.naturalHeight;
      ctx.drawImage(image, x0 * sw, y0 * sh, (x1 - x0) * sw, (y1 - y0) * sh, (canvas.width - w) / 2, (canvas.height - h) / 2, w, h);
    }
    canvas.classList.remove('pop');
    void canvas.offsetWidth; // restart the animation
    canvas.classList.add('pop');
  }

  // ---- The stall ----

  const boxes = []; // per box: {item, price} canvases, and what they show

  function place(node, rect) {
    node.width = rect.w;
    node.height = rect.h;
    Object.assign(node.style, {
      left: `${rect.cx - rect.w / 2}px`,
      top: `${rect.cy - rect.h / 2}px`,
      width: `${rect.w}px`,
      height: `${rect.h}px`,
      transform: `rotate(${rect.angle}deg)`,
    });
    return node;
  }

  function build(layout) {
    for (const spot of layout.boxes) {
      const item = place(document.createElement('canvas'), spot.item);
      const price = place(document.createElement('canvas'), spot.price);
      item.className = 'item';
      price.className = 'price';
      stage.append(item, price);
      boxes.push({ item, price, shown: { item: '', url: '', price: 0 } });
    }
  }

  function show(stock) {
    stock.forEach((box, i) => {
      const spot = boxes[i];
      if (!spot) return;
      if (box.url !== spot.shown.url) drawItem(spot.item, box.url).catch((err) => console.error(err));
      if (box.price !== spot.shown.price) drawPrice(spot.price, box.item ? box.price : '');
      spot.shown = { item: box.item, url: box.url || '', price: box.price };
    });
  }

  async function loadStock() {
    const res = await fetch('/shop/stock', { cache: 'no-store' });
    if (!res.ok) throw new Error(`/shop/stock: ${res.status}`);
    show((await res.json()).boxes);
  }

  // Restocking on /admin arrives as shop.update. The page reloads the stock
  // whenever it (re)connects, so a change made while it was away still shows.
  function connect() {
    const protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const socket = new WebSocket(`${protocol}//${location.host}/ws/overlay`);
    socket.addEventListener('open', () => loadStock().catch((err) => console.error(err)));
    socket.addEventListener('message', (event) => {
      try {
        const payload = JSON.parse(event.data);
        if (payload.type === 'shop.update') show(payload.boxes);
      } catch (err) {
        console.error('Failed to parse message', err);
      }
    });
    socket.addEventListener('close', () => setTimeout(connect, 2000));
    socket.addEventListener('error', () => socket.close());
  }

  fetch('layout.json', { cache: 'no-store' })
    .then((res) => res.json())
    .then((layout) => {
      build(layout);
      connect();
    })
    .catch((err) => console.error('Failed to load layout.json', err));
})();
