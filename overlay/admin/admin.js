(function () {
  // Every API write carries this header. Browsers won't add it to a request
  // from another site, which is how the controller tells this page apart
  // from some random website posting at it.
  const ADMIN_HEADER = { 'X-Overlay-Admin': '1' };
  const SOUND_NAME = /^[a-z0-9][a-z0-9_-]{0,31}$/;
  const COSMETIC_ID = /^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$/;
  const SLOT_NAME = /^[a-z0-9][a-z0-9_-]{0,31}$/;
  const MAX_COOLDOWN = 86400;
  const AUDIO_ACCEPT = '.wav,.mp3,.ogg,audio/wav,audio/x-wav,audio/mpeg,audio/ogg';

  async function api(method, path, body) {
    const options = { method, headers: { ...ADMIN_HEADER } };
    if (body instanceof FormData) {
      options.body = body;
    } else if (body !== undefined) {
      options.headers['Content-Type'] = 'application/json';
      options.body = JSON.stringify(body);
    }
    const res = await fetch(`/admin/api/${path}`, options);
    let data = {};
    try {
      data = await res.json();
    } catch (err) {
      // Not JSON (a proxy error page, say): fall back to the status below.
    }
    if (!res.ok) throw new Error(data.error || `${res.status} ${res.statusText}`);
    return data;
  }

  // el builds DOM without innerHTML, so names that come off disk can never
  // inject markup into the page.
  function el(tag, props = {}, ...children) {
    const node = document.createElement(tag);
    for (const [key, value] of Object.entries(props)) {
      if (key === 'class') node.className = value;
      else if (key.startsWith('on')) node.addEventListener(key.slice(2), value);
      else if (key in node) node[key] = value;
      else node.setAttribute(key, value);
    }
    for (const child of children.flat()) {
      if (child == null || child === false) continue;
      node.append(child instanceof Node ? child : String(child));
    }
    return node;
  }

  const toastBox = document.querySelector('[data-toasts]');
  function toast(message, kind = 'ok') {
    const node = el('div', { class: `toast ${kind}`, role: kind === 'error' ? 'alert' : 'status' }, message);
    toastBox.append(node);
    setTimeout(() => node.remove(), kind === 'error' ? 9000 : 4500);
  }
  const fail = (err) => toast(err.message || String(err), 'error');

  function flash(node) {
    node.classList.remove('saved');
    void node.offsetWidth; // restart the animation
    node.classList.add('saved');
  }

  // A delete button that needs a second click within a few seconds, rather
  // than a blocking confirm() dialog.
  function confirmButton(label, armedLabel, action) {
    const button = el('button', { type: 'button', class: 'btn danger' }, label);
    let timer = null;
    button.addEventListener('click', () => {
      if (!button.classList.contains('armed')) {
        button.classList.add('armed');
        button.textContent = armedLabel;
        timer = setTimeout(() => {
          button.classList.remove('armed');
          button.textContent = label;
        }, 4000);
        return;
      }
      clearTimeout(timer);
      button.disabled = true;
      action();
    });
    return button;
  }

  function formatSize(bytes) {
    if (bytes < 1024 * 1024) return `${Math.max(1, Math.round(bytes / 1024))} KB`;
    return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
  }

  // nameFromFile suggests a command or cosmetic name: "Buhee Final.wav"
  // becomes "Buhee_Final".
  function nameFromFile(fileName) {
    return fileName
      .replace(/\.[^.]*$/, '')
      .replace(/[\s.]+/g, '_')
      .replace(/[^A-Za-z0-9_-]/g, '')
      .replace(/^[_-]+/, '')
      .slice(0, 32);
  }

  function dropzone(zone, onFiles) {
    const input = zone.querySelector('input[type="file"]');
    input.addEventListener('change', () => {
      onFiles([...input.files]);
      input.value = '';
    });
    zone.addEventListener('dragover', (event) => {
      event.preventDefault();
      zone.classList.add('over');
    });
    zone.addEventListener('dragleave', (event) => {
      if (!zone.contains(event.relatedTarget)) zone.classList.remove('over');
    });
    zone.addEventListener('drop', (event) => {
      event.preventDefault();
      zone.classList.remove('over');
      onFiles([...event.dataTransfer.files]);
    });
  }

  // A file dropped next to a dropzone would otherwise open in the tab.
  window.addEventListener('dragover', (event) => event.preventDefault());
  window.addEventListener('drop', (event) => event.preventDefault());

  // ---- Tabs ----

  const tabButtons = [...document.querySelectorAll('[data-tab]')];

  function showTab(name) {
    if (!tabButtons.some((button) => button.dataset.tab === name)) name = tabButtons[0].dataset.tab;
    tabButtons.forEach((button) => button.setAttribute('aria-selected', String(button.dataset.tab === name)));
    document.querySelectorAll('[data-panel]').forEach((panel) => {
      panel.hidden = panel.dataset.panel !== name;
    });
  }

  tabButtons.forEach((button) =>
    button.addEventListener('click', () => {
      history.replaceState(null, '', `#${button.dataset.tab}`);
      showTab(button.dataset.tab);
    }),
  );
  window.addEventListener('hashchange', () => showTab(location.hash.slice(1)));
  showTab(location.hash.slice(1));

  // ---- Sounds ----

  const soundList = document.querySelector('[data-sound-list]');
  const soundCount = document.querySelector('[data-sound-count]');
  const soundStaging = document.querySelector('[data-sound-staging]');
  const soundStagingList = document.querySelector('[data-sound-staging-list]');
  const addAllSounds = document.querySelector('[data-add-all-sounds]');
  const stagedSounds = new Set();
  let defaultCooldown = 5;

  async function loadSounds() {
    renderSounds(await api('GET', 'sounds'));
  }

  function renderSounds(state) {
    defaultCooldown = state.default_cooldown_seconds;
    soundCount.textContent = state.sounds.length ? `(${state.sounds.length})` : '';
    soundList.replaceChildren(
      ...(state.sounds.length
        ? state.sounds.map(soundRow)
        : [el('p', { class: 'empty' }, 'No sounds yet. Drop some files above.')]),
    );
  }

  // soundControls builds the cooldown and volume fields that saved and staged
  // rows share. The preview player follows the volume slider, so it sounds
  // the way the stream will.
  function soundControls(settings, preview) {
    const cooldown = el('input', {
      type: 'number',
      min: 0,
      max: MAX_COOLDOWN,
      step: 1,
      value: settings.cooldown_seconds,
      inputMode: 'numeric',
      title: '0 means no cooldown',
    });
    const volume = el('input', { type: 'range', min: 0, max: 100, step: 1, value: settings.volume });
    const volumeLabel = el('span', { class: 'volume-label' }, `${settings.volume}%`);
    volume.addEventListener('input', () => {
      volumeLabel.textContent = `${volume.value}%`;
      preview.volume = volume.value / 100;
    });
    preview.volume = settings.volume / 100;
    const cells = [
      el('label', { class: 'cell field' },
        el('span', { class: 'label' }, 'Cooldown'),
        el('span', { class: 'with-unit' }, cooldown, el('span', { class: 'unit' }, 'sec'))),
      el('label', { class: 'cell field' },
        el('span', { class: 'label' }, 'Volume'),
        el('span', { class: 'with-unit' }, volume, volumeLabel)),
    ];
    return { cooldown, volume, volumeLabel, cells };
  }

  function soundRow(sound) {
    let saved = sound; // what the server last confirmed
    const row = el('div', { class: 'row' });
    const preview = el('audio', { controls: true, preload: 'none' });
    const meta = el('p', { class: 'meta' });
    const name = el('input', { class: 'name-input', spellcheck: false, autocomplete: 'off', 'aria-label': 'Command name' });
    const { cooldown, volume, volumeLabel, cells } = soundControls(sound, preview);
    const replaceInput = el('input', { type: 'file', accept: AUDIO_ACCEPT, hidden: true });
    const path = () => `sounds/${encodeURIComponent(saved.name)}`;

    function show(s) {
      saved = s;
      name.value = s.name;
      cooldown.value = s.cooldown_seconds;
      volume.value = s.volume;
      volumeLabel.textContent = `${s.volume}%`;
      preview.volume = s.volume / 100;
      if (preview.getAttribute('src') !== s.url) preview.src = s.url;
      meta.textContent = `${s.file} · ${formatSize(s.size)}`;
    }

    // Edits save as soon as a field is left, so there's no Save button to forget.
    async function save(changes) {
      const body = { name: saved.name, cooldown_seconds: saved.cooldown_seconds, volume: saved.volume, ...changes };
      try {
        const updated = await api('PUT', path(), body);
        if (updated.name !== saved.name) {
          toast(`Renamed !${saved.name} to !${updated.name}`);
          await loadSounds();
          return;
        }
        show(updated);
        flash(row);
      } catch (err) {
        show(saved);
        fail(err);
      }
    }

    name.addEventListener('keydown', (event) => {
      if (event.key === 'Enter') name.blur();
      if (event.key === 'Escape') {
        name.value = saved.name;
        name.blur();
      }
    });
    name.addEventListener('change', () => {
      const next = name.value.trim().toLowerCase();
      if (next === saved.name) {
        name.value = saved.name;
        return;
      }
      if (!SOUND_NAME.test(next)) {
        fail(new Error('Command names are up to 32 lowercase letters, digits, - and _'));
        name.value = saved.name;
        return;
      }
      save({ name: next });
    });
    cooldown.addEventListener('change', () => {
      const seconds = Number(cooldown.value);
      if (cooldown.value === '' || !Number.isInteger(seconds) || seconds < 0 || seconds > MAX_COOLDOWN) {
        fail(new Error(`The cooldown is whole seconds, from 0 to ${MAX_COOLDOWN}`));
        cooldown.value = saved.cooldown_seconds;
        return;
      }
      save({ cooldown_seconds: seconds });
    });
    volume.addEventListener('change', () => save({ volume: Number(volume.value) }));
    replaceInput.addEventListener('change', async () => {
      const file = replaceInput.files[0];
      replaceInput.value = '';
      if (!file) return;
      const form = new FormData();
      form.append('file', file);
      try {
        show(await api('POST', `${path()}/file`, form));
        flash(row);
        toast(`Replaced the sound for !${saved.name}`);
      } catch (err) {
        fail(err);
      }
    });

    row.append(
      el('div', { class: 'cell name-cell' }, el('span', { class: 'bang' }, '!'), name),
      el('div', { class: 'cell audio-cell' }, preview, meta),
      ...cells,
      el('div', { class: 'cell actions' },
        el('button', { type: 'button', class: 'btn', onclick: () => replaceInput.click() }, 'Replace'),
        el('button', {
          type: 'button',
          class: 'btn',
          title: 'Play it through the overlay now, ignoring the cooldown',
          onclick: () => api('POST', `${path()}/play`).then(() => toast(`Playing !${saved.name} on stream`), fail),
        }, 'Play on stream'),
        confirmButton('Delete', 'Confirm', async () => {
          try {
            renderSounds(await api('DELETE', path()));
            toast(`Deleted !${saved.name}`);
          } catch (err) {
            fail(err);
            await loadSounds();
          }
        }),
        replaceInput),
    );
    show(sound);
    return row;
  }

  function updateSoundStaging() {
    soundStaging.hidden = stagedSounds.size === 0;
    addAllSounds.hidden = stagedSounds.size < 2;
  }

  function stageSounds(files) {
    for (const file of files) {
      if (!/\.(wav|mp3|ogg)$/i.test(file.name)) {
        fail(new Error(`${file.name}: sounds have to be wav, mp3 or ogg`));
        continue;
      }
      const row = stagedSoundRow(file);
      stagedSounds.add(row);
      soundStagingList.append(row);
    }
    updateSoundStaging();
  }

  function stagedSoundRow(file) {
    const url = URL.createObjectURL(file);
    const row = el('div', { class: 'row staged' });
    const preview = el('audio', { controls: true, preload: 'metadata', src: url });
    const name = el('input', {
      class: 'name-input',
      value: nameFromFile(file.name).toLowerCase(),
      spellcheck: false,
      autocomplete: 'off',
      'aria-label': 'Command name',
    });
    const { cooldown, volume, cells } = soundControls({ cooldown_seconds: defaultCooldown, volume: 100 }, preview);
    const add = el('button', { type: 'button', class: 'btn accent' }, 'Add');

    function discard() {
      URL.revokeObjectURL(url);
      row.remove();
      stagedSounds.delete(row);
      updateSoundStaging();
    }

    row.upload = async () => {
      const command = name.value.trim().toLowerCase();
      if (!SOUND_NAME.test(command)) {
        fail(new Error(`"${command}" won't work as a command: use up to 32 lowercase letters, digits, - and _`));
        name.focus();
        return false;
      }
      const form = new FormData();
      form.append('name', command);
      form.append('cooldown_seconds', cooldown.value);
      form.append('volume', volume.value);
      form.append('file', file);
      add.disabled = true;
      try {
        renderSounds(await api('POST', 'sounds', form));
        toast(`Added !${command}`);
        discard();
        return true;
      } catch (err) {
        fail(err);
        add.disabled = false;
        return false;
      }
    };
    add.addEventListener('click', row.upload);
    name.addEventListener('keydown', (event) => {
      if (event.key === 'Enter') row.upload();
    });

    row.append(
      el('div', { class: 'cell name-cell' }, el('span', { class: 'bang' }, '!'), name),
      el('div', { class: 'cell audio-cell' }, preview, el('p', { class: 'meta' }, `${file.name} · ${formatSize(file.size)}`)),
      ...cells,
      el('div', { class: 'cell actions' }, add, el('button', { type: 'button', class: 'btn', onclick: discard }, 'Cancel')),
    );
    return row;
  }

  addAllSounds.addEventListener('click', async () => {
    for (const row of [...stagedSounds]) {
      if (!(await row.upload())) break;
    }
  });

  // ---- Cosmetics ----

  const slotList = document.querySelector('[data-slot-list]');
  const slotForm = document.querySelector('[data-slot-form]');
  const cosmeticGrid = document.querySelector('[data-cosmetic-grid]');
  const cosmeticCount = document.querySelector('[data-cosmetic-count]');
  const cosmeticStaging = document.querySelector('[data-cosmetic-staging]');
  const cosmeticStagingGrid = document.querySelector('[data-cosmetic-staging-grid]');
  const addAllCosmetics = document.querySelector('[data-add-all-cosmetics]');
  const stagedCosmetics = new Set();
  let catalog = { slots: [], items: [], base: '/assets/Dabling.png', canvas: [500, 600] };

  async function loadCosmetics() {
    renderCatalog(await api('GET', 'cosmetics'));
  }

  // Every cosmetic write answers with the whole catalog, so the page just
  // redraws from it.
  function renderCatalog(state) {
    catalog = state;
    renderSlots();
    cosmeticCount.textContent = state.items.length ? `(${state.items.length})` : '';
    cosmeticGrid.replaceChildren(
      ...(state.items.length
        ? state.items.map(cosmeticCard)
        : [el('p', { class: 'empty' }, 'No cosmetics yet. Drop some PNGs above.')]),
    );
    stagedCosmetics.forEach((card) => card.refreshSlots());
    if (shop) loadShop().catch(fail); // what the shop can sell, and its pictures, follow the catalog
  }

  // doll stacks a cosmetic over the untinted base sprite, like the overlay.
  function doll(layerURL) {
    return el('div', { class: 'doll' }, el('img', { src: catalog.base, alt: '' }), el('img', { src: layerURL, alt: '' }));
  }

  function fillSlotSelect(select, value) {
    select.replaceChildren(...catalog.slots.map((slot) => el('option', { value: slot }, slot)));
    select.value = catalog.slots.includes(value) ? value : catalog.slots[0] || '';
  }

  function renderSlots() {
    const used = new Set(catalog.items.map((item) => item.slot));
    const last = catalog.slots.length - 1;
    slotList.replaceChildren(
      ...catalog.slots.map((slot, i) =>
        el('li', { class: 'slot-chip' },
          el('span', { class: 'slot-order' }, String(i + 1)),
          el('span', { class: 'slot-name' }, slot),
          el('button', {
            type: 'button',
            class: 'icon-btn',
            title: 'Draw earlier (underneath)',
            'aria-label': `Move ${slot} earlier`,
            disabled: i === 0,
            onclick: () => moveSlot(i, -1),
          }, '‹'),
          el('button', {
            type: 'button',
            class: 'icon-btn',
            title: 'Draw later (on top)',
            'aria-label': `Move ${slot} later`,
            disabled: i === last,
            onclick: () => moveSlot(i, 1),
          }, '›'),
          el('button', {
            type: 'button',
            class: 'icon-btn',
            title: used.has(slot) ? 'Move or delete its cosmetics first' : 'Remove this slot',
            'aria-label': `Remove ${slot}`,
            disabled: used.has(slot),
            onclick: () => saveSlots(catalog.slots.filter((s) => s !== slot)),
          }, '×'),
        ),
      ),
    );
    if (!catalog.slots.length) slotList.append(el('li', { class: 'empty' }, 'No slots yet. Add one before uploading cosmetics.'));
  }

  async function saveSlots(slots) {
    try {
      renderCatalog(await api('PUT', 'slots', { slots }));
      return true;
    } catch (err) {
      fail(err);
      return false;
    }
  }

  function moveSlot(i, delta) {
    const slots = [...catalog.slots];
    [slots[i], slots[i + delta]] = [slots[i + delta], slots[i]];
    saveSlots(slots);
  }

  slotForm.addEventListener('submit', async (event) => {
    event.preventDefault();
    const input = slotForm.elements.slot;
    const slot = input.value.trim().toLowerCase();
    if (!SLOT_NAME.test(slot)) {
      fail(new Error('Slot names are up to 32 lowercase letters, digits, - and _'));
      return;
    }
    if (catalog.slots.includes(slot)) {
      fail(new Error(`There's already a ${slot} slot`));
      return;
    }
    if (await saveSlots([...catalog.slots, slot])) {
      input.value = '';
      toast(`Added the ${slot} slot`);
    }
  });

  function cosmeticCard(item) {
    const path = `cosmetics/${encodeURIComponent(item.id)}`;
    const select = el('select', { 'aria-label': `Slot for ${item.id}` });
    fillSlotSelect(select, item.slot);
    select.addEventListener('change', async () => {
      try {
        renderCatalog(await api('PUT', path, { slot: select.value }));
        toast(`Moved ${item.id} to ${select.value}. Anyone wearing it has to !equip it again.`);
      } catch (err) {
        select.value = item.slot;
        fail(err);
      }
    });
    const replaceInput = el('input', { type: 'file', accept: '.png,image/png', hidden: true });
    replaceInput.addEventListener('change', async () => {
      const file = replaceInput.files[0];
      replaceInput.value = '';
      if (!file) return;
      const form = new FormData();
      form.append('file', file);
      try {
        renderCatalog(await api('POST', `${path}/image`, form));
        toast(`Replaced ${item.id}'s image`);
      } catch (err) {
        fail(err);
      }
    });

    return el('article', { class: 'card' },
      doll(item.url),
      el('div', { class: 'card-body' },
        el('h3', { class: 'card-title' }, item.id),
        el('label', { class: 'field' }, el('span', { class: 'label' }, 'Slot'), select),
        el('div', { class: 'actions' },
          el('button', { type: 'button', class: 'btn', onclick: () => replaceInput.click() }, 'Replace'),
          confirmButton('Delete', 'Confirm', async () => {
            try {
              renderCatalog(await api('DELETE', path));
              toast(`Deleted ${item.id}. Owners lose it until a cosmetic called ${item.id} is added again.`);
            } catch (err) {
              fail(err);
              await loadCosmetics();
            }
          }),
          replaceInput)),
    );
  }

  function updateCosmeticStaging() {
    cosmeticStaging.hidden = stagedCosmetics.size === 0;
    addAllCosmetics.hidden = stagedCosmetics.size < 2;
  }

  function stageCosmetics(files) {
    for (const file of files) {
      if (!/\.png$/i.test(file.name)) {
        fail(new Error(`${file.name}: cosmetics have to be PNGs`));
        continue;
      }
      const card = stagedCosmeticCard(file);
      stagedCosmetics.add(card);
      cosmeticStagingGrid.append(card);
    }
    updateCosmeticStaging();
  }

  function stagedCosmeticCard(file) {
    const url = URL.createObjectURL(file);
    const card = el('article', { class: 'card staged' });
    const id = el('input', { value: nameFromFile(file.name), spellcheck: false, autocomplete: 'off', 'aria-label': 'Cosmetic name' });
    const select = el('select', { 'aria-label': 'Slot' });
    const sizeNote = el('p', { class: 'meta' }, 'Checking the canvas…');
    const add = el('button', { type: 'button', class: 'btn accent' }, 'Add');

    // Catch the wrong canvas here, before the server refuses the upload.
    const probe = new Image();
    probe.onload = () => {
      const [w, h] = catalog.canvas;
      const fits = probe.naturalWidth * h === probe.naturalHeight * w;
      sizeNote.textContent = `${probe.naturalWidth}×${probe.naturalHeight}` + (fits ? '' : `, but it needs to be ${w}×${h}`);
      sizeNote.classList.toggle('bad', !fits);
    };
    probe.onerror = () => {
      sizeNote.textContent = "Couldn't read this image";
      sizeNote.classList.add('bad');
    };
    probe.src = url;

    card.refreshSlots = () => fillSlotSelect(select, select.value);
    card.refreshSlots();

    function discard() {
      URL.revokeObjectURL(url);
      card.remove();
      stagedCosmetics.delete(card);
      updateCosmeticStaging();
    }

    card.upload = async () => {
      const name = id.value.trim();
      if (!COSMETIC_ID.test(name)) {
        fail(new Error(`"${name}" won't work as a cosmetic name: use up to 32 letters, digits, - and _`));
        id.focus();
        return false;
      }
      if (!select.value) {
        fail(new Error('Add a slot first'));
        return false;
      }
      const form = new FormData();
      form.append('id', name);
      form.append('slot', select.value);
      form.append('file', file);
      add.disabled = true;
      try {
        renderCatalog(await api('POST', 'cosmetics', form));
        toast(`Added ${name}. Hand it out with !give <name> ${name}`);
        discard();
        return true;
      } catch (err) {
        fail(err);
        add.disabled = false;
        return false;
      }
    };
    add.addEventListener('click', card.upload);

    card.append(
      doll(url),
      el('div', { class: 'card-body' },
        el('label', { class: 'field' }, el('span', { class: 'label' }, 'Name'), id),
        el('label', { class: 'field' }, el('span', { class: 'label' }, 'Slot'), select),
        sizeNote,
        el('div', { class: 'actions' }, add, el('button', { type: 'button', class: 'btn', onclick: discard }, 'Cancel'))),
    );
    return card;
  }

  addAllCosmetics.addEventListener('click', async () => {
    for (const card of [...stagedCosmetics]) {
      if (!(await card.upload())) break;
    }
  });

  // ---- Shop ----

  const shopBoxes = document.querySelector('[data-shop-boxes]');
  const shopDefault = document.querySelector('[data-shop-default]');
  const MAX_PRICE = 1000000;
  let shop = null; // what the server last confirmed

  async function loadShop() {
    renderShop(await api('GET', 'shop'));
  }

  function renderShop(state) {
    shop = state;
    shopDefault.textContent = state.default_price;
    shopBoxes.replaceChildren(...state.boxes.map(shopBoxCard));
  }

  // Every change sends all three boxes, so the controller checks the stall as
  // a whole (one item can't be in two boxes).
  async function saveShop(index, change) {
    const boxes = shop.boxes.map((box) => ({ item: box.item, price: box.price || 0 }));
    boxes[index] = { ...boxes[index], ...change };
    try {
      renderShop(await api('PUT', 'shop', { boxes }));
      flash(shopBoxes.children[index]);
    } catch (err) {
      renderShop(shop);
      fail(err);
    }
  }

  function shopBoxCard(box, index) {
    const items = new Map(shop.items.map((item) => [item.id, item]));
    const select = el('select', { 'aria-label': `What box ${index + 1} sells` },
      el('option', { value: '' }, 'Nothing'),
      ...shop.items.map((item) => el('option', { value: item.id }, item.id)));
    select.value = box.item;
    const price = el('input', {
      type: 'number',
      min: 1,
      max: MAX_PRICE,
      step: 1,
      value: box.item ? box.price : '',
      disabled: !box.item,
      inputMode: 'numeric',
      'aria-label': `Price of box ${index + 1}`,
    });

    // A newly picked item comes in at its own price: the default, or what it
    // cost the last time it was in the shop.
    select.addEventListener('change', () => {
      const item = items.get(select.value);
      saveShop(index, { item: select.value, price: item ? item.price : 0 });
    });
    price.addEventListener('keydown', (event) => {
      if (event.key === 'Enter') price.blur();
    });
    price.addEventListener('change', () => {
      const points = Number(price.value);
      if (!Number.isInteger(points) || points < 1 || points > MAX_PRICE) {
        fail(new Error(`Prices are whole channel points, from 1 to ${MAX_PRICE.toLocaleString()}`));
        price.value = box.price;
        return;
      }
      saveShop(index, { price: points });
    });

    return el('article', { class: 'card shop-box' },
      box.item ? doll(box.url) : el('div', { class: 'doll empty-box' }, 'Empty'),
      el('div', { class: 'card-body' },
        el('h3', { class: 'card-title' }, `Box ${index + 1}`),
        el('label', { class: 'field' }, el('span', { class: 'label' }, 'Sells'), select),
        el('label', { class: 'field' },
          el('span', { class: 'label' }, 'Price'),
          el('span', { class: 'with-unit' }, price, el('span', { class: 'unit' }, 'points')))));
  }

  // ---- Party guide ----

  // The words live in the page and the numbers come from the controller, so
  // the guide stays right when the rules are tuned.
  const guidePanel = document.querySelector('[data-panel="guide"]');
  const guideLevels = document.querySelector('[data-guide-levels]');
  const guideRewards = document.querySelector('[data-guide-rewards]');
  const listFormat = new Intl.ListFormat('en', { type: 'conjunction' });

  // Non-breaking spaces keep each number with its unit when a cell wraps.
  function formatDuration(seconds) {
    const minutes = Math.max(1, Math.round(seconds / 60));
    if (minutes < 60) return `${minutes} min`;
    const hours = Math.floor(minutes / 60);
    return minutes % 60 ? `${hours} h ${minutes % 60} min` : `${hours} h`;
  }

  async function loadGuide() {
    const guide = await api('GET', 'guide');
    const [newcomer, ...veterans] = guide.exp_rates;
    const values = {
      join: guide.redeems.join,
      daily_login: guide.redeems.daily_login,
      first_login: guide.redeems.first_login,
      party_size: guide.party_size,
      voice_tiers: listFormat.format(guide.voice_tiers.map(String)),
      exp_rates: [
        `${newcomer.exp} per message for a new chatter`,
        ...veterans.map((rate) => `${rate.exp} at ${rate.logins} daily logins`),
      ].join(', '),
      exp_cooldown: guide.exp_cooldown_seconds,
      revive: guide.revive_levels === 1 ? '1 level' : `${guide.revive_levels} levels`,
      tavern_idle: guide.tavern_idle_minutes,
      sheet: guide.sheet_seconds,
    };
    guidePanel.querySelectorAll('[data-guide]').forEach((node) => {
      node.textContent = values[node.dataset.guide] ?? '?';
    });

    guideLevels.replaceChildren(
      ...guide.levels.map((row) =>
        el(
          'tr',
          {},
          el('td', {}, row.level),
          el('td', {}, row.exp.toLocaleString()),
          el('td', {}, row.max_hp),
          el(
            'td',
            {},
            row.level === 1
              ? 'Where everyone starts'
              : `${row.messages.toLocaleString()} messages, ${formatDuration(row.messages * guide.exp_cooldown_seconds)}`,
          ),
        ),
      ),
    );

    guideRewards.replaceChildren(
      ...(guide.auto_rewards.length
        ? guide.auto_rewards.map((reward) =>
            el(
              'li',
              {},
              el('strong', {}, `${reward.cosmetic} ${reward.noun}`),
              ` for redeeming ${reward.redeem || reward.counter} ${reward.count === 1 ? 'once' : `${reward.count} times`}.`,
            ),
          )
        : [el('li', {}, 'None yet.')]),
    );
  }

  dropzone(document.querySelector('[data-sound-drop]'), stageSounds);
  dropzone(document.querySelector('[data-cosmetic-drop]'), stageCosmetics);
  loadSounds().catch(fail);
  loadCosmetics().catch(fail);
  loadShop().catch(fail);
  loadGuide().catch(fail);
})();
