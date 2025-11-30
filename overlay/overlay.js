(function () {
  const chatBox = document.getElementById('chat-box');

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
    chatBox.appendChild(line);
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
    const badges = (payload.badges || [])
      .map((badge) => {
        if (badge.image_url) {
          const alt = escapeHTML(badge.set_id || badge.id || 'badge');
          return `<img class="badge" src="${badge.image_url}" alt="${alt}" />`;
        }
        if (badge.set_id) return `<span class="badge">[${escapeHTML(badge.set_id)}]</span>`;
        if (badge.id) return `<span class="badge">[${escapeHTML(badge.id)}]</span>`;
        return '';
      })
      .filter(Boolean)
      .join(' ');

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
    const prefix = badges ? `${badges} ${user}` : user;
    return `${prefix}${bodyHTML}`.trim();
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
