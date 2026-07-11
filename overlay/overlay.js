(function () {
  const chatBox = document.getElementById('chat-box');
  const otherBox = document.getElementById('other-box');
  const partyCardsBox = document.getElementById('party-cards');
  let otherBaseHTML = '';
  let pongActive = false;

  function renderPartyCards(members) {
    partyCardsBox.innerHTML = '';
    (members || []).forEach((member) => {
      const hpPct = member.max_hp > 0 ? Math.max(0, Math.min(100, (member.hp / member.max_hp) * 100)) : 0;
      const expPct = member.exp_next > 0 ? Math.max(0, Math.min(100, (member.exp / member.exp_next) * 100)) : 0;
      const hue = (member.variant || 0) * 40;

      const card = document.createElement('div');
      card.className = 'party-card';
      card.innerHTML = `
        <img class="avatar" src="assets/Dabling.png" style="filter: hue-rotate(${hue}deg)" alt="${escapeHTML(member.name || '')}" />
        <div class="name">${escapeHTML(member.name || '')}</div>
        <div class="level">Lv ${member.level ?? 1}</div>
        <div class="bar"><div class="bar-fill hp-fill" style="width:${hpPct}%"></div></div>
        <div class="bar-label">${member.hp ?? 0}/${member.max_hp ?? 0} hp</div>
        <div class="bar"><div class="bar-fill exp-fill" style="width:${expPct}%"></div></div>
        <div class="bar-label">${member.exp ?? 0}/${member.exp_next ?? 0} exp</div>
      `;
      partyCardsBox.appendChild(card);
    });
  }

  const tavernArea = document.getElementById('tavern-area');
  const tavernDudes = new Map(); // lowercase name -> {el, x, wanderTimer}
  let partyNames = new Set();
  const DUDE_BASE_HEIGHT = 96;

  function dudeScale(level) {
    return Math.min(1 + ((level || 1) - 1) * 0.08, 2.5);
  }

  function wanderTo(dude) {
    const x = 2 + Math.random() * 88; // % across the walking strip
    const distance = Math.abs(x - (dude.x ?? x));
    dude.el.style.transition = `left ${(2 + distance * 0.12).toFixed(1)}s linear`;
    dude.el.style.left = `${x}%`;
    const sprite = dude.el.querySelector('.dude-sprite');
    if (x < dude.x) sprite.style.transform = 'scaleX(-1)';
    else if (x > dude.x) sprite.style.transform = 'scaleX(1)';
    dude.x = x;
  }

  function scheduleWander(dude) {
    dude.wanderTimer = setTimeout(() => {
      wanderTo(dude);
      scheduleWander(dude);
    }, 3000 + Math.random() * 9000);
  }

  function removeDude(key) {
    const dude = tavernDudes.get(key);
    if (!dude) return;
    clearTimeout(dude.wanderTimer);
    dude.el.remove();
    tavernDudes.delete(key);
  }

  function renderTavern(dudes) {
    const seen = new Set();
    (dudes || []).forEach((d) => {
      const key = (d.name || '').toLowerCase();
      if (!key || partyNames.has(key)) return; // possessed dudes are on cards, not the floor
      seen.add(key);
      let dude = tavernDudes.get(key);
      if (!dude) {
        const el = document.createElement('div');
        el.className = 'tavern-dude';
        el.innerHTML = `
          <div class="dude-sprite"><img src="assets/Dabling.png" style="filter: hue-rotate(${(d.variant || 0) * 40}deg)" alt="" /></div>
          <div class="dude-name">${escapeHTML(d.name || '')}</div>
        `;
        el.style.bottom = `${Math.floor(Math.random() * 14)}px`;
        tavernArea.appendChild(el);
        dude = { el, x: 2 + Math.random() * 88 };
        el.style.left = `${dude.x}%`;
        tavernDudes.set(key, dude);
        scheduleWander(dude);
      }
      dude.el.style.setProperty('--dude-height', `${Math.round(DUDE_BASE_HEIGHT * dudeScale(d.level))}px`);
    });
    for (const key of [...tavernDudes.keys()]) {
      if (!seen.has(key)) removeDude(key);
    }
  }

  const rollBox = document.getElementById('roll-box');
  let rollHideTimer = null;
  let rollCycleTimer = null;
  let audioCtx = null;

  function beep(freq, startOffset, duration, type, volume) {
    const osc = audioCtx.createOscillator();
    const gain = audioCtx.createGain();
    osc.type = type;
    osc.frequency.value = freq;
    const t = audioCtx.currentTime + startOffset;
    gain.gain.setValueAtTime(0.0001, t);
    gain.gain.exponentialRampToValueAtTime(volume, t + 0.02);
    gain.gain.exponentialRampToValueAtTime(0.0001, t + duration);
    osc.connect(gain).connect(audioCtx.destination);
    osc.start(t);
    osc.stop(t + duration + 0.05);
  }

  function ensureAudio() {
    if (!audioCtx) audioCtx = new (window.AudioContext || window.webkitAudioContext)();
  }

  // Synthesized placeholder jingles — swap for real audio assets later.
  function playFanfare() {
    ensureAudio();
    [[523.25, 0, 0.12], [659.25, 0.12, 0.12], [783.99, 0.24, 0.12], [1046.5, 0.36, 0.5]]
      .forEach(([f, s, d]) => beep(f, s, d, 'triangle', 0.2));
  }

  function playWomp() {
    ensureAudio();
    [[233.08, 0, 0.28], [220, 0.3, 0.28], [207.65, 0.6, 0.7]]
      .forEach(([f, s, d]) => beep(f, s, d, 'sawtooth', 0.12));
  }

  function burstConfetti() {
    const colors = ['#f8d34a', '#e0533d', '#4a9de0', '#67c76a', '#c76ac2'];
    for (let i = 0; i < 28; i++) {
      const p = document.createElement('span');
      p.className = 'confetti';
      const angle = Math.random() * 2 * Math.PI;
      const dist = 90 + Math.random() * 130;
      p.style.setProperty('--dx', `${Math.cos(angle) * dist}px`);
      p.style.setProperty('--dy', `${Math.sin(angle) * dist - 60}px`);
      p.style.background = colors[i % colors.length];
      p.style.animationDelay = `${Math.random() * 120}ms`;
      rollBox.appendChild(p);
      setTimeout(() => p.remove(), 1700);
    }
  }

  function showRoll(payload) {
    clearTimeout(rollHideTimer);
    clearInterval(rollCycleTimer);
    const crit = payload.crit || '';
    rollBox.className = 'rolling';
    rollBox.innerHTML = `
      <div class="die"><span class="die-value">?</span></div>
      <div class="roll-math"></div>
      <div class="roll-name">${escapeHTML(payload.name || '')}</div>
    `;
    const valueEl = rollBox.querySelector('.die-value');
    const mathEl = rollBox.querySelector('.roll-math');
    let ticks = 0;
    rollCycleTimer = setInterval(() => {
      ticks++;
      valueEl.textContent = 1 + Math.floor(Math.random() * 20);
      if (ticks >= 12) {
        clearInterval(rollCycleTimer);
        valueEl.textContent = payload.roll;
        mathEl.textContent = `${payload.roll} + ${payload.level} = ${payload.total}`;
        rollBox.className = crit ? `settled ${crit}` : 'settled';
        if (crit === 'nat20') {
          playFanfare();
          burstConfetti();
        } else if (crit === 'nat1') {
          playWomp();
        }
        rollHideTimer = setTimeout(() => {
          rollBox.className = '';
          rollBox.innerHTML = '';
        }, crit ? 8000 : 6000);
      }
    }, 90);
  }

  const ttsQueue = [];
  let ttsPlaying = false;

  function playNextTTS() {
    const next = ttsQueue.shift();
    if (!next) {
      ttsPlaying = false;
      return;
    }
    ttsPlaying = true;
    const audio = new Audio(next.url);
    audio.onended = playNextTTS;
    audio.onerror = playNextTTS;
    audio.play().catch(playNextTTS);
  }

  const emoteStores = {
    bttv: { global: new Map(), channels: new Map(), globalLoaded: false },
    ffz: { global: new Map(), channels: new Map(), globalLoaded: false },
    seventv: { global: new Map(), channels: new Map(), globalLoaded: false },
  };

  function escapeHTML(str) {
    return str
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;');
  }

  function appendChatLine(html) {
    const line = document.createElement('div');
    line.className = 'chat-line';
    line.innerHTML = html;
    if (chatBox.firstChild) {
      chatBox.insertBefore(line, chatBox.firstChild);
    } else {
      chatBox.appendChild(line);
    }
  }

  function setOtherContent(html) {
    otherBox.innerHTML = `<div class="other-content">${html || ''}</div>`;
  }

  async function loadGlobalEmotes() {
    await Promise.all([
      loadBTTVGlobal(),
      loadFFZGlobal(),
      loadSevenTVGlobal(),
    ]);
  }

  function getMap(store, channelId) {
    if (!channelId) return store.global;
    if (!store.channels.has(channelId)) {
      store.channels.set(channelId, new Map());
    }
    return store.channels.get(channelId);
  }

  async function loadBTTVGlobal() {
    if (emoteStores.bttv.globalLoaded) return;
    try {
      const res = await fetch('https://api.betterttv.net/3/cached/emotes/global');
      if (!res.ok) throw new Error('bttv global');
      const data = await res.json();
      data.forEach((emote) => {
        emoteStores.bttv.global.set(emote.code, `https://cdn.betterttv.net/emote/${emote.id}/2x`);
      });
      emoteStores.bttv.globalLoaded = true;
    } catch (err) {
      console.warn('BTTV global emotes failed', err);
    }
  }

  async function loadBTTVChannel(channelId) {
    if (!channelId || emoteStores.bttv.channels.has(channelId)) return;
    try {
      const res = await fetch(`https://api.betterttv.net/3/cached/users/twitch/${channelId}`);
      if (!res.ok) throw new Error('bttv channel');
      const data = await res.json();
      const map = getMap(emoteStores.bttv, channelId);
      [...(data.channelEmotes || []), ...(data.sharedEmotes || [])].forEach((emote) => {
        map.set(emote.code, `https://cdn.betterttv.net/emote/${emote.id}/2x`);
      });
    } catch (err) {
      console.warn('BTTV channel emotes failed', err);
    }
  }

  async function loadFFZGlobal() {
    if (emoteStores.ffz.globalLoaded) return;
    try {
      const res = await fetch('https://api.frankerfacez.com/v1/set/global');
      if (!res.ok) throw new Error('ffz global');
      const data = await res.json();
      const defaultSets = data.default_sets || [];
      defaultSets.forEach((setId) => {
        const set = data.sets?.[setId];
        (set?.emoticons || []).forEach((emote) => {
          const url = emote.urls['2'] || emote.urls['1'];
          if (url) {
            emoteStores.ffz.global.set(emote.name, url.startsWith('http') ? url : `https:${url}`);
          }
        });
      });
      emoteStores.ffz.globalLoaded = true;
    } catch (err) {
      console.warn('FFZ global emotes failed', err);
    }
  }

  async function loadFFZChannel(channelId) {
    if (!channelId || emoteStores.ffz.channels.has(channelId)) return;
    try {
      const res = await fetch(`https://api.frankerfacez.com/v1/room/id/${channelId}`);
      if (!res.ok) throw new Error('ffz channel');
      const data = await res.json();
      const map = getMap(emoteStores.ffz, channelId);
      const setId = data.room?.set;
      const set = data.sets?.[setId];
      (set?.emoticons || []).forEach((emote) => {
        const url = emote.urls['2'] || emote.urls['1'];
        if (url) {
          map.set(emote.name, url.startsWith('http') ? url : `https:${url}`);
        }
      });
    } catch (err) {
      console.warn('FFZ channel emotes failed', err);
    }
  }

  async function loadSevenTVGlobal() {
    if (emoteStores.seventv.globalLoaded) return;
    try {
      const res = await fetch('https://7tv.io/v3/emote-sets/global');
      if (!res.ok) throw new Error('7tv global');
      const data = await res.json();
      (data.emotes || []).forEach((emote) => {
        emoteStores.seventv.global.set(emote.name, `https://cdn.7tv.app/emote/${emote.id}/2x.webp`);
      });
      emoteStores.seventv.globalLoaded = true;
    } catch (err) {
      console.warn('7TV global emotes failed', err);
    }
  }

  async function loadSevenTVChannel(channelId) {
    if (!channelId || emoteStores.seventv.channels.has(channelId)) return;
    try {
      const res = await fetch(`https://7tv.io/v3/users/twitch/${channelId}`);
      if (!res.ok) throw new Error('7tv channel');
      const data = await res.json();
      const map = getMap(emoteStores.seventv, channelId);
      (data.emote_set?.emotes || []).forEach((emote) => {
        map.set(emote.name, `https://cdn.7tv.app/emote/${emote.id}/2x.webp`);
      });
    } catch (err) {
      console.warn('7TV channel emotes failed', err);
    }
  }

  function lookupThirdPartyEmote(code, channelId) {
    return (
      getMap(emoteStores.bttv, channelId).get(code) ||
      getMap(emoteStores.ffz, channelId).get(code) ||
      getMap(emoteStores.seventv, channelId).get(code) ||
      emoteStores.bttv.global.get(code) ||
      emoteStores.ffz.global.get(code) ||
      emoteStores.seventv.global.get(code)
    );
  }

  function renderTextWithThirdParty(text, channelId) {
    if (!text) return '';
    return text
      .split(/(\s+)/)
      .map((part) => {
        if (/^\s+$/.test(part)) return part;
        const emoteUrl = lookupThirdPartyEmote(part, channelId);
        if (emoteUrl) {
          return `<img class="emote emote-third-party" src="${emoteUrl}" alt="${escapeHTML(part)}" />`;
        }
        return escapeHTML(part);
      })
      .join('');
  }

  function renderMessage(payload) {
    const user = `<span class="username">${escapeHTML(payload.username || 'user')}</span>:`;

    const fragmentHTML = (payload.fragments || []).map((frag) => {
      if (frag.type === 'emote' && frag.emote_url) {
        return ` <img class="emote" src="${frag.emote_url}" alt="${escapeHTML(frag.text || '')}" />`;
      }
      if (frag.type === 'text') {
        return ` <span class="text">${renderTextWithThirdParty(frag.text || '', payload.channel_id)}</span>`;
      }
      return '';
    }).join('');

    const bodyHTML = fragmentHTML || ` <span class="text">${renderTextWithThirdParty(payload.message || '', payload.channel_id)}</span>`;
    return `${user}${bodyHTML}`.trim();
  }

  async function ensureChannelEmotes(channelId) {
    await Promise.all([
      loadBTTVChannel(channelId),
      loadFFZChannel(channelId),
      loadSevenTVChannel(channelId),
    ]);
  }

  function connect() {
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const socket = new WebSocket(`${protocol}//${window.location.host}/ws/overlay`);

    socket.addEventListener('message', async (event) => {
      try {
        const payload = JSON.parse(event.data);
        if (payload.type === 'chat.message') {
          await ensureChannelEmotes(payload.channel_id);
          appendChatLine(renderMessage(payload));
          return;
        }

        if (payload.type === 'other.update') {
          if (payload.mode === 'base') {
            otherBaseHTML = payload.html || '';
            setOtherContent(payload.html || '');
          } else if (payload.mode === 'announcement') {
            setOtherContent(payload.html || '');
          } else if (payload.mode === 'force_restore' || payload.mode === 'base_restore') {
            setOtherContent(payload.html || otherBaseHTML);
          }
          pongActive = false;
          return;
        }

        if (payload.type === 'other.pong_start') {
          pongActive = true;
          setOtherContent(payload.html || '');
          return;
        }

        if (payload.type === 'other.pong_frame' && pongActive) {
          setOtherContent(payload.html || '');
        }

        if (payload.type === 'party.update') {
          renderPartyCards(payload.members);
          partyNames = new Set((payload.members || []).map((m) => (m.name || '').toLowerCase()));
          for (const key of [...tavernDudes.keys()]) {
            if (partyNames.has(key)) removeDude(key);
          }
        }

        if (payload.type === 'tavern.roster') {
          renderTavern(payload.dudes);
        }

        if (payload.type === 'tts.play') {
          ttsQueue.push(payload);
          if (!ttsPlaying) playNextTTS();
        }

        if (payload.type === 'roll.result') {
          showRoll(payload);
        }
      } catch (err) {
        console.error('Failed to parse message', err);
      }
    });

    socket.addEventListener('close', () => {
      setTimeout(connect, 2000);
    });

    socket.addEventListener('error', () => {
      socket.close();
    });
  }

  loadGlobalEmotes().finally(connect);
})();
